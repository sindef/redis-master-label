package main

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Module identity check: a Go module path is the import and install identity of
// the code. `go install <module>@latest`, a module proxy fetch and every
// downstream `import "<module>"` resolve against it, so it has to name the same
// repository as the OCI source label and the image published from this
// repository. The reviewed defect was `module github.com/redis-master-label` in
// go.mod: the path named a GitHub account - a code host and an owner - and not a
// repository, so `go install github.com/redis-master-label@latest` and
// `import "github.com/redis-master-label"` resolve against a VCS path that does
// not exist, and `go mod` cannot report which repository the code came from.
// Every other identity the project declares names the repository: the build
// file's org.opencontainers.image.source label is
// https://github.com/redis-master-label/redis-master-label, release.yml's IMAGE
// is ghcr.io/${{ github.repository }}, and the example manifests, the README and
// main_test.go's releaseImage all pin
// ghcr.io/redis-master-label/redis-master-label:<tag>.
//
// The check keeps three identities in agreement:
//
//   - go.mod's module path,
//   - the build file's org.opencontainers.image.source label (the pointer from
//     the published image back to the repository it was built from),
//   - the released image reference (releaseImage, the pin the example manifests
//     and the README share).
//
// With those three naming one repository, a module path that drifts - a renamed
// owner, an account-only path, another repository - fails `go test -count=1
// ./...`, which CI and release.yml run before anything is built or published.

// imageSourceLabel is the OCI image annotation naming the repository the image
// was built from. Its value is a repository URL, so it can be compared with the
// module path.
const imageSourceLabel = "org.opencontainers.image.source"

// githubHost is the code host this repository lives on, so the prefix of its
// module path, and ghcrRegistry is the registry the release workflow publishes
// the image to. Both are fixed by the identities the project already declares.
const (
	githubHost   = "github.com"
	ghcrRegistry = "ghcr.io"
)

// moduleMajorVersionRe matches the major-version element a v2+ module path
// carries after the repository element, for example the `v2` in
// github.com/owner/repository/v2.
var moduleMajorVersionRe = regexp.MustCompile(`^v([0-9]+)$`)

// goModModulePath returns the module path declared by the `module` directive of
// the module file at path.
func goModModulePath(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		if fields := strings.Fields(line); len(fields) == 2 && fields[0] == "module" {
			return fields[1], nil
		}
	}
	return "", fmt.Errorf("%s: no module directive declaring the module path", path)
}

// moduleRepositoryPath returns the <host>/<owner>/<repository> prefix of a
// module path. A v2+ module path ends in a major-version element
// (github.com/owner/repository/v2) but still lives in the same repository, so
// that suffix is dropped instead of being read as another repository. Any other
// element count - notably `github.com/owner`, which names a code host and an
// account but no repository at all - is an error.
func moduleRepositoryPath(modulePath string) (string, error) {
	parts := strings.Split(strings.Trim(strings.TrimSpace(modulePath), "/"), "/")
	if len(parts) == 4 {
		if m := moduleMajorVersionRe.FindStringSubmatch(parts[3]); m != nil {
			if major, err := strconv.Atoi(m[1]); err == nil && major >= 2 {
				parts = parts[:3]
			}
		}
	}
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", fmt.Errorf("module path %q does not name a <host>/<owner>/<repository> repository", modulePath)
	}
	return strings.Join(parts, "/"), nil
}

// sourceLabelModulePath converts an org.opencontainers.image.source label value
// into the module path it names, so the label and go.mod can be compared as
// strings: "https://github.com/o/r" gives "github.com/o/r" (a scheme and a
// trailing "/" or ".git" are the ways a repository URL is written). A label that
// is empty, names a host other than github.com, or names no repository is an
// error: the image is published from this repository on GitHub, so the label is
// the pointer users follow back to the code.
func sourceLabelModulePath(label string) (string, error) {
	ref := strings.TrimSpace(label)
	if ref == "" {
		return "", fmt.Errorf("the %s label is empty, so the repository the image was built from cannot be read", imageSourceLabel)
	}
	lower := strings.ToLower(ref)
	for _, scheme := range []string{"https://", "http://"} {
		if strings.HasPrefix(lower, scheme) {
			ref = ref[len(scheme):]
			break
		}
	}
	ref = strings.TrimSuffix(strings.Trim(ref, "/"), ".git")
	parts := strings.Split(ref, "/")
	if len(parts) != 3 || parts[0] != githubHost || parts[1] == "" || parts[2] == "" {
		return "", fmt.Errorf("the %s label names %q, want %s/<owner>/<repository> pointing at the repository the code lives in", imageSourceLabel, label, githubHost)
	}
	return ref, nil
}

