package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Module identity check: a module path is the import/install identity of the
// code, so it has to name the repository the project names everywhere else.
// The reviewed defect was go.mod declaring `module github.com/redis-master-label`
// - a GitHub account, not a repository - while the OCI source label (the
// build file's org.opencontainers.image.source), the GHCR release image
// (release.yml's `ghcr.io/${{ github.repository }}`), the image the example
// manifests pin, main_test.go's releaseImage and the README all name
// github.com/redis-master-label/redis-master-label. `go install
// github.com/redis-master-label@latest`, a module proxy fetch and any
// downstream `import "github.com/redis-master-label"` therefore resolved
// against a VCS path that does not exist, and `go mod` could not report which
// repository the code came from.
//
// The declaration now names that repository, and this file keeps the three
// identities in agreement: the module path, the image source label, the
// registry references the project publishes and deploys (README, manifests,
// releaseImage) and release.yml's image name are all read and compared, so
// editing one of them alone fails `go test -count=1 ./...`, which CI and
// release.yml both run before they build or publish anything.

// modulePathFile is the file whose `module` directive is the module's
// import/install identity.
const modulePathFile = "go.mod"

// imageSourceLabel is the OCI label naming the repository the published image
// was built from. Its value has to name the repository the module path names.
const imageSourceLabel = "org.opencontainers.image.source"

// releaseWorkflowFile is the workflow that publishes the image, and
// releaseImageEnv is the env key holding the reference it publishes. It is
// derived from the repository GitHub reports, so the published image is the
// repository the module path has to name.
const (
	releaseWorkflowFile = "release.yml"
	releaseImageEnv     = "IMAGE"
	releaseImageEnvWant = "ghcr.io/${{ github.repository }}"
)

// goModulePath returns the path declared by the module file's `module`
// directive, for example "github.com/redis-master-label/redis-master-label",
// dropping any trailing comment. An error means no import identity can be read
// from the file at all.
func goModulePath(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}

	for _, line := range strings.Split(string(data), "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "module" {
			return fields[1], nil
		}
	}
	return "", fmt.Errorf("%s: no module directive declaring the module path", path)
}

// repositoryIdentity is the repository a module path, a repository URL or a
// registry image reference names: the host the code lives on plus the
// "owner/repo" slug under it, lowercased because container registries require
// lowercase names and GitHub repository names compare case-insensitively.
type repositoryIdentity struct {
	Host string
	Slug string
}

// parseRepositoryIdentity reduces a module path, a repository URL or a
// registry-qualified image reference to the repository it names. All three
// forms carry the repository in their first two path segments after the host
// (host/owner/repo...), so that is what is read; a scheme, a `#fragment`, an
// `@digest`, a trailing `/`, a `.git` suffix and an image tag belong to the
// reference, not to the identity, and are dropped.
//
// A reference with fewer than three path segments names a host and an account
// but no repository - the reviewed `github.com/redis-master-label` shape - and
// is rejected, so a module path can never pass as "the same repository" as
// another account-only string.
func parseRepositoryIdentity(raw string) (repositoryIdentity, error) {
	ref := strings.Trim(strings.TrimSpace(raw), "`\"'")
	if ref == "" {
		return repositoryIdentity{}, errors.New("empty repository reference")
	}
	if i := strings.Index(ref, "://"); i >= 0 {
		ref = ref[i+3:]
	}
	if i := strings.Index(ref, "#"); i >= 0 {
		ref = ref[:i]
	}
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	ref = strings.TrimSuffix(ref, "/")
	ref = strings.TrimSuffix(ref, ".git")

	segments := strings.Split(ref, "/")
	if len(segments) < 3 || segments[1] == "" {
		return repositoryIdentity{}, fmt.Errorf(
			"%q names %q, which is a host and an account, not a repository: a module path, a source URL and an image reference all name host/owner/repo", raw, ref)
	}
	repo := segments[2]
	if i := strings.Index(repo, ":"); i >= 0 {
		repo = repo[:i]
	}
	if repo == "" {
		return repositoryIdentity{}, fmt.Errorf("%q names no repository", raw)
	}

	return repositoryIdentity{
		Host: strings.ToLower(segments[0]),
		Slug: strings.ToLower(segments[1] + "/" + repo),
	}, nil
}

