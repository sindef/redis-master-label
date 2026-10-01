package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Go toolchain pin check: every setup-go step must state exactly one Go version
// source, and it must be go.mod's `go` directive. actions/setup-go accepts both
// go-version and go-version-file but honours only one of them - with both inputs
// given it warns
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
// plus one per setup-go step that declares no version source of its own (a
// source another step or another file declares never covers it). The second
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
// Each setup-go step is judged on its own inputs: two setup-go steps in one
// workflow are two installs, so a version source declared by one of them says
// nothing about the other. Counting the sources per file let the reviewed shape
// pass - a build job with `go-version-file: go.mod` plus a second job whose step
// declared only `cache: true` produced no problem at all, and that step silently
// installed whatever Go the runner defaulted to, which is exactly the drift this
// check exists to catch.
func workflowGoToolchainProblemsIn(path, text string) ([]string, int) {
	var problems []string

	// One record per YAML step item, so the version sources a step declares can
	// be attributed to that step. A step item opens at the workflow's step list
	// indentation; a list indented deeper is nested inside the step it belongs
	// to (a step's `args:` list) and does not open a new one.
	type setupGoStep struct {
		line           int
		setupGo        bool
		versionSources int
	}
	var steps []*setupGoStep
	var current *setupGoStep
	stepIndent := -1

	for i, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		// A commented-out input is not a pin: the reviewed workflow only ever
		// had live keys, and CI does not read comments.
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if indent, isItem := workflowStepItemIndent(line); isItem && (stepIndent < 0 || indent <= stepIndent) {
			stepIndent = indent
			current = &setupGoStep{line: i + 1}
			steps = append(steps, current)
		}

		key, value, ok := workflowInput(workflowItemText(trimmed))
		if !ok {
			continue
		}
		if key == "uses" && strings.Contains(value, "actions/setup-go") {
			if current == nil {
				current = &setupGoStep{line: i + 1}
				steps = append(steps, current)
			}
			current.setupGo = true
			continue
		}
		switch key {
		case goVersionInput:
			problems = append(problems, fmt.Sprintf(
				"%s:%d: declares an explicit toolchain (go-version: %s); setup-go then ignores %s, so the job installs a Go version go.mod does not declare and the CI toolchain cannot be read from the files",
				path, i+1, value, goVersionFileInput))
		case goVersionFileInput:
			if value != goToolchainFile {
				problems = append(problems, fmt.Sprintf(
					"%s:%d: %s: %s, want %s (the file declaring the toolchain version)",
					path, i+1, goVersionFileInput, value, goToolchainFile))
			}
			// A source outside any step cannot cover a setup-go step either, so
			// the count is only read back for the step that declared it.
			if current != nil {
				current.versionSources++
			}
		}
	}

	setupGoSteps := 0
	for _, step := range steps {
		if !step.setupGo {
			continue
		}
		setupGoSteps++
		if step.versionSources == 0 {
			problems = append(problems, fmt.Sprintf(
				"%s:%d: the setup-go step declares no %s, so that step installs an unpinned Go toolchain",
				path, step.line, goVersionFileInput))
		}
	}
	return problems, setupGoSteps
}

// workflowStepItemIndent reports the indentation of a YAML sequence item, so a
// workflow step can be told apart from a deeper list nested inside a step.
func workflowStepItemIndent(line string) (int, bool) {
	indent := len(line) - len(strings.TrimLeft(line, " "))
	if !strings.HasPrefix(line[indent:], "- ") {
		return 0, false
	}
	return indent, true
}

// workflowItemText drops the "- " opening a sequence item, so a step's first
// line (`- uses: actions/setup-go@v7`) parses like any other mapping entry.
func workflowItemText(trimmed string) string {
	rest, ok := strings.CutPrefix(trimmed, "- ")
	if !ok {
		return trimmed
	}
	return strings.TrimSpace(rest)
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
// is not such a mapping entry, so step names and other non-mapping lines are
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
