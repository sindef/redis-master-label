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
