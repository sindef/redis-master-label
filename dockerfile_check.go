package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Dockerfile version-stamp check: `docker build .` with no --build-arg VERSION
// must produce an image whose `--version` prints "redis-master-label dev", and
// `--build-arg VERSION=vX.Y.Z` must print that exact tag. Go tests compile
// without the linker stamp, so TestVersionDefaultsToDev cannot see a build file
// that expands an empty VERSION into `-ldflags "-X main.version="` and so
// overrides main.go's `var version = "dev"` with an empty string. Reading the
// build file here makes that regression fail offline; CI's "Verify image
// version output" step runs the real image and asserts the same output.
//
// The same reader also reports the LABELs of the build file's final stage
// (dockerfileFinalStageLabels), because image metadata written in a discarded
// builder stage - the license label among it - never reaches the shipped image.
// The tests in main_test.go use it to keep the advertised license and the
// LICENSE file the repository actually grants in agreement.

// versionShellDefaultRe matches the shell parameter-expansion default that the
// go build step uses, for example ${VERSION:-dev}.
var versionShellDefaultRe = regexp.MustCompile(`\$\{VERSION:-([^}]*)\}`)

// buildFileUnstampedVersion returns the version the build file bakes into the
// binary when the build runs with no --build-arg VERSION: the `ARG VERSION`
// default, or the go build step's shell fallback when that default is empty. An
// empty result means the image would report an empty version, which is the
// defect this guards.
func buildFileUnstampedVersion(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}

	argDefault := ""
	shellDefault := ""
	sawArg := false
	sawStamp := false
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch strings.ToUpper(fields[0]) {
		case "ARG":
			if len(fields) < 2 || !strings.HasPrefix(fields[1], "VERSION") {
				continue
			}
			sawArg = true
			if i := strings.Index(fields[1], "="); i >= 0 {
				argDefault = strings.Trim(fields[1][i+1:], `"'`)
			}
		case "RUN":
			// Only the stamping build step counts: it must pass the version
			// variable itself to the linker, so a tag given through
			// --build-arg VERSION reaches the binary.
			if !strings.Contains(line, "go build") || !strings.Contains(line, "-X main.version=${VERSION}") {
				continue
			}
			sawStamp = true
			if m := versionShellDefaultRe.FindStringSubmatch(line); m != nil {
				shellDefault = m[1]
			}
		}
	}

	if !sawArg {
		return "", fmt.Errorf("%s: no ARG VERSION declaring the version default", path)
	}
	if !sawStamp {
		return "", fmt.Errorf("%s: no go build step stamping -X main.version=${VERSION}", path)
	}
	if argDefault != "" {
		return argDefault, nil
	}
	return shellDefault, nil
}

// locateBuildFile returns the repository build file, matching the same
// Dockerfile-then-Containerfile resolution CI uses.
func locateBuildFile() (string, error) {
	for _, name := range []string{"Dockerfile", "Containerfile"} {
		if _, err := os.Stat(name); err == nil {
			return name, nil
		}
	}
	return "", fmt.Errorf("no build file found (looked for Dockerfile, Containerfile)")
}

// licenseFile is the repo-root file granting the project's license. It is the
// grant behind the image's org.opencontainers.image.licenses label: without it
// the label claims a license the project never gives.
const licenseFile = "LICENSE"

// imageLicenseLabel is the OCI label (image annotation) that declares which
// license the shipped image is under. Its value must name the license the
// repository grants in licenseFile.
const imageLicenseLabel = "org.opencontainers.image.licenses"

// labelTokenRe matches one key="value", key='value' or key=value token of a
// LABEL instruction.
var labelTokenRe = regexp.MustCompile(`([A-Za-z0-9._-]+)=("[^"]*"|'[^']*'|\S+)`)

// finalStageStart returns the index of the last FROM line in a build file, that
// is the start of the stage whose filesystem becomes the shipping image, or -1
// when the file declares no stage at all.
func finalStageStart(lines []string) int {
	start := -1
	for i, line := range lines {
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(line)), "FROM ") {
			start = i
		}
	}
	return start
}

