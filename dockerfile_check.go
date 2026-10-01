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
// The same file reads the OCI labels of the shipping stage, because the image's
// license claim is metadata no Go test can observe otherwise: a label written in
// the discarded builder stage never reaches the published image (the release
// hygiene defect this guards), and a label naming a license the repository does
// not grant misleads everyone who pulls the image. CI's "Verify image license
// metadata" step checks the built image with `docker inspect`.

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

// finalStageStart returns the build file's lines and the index of the line that
// begins its final stage. The final stage is what ships the image, so it is the
// only stage whose instructions (USER, LABEL) describe the published image: an
// instruction in an earlier builder stage is discarded with that stage and must
// not be read as if the image carried it.
func finalStageStart(path string) (lines []string, start int, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}

	lines = strings.Split(string(data), "\n")
	start = -1
	for i, line := range lines {
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(line)), "FROM ") {
			start = i
		}
	}
	if start < 0 {
		return nil, 0, fmt.Errorf("%s: no FROM stage found", path)
	}
	return lines, start, nil
}

// The license the repository grants is stated by the LICENSE file, and the
// image states the same license through this OCI label. Both are checked
// against each other so the repository cannot claim one license in its
// published image metadata and grant another (or nothing at all) in its files.
const (
	licenseFile       = "LICENSE"
	imageLicenseLabel = "org.opencontainers.image.licenses"

	// mitLicensePermission is a distinctive sentence from the MIT license: its
	// presence is what makes the file a grant of MIT rather than a document
	// that merely mentions the name.
	mitLicensePermission = "Permission is hereby granted, free of charge"
)

// licenseIdentifier returns the SPDX identifier of the license the repository
// grants, read from the LICENSE file. The full text must be present: a file
// whose title or abbreviation mentions MIT without the permission grant and
// warranty disclaimer grants nothing, and any other license must be rejected
// instead of being reported as MIT.
func licenseIdentifier(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	text := string(data)
	if strings.Contains(text, "MIT License") && strings.Contains(text, mitLicensePermission) {
		return "MIT", nil
	}
	return "", fmt.Errorf("%s does not grant MIT: %q or the MIT permission grant is missing", path, "MIT License")
}

// dockerfileFinalStageLabels returns the OCI labels the build file's final stage
// sets, keyed by label name. Only the last FROM stage is read, so a LABEL in a
// discarded builder stage is not reported as metadata of the published image,
// and backslash continuations are joined before parsing because a LABEL block
// is normally split over several lines.
func dockerfileFinalStageLabels(path string) (map[string]string, error) {
	lines, start, err := finalStageStart(path)
	if err != nil {
		return nil, err
	}

	labels := map[string]string{}
	for _, instruction := range buildInstructions(lines[start:]) {
		fields := strings.Fields(instruction)
		if len(fields) < 2 || !strings.EqualFold(fields[0], "LABEL") {
			continue
		}
		for name, value := range parseLabelArgs(strings.TrimSpace(strings.TrimPrefix(instruction, fields[0]))) {
			labels[name] = value
		}
	}
	return labels, nil
}

// buildInstructions joins backslash continuations so each returned string is
// one whole instruction, and drops blank lines and comments.
func buildInstructions(lines []string) []string {
	var instructions []string
	var current strings.Builder
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if current.Len() == 0 {
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
		}
		continued := strings.HasSuffix(trimmed, "\\")
		current.WriteString(strings.TrimSpace(strings.TrimSuffix(trimmed, "\\")))
		if continued {
			current.WriteString(" ")
			continue
		}
		instructions = append(instructions, strings.TrimSpace(current.String()))
		current.Reset()
	}
	if current.Len() > 0 {
		instructions = append(instructions, strings.TrimSpace(current.String()))
	}
	return instructions
}

// parseLabelArgs splits a LABEL instruction's arguments into label name/value
// pairs. Both the modern `name=value` form and the legacy `name value` form are
// accepted, and a quoted value keeps the spaces it contains.
func parseLabelArgs(args string) map[string]string {
	pairs := map[string]string{}
	tokens := splitQuotedFields(args)
	for i := 0; i < len(tokens); i++ {
		name, value, hasValue := strings.Cut(tokens[i], "=")
		if !hasValue {
			// Legacy form: LABEL name value
			if i+1 >= len(tokens) {
				continue
			}
			i++
			value = tokens[i]
		}
		if name != "" {
			pairs[name] = value
		}
	}
	return pairs
}

// splitQuotedFields splits s on unquoted whitespace and strips the quotes
// around a value, so `name="two words"` yields the tokens `name=` and
// `two words`.
func splitQuotedFields(s string) []string {
	var fields []string
	var current strings.Builder
	var quote rune
	started := false
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
				continue
			}
			current.WriteRune(r)
		case r == '"' || r == '\'':
			quote = r
			started = true
		case r == ' ' || r == '\t':
			if started {
				fields = append(fields, current.String())
				current.Reset()
				started = false
			}
		default:
			started = true
			current.WriteRune(r)
		}
	}
	if started {
		fields = append(fields, current.String())
	}
	return fields
}

// imageLicenseProblem reports why the shipping image does not advertise the
// license the repository grants (granted, normally from licenseIdentifier), or
// nil when the image and the repository agree. A missing label means a pulled
// image advertises no license at all, and a different value means the image
// claims a license this project does not grant.
func imageLicenseProblem(labels map[string]string, granted string) error {
	claimed := strings.TrimSpace(labels[imageLicenseLabel])
	if claimed == "" {
		return fmt.Errorf("the build file's final stage sets no %s label, so the published image advertises no license (the repository grants %s in %s)", imageLicenseLabel, granted, licenseFile)
	}
	if !strings.EqualFold(claimed, granted) {
		return fmt.Errorf("the image advertises license %q but the repository grants %q in %s", claimed, granted, licenseFile)
	}
	return nil
}