// imageRepositoryPath returns the <owner>/<repository> path of a published image
// reference: ghcr.io/o/r:v1.2.3 and ghcr.io/o/r@sha256:... both give "o/r". The
// registry must be ghcr.io (release.yml pushes there) and the path must name a
// repository, so an image on another registry or an owner-only reference cannot
// stand in for the code's identity.
func imageRepositoryPath(ref string) (string, error) {
	image := strings.TrimSpace(ref)
	if i := strings.Index(image, "@"); i >= 0 {
		image = image[:i]
	}
	// A tag is the colon after the last slash; a colon before it belongs to a
	// port in the registry name.
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		image = image[:i]
	}
	parts := strings.Split(image, "/")
	if len(parts) != 3 || parts[0] != ghcrRegistry || parts[1] == "" || parts[2] == "" {
		return "", fmt.Errorf("image reference %q does not name a %s/<owner>/<repository> repository, so the image and the module path cannot be compared", ref, ghcrRegistry)
	}
	return parts[1] + "/" + parts[2], nil
}

// moduleIdentityProblems returns one message per disagreement among the module
// path, the image source label and the published image reference, so a caller
// fails the pull request instead of shipping identities that disagree about
// which repository the code is. An empty result means all three name the same
// repository. When the module path itself names no repository the comparisons
// against it are meaningless, so only that problem is reported.
func moduleIdentityProblems(modulePath, sourceLabel, imageRef string) []string {
	repoPath, err := moduleRepositoryPath(modulePath)
	if err != nil {
		declared := strings.TrimSpace(modulePath)
		return []string{fmt.Sprintf(
			"%s declares module %q, which names no repository: a module path is the import and install identity of the code, so `go install %s@latest` and `import %q` resolve against a VCS path that does not exist; declare %s/<owner>/<repository>",
			goToolchainFile, declared, declared, declared, githubHost)}
	}

	var problems []string
	labelPath, err := sourceLabelModulePath(sourceLabel)
	switch {
	case err != nil:
		problems = append(problems, err.Error())
	case repoPath != labelPath:
		problems = append(problems, fmt.Sprintf(
			"%s declares module %q, but the build file's %s label names %q: the module path is the code's import/install identity and the label points at the repository the image was built from, so both must name the same repository (change one, not the other)",
			goToolchainFile, repoPath, imageSourceLabel, labelPath))
	}

	imageRepo, err := imageRepositoryPath(imageRef)
	switch {
	case err != nil:
		problems = append(problems, err.Error())
	default:
		if want := githubHost + "/" + imageRepo; repoPath != want {
			problems = append(problems, fmt.Sprintf(
				"%s declares module %q, but the released image %q is built from %s/%s: the module path and the image users pull must name the same repository (change the module path and the image reference in the same change)",
				goToolchainFile, repoPath, strings.TrimSpace(imageRef), ghcrRegistry, imageRepo))
		}
	}
	return problems
}

