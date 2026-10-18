package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Module-path check: go.mod's module directive is both the import path of the
// package set and the VCS location Go resolves it against, while the rest of the
// repository names one released project:
//
//   - the shipping image's org.opencontainers.image.source label in the build
//     file, the URL `docker inspect` on the pulled image reports,
//   - releaseImage, the pinned image reference the manifests and the README
//     release section use, whose registry path must be the module path's
//     repository path, and
//   - the README's release section, which publishes under the same path.
//
// The declaration read `module github.com/redis-master-label` (the parent
// account) while every reference above named
// github.com/redis-master-label/redis-master-label, so `go get`/proxy lookups
// and any SBOM/purl metadata derived from the module path pointed at a
// repository the project is not published from. No check could see it: the file
// tests read go.mod's `go` directive (goModGoDirective) only, and nothing in the
// module imports the module path, so only an edit that made the two disagree
// visible in review could catch it.
//
// The module path must now agree with both references, so `go test -count=1
// ./...` (which CI and release.yml run before building or publishing) fails when
// either side drifts instead of leaving the disagreement unread.

// imageSourceLabel is the OCI annotation naming the repository the image was
// built from, compared here with the go.mod module path.
const imageSourceLabel = "org.opencontainers.image.source"

// modulePathDeclaration names the one place the module path is declared, so a
// failure points at the line to edit.
const modulePathDeclaration = "go.mod line 1 (module directive)"

// goModModulePath returns the module path of the module file's `module`
// directive, for example "github.com/redis-master-label/redis-master-label".
// Comments are stripped first: a commented-out module line decides nothing, and
// a `module` word inside a comment must not be read as the declaration.
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
	return "", fmt.Errorf("%s declares no module directive", path)
}

// repositoryPathFromImage returns the <owner>/<repo> path of an image reference
// like ghcr.io/redis-master-label/redis-master-label:v0.1.0, that is the
// reference with its registry host, tag and digest removed. The registry host is
// the leading path component when it carries a dot or a colon (or is
// "localhost"); otherwise the reference names no host and keeps all components.
func repositoryPathFromImage(image string) (string, error) {
	ref := image
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	if i := strings.Index(ref, "/"); i > 0 {
		if first := ref[:i]; strings.ContainsAny(first, ".:") || first == "localhost" {
			ref = ref[i+1:]
		}
	}
	if i := strings.LastIndex(ref, ":"); i >= 0 && !strings.Contains(ref[i+1:], "/") {
		ref = ref[:i]
	}
	if ref == "" {
		return "", fmt.Errorf("%q names no repository path", image)
	}
	return ref, nil
}

// repositoryPathFromURL returns the <owner>/<repo> path an
// org.opencontainers.image.source URL points at, with the scheme and any
// trailing slash removed.
func repositoryPathFromURL(url string) string {
	path := strings.TrimPrefix(strings.TrimPrefix(url, "https://"), "http://")
	return strings.TrimSuffix(path, "/")
}

// repositoryPathFromModule returns the <owner>/<repo> path of a module path, the
// form an image reference carries: the module path's leading host component is
// dropped, so github.com/redis-master-label/redis-master-label and the image
// path ghcr.io/redis-master-label/redis-master-label both name
// redis-master-label/redis-master-label. The module path is what CI and the
// release workflow build; the registry host is not part of it.
func repositoryPathFromModule(modulePath string) string {
	if i := strings.Index(modulePath, "/"); i > 0 {
		return modulePath[i+1:]
	}
	return modulePath
}

// modulePathProblems returns one message per disagreement between the module
// path declared in goModPath and the repository named by the build file's final
// stage org.opencontainers.image.source label and by the pinned release image
// reference. An empty result means the three agree.
func modulePathProblems(goModPath, dockerfilePath, image string) ([]string, error) {
	modulePath, err := goModModulePath(goModPath)
	if err != nil {
		return nil, err
	}

	var problems []string

	labels, err := dockerfileFinalStageLabels(dockerfilePath)
	if err != nil {
		return nil, err
	}
	source := labels[imageSourceLabel]
	switch {
	case source == "":
		problems = append(problems, fmt.Sprintf(
			"%s: shipping image declares no non-empty %s label, so the repository %s must name cannot be read from the files",
			dockerfilePath, imageSourceLabel, modulePathDeclaration))
	case repositoryPathFromURL(source) != modulePath:
		problems = append(problems, fmt.Sprintf(
			"%s: %s=%q names %s, but %s declares module %q; the module path is the repository Go resolves, so it must be the released one",
			dockerfilePath, imageSourceLabel, source, repositoryPathFromURL(source), goModPath, modulePath))
	}

	imageRepo, err := repositoryPathFromImage(image)
	if err != nil {
		return nil, err
	}
	if want := repositoryPathFromModule(modulePath); imageRepo != want {
		problems = append(problems, fmt.Sprintf(
			"pinned release image %s names %s, but %s declares module %q, whose repository path is %s; the image must be published from the released module path",
			image, imageRepo, goModPath, modulePath, want))
	}

	return problems, nil
}