// modulePathProblem returns why a module path does not name the repository a
// code-host reference (the image source label's URL) names, or nil when it
// does. The host has to match as well: the same slug on another host is a
// different repository.
func modulePathProblem(modulePath, reference string) error {
	module, err := parseRepositoryIdentity(modulePath)
	if err != nil {
		return fmt.Errorf("%s: %v", modulePathFile, err)
	}
	ref, err := parseRepositoryIdentity(reference)
	if err != nil {
		return err
	}
	if module.Slug != ref.Slug {
		return fmt.Errorf("module path %q names repository %q, but %q names %q: the module path is the code's import/install identity, so both have to name the same repository", modulePath, module.Slug, reference, ref.Slug)
	}
	if module.Host != ref.Host {
		return fmt.Errorf("module path %q lives on %s, but %q lives on %s: the same slug on another host is another repository", modulePath, module.Host, reference, ref.Host)
	}
	return nil
}

// imageRepositoryProblem returns why an image reference does not name the
// repository a module path names, or nil when it does. Only the owner/repo slug
// is compared: the image lives on a registry host (ghcr.io) and the code on a
// code host (github.com), so the hosts differ by design while the repository
// has to be the same one.
func imageRepositoryProblem(modulePath, image string) error {
	module, err := parseRepositoryIdentity(modulePath)
	if err != nil {
		return fmt.Errorf("%s: %v", modulePathFile, err)
	}
	ref, err := parseRepositoryIdentity(image)
	if err != nil {
		return err
	}
	if module.Slug != ref.Slug {
		return fmt.Errorf("image reference %q names repository %q, but the module path %q names %q: the published image has to be the code the module path installs", image, ref.Slug, modulePath, module.Slug)
	}
	return nil
}

// readmeImageRe matches a GHCR image reference in prose, capturing
// "owner/repo". A placeholder reference such as `ghcr.io/<owner>/repo:<tag>`
// does not match: `<` is not part of a repository name.
var readmeImageRe = regexp.MustCompile(`ghcr\.io/([A-Za-z0-9._-]+/[A-Za-z0-9._-]+)`)

// readmeImageReferences returns every GHCR repository the README names, so the
// documented release reference is compared with the module path too.
func readmeImageReferences(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var refs []string
	for _, match := range readmeImageRe.FindAllStringSubmatch(string(data), -1) {
		refs = append(refs, match[1])
	}
	return refs, nil
}

