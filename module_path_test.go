package main

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
)

// go.mod's `module` directive is not a local label: it is both the import path
// of this package and the VCS location `go get` and the module proxy resolve,
// and tooling derives purls/SBOM coordinates from it. The repository ships and
// publishes from github.com/redis-master-label/redis-master-label - that is the
// URL in the image's org.opencontainers.image.source label (Dockerfile) and the
// repository of the pinned release image (main_test.go's releaseImage and
// manifests/*.yaml) - so declaring the bare org path `github.com/redis-master-label`
// names a different repository than the one built and released. Nothing in the
// suite saw it: no file imports the module path and the existing go.mod reads
// only the `go` directive.

// imageSourceLabel is the OCI annotation naming the repository the shipped image
// was built from. It is the second declaration of this project's VCS location
// and must agree with go.mod's module path.
const imageSourceLabel = "org.opencontainers.image.source"

// goModModulePath returns the path declared by the module file's `module`
// directive, or an error when the file declares none.
func goModModulePath(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, "module") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "module" {
			continue
		}
		return strings.Trim(fields[1], `"'`), nil
	}
	return "", fmt.Errorf("%s: no `module <path>` directive found", path)
}

// repositoryPathFromURL returns the host/path of a repository URL - the VCS
// location a Go module path must equal - rejecting a value that does not name a
// repository (for example a bare host, or a relative path).
func repositoryPathFromURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("%q is not an absolute repository URL", raw)
	}
	path := strings.Trim(strings.TrimSuffix(parsed.Path, "/"), "/")
	path = strings.TrimSuffix(path, ".git")
	if path == "" {
		return "", fmt.Errorf("%q names no repository path", raw)
	}
	return parsed.Host + "/" + path, nil
}

// modulePathProblem returns why the module file's declared path is not the
// repository the shipped image names, or nil when the two agree.
func modulePathProblem(modulePath, imageSource string) error {
	want, err := repositoryPathFromURL(imageSource)
	if err != nil {
		return fmt.Errorf("cannot read the repository path from %s: %v", imageSourceLabel, err)
	}
	if modulePath != want {
		return fmt.Errorf("go.mod declares module %q, but the shipping image's %s is %q: the module path is the import path and VCS location, so it must be the repository path %q", modulePath, imageSourceLabel, imageSource, want)
	}
	return nil
}

// imageRepositoryPath returns the registry-independent repository path of an
// image reference, for example ghcr.io/redis-master-label/redis-master-label:v0.1.0
// gives redis-master-label/redis-master-label.
func imageRepositoryPath(image string) (string, error) {
	trimmed := strings.TrimSpace(image)
	if i := strings.LastIndex(trimmed, "@"); i >= 0 {
		trimmed = trimmed[:i]
	}
	if i := strings.LastIndex(trimmed, ":"); i > strings.LastIndex(trimmed, "/") {
		trimmed = trimmed[:i]
	}
	parts := strings.Split(trimmed, "/")
	if len(parts) < 3 {
		return "", fmt.Errorf("%q is not a fully qualified image reference", image)
	}
	return strings.Join(parts[1:], "/"), nil
}

