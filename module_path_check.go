package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// go.mod module path check: go.mod declared `module github.com/redis-master-label`,
// the import path of a repository one level above this one, while every surface
// that publishes the project names github.com/redis-master-label/redis-master-label:
// the image's org.opencontainers.image.source label (Dockerfile), the pinned
// release image reference (`const releaseImage` in main_test.go, the example
// manifests) and the README release section. A Go module path is both the import
// path and the VCS location `go get` and the module proxy resolve, so the old
// declaration pointed at a different repository: module lookups never resolved to
// the released project and module-derived SBOM/purl metadata named another one.
// Nothing in the suite could see it - no package imports the module path, and the
// existing go.mod readers parse only the `go` directive - so this check compares
// the declared path with the repository every publishing surface names.
const (
	// modulePathFile is the module file whose `module` directive is checked.
	modulePathFile = "go.mod"

	// imageSourceLabel is the OCI label naming the repository the image is built
	// and published from, and imageSourceFile is the build file declaring it.
	imageSourceLabel = "org.opencontainers.image.source"
	imageSourceFile  = "Dockerfile"
)

// modulePathFromGoMod returns the import path declared by go.mod's `module`
// directive. A file without a module directive, or with one that declares no
// path, is an error rather than an empty string: an unreadable module path must
// not read as agreement.
func modulePathFromGoMod(content string) (string, error) {
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "module" {
			continue
		}
		if len(fields) < 2 {
			return "", fmt.Errorf("%s: the module directive declares no path", modulePathFile)
		}
		path := strings.Trim(fields[1], `"`)
		if path == "" {
			return "", fmt.Errorf("%s: the module directive declares an empty path", modulePathFile)
		}
		return path, nil
	}
	return "", fmt.Errorf("%s: declares no module directive, so the module path cannot be read", modulePathFile)
}

// imageSourceValue returns the URL the image's org.opencontainers.image.source
// label carries. Both the quoted (`label="value"`) and unquoted (`label=value`)
// forms are read, so the check does not depend on how the label is spelled.
func imageSourceValue(dockerfile string) (string, error) {
	idx := strings.Index(dockerfile, imageSourceLabel)
	if idx < 0 {
		return "", fmt.Errorf("%s: declares no %s label, so the repository the image is published from cannot be read", imageSourceFile, imageSourceLabel)
	}
	rest := strings.TrimSpace(dockerfile[idx+len(imageSourceLabel):])
	rest = strings.TrimSpace(strings.TrimPrefix(rest, "="))
	rest = strings.TrimPrefix(rest, `"`)
	if end := strings.IndexAny(rest, "\"\\ \t\r\n"); end >= 0 {
		rest = rest[:end]
	}
	if rest == "" {
		return "", fmt.Errorf("%s: the %s label carries no value", imageSourceFile, imageSourceLabel)
	}
	return rest, nil
}

// repositoryPathFromURL splits a repository URL into its host and its
// owner/repo path: https://github.com/redis-master-label/redis-master-label
// becomes github.com and redis-master-label/redis-master-label.
func repositoryPathFromURL(source string) (host, path string, err error) {
	trimmed := strings.TrimSpace(source)
	trimmed = strings.TrimSuffix(trimmed, "/")
	if i := strings.Index(trimmed, "://"); i >= 0 {
		trimmed = trimmed[i+3:]
	}
	trimmed = strings.TrimSuffix(trimmed, ".git")
	parts := strings.Split(trimmed, "/")
	if len(parts) < 3 {
		return "", "", fmt.Errorf("%q does not name host/owner/repo", source)
	}
	return parts[0], strings.Join(parts[1:], "/"), nil
}

// repositoryPathFromImageRef returns the owner/repo path of a registry-qualified
// image reference, dropping the registry host, the tag and the digest:
// ghcr.io/redis-master-label/redis-master-label:v0.1.0 becomes
// redis-master-label/redis-master-label. A reference without a registry host is
// an error, because its repository path cannot be read unambiguously.
func repositoryPathFromImageRef(image string) (string, error) {
	ref := strings.TrimSpace(image)
	if ref == "" {
		return "", fmt.Errorf("empty image reference")
	}
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		ref = ref[:i]
	}
	parts := strings.Split(ref, "/")
	if len(parts) < 3 {
		return "", fmt.Errorf("%q is not a registry-qualified reference (want registry/owner/repo[:tag])", image)
	}
	return strings.Join(parts[1:], "/"), nil
}

// releaseImageReference returns the image reference pinned by the Go source's
// `const releaseImage = "..."` declaration.
func releaseImageReference(source string) (string, error) {
	re := regexp.MustCompile(`(?m)^\s*const\s+` + regexp.QuoteMeta(releaseImageConstName) + `\s*=\s*"([^"]+)"`)
	match := re.FindStringSubmatch(source)
	if match == nil {
		return "", fmt.Errorf("%s: declares no %s constant, so the image the repository deploys cannot be read", releasePinFile, releaseImageConstName)
	}
	return match[1], nil
}

