package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Module identity check: go.mod's module path is the code's import and install
// identity, not a label. `go install <module path>@latest`, a module-proxy
// fetch and every downstream `import` resolve against the VCS path the module
// path names, so that path has to name the repository the rest of the project
// names. The reviewed defect was `module github.com/redis-master-label`: it
// names the GitHub account, not the repository, so the path resolved to no
// repository at all - `go install github.com/redis-master-label@latest` and
// `import "github.com/redis-master-label"` both address a VCS path that does
// not exist - while the shipping image declared
// org.opencontainers.image.source="https://github.com/redis-master-label/redis-master-label",
// the release workflow published ghcr.io/<repository>
// (.github/workflows/release.yml:26) and main_test.go pinned the manifest image
// ghcr.io/redis-master-label/redis-master-label:<tag>. Three identities for one
// project, two of them naming the repository and the module path naming only
// the account.
//
// The three identities are now read from their own files and compared, so the
// module path cannot drift back to an account-only path (or to another
// repository) without failing `go test -count=1 ./...`, which both CI and
// release.yml run before they build or publish anything:
//
//   - go.mod's module directive,
//   - the OCI source label of the build file's final stage,
//   - the releaseImage pin the example manifests must match,
//   - the ghcr.io/<owner>/<repository> reference the README documents.

// imageSourceLabel is the OCI label naming the repository the image's source
// came from. Its value must name the same repository as go.mod's module path:
// the label tells a puller where the code came from and the module path is
// where a Go tool fetches it, so the two disagreeing means one of them is a
// dead reference.
const imageSourceLabel = "org.opencontainers.image.source"

var (
	// goModModulePathRe captures the import path of the module file's module
	// directive.
	goModModulePathRe = regexp.MustCompile(`(?m)^module[ \t]+([^ \t\r\n]+)[ \t]*$`)

	// sourceRepositoryRe matches an OCI source label's repository URL and
	// captures host, owner and repository, with an optional .git suffix and
	// trailing slash dropped.
	sourceRepositoryRe = regexp.MustCompile(`^https?://([^/]+)/([^/]+)/([^/?#]+?)(?:\.git)?/?$`)

	// imageRepositoryRe matches a registry-qualified image reference
	// (<registry>/<owner>/<repository>[:<tag>][@<digest>]) and captures the
	// three path elements. A reference without a registry does not match: the
	// repository publishing a released image has to be readable from the file.
	imageRepositoryRe = regexp.MustCompile(`^(?:([^/@]+)/)?([^/:@]+)/([^/:@]+)(?::[^/@]+)?(?:@[^/]+)?$`)

	// documentedImageRepositoryRe finds ghcr.io/<owner>/<repository> references
	// in prose. The README's `ghcr.io/<owner>/redis-master-label` placeholder
	// carries no owner name and does not match, so only real references count.
	documentedImageRepositoryRe = regexp.MustCompile(`ghcr\.io/([A-Za-z0-9._-]+)/([A-Za-z0-9._-]+)`)
)

// goModModulePath returns the import path declared by the module file's `module`
// directive. A module file without that directive names no import path, which
// is reported rather than treated as agreement with the other identities.
func goModModulePath(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	m := goModModulePathRe.FindStringSubmatch(string(data))
	if m == nil {
		return "", fmt.Errorf("%s: no `module <path>` directive, so the module's import/install identity cannot be read", path)
	}
	return m[1], nil
}

// repositoryIdentity is the host/owner/repository triple a reference names.
type repositoryIdentity struct {
	Host  string
	Owner string
	Repo  string
}

// String is the full host/owner/repository triple, named in failure messages so
// the disagreeing file is reported as a whole rather than by a bare element.
func (id repositoryIdentity) String() string {
	return id.Host + "/" + id.Owner + "/" + id.Repo
}

// repositoryRef is the <owner>/<repository> pair, the part of the identity that
// stays comparable across hosts: the module is fetched from GitHub while the
// image is published to GHCR, so the two names cannot be compared whole.
func (id repositoryIdentity) repositoryRef() string {
	return id.Owner + "/" + id.Repo
}