// The shipped declaration must agree with the image source label and the pinned
// release image, so the module path cannot silently name another repository.
func TestGoModModulePathMatchesImageSource(t *testing.T) {
	modulePath, err := goModModulePath(goToolchainFile)
	if err != nil {
		t.Fatal(err)
	}
	if modulePath == "" {
		t.Fatalf("%s declares an empty module path", goToolchainFile)
	}
	if !strings.Contains(modulePath, "/") {
		t.Errorf("%s declares module %q, want an owner/repository path: a bare host or account cannot be imported or fetched", goToolchainFile, modulePath)
	}

	problems, err := modulePathProblems(goToolchainFile, dockerfilePath, releaseImage)
	if err != nil {
		t.Fatal(err)
	}
	for _, problem := range problems {
		t.Errorf("%s", problem)
	}
}

// The failure modes the check has to see: the bare account path that was
// declared, an image source label naming another repository, a missing label,
// and a pinned release image whose registry path is not the module path. The
// shipped shape passes, so the fixtures prove the single edit is what was
// caught.
func TestGoModModulePathProblems_RegressionFixtures(t *testing.T) {
	const correctModule = "github.com/redis-master-label/redis-master-label"
	// correctImage is the reference an image published from correctModule
	// carries: the registry host (ghcr.io) replaces the module path's host.
	const correctImage = "ghcr.io/redis-master-label/redis-master-label:v0.1.0"

	dockerfileWith := func(source string) string {
		return "FROM alpine:3.24\n" +
			"LABEL org.opencontainers.image.source=\"" + source + "\"\n" +
			"USER 10001:10001\n"
	}

	tests := []struct {
		name        string
		goMod       string
		dockerfile  string
		image       string
		wantProblem string
	}{
		{
			name:       "shipped shape",
			goMod:      "module " + correctModule + "\n\ngo 1.27.0\n",
			dockerfile: dockerfileWith("https://" + correctModule),
			image:      correctImage,
		},
		{
			name:        "bare account path",
			goMod:       "module github.com/redis-master-label\n\ngo 1.27.0\n",
			dockerfile:  dockerfileWith("https://" + correctModule),
			image:       correctImage,
			wantProblem: `declares module "github.com/redis-master-label"`,
		},
		{
			name:        "source label names another repository",
			goMod:       "module " + correctModule + "\n\ngo 1.27.0\n",
			dockerfile:  dockerfileWith("https://github.com/other/repo"),
			image:       correctImage,
			wantProblem: "names github.com/other/repo",
		},
		{
			name:        "shipping image declares no source label",
			goMod:       "module " + correctModule + "\n\ngo 1.27.0\n",
			dockerfile:  "FROM alpine:3.24\nUSER 10001:10001\n",
			image:       correctImage,
			wantProblem: "no non-empty " + imageSourceLabel,
		},
		{
			name:        "release image names another repository",
			goMod:       "module " + correctModule + "\n\ngo 1.27.0\n",
			dockerfile:  dockerfileWith("https://" + correctModule),
			image:       "ghcr.io/other/repo:v0.1.0",
			wantProblem: "ghcr.io/other/repo:v0.1.0 names other/repo",
		},
		{
			name:       "module directive preceded by a comment naming another module",
			goMod:      "// module github.com/other/repo\nmodule " + correctModule + "\n\ngo 1.27.0\n",
			dockerfile: dockerfileWith("https://" + correctModule),
			image:      correctImage,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			write := func(name, content string) string {
				path := filepath.Join(dir, name)
				if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
				return path
			}
			goMod := write("go.mod", tt.goMod)
			dockerfile := write("Dockerfile", tt.dockerfile)

			problems, err := modulePathProblems(goMod, dockerfile, tt.image)
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantProblem == "" {
				if len(problems) != 0 {
					t.Fatalf("problems = %v, want none", problems)
				}
				return
			}
			if len(problems) == 0 {
				t.Fatal("declaration that disagrees with the released repository accepted, want a problem reported")
			}
			if !strings.Contains(strings.Join(problems, "\n"), tt.wantProblem) {
				t.Fatalf("problems = %v, want one naming %q", problems, tt.wantProblem)
			}
		})
	}
}

// A module file with no module directive cannot declare a repository at all, and
// the paths the check reads must surface a read error rather than pass as
// agreement: a missing or unreadable file that reports "no problems" would hide
// the drift entirely.
func TestGoModModulePathProblems_RejectsUnreadableInputs(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	noModule := write("no-module.mod", "go 1.27.0\n\nrequire k8s.io/api v0.37.1\n")
	noFrom := write("no-from.Dockerfile", "LABEL org.opencontainers.image.source=\"https://github.com/redis-master-label/redis-master-label\"\n")
	valid := write("valid.mod", "module github.com/redis-master-label/redis-master-label\n\ngo 1.27.0\n")
	dockerfile := write("Dockerfile", "FROM alpine:3.24\nLABEL org.opencontainers.image.source=\"https://github.com/redis-master-label/redis-master-label\"\n")

	if got, err := goModModulePath(noModule); err == nil {
		t.Errorf("goModModulePath(%s) = %q, want an error for a module file with no module directive", noModule, got)
	}
	if _, err := modulePathProblems(filepath.Join(dir, "missing.mod"), dockerfile, releaseImage); err == nil {
		t.Error("missing module file accepted, want a read error")
	}
	if _, err := modulePathProblems(valid, filepath.Join(dir, "missing.Dockerfile"), releaseImage); err == nil {
		t.Error("missing build file accepted, want a read error")
	}
	if _, err := modulePathProblems(valid, noFrom, releaseImage); err == nil {
		t.Error("build file without a FROM stage accepted, want an error")
	}
}
