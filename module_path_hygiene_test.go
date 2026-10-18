package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

// A go.mod module path is both the import path and the VCS location of the
// project: Go tooling and the module proxy resolve the module from it, and the
// SBOM/purl metadata published with the image is derived from it. It must
// therefore name the repository the project actually builds and publishes from
// - the path carried by the build file's org.opencontainers.image.source label
// and by the release image reference the manifests pin.
//
// The reviewed drift was `module github.com/redis-master-label` (the
// organization, not the repository) beside
// org.opencontainers.image.source="https://github.com/redis-master-label/redis-master-label",
// so `go get`, proxy lookups and purl metadata pointed at a repository that is
// not the released project. Nothing in the suite imported the module path and
// the other file checks read only the `go` directive, which is why the mismatch
// was invisible. modulePathProblems is the single comparison those three
// sources are held to.

// imageSourceURLRe matches the https URL of an image source label, capturing
// the host/owner/repository path without the scheme or a trailing slash.
var imageSourceURLRe = regexp.MustCompile(`^https?://([^/]+/[^/]+/[^/]+?)/?$`)

// goModModulePath returns the path declared by go.mod's module directive, or ""
// when the file declares none.
func goModModulePath(text string) string {
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "module" {
			return fields[1]
		}
	}
	return ""
}

// repoPathFromImageSource returns the host/owner/repository path of an
// org.opencontainers.image.source label value, or "" when the value is not a
// plain https URL naming a repository below an owner.
func repoPathFromImageSource(url string) string {
	m := imageSourceURLRe.FindStringSubmatch(strings.TrimSpace(url))
	if m == nil {
		return ""
	}
	return m[1]
}

// imageRepoPath returns the owner/repository part of an image reference,
// dropping the registry host and any tag or digest.
func imageRepoPath(ref string) string {
	ref = strings.TrimSpace(ref)
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	// A tag's colon comes after the last slash; a registry port's colon comes
	// before it.
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		ref = ref[:i]
	}
	parts := strings.Split(strings.Trim(ref, "/"), "/")
	if len(parts) < 3 {
		return ""
	}
	return strings.Join(parts[len(parts)-2:], "/")
}

// modulePathProblems returns one message per way the module path fails to name
// the repository the project builds and publishes from: the path declared in
// go.mod, the repository the build file's org.opencontainers.image.source label
// names, and the repository of the release image the manifests pin must all
// agree.
func modulePathProblems(modulePath, imageSource, releaseImage string) []string {
	var problems []string

	repoPath := repoPathFromImageSource(imageSource)
	if repoPath == "" {
		problems = append(problems, fmt.Sprintf("build file's org.opencontainers.image.source is not an https repository URL (%q), so the repository the module path must name cannot be read", imageSource))
	}

	if modulePath == "" {
		problems = append(problems, "go.mod declares no module path")
	}

	if modulePath != "" && repoPath != "" && modulePath != repoPath {
		problems = append(problems, fmt.Sprintf("go.mod module path %q is not the repository %q that org.opencontainers.image.source names: `go get`, module proxy lookups and the SBOM purl metadata would resolve the released project at a different repository", modulePath, repoPath))
	}

	if repoPath != "" && releaseImage != "" {
		want := repoPath[strings.Index(repoPath, "/")+1:]
		if got := imageRepoPath(releaseImage); got != want {
			problems = append(problems, fmt.Sprintf("release image %q names repository %q, but the module path names %q: the pin and the module path must identify the same project", releaseImage, got, want))
		}
	}

	return problems
}

// TestModulePathMatchesPublishedRepository pins go.mod's module path to the
// repository the image source label and the release image pin name. Bumping one
// without the others leaves the module path pointing at a repository the
// project does not publish from.
func TestModulePathMatchesPublishedRepository(t *testing.T) {
	data, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}

	buildFile, err := locateBuildFile()
	if err != nil {
		t.Fatalf("locate build file: %v", err)
	}

	labels, err := dockerfileFinalStageLabels(buildFile)
	if err != nil {
		t.Fatalf("read %s labels: %v", buildFile, err)
	}

	modulePath := goModModulePath(string(data))
	if problems := modulePathProblems(modulePath, labels["org.opencontainers.image.source"], releaseImage); len(problems) > 0 {
		t.Fatalf("module path %q does not name the repository this project builds and publishes from:\n- %s", modulePath, strings.Join(problems, "\n- "))
	}
}

// TestModulePathProblems_FailureModes proves the check rejects each way the
// module path can drift from the published repository, using fixtures of the
// real module path, source label and release image pin with one edit each.
func TestModulePathProblems_FailureModes(t *testing.T) {
	const (
		modulePath = "github.com/redis-master-label/redis-master-label"
		sourceURL  = "https://github.com/redis-master-label/redis-master-label"
		imageRef   = "ghcr.io/redis-master-label/redis-master-label:v0.1.0"
	)

	if problems := modulePathProblems(modulePath, sourceURL, imageRef); len(problems) > 0 {
		t.Fatalf("consistent module path, source label and release image reported problems:\n- %s", strings.Join(problems, "\n- "))
	}

	cases := []struct {
		name          string
		modulePath    string
		imageSource   string
		releaseImage  string
		wantSubstring string
	}{
		{
			name:          "reviewed drift: module path names the organization only",
			modulePath:    "github.com/redis-master-label",
			imageSource:   sourceURL,
			releaseImage:  imageRef,
			wantSubstring: "module path",
		},
		{
			name:          "module directive missing",
			modulePath:    goModModulePath("go 1.27.0\n"),
			imageSource:   sourceURL,
			releaseImage:  imageRef,
			wantSubstring: "declares no module path",
		},
		{
			name:          "module path names another repository",
			modulePath:    "github.com/redis-master-label/redis-master",
			imageSource:   sourceURL,
			releaseImage:  imageRef,
			wantSubstring: "is not the repository",
		},
		{
			name:          "source label names another repository",
			modulePath:    modulePath,
			imageSource:   "https://github.com/redis-master-label/other-project",
			releaseImage:  imageRef,
			wantSubstring: "is not the repository",
		},
		{
			name:          "source label is not a repository URL",
			modulePath:    modulePath,
			imageSource:   "redis-master-label/redis-master-label",
			releaseImage:  imageRef,
			wantSubstring: "not an https repository URL",
		},
		{
			name:          "release image names another repository",
			modulePath:    modulePath,
			imageSource:   sourceURL,
			releaseImage:  "ghcr.io/redis-master-label/other-project:v0.1.0",
			wantSubstring: "release image",
		},
		{
			name:          "release image has no owner path",
			modulePath:    modulePath,
			imageSource:   sourceURL,
			releaseImage:  "other-project:v0.1.0",
			wantSubstring: "release image",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			problems := modulePathProblems(tt.modulePath, tt.imageSource, tt.releaseImage)
			if len(problems) == 0 {
				t.Fatalf("modulePathProblems(%q, %q, %q) reported no problem, want one mentioning %q", tt.modulePath, tt.imageSource, tt.releaseImage, tt.wantSubstring)
			}
			joined := strings.Join(problems, "\n")
			if !strings.Contains(joined, tt.wantSubstring) {
				t.Fatalf("problems do not mention %q:\n%s", tt.wantSubstring, joined)
			}
		})
	}
}