// The shipped identities: the module path, the image source label, the release
// image the project pins and the image references it documents and deploys all
// have to name github.com/redis-master-label/redis-master-label.
func TestGoModulePathNamesTheProjectRepository(t *testing.T) {
	modulePath, err := goModulePath(modulePathFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseRepositoryIdentity(modulePath); err != nil {
		t.Fatalf("%s declares module %q: %v", modulePathFile, modulePath, err)
	}

	buildFile, err := locateBuildFile()
	if err != nil {
		t.Fatal(err)
	}
	labels, err := dockerfileFinalStageLabels(buildFile)
	if err != nil {
		t.Fatal(err)
	}
	source := labels[imageSourceLabel]
	if source == "" {
		t.Fatalf("%s declares no %s label, so the repository the image comes from cannot be compared with the module path", buildFile, imageSourceLabel)
	}
	if err := modulePathProblem(modulePath, source); err != nil {
		t.Errorf("%s: %v", buildFile, err)
	}

	// The pinned release image: main_test.go's releaseImage is what
	// TestManifestsPinVersionedReleaseImage holds the example manifests to, so
	// reading it here ties the module path to the image the project deploys.
	if err := imageRepositoryProblem(modulePath, releaseImage); err != nil {
		t.Errorf("%s: %v", modulePathFile, err)
	}

	images, sidecars, err := sidecarImages(manifestDir)
	if err != nil {
		t.Fatal(err)
	}
	if sidecars == 0 {
		t.Fatalf("no sidecar container found in %s: the image identity check ran against nothing", manifestDir)
	}
	for _, image := range images {
		if err := imageRepositoryProblem(modulePath, image); err != nil {
			t.Errorf("%s: %v", manifestDir, err)
		}
	}

	refs, err := readmeImageReferences("README.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) == 0 {
		t.Errorf("README.md names no ghcr.io image, so the documented release reference cannot be compared with the module path")
	}
	for _, ref := range refs {
		if err := imageRepositoryProblem(modulePath, "ghcr.io/"+ref); err != nil {
			t.Errorf("README.md: %v", err)
		}
	}
}

// The published image comes from this repository by construction: release.yml
// names it with GitHub's own repository expression, so the reference it pushes
// is the repository the module path names.
func TestReleaseWorkflowPublishesTheModuleRepository(t *testing.T) {
	path := filepath.Join(ciWorkflowDir, releaseWorkflowFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	found := false
	for i, line := range strings.Split(string(data), "\n") {
		key, value, ok := workflowInput(strings.TrimSpace(line))
		if !ok || key != releaseImageEnv {
			continue
		}
		found = true
		if value != releaseImageEnvWant {
			t.Errorf("%s:%d: %s = %q, want %q so the image published is the repository the module path names", path, i+1, releaseImageEnv, value, releaseImageEnvWant)
		}
	}
	if !found {
		t.Errorf("%s declares no %s, so the reference it publishes cannot be read", path, releaseImageEnv)
	}
}

// The reader that made the defect visible: a reference naming a host and an
// account but no repository must be rejected, while the shipped module path and
// the reference forms the project uses must be read as one repository.
func TestParseRepositoryIdentity_RegressionFixtures(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    repositoryIdentity
		wantErr bool
	}{
		{
			name: "the module path as declared after the fix",
			raw:  "github.com/redis-master-label/redis-master-label",
			want: repositoryIdentity{Host: "github.com", Slug: "redis-master-label/redis-master-label"},
		},
		{
			name: "the image source label's URL",
			raw:  "https://github.com/redis-master-label/redis-master-label",
			want: repositoryIdentity{Host: "github.com", Slug: "redis-master-label/redis-master-label"},
		},
		{
			name: "a clone URL with a .git suffix",
			raw:  "https://github.com/redis-master-label/redis-master-label.git",
			want: repositoryIdentity{Host: "github.com", Slug: "redis-master-label/redis-master-label"},
		},
		{
			name: "the pinned image reference",
			raw:  releaseImage,
			want: repositoryIdentity{Host: "ghcr.io", Slug: "redis-master-label/redis-master-label"},
		},
		{
			name: "an image reference with a digest",
			raw:  "ghcr.io/redis-master-label/redis-master-label@sha256:0000000000000000000000000000000000000000000000000000000000000000",
			want: repositoryIdentity{Host: "ghcr.io", Slug: "redis-master-label/redis-master-label"},
		},
		{
			// The reviewed defect: an account, not a repository.
			name:    "the account-only module path",
			raw:     "github.com/redis-master-label",
			wantErr: true,
		},
		{
			name:    "an account with a trailing slash",
			raw:     "github.com/redis-master-label/",
			wantErr: true,
		},
		{
			name:    "an empty reference",
			raw:     "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseRepositoryIdentity(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseRepositoryIdentity(%q) = %+v, want an error: a reference without a repository cannot be compared", tt.raw, got)
				}
				if !strings.Contains(err.Error(), "repository") {
					t.Errorf("error %q does not explain that the reference names no repository", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseRepositoryIdentity(%q): %v", tt.raw, err)
			}
			if got != tt.want {
				t.Fatalf("parseRepositoryIdentity(%q) = %+v, want %+v", tt.raw, got, tt.want)
			}
		})
	}
}