// modulePathRepository reads the repository a module path names. A module path
// is <host>/<owner>/<repository> with optional subpackages after it, so a path
// with fewer than three elements - `github.com/redis-master-label`, the
// reviewed defect - names no repository at all: it resolves to an account, and
// every fetch, import or `go install` of it fails.
func modulePathRepository(modulePath string) (repositoryIdentity, error) {
	parts := strings.Split(strings.Trim(strings.TrimSpace(modulePath), "/"), "/")
	if len(parts) < 3 {
		return repositoryIdentity{}, fmt.Errorf("module path %q names no repository: a module path is <host>/<owner>/<repository> (subpackages may follow), so an account-only path cannot be fetched, imported or installed", modulePath)
	}
	for _, part := range parts[:3] {
		if part == "" {
			return repositoryIdentity{}, fmt.Errorf("module path %q has an empty path element, so the repository it names cannot be read", modulePath)
		}
	}
	return repositoryIdentity{Host: parts[0], Owner: parts[1], Repo: parts[2]}, nil
}

// sourceLabelRepository reads the repository an OCI source label names, for
// example https://github.com/redis-master-label/redis-master-label.
func sourceLabelRepository(source string) (repositoryIdentity, error) {
	m := sourceRepositoryRe.FindStringSubmatch(strings.TrimSpace(source))
	if m == nil {
		return repositoryIdentity{}, fmt.Errorf("OCI source label %q does not name a repository URL of the form https://<host>/<owner>/<repository>", source)
	}
	return repositoryIdentity{Host: m[1], Owner: m[2], Repo: m[3]}, nil
}

// imageRepository reads the repository a registry-qualified image reference
// names, for example ghcr.io/redis-master-label/redis-master-label:v0.1.0. A
// reference without a registry is rejected: the repository publishing the image
// would not be readable from the file.
func imageRepository(image string) (repositoryIdentity, error) {
	m := imageRepositoryRe.FindStringSubmatch(strings.TrimSpace(image))
	if m == nil {
		return repositoryIdentity{}, fmt.Errorf("image reference %q is not <registry>/<owner>/<repository>[:<tag>], so the repository publishing it cannot be read", image)
	}
	if m[1] == "" {
		return repositoryIdentity{}, fmt.Errorf("image reference %q is not registry-qualified, so the repository publishing it cannot be read", image)
	}
	return repositoryIdentity{Host: m[1], Owner: m[2], Repo: m[3]}, nil
}

// documentedImageRepositories returns every GHCR repository reference in a
// README's text, so the image the release section documents is held to the same
// repository as the module path.
func documentedImageRepositories(readme string) []string {
	var refs []string
	for _, m := range documentedImageRepositoryRe.FindAllStringSubmatch(readme, -1) {
		refs = append(refs, m[1]+"/"+m[2])
	}
	return refs
}

// moduleIdentityProblems returns why the module path, the OCI source label and
// the pinned release image do not name the same repository. An unreadable
// identity is a problem too: a missing or malformed reference cannot be taken
// as agreement.
func moduleIdentityProblems(modulePath string, labels map[string]string, image string) []string {
	var problems []string

	moduleID, moduleErr := modulePathRepository(modulePath)
	if moduleErr != nil {
		problems = append(problems, moduleErr.Error())
	}

	source := strings.TrimSpace(labels[imageSourceLabel])
	sourceID, sourceErr := sourceLabelRepository(source)
	if sourceErr != nil {
		problems = append(problems, fmt.Sprintf("%s = %q: %v", imageSourceLabel, source, sourceErr))
	}

	imageID, imageErr := imageRepository(image)
	if imageErr != nil {
		problems = append(problems, fmt.Sprintf("releaseImage = %q: %v", image, imageErr))
	}

	if moduleErr == nil && sourceErr == nil && moduleID != sourceID {
		problems = append(problems, fmt.Sprintf(
			"go.mod module path names %s, but the %s label names %s: the module's import path must name the repository the image says the code comes from",
			moduleID, imageSourceLabel, sourceID))
	}
	if moduleErr == nil && imageErr == nil && moduleID.repositoryRef() != imageID.repositoryRef() {
		problems = append(problems, fmt.Sprintf(
			"go.mod module path names %s, but the pinned release image names %s: a fetch, import or `go install` of the module path must resolve to the repository that publishes the image",
			moduleID.repositoryRef(), imageID.repositoryRef()))
	}
	return problems
}

// The reviewed defect: go.mod declared `module github.com/redis-master-label`
// while the OCI source label, the release image and the README all named
// github.com/redis-master-label/redis-master-label. The three identities must
// agree, and the check reads each from its own file so none of them can be
// changed alone.
func TestGoModulePathNamesProjectRepository(t *testing.T) {
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

	for _, problem := range moduleIdentityProblems(modulePath, labels, releaseImage) {
		t.Errorf("%s", problem)
	}

	moduleID, err := modulePathRepository(modulePath)
	if err != nil {
		t.Fatal(err)
	}

	// The README documents the image users pull, which is a fourth statement of
	// the same repository. No reference at all means the README stopped naming
	// the published image, which is the same silence as a wrong one.
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	documented := documentedImageRepositories(string(readme))
	if len(documented) == 0 {
		t.Fatalf("README.md names no ghcr.io/<owner>/<repository> reference, so the published image's repository cannot be read")
	}
	for _, ref := range documented {
		if ref != moduleID.repositoryRef() {
			t.Errorf("README.md documents ghcr.io/%s, but go.mod's module path names %s: the documented image and the module's import identity must be the same repository", ref, moduleID.repositoryRef())
		}
	}
}