// The shipped module path, the build file's source label and the released image
// reference must name one repository: go.mod's `module` directive is the
// identity `go install`, a proxy fetch and every downstream import resolve, so a
// module path naming a different repository - or naming an owner with no
// repository, as `github.com/redis-master-label` did - fails here.
func TestModulePathNamesTheRepositoryOfTheImageIdentities(t *testing.T) {
	modulePath, err := goModModulePath(goToolchainFile)
	if err != nil {
		t.Fatal(err)
	}
	buildFile, err := locateBuildFile()
	if err != nil {
		t.Fatal(err)
	}
	labels, err := dockerfileFinalStageLabels(buildFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, problem := range moduleIdentityProblems(modulePath, labels[imageSourceLabel], releaseImage) {
		t.Errorf("%s", problem)
	}
}

// The failure modes the check has to see, and the shapes it must accept: the
// shipped identities, a v2 module path in the same repository, a label written
// with or without a scheme and a ".git" suffix, and a digest-pinned image. The
// fixtures pin the reviewed defect (an owner-only module path) and each way one
// identity can drift from the others.
func TestModuleIdentityProblems_FailureModes(t *testing.T) {
	const repo = "redis-master-label/redis-master-label"
	module := githubHost + "/" + repo
	label := "https://" + githubHost + "/" + repo
	image := ghcrRegistry + "/" + repo + ":v0.1.0"

	tests := []struct {
		name         string
		module       string
		label        string
		image        string
		wantProblems int
		want         []string
	}{
		{
			name:   "the shipped identities",
			module: module,
			label:  label,
			image:  image,
		},
		{
			name:   "v2 module path in the same repository",
			module: module + "/v2",
			label:  label,
			image:  image,
		},
		{
			name:   "label without a scheme and with a .git suffix",
			module: module,
			label:  githubHost + "/" + repo + ".git",
			image:  image,
		},
		{
			name:   "digest-pinned image reference",
			module: module,
			label:  label,
			image:  ghcrRegistry + "/" + repo + "@sha256:0000000000000000000000000000000000000000000000000000000000000000",
		},
		{
			name:         "reviewed defect: module path names an owner, not a repository",
			module:       githubHost + "/redis-master-label",
			label:        label,
			image:        image,
			wantProblems: 1,
			want:         []string{"names no repository", "go install github.com/redis-master-label@latest"},
		},
		{
			name:         "module path is a bare name",
			module:       "redis-master-label",
			label:        label,
			image:        image,
			wantProblems: 1,
			want:         []string{"names no repository"},
		},
		{
			name:         "module path names another repository",
			module:       githubHost + "/redis-master-label/labeler",
			label:        label,
			image:        image,
			wantProblems: 2,
			want:         []string{imageSourceLabel + " label names", "released image"},
		},
		{
			name:         "label names another repository",
			module:       module,
			label:        "https://" + githubHost + "/redis-master-label/labeler",
			image:        image,
			wantProblems: 1,
			want:         []string{imageSourceLabel + " label names"},
		},
		{
			name:         "label missing",
			module:       module,
			label:        "",
			image:        image,
			wantProblems: 1,
			want:         []string{imageSourceLabel + " label is empty"},
		},
		{
			name:         "label on another code host",
			module:       module,
			label:        "https://gitlab.com/redis-master-label/redis-master-label",
			image:        image,
			wantProblems: 1,
			want:         []string{"want github.com/<owner>/<repository>"},
		},
		{
			name:         "image on another registry",
			module:       module,
			label:        label,
			image:        "docker.io/redis-master-label/redis-master-label:v0.1.0",
			wantProblems: 1,
			want:         []string{"does not name a ghcr.io/<owner>/<repository>"},
		},
		{
			name:         "image names an owner, not a repository",
			module:       module,
			label:        label,
			image:        ghcrRegistry + "/redis-master-label:v0.1.0",
			wantProblems: 1,
			want:         []string{"does not name a ghcr.io/<owner>/<repository>"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			problems := moduleIdentityProblems(tt.module, tt.label, tt.image)
			if len(problems) != tt.wantProblems {
				t.Fatalf("problems = %v, want %d", problems, tt.wantProblems)
			}
			joined := strings.Join(problems, "\n")
			for _, want := range tt.want {
				if !strings.Contains(joined, want) {
					t.Errorf("problems = %v, want one naming %q", problems, want)
				}
			}
		})
	}
}

// The reader must find the directive among the rest of the module file, and an
// empty or unreadable declaration must be an error rather than an empty module
// path that silently disagrees with nothing.
func TestGoModModulePath_ReadsModuleDirective(t *testing.T) {
	dir := t.TempDir()
	write := func(name, text string) string {
		path := dir + string(os.PathSeparator) + name
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	withRest := write("go.mod", "// module github.com/commented/out\nmodule "+githubHost+"/redis-master-label/redis-master-label // primary module\n\ngo 1.27.0\n")
	got, err := goModModulePath(withRest)
	if err != nil {
		t.Fatal(err)
	}
	if want := githubHost + "/redis-master-label/redis-master-label"; got != want {
		t.Errorf("goModModulePath = %q, want %q", got, want)
	}

	missing := write("missing.mod", "go 1.27.0\n")
	if _, err := goModModulePath(missing); err == nil {
		t.Error("module file without a module directive accepted, want an error")
	}
	if _, err := goModModulePath(dir + string(os.PathSeparator) + "absent.mod"); err == nil {
		t.Error("absent module file accepted, want a read error")
	}
}