// The comparison's failure modes: a module path that differs from the source
// label in the slug or on the host, and an image that belongs to another
// repository, must each be reported.
func TestModulePathProblem_RegressionFixtures(t *testing.T) {
	tests := []struct {
		name       string
		modulePath string
		reference  string
		wantErr    bool
	}{
		{
			name:       "module path and source label name the same repository",
			modulePath: "github.com/redis-master-label/redis-master-label",
			reference:  "https://github.com/redis-master-label/redis-master-label",
		},
		{
			name:       "module path names another repository",
			modulePath: "github.com/redis-master-label/labeler",
			reference:  "https://github.com/redis-master-label/redis-master-label",
			wantErr:    true,
		},
		{
			name:       "module path on another host",
			modulePath: "gitlab.com/redis-master-label/redis-master-label",
			reference:  "https://github.com/redis-master-label/redis-master-label",
			wantErr:    true,
		},
		{
			// The reviewed defect at the declaring end.
			name:       "module path names the account only",
			modulePath: "github.com/redis-master-label",
			reference:  "https://github.com/redis-master-label/redis-master-label",
			wantErr:    true,
		},
		{
			name:       "source label names the account only",
			modulePath: "github.com/redis-master-label/redis-master-label",
			reference:  "https://github.com/redis-master-label",
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := modulePathProblem(tt.modulePath, tt.reference)
			if (err != nil) != tt.wantErr {
				t.Fatalf("modulePathProblem(%q, %q) error = %v, wantErr %v", tt.modulePath, tt.reference, err, tt.wantErr)
			}
		})
	}
}

// The image side of the same agreement: the registry host may differ from the
// code host, the repository may not.
func TestImageRepositoryProblem_RegressionFixtures(t *testing.T) {
	const modulePath = "github.com/redis-master-label/redis-master-label"

	tests := []struct {
		name    string
		image   string
		wantErr bool
	}{
		{name: "the pinned release image", image: releaseImage},
		{name: "another repository's image", image: "ghcr.io/redis-master-label/labeler:v0.1.0", wantErr: true},
		{name: "another owner's image", image: "ghcr.io/someone-else/redis-master-label:v0.1.0", wantErr: true},
		{name: "an image naming no repository", image: "ghcr.io/redis-master-label", wantErr: true},
		{name: "an empty reference", image: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := imageRepositoryProblem(modulePath, tt.image)
			if (err != nil) != tt.wantErr {
				t.Fatalf("imageRepositoryProblem(%q, %q) error = %v, wantErr %v", modulePath, tt.image, err, tt.wantErr)
			}
		})
	}
}

// The module file reader itself: the directive is read with a trailing comment,
// and a file that declares no module path is reported instead of yielding an
// empty identity that would compare against nothing.
func TestGoModulePath_RegressionFixtures(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "go.mod")

	tests := []struct {
		name    string
		content string
		want    string
		wantErr bool
	}{
		{
			name:    "the shipped shape",
			content: "module github.com/redis-master-label/redis-master-label\n\ngo 1.27.0\n",
			want:    "github.com/redis-master-label/redis-master-label",
		},
		{
			name:    "a trailing comment",
			content: "module github.com/redis-master-label/redis-master-label // the repository\n\ngo 1.27.0\n",
			want:    "github.com/redis-master-label/redis-master-label",
		},
		{
			name:    "no module directive",
			content: "go 1.27.0\n",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tt.content), 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := goModulePath(path)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("goModulePath() = %q, want an error for a file with no module directive", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("goModulePath(): %v", err)
			}
			if got != tt.want {
				t.Errorf("goModulePath() = %q, want %q", got, tt.want)
			}
		})
	}

	if _, err := goModulePath(filepath.Join(dir, "missing.mod")); err == nil {
		t.Error("missing module file accepted, want a read error")
	}
}