// The failure modes the identity check has to see: an account-only module path
// (the reviewed defect), a module path naming another repository or another
// host, a missing, malformed or mismatched OCI source label, and a pinned image
// that names another repository or no registry at all. The shipped shapes must
// pass, so the fixtures prove the edit is what was caught.
func TestModuleIdentityProblems_RegressionFixtures(t *testing.T) {
	shippedLabels := map[string]string{imageSourceLabel: "https://github.com/redis-master-label/redis-master-label"}
	const shippedModule = "github.com/redis-master-label/redis-master-label"

	tests := []struct {
		name        string
		modulePath  string
		labels      map[string]string
		image       string
		wantProblem string
	}{
		{
			name:       "the shipped identities",
			modulePath: shippedModule,
			labels:     shippedLabels,
			image:      releaseImage,
		},
		{
			name:        "account-only module path (the reviewed defect)",
			modulePath:  "github.com/redis-master-label",
			labels:      shippedLabels,
			image:       releaseImage,
			wantProblem: "names no repository",
		},
		{
			name:        "module path without a host",
			modulePath:  "redis-master-label/redis-master-label",
			labels:      shippedLabels,
			image:       releaseImage,
			wantProblem: "names no repository",
		},
		{
			name:        "module path names another repository",
			modulePath:  "github.com/redis-master-label/other",
			labels:      shippedLabels,
			image:       releaseImage,
			wantProblem: "module path names",
		},
		{
			name:        "source label names another repository",
			modulePath:  shippedModule,
			labels:      map[string]string{imageSourceLabel: "https://github.com/redis-master-label/other"},
			image:       releaseImage,
			wantProblem: imageSourceLabel,
		},
		{
			name:        "source label missing",
			modulePath:  shippedModule,
			labels:      map[string]string{},
			image:       releaseImage,
			wantProblem: "does not name a repository URL",
		},
		{
			name:        "source label on another host",
			modulePath:  shippedModule,
			labels:      map[string]string{imageSourceLabel: "https://gitlab.com/redis-master-label/redis-master-label"},
			image:       releaseImage,
			wantProblem: "gitlab.com",
		},
		{
			name:        "pinned image names another repository",
			modulePath:  shippedModule,
			labels:      shippedLabels,
			image:       "ghcr.io/redis-master-label/other:v0.1.0",
			wantProblem: "pinned release image",
		},
		{
			name:        "pinned image not registry-qualified",
			modulePath:  shippedModule,
			labels:      shippedLabels,
			image:       "redis-master-label:latest",
			wantProblem: "registry",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			problems := moduleIdentityProblems(tt.modulePath, tt.labels, tt.image)
			if tt.wantProblem == "" {
				if len(problems) != 0 {
					t.Fatalf("problems = %v, want none", problems)
				}
				return
			}
			if len(problems) == 0 {
				t.Fatalf("want a problem naming %q, got none", tt.wantProblem)
			}
			if !strings.Contains(strings.Join(problems, "\n"), tt.wantProblem) {
				t.Fatalf("problems = %v, want one naming %q", problems, tt.wantProblem)
			}
		})
	}
}

// A module file that declares no module directive names no import path: the
// check must report that rather than read an empty identity as agreement, and a
// missing module file is a read error.
func TestGoModModulePath_RequiresModuleDirective(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "go.mod")

	if _, err := goModModulePath(filepath.Join(dir, "missing", "go.mod")); err == nil {
		t.Error("missing module file accepted, want a read error")
	}

	if err := os.WriteFile(path, []byte("go 1.27.0\n\nrequire (\n)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := goModModulePath(path); err == nil {
		t.Error("module file without a module directive accepted, want an error")
	}

	if err := os.WriteFile(path, []byte("module github.com/redis-master-label/redis-master-label\n\ngo 1.27.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := goModModulePath(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != "github.com/redis-master-label/redis-master-label" {
		t.Fatalf("module path = %q, want the repository-qualified path", got)
	}
}
