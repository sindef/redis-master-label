package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Go toolchain pin check: CI must state exactly one Go version source, and it
// must be go.mod's `go` directive. actions/setup-go accepts both go-version and
// go-version-file but honours only one of them - with both inputs given it warns
// that it ignores the file input - so a stray `go-version:` line silently
// decides which toolchain the job installs while the workflow text claims
// otherwise. The reviewed defect was exactly that: `go-version: "1.23"` on the
// same step as `go-version-file: go.mod` while go.mod declared go 1.26.0, so the
// job installed a Go the build never used (GOTOOLCHAIN=auto then downloaded the
// go.mod toolchain) or carried a dead pin that misreads as the CI toolchain.
//
// No compiler or `go test` invocation can see a workflow file, so the check
// lives here (main_test.go: TestWorkflowsDeclareSingleGoToolchainSource) and the
// CI job's "Check Go toolchain" step proves the same agreement at run time by
// printing the installed toolchain next to go.mod's directive.

// ciWorkflowDir holds the workflow definitions GitHub Actions runs.
const ciWorkflowDir = ".github/workflows"

// goToolchainFile is the file whose `go` directive is the single source of
// truth for the toolchain every workflow must install.
const goToolchainFile = "go.mod"

// The setup-go inputs that name a toolchain version. Only goVersionFileInput may
// appear, and only with goToolchainFile as its value.
const (
	goVersionInput     = "go-version"
	goVersionFileInput = "go-version-file"
)

// workflowGoToolchainProblems returns one message per Go toolchain declaration
// in the workflows under dir that is not "install the version go.mod declares",
// plus one per setup-go step that declares no version source at all. The second
// result counts the setup-go steps seen, so a caller can tell "no problems"
// apart from "no workflow declared a toolchain, so nothing was checked".
func workflowGoToolchainProblems(dir string) ([]string, int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, err
	}

	var problems []string
	setupGoSteps := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if ext := filepath.Ext(name); ext != ".yml" && ext != ".yaml" {
			continue
		}
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, 0, err
		}
		found, steps := workflowGoToolchainProblemsIn(path, string(data))
		problems = append(problems, found...)
		setupGoSteps += steps
	}
	sort.Strings(problems)
	return problems, setupGoSteps, nil
}

// workflowGoToolchainProblemsIn scans one workflow's text for toolchain pins.
func workflowGoToolchainProblemsIn(path, text string) ([]string, int) {
	var problems []string
	setupGoSteps := 0
	versionFiles := 0
	for i, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		// A commented-out input is not a pin: the reviewed workflow only ever
		// had live keys, and CI does not read comments.
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.Contains(trimmed, "actions/setup-go") {
			setupGoSteps++
		}
		key, value, ok := workflowInput(trimmed)
		if !ok {
			continue
		}
		switch key {
		case goVersionInput:
			problems = append(problems, fmt.Sprintf(
				"%s:%d: declares an explicit toolchain (go-version: %s); setup-go then ignores %s, so the job installs a Go version go.mod does not declare and the CI toolchain cannot be read from the files",
				path, i+1, value, goVersionFileInput))
		case goVersionFileInput:
			versionFiles++
			if value != goToolchainFile {
				problems = append(problems, fmt.Sprintf(
					"%s:%d: %s: %s, want %s (the file declaring the toolchain version)",
					path, i+1, goVersionFileInput, value, goToolchainFile))
			}
		}
	}
	if setupGoSteps > 0 && versionFiles == 0 {
		problems = append(problems, fmt.Sprintf(
			"%s: the setup-go step declares no %s, so the job installs an unpinned Go toolchain",
			path, goVersionFileInput))
	}
	return problems, setupGoSteps
}

// goModGoDirective returns the version of the module file's `go` directive, for
// example "1.26.0" for `go 1.26.0`. setup-go's go-version-file input reads this
// line, so it is the version CI must install and the version the job's
// "Check Go toolchain" step prints back.
func goModGoDirective(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		if fields := strings.Fields(line); len(fields) == 2 && fields[0] == "go" {
			return fields[1], nil
		}
	}
	return "", fmt.Errorf("%s: no go directive declaring the toolchain version", path)
}

// workflowInput splits a `key: value` YAML mapping line, dropping a trailing
// comment and the quotes around the value. It reports ok=false for any line that
// is not such a mapping entry, so step names, list items and `uses:` lines are
// not mistaken for toolchain inputs.
func workflowInput(line string) (key, value string, ok bool) {
	key, value, ok = strings.Cut(line, ":")
	if !ok {
		return "", "", false
	}
	key = strings.TrimSpace(key)
	if key == "" || strings.ContainsAny(key, " \t\"'") {
		return "", "", false
	}
	value = strings.TrimSpace(value)
	if i := strings.Index(value, " #"); i >= 0 {
		value = strings.TrimSpace(value[:i])
	}
	value = strings.Trim(strings.TrimSpace(value), `"'`)
	if value == "" {
		return "", "", false
	}
	return key, value, true
}