// TestGoModModulePathMatchesImageSource fails when go.mod names a repository
// other than the one the shipping image advertises as its source.
func TestGoModModulePathMatchesImageSource(t *testing.T) {
	buildFile, err := locateBuildFile()
	if err != nil {
		t.Fatal(err)
	}
	labels, err := dockerfileFinalStageLabels(buildFile)
	if err != nil {
		t.Fatal(err)
	}
	modulePath, err := goModModulePath(goToolchainFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := modulePathProblem(modulePath, labels[imageSourceLabel]); err != nil {
		t.Error(err)
	}
}

// TestGoModModulePathMatchesReleaseImage ties the module path to the image the
// manifests deploy: the release image's repository path is the module path with
// a forge host in front, so a module path that names another repository is also
// a project users cannot `go get` from the coordinates the image publishes.
func TestGoModModulePathMatchesReleaseImage(t *testing.T) {
	modulePath, err := goModModulePath(goToolchainFile)
	if err != nil {
		t.Fatal(err)
	}
	repoPath, err := imageRepositoryPath(releaseImage)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(modulePath, "/"+repoPath) {
		t.Errorf("go.mod declares module %q, but the pinned release image %q lives at repository path %q: the module path must name that repository", modulePath, releaseImage, repoPath)
	}
}

// TestModulePathProblem_RegressionFixtures pins the reviewed defect and the
// accepted shapes. The first fixture is the exact value go.mod declared while
// the image advertised github.com/redis-master-label/redis-master-label.
func TestModulePathProblem_RegressionFixtures(t *testing.T) {
	tests := []struct {
		name        string
		modulePath  string
		imageSource string
		wantProblem string
	}{
		{
			name:        "reviewed defect: bare org path instead of the repository",
			modulePath:  "github.com/redis-master-label",
			imageSource: "https://github.com/redis-master-label/redis-master-label",
			wantProblem: "must be the repository path \"github.com/redis-master-label/redis-master-label\"",
		},
		{
			name:        "declared path is the repository path",
			modulePath:  "github.com/redis-master-label/redis-master-label",
			imageSource: "https://github.com/redis-master-label/redis-master-label",
		},
		{
			name:        "another repository",
			modulePath:  "github.com/redis-master-label/other-project",
			imageSource: "https://github.com/redis-master-label/redis-master-label",
			wantProblem: "must be the repository path",
		},
		{
			name:        "trailing slash in the source label is not a difference",
			modulePath:  "github.com/redis-master-label/redis-master-label",
			imageSource: "https://github.com/redis-master-label/redis-master-label/",
		},
		{
			name:        "clone URL suffix in the source label is not a difference",
			modulePath:  "github.com/redis-master-label/redis-master-label",
			imageSource: "https://github.com/redis-master-label/redis-master-label.git",
		},
		{
			name:        "source label without a scheme",
			modulePath:  "github.com/redis-master-label/redis-master-label",
			imageSource: "github.com/redis-master-label/redis-master-label",
			wantProblem: "not an absolute repository URL",
		},
		{
			name:        "empty source label",
			modulePath:  "github.com/redis-master-label/redis-master-label",
			imageSource: "",
			wantProblem: "not an absolute repository URL",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := modulePathProblem(tt.modulePath, tt.imageSource)
			if tt.wantProblem == "" {
				if err != nil {
					t.Fatalf("modulePathProblem(%q, %q) = %v, want nil", tt.modulePath, tt.imageSource, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("modulePathProblem(%q, %q) = nil, want a problem containing %q", tt.modulePath, tt.imageSource, tt.wantProblem)
			}
			if !strings.Contains(err.Error(), tt.wantProblem) {
				t.Errorf("modulePathProblem(%q, %q) = %v, want a problem containing %q", tt.modulePath, tt.imageSource, err, tt.wantProblem)
			}
		})
	}
}

// TestGoModModulePath_ReadsModuleDirective covers the module-file shapes a
// rename could produce: a commented-out line is not a declaration, and a file
// without one must be reported rather than read as an empty path.
func TestGoModModulePath_ReadsModuleDirective(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
		wantErr bool
	}{
		{
			name:    "plain directive",
			content: "module github.com/redis-master-label/redis-master-label\n\ngo 1.27.0\n",
			want:    "github.com/redis-master-label/redis-master-label",
		},
		{
			name:    "quoted directive",
			content: "module \"github.com/redis-master-label/redis-master-label\"\n",
			want:    "github.com/redis-master-label/redis-master-label",
		},
		{
			name:    "commented-out directive only",
			content: "// module github.com/redis-master-label/redis-master-label\n",
			wantErr: true,
		},
		{
			name:    "no directive",
			content: "go 1.27.0\n",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := t.TempDir() + "/go.mod"
			if err := os.WriteFile(path, []byte(tt.content), 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := goModModulePath(path)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("goModModulePath(%q) = %q, want an error", tt.content, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("goModModulePath(%q): %v", tt.content, err)
			}
			if got != tt.want {
				t.Errorf("goModModulePath(%q) = %q, want %q", tt.content, got, tt.want)
			}
		})
	}
}
