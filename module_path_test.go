package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

// Module path check: go.mod's module directive is both the import path of this
// source tree and the VCS location its packages are fetched from, so it has to
// name the same repository the project builds and publishes from.
//
// The reviewed release_hygiene defect: go.mod declared
// `module github.com/redis-master-label` - the import path of the GitHub account
// itself - while every other location named the repository:
// the shipped image's org.opencontainers.image.source label in the build file
// ("https://github.com/redis-master-label/redis-master-label"), the pinned
// release image reference (releaseImage in main_test.go, and the same reference
// in manifests/*.yaml) and the README release section. A module path a proxy
// lookup resolves to a different repository makes `go get` of this project, the
// module cache entry and any SBOM/purl metadata derived from the path point at a
// repository that is not the released one. Nothing in the suite could see it:
// no source file imports the module path, and the workflow checker
// (workflow_check.go) reads the module file's `go` directive only.
//
// This file reads the declared path back and compares it with the repository
// location the build file and the pinned release image name, so editing either
// side alone fails `go test -count=1 ./...` - which CI and release.yml both run
// before they build or publish anything.

// moduleFile is the module file whose first `module` directive is the declared
// import path of this source tree.
const moduleFile = "go.mod"

// goModuleDirectiveRe matches the module directive of a module file and captures
// the path it declares.
var goModuleDirectiveRe = regexp.MustCompile(`(?m)^module[ \t]+([^" \t\r\n]+)[ \t]*$`)

// goModulePath returns the module path declared by the module file at path. An
// empty result with an error means the path could not be read, which is itself a
// failure: the import path of the tree is then unknown.
func goModulePath(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	m := goModuleDirectiveRe.FindStringSubmatch(string(data))
	if m == nil {
		return "", fmt.Errorf("%s: no module directive found, so the import path of this source tree cannot be read", path)
	}
	return strings.Trim(m[1], `"`), nil
}

// imageSourceRepoPath returns the repository path (host/owner/name) an OCI
// image source URL names, for example
// "https://github.com/redis-master-label/redis-master-label" gives
// "github.com/redis-master-label/redis-master-label". The value is compared with
// the module path, so the scheme and any trailing "/" or ".git" suffix - the
// spellings a source label may legitimately use - are stripped first.
func imageSourceRepoPath(source string) (string, error) {
	target := strings.TrimSpace(source)
	if target == "" {
		return "", fmt.Errorf("the shipping image's %s label is empty (or missing), so the repository this project publishes from cannot be read", imageSourceLabel)
	}
	target = strings.TrimSuffix(target, "/")
	target = strings.TrimSuffix(target, ".git")
	for _, scheme := range []string{"git+https://", "git+ssh://", "https://", "http://", "git://"} {
		if rest, ok := strings.CutPrefix(target, scheme); ok {
			target = rest
			break
		}
	}
	if !strings.Contains(target, "/") {
		return "", fmt.Errorf("the shipping image's %s label %q does not name a repository (host/owner/name)", imageSourceLabel, source)
	}
	return target, nil
}

// imageSourceLabel is the OCI label naming the repository the shipped image was
// built from. A module path names a repository too, so the two must agree.
const imageSourceLabel = "org.opencontainers.image.source"

// imageRefRepoPath returns the repository path of a registry-qualified container
// image reference, for example
// "ghcr.io/redis-master-label/redis-master-label:v0.1.0" gives
// "redis-master-label/redis-master-label". The registry host is dropped because
// the module path lives on the code host (github.com) while the release image is
// published to the container registry (ghcr.io); the account/repository path
// after the host is what the two have to agree on.
func imageRefRepoPath(ref string) (string, error) {
	name := strings.TrimSpace(ref)
	if i := strings.IndexByte(name, '@'); i >= 0 {
		name = name[:i]
	}
	if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		name = name[:i]
	}
	parts := strings.Split(name, "/")
	if len(parts) < 3 {
		return "", fmt.Errorf("container image reference %q is not registry-qualified (host/owner/name:tag), so the repository it is published from cannot be read", ref)
	}
	return strings.Join(parts[1:], "/"), nil
}

// modulePathProblems returns one message per way the declared module path
// contradicts a repository location this project builds and publishes from, or
// nil when every location agrees. The source label and the release image
// references are read from the files rather than assumed, so a declared path
// that points at a different repository - or a release reference that moved to
// one - is reported instead of passing.
func modulePathProblems(modulePath, imageSource string, releasedImages []string) []string {
	var problems []string

	if strings.TrimSpace(modulePath) == "" {
		problems = append(problems, fmt.Sprintf("%s: no module path declared, so the import path of this source tree cannot be read", moduleFile))
		return problems
	}

	want, err := imageSourceRepoPath(imageSource)
	if err != nil {
		problems = append(problems, err.Error())
		return problems
	}
	if modulePath != want {
		problems = append(problems, fmt.Sprintf("%s declares module path %q, but the shipping image's %s label names repository %q: the module path is both the import path and the VCS location, so it has to name the repository the project builds and publishes from", moduleFile, modulePath, imageSourceLabel, want))
	}

	// The release image is published to a container registry, so only the
	// account/repository path after the code host is comparable: the module path
	// "github.com/redis-master-label/redis-master-label" and the image reference
	// "ghcr.io/redis-master-label/redis-master-label:v0.1.0" share
	// "redis-master-label/redis-master-label".
	wantImagePath := want
	if i := strings.Index(want, "/"); i >= 0 {
		wantImagePath = want[i+1:]
	}

	if len(releasedImages) == 0 {
		problems = append(problems, "no container image reference found in the manifests or the release pin, so the repository the released image is published from cannot be read and the module path cannot be confirmed against it")
		return problems
	}
	for _, ref := range releasedImages {
		repo, err := imageRefRepoPath(ref)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		if repo != wantImagePath {
			problems = append(problems, fmt.Sprintf("release image reference %q names repository %q, but the shipping image's %s label names %q: the module path has to name the repository both describe", ref, repo, imageSourceLabel, want))
		}
	}

	return problems
}