// dockerfileFinalStageLabels returns the LABEL key/value pairs declared by the
// build file's final stage, keyed by lowercased label name. Only the last FROM
// stage counts: a LABEL in an earlier (builder) stage is discarded with that
// stage, so `docker inspect` on the pulled image shows neither the label nor
// anything it claims - which is exactly how an image can advertise a license
// nobody sees.
func dockerfileFinalStageLabels(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	lines := strings.Split(string(data), "\n")
	start := finalStageStart(lines)
	if start < 0 {
		return nil, fmt.Errorf("%s: no FROM stage found", path)
	}

	labels := make(map[string]string)
	pending := ""
	flush := func() {
		for _, token := range labelTokenRe.FindAllStringSubmatch(pending, -1) {
			labels[strings.ToLower(token[1])] = strings.Trim(token[2], `"'`)
		}
		pending = ""
	}
	for _, line := range lines[start+1:] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if pending == "" && !strings.HasPrefix(strings.ToUpper(trimmed), "LABEL ") {
			continue
		}
		// LABEL values may be split over several lines with a trailing
		// backslash; join them before reading the key=value tokens.
		continues := strings.HasSuffix(trimmed, "\\")
		if continues {
			trimmed = strings.TrimSpace(strings.TrimSuffix(trimmed, "\\"))
		}
		pending = strings.TrimSpace(pending + " " + trimmed)
		if !continues {
			flush()
		}
	}
	flush()
	return labels, nil
}

// ociLabelKeys are the OCI image metadata labels the published image must
// carry. They belong to the build file's final stage: a LABEL written in a
// discarded builder stage parses exactly the same in the file yet reaches no
// image, which is how a shipped image can end up with no title, source, license
// or version for anyone pulling it.
var ociLabelKeys = []string{
	"org.opencontainers.image.title",
	"org.opencontainers.image.description",
	"org.opencontainers.image.source",
	"org.opencontainers.image.licenses",
	"org.opencontainers.image.version",
}

// ociLabelsProblem returns why the shipping image's OCI metadata is incomplete,
// or nil when every expected label carries a non-empty value.
func ociLabelsProblem(labels map[string]string) error {
	var missing []string
	for _, key := range ociLabelKeys {
		if strings.TrimSpace(labels[key]) == "" {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("shipping image declares no non-empty %s (a label in a discarded builder stage never reaches the image)", strings.Join(missing, ", "))
	}
	return nil
}

// ociVersionLabelProblem returns why the version label would not report the
// release tag, or nil when it is stamped from the VERSION build arg. A literal
// tag in the label would go stale at the next release, and a VERSION that does
// not reach the label ships an image nobody can identify.
func ociVersionLabelProblem(labels map[string]string) error {
	const key = "org.opencontainers.image.version"
	if got := labels[key]; got != "${VERSION}" {
		return fmt.Errorf("%s = %q, want ${VERSION} so --build-arg VERSION=<tag> stamps the label", key, got)
	}
	return nil
}

// dockerfileFinalStageDeclaresArg reports whether the build file's final stage
// declares the named ARG. Build args do not cross stage boundaries, so an ARG
// left in the builder stage leaves the final stage's ${VERSION} empty and the
// version label blank.
func dockerfileFinalStageDeclaresArg(path, name string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}

	lines := strings.Split(string(data), "\n")
	start := finalStageStart(lines)
	if start < 0 {
		return false, fmt.Errorf("%s: no FROM stage found", path)
	}

	for _, raw := range lines[start+1:] {
		fields := strings.Fields(raw)
		if len(fields) < 2 || !strings.EqualFold(fields[0], "ARG") {
			continue
		}
		if fields[1] == name || strings.HasPrefix(fields[1], name+"=") {
			return true, nil
		}
	}
	return false, nil
}

// imageLicenseProblem returns why the shipping image's declared license does not
// match the license the repository grants, or nil when the two agree.
func imageLicenseProblem(labels map[string]string, granted string) error {
	declared, ok := labels[imageLicenseLabel]
	if !ok {
		return fmt.Errorf("shipping image declares no %s label, want %q", imageLicenseLabel, granted)
	}
	if declared != granted {
		return fmt.Errorf("shipping image declares %s=%q, but %s grants %q", imageLicenseLabel, declared, licenseFile, granted)
	}
	return nil
}