// manifestImageReferences returns every `image:` value declared in the manifests
// under dir, sorted so a failure reports the same order every run. Documents
// without containers (Service, Role, RoleBinding, ServiceAccount) contribute
// nothing, and the caller counts what was read, so an empty directory cannot
// read as agreement.
func manifestImageReferences(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var images []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if ext := filepath.Ext(entry.Name()); ext != ".yaml" && ext != ".yml" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			// A container entry may open with `- image: ...`; the key is the
			// same, so the list marker is dropped before the key is read.
			trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, "-"))
			if !strings.HasPrefix(trimmed, "image:") {
				continue
			}
			value := strings.TrimSpace(strings.TrimPrefix(trimmed, "image:"))
			value = strings.Trim(value, `"'`)
			if value == "" {
				continue
			}
			images = append(images, value)
		}
	}
	sort.Strings(images)
	return images, nil
}

// labelerImageReferences keeps the image references that publish the labeler
// itself: registry-qualified references whose repository the sidecar image name
// identifies. The example manifests also run `redis:7-alpine`, which has no
// repository path to compare and is not this project's image.
func labelerImageReferences(images []string) []string {
	var refs []string
	for _, image := range images {
		path, err := repositoryPathFromImageRef(image)
		if err != nil {
			continue
		}
		if strings.HasSuffix(path, "/"+sidecarImage) {
			refs = append(refs, image)
		}
	}
	return refs
}

// modulePathProblems compares go.mod's module path with the repository path named
// by the image source label, the pinned release image and the manifests. One
// message is returned per surface that names a different repository, plus one per
// surface that could not be read at all, so a missing label or an empty manifest
// set cannot pass as agreement.
func modulePathProblems(modulePath, imageSource, releaseImage string, manifestImages []string) []string {
	var problems []string

	var wantModulePath string
	host, repoPath, err := repositoryPathFromURL(imageSource)
	switch {
	case strings.TrimSpace(imageSource) == "":
		problems = append(problems, fmt.Sprintf("no %s value was read, so the repository the image is published from cannot be compared with the module path", imageSourceLabel))
	case err != nil:
		problems = append(problems, fmt.Sprintf("%s value %q cannot be read as a repository: %v", imageSourceLabel, imageSource, err))
	default:
		wantModulePath = host + "/" + repoPath
	}

	if strings.TrimSpace(modulePath) == "" {
		problems = append(problems, fmt.Sprintf("%s declares no module path, so the import path and the VCS location `go get` resolves are unknown", modulePathFile))
	} else if wantModulePath != "" && modulePath != wantModulePath {
		problems = append(problems, fmt.Sprintf(
			"%s declares module %q, but the image's %s label names repository %q: the module path is both the import path and the VCS location `go get` and the module proxy resolve, so it must be %q, the repository the project is published from; a different one points at another repository and module-derived SBOM/purl metadata never names the released project",
			modulePathFile, modulePath, imageSourceLabel, wantModulePath, wantModulePath))
	}

	switch {
	case strings.TrimSpace(releaseImage) == "":
		problems = append(problems, fmt.Sprintf("no %s reference was read from %s, so the image the repository deploys cannot be compared with the module path", releaseImageConstName, releasePinFile))
	default:
		got, err := repositoryPathFromImageRef(releaseImage)
		switch {
		case err != nil:
			problems = append(problems, fmt.Sprintf("release image %q cannot be read as a repository reference: %v", releaseImage, err))
		case repoPath != "" && got != repoPath:
			problems = append(problems, fmt.Sprintf("release image %q publishes from repository %q, but the image's %s label names %q: the module path %q describes only the second repository", releaseImage, got, imageSourceLabel, repoPath, wantModulePath))
		}
	}

	refs := labelerImageReferences(manifestImages)
	if len(refs) == 0 {
		problems = append(problems, fmt.Sprintf("no %s image reference was read from %s, so the manifests cannot be compared with the module path", sidecarImage, manifestDir))
	}
	for _, ref := range refs {
		got, err := repositoryPathFromImageRef(ref)
		if err != nil {
			problems = append(problems, fmt.Sprintf("manifest image %q cannot be read as a repository reference: %v", ref, err))
			continue
		}
		if repoPath != "" && got != repoPath {
			problems = append(problems, fmt.Sprintf("manifest image %q publishes from repository %q, but the image's %s label names %q: the module path %q describes only the second repository", ref, got, imageSourceLabel, repoPath, wantModulePath))
		}
	}

	return problems
}