// The reviewed release_hygiene defect (go.mod declared the GitHub account's own
// path) is invisible to every other check, so this test reads the declared path
// and the repository locations the project ships from. `go test -count=1 ./...`
// - run by CI and by release.yml before build or publish - fails when the module
// path and those locations stop naming the same repository.
func TestModulePathMatchesImageSourceAndReleaseImage(t *testing.T) {
	modulePath, err := goModulePath(moduleFile)
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

	manifestImages, sidecars, err := sidecarImages(manifestDir)
	if err != nil {
		t.Fatal(err)
	}
	if sidecars == 0 {
		t.Fatalf("no labeler container found across %s: the released image the example manifests run is not visible to this check, so a module path pointing elsewhere would pass", manifestDir)
	}

	refs := append([]string{releaseImage}, manifestImages...)
	if problems := modulePathProblems(modulePath, labels[imageSourceLabel], refs); len(problems) != 0 {
		t.Fatalf("module path does not name the repository this project is released from:\n\t%s", strings.Join(problems, "\n\t"))
	}
}

// TestModulePathProblems pins the reader above against fixtures of the real
// values with one edit each: the reviewed bare-account path, a path naming
// another repository, a source label naming another repository, a release
// reference on another repository, an unqualified image reference, a missing
// source label and a missing release reference must all be reported, while the
// spellings a source label may legitimately use must not be.
func TestModulePathProblems(t *testing.T) {
	const (
		goodSource = "https://github.com/redis-master-label/redis-master-label"
		goodImage  = "ghcr.io/redis-master-label/redis-master-label:v0.1.0"
		goodModule = "github.com/redis-master-label/redis-master-label"
	)

	tests := []struct {
		name        string
		modulePath  string
		source      string
		images      []string
		wantProblem string
	}{
		{
			name:       "declared path matches the source label and the release image",
			modulePath: goodModule,
			source:     goodSource,
			images:     []string{goodImage},
		},
		{
			name:        "bare account path is the reviewed release_hygiene defect",
			modulePath:  "github.com/redis-master-label",
			source:      goodSource,
			images:      []string{goodImage},
			wantProblem: "module path",
		},
		{
			name:        "module path names another repository",
			modulePath:  "github.com/example/other",
			source:      goodSource,
			images:      []string{goodImage},
			wantProblem: "module path",
		},
		{
			name:        "source label names another repository",
			modulePath:  goodModule,
			source:      "https://github.com/example/other",
			images:      []string{goodImage},
			wantProblem: "module path",
		},
		{
			name:        "release image names another repository",
			modulePath:  goodModule,
			source:      goodSource,
			images:      []string{"ghcr.io/example/other:v0.1.0"},
			wantProblem: "release image",
		},
		{
			name:        "unqualified image reference",
			modulePath:  goodModule,
			source:      goodSource,
			images:      []string{"redis-master-label:latest"},
			wantProblem: "registry-qualified",
		},
		{
			name:        "no release image reference",
			modulePath:  goodModule,
			source:      goodSource,
			wantProblem: "no container image",
		},
		{
			name:        "missing source label",
			modulePath:  goodModule,
			images:      []string{goodImage},
			wantProblem: imageSourceLabel,
		},
		{
			name:        "empty module path",
			modulePath:  "",
			source:      goodSource,
			images:      []string{goodImage},
			wantProblem: "no module path",
		},
		{
			name:       "source label with .git suffix",
			modulePath: goodModule,
			source:     goodSource + ".git",
			images:     []string{goodImage},
		},
		{
			name:       "source label without scheme or trailing slash",
			modulePath: goodModule,
			source:     "github.com/redis-master-label/redis-master-label/",
			images:     []string{goodImage},
		},
		{
			name:       "pinned release image is read without its tag",
			modulePath: goodModule,
			source:     goodSource,
			images:     []string{"ghcr.io/redis-master-label/redis-master-label@sha256:0000000000000000000000000000000000000000000000000000000000000000"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			problems := modulePathProblems(tt.modulePath, tt.source, tt.images)
			if tt.wantProblem == "" {
				if len(problems) != 0 {
					t.Fatalf("modulePathProblems(%q, %q, %v) reported %v, want no problem", tt.modulePath, tt.source, tt.images, problems)
				}
				return
			}
			if len(problems) == 0 {
				t.Fatalf("modulePathProblems(%q, %q, %v) reported no problem, want one mentioning %q", tt.modulePath, tt.source, tt.images, tt.wantProblem)
			}
			joined := strings.Join(problems, "\n")
			if !strings.Contains(joined, tt.wantProblem) {
				t.Fatalf("modulePathProblems(%q, %q, %v) reported:\n%s\nwant a problem mentioning %q", tt.modulePath, tt.source, tt.images, joined, tt.wantProblem)
			}
		})
	}
}
