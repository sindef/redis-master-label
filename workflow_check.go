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
// plus one message per setup-go step that declares no version source at all:
// version sources count per setup-go step, not per workflow file, so each step
// that uses actions/setup-go must carry its own. The second
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
// Version sources are tracked per setup-go step, not per file: setup-go
// installs a toolchain for the step that uses it, so a `go-version-file:` in
// some other step never pins this one and a second setup-go step is not
// covered by the first step's declaration. Every step that uses
// actions/setup-go must carry its own version source; a step without one is
// reported once, at the line of its `uses:`.
func workflowGoToolchainProblemsIn(path, text string) ([]string, int) {
	var problems []string
	setupGoSteps := 0
	// Step state: which setup-go step is the current `uses:` in, where that
	// `uses:` line stands, and which version sources its own `with:` block has
	// declared so far.
	setupWithOpen := false
	stepIndent := 0
	setupStartLine := 0
	sources := 0
	finishSetupStep := func(line int) {
		if setupWithOpen {
			if sources == 0 {
				problems = append(problems, fmt.Sprintf(
					"%s:%d: a setup-go step declares no %s, so the job installs an unpinned Go toolchain",
					path, line, goVersionFileInput))
			}
			setupWithOpen = false
		}
	}
	for i, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		// A commented-out input is not a pin: the reviewed workflow only ever
		// had live keys, and CI does not read comments.
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(rawLineIndent(line))
		onSetupGo := strings.Contains(trimmed, "actions/setup-go")
		key, value, ok := workflowInput(trimmed)
		// A later `uses:`, a step item like `- name:` at the step's own
		// indentation, or any mapping key the step does not own ends the
		// setup-go step: its `with:` block is closed by now.
		nextStep := setupWithOpen && (onSetupGo ||
			(ok && indent <= stepIndent) ||
			(strings.HasPrefix(trimmed, "- ") && indent <= stepIndent))
		if nextStep {
			finishSetupStep(setupStartLine)
		}
		if onSetupGo {
			setupGoSteps++
			stepIndent = indent
			setupStartLine = i + 1
			sources = 0
			setupWithOpen = true
			continue
		}
		if !ok {
			continue
		}
		switch key {
		case goVersionInput:
			problems = append(problems, fmt.Sprintf(
				"%s:%d: declares an explicit toolchain (go-version: %s); setup-go then ignores %s, so the job installs a Go version go.mod does not declare and the CI toolchain cannot be read from the files",
				path, i+1, value, goVersionFileInput))
			// A go-version still is a version source for its own step: the
			// problem above already rejects the pin, so the source tally must
			// not add a second message for the same step.
			if setupWithOpen {
				sources++
			}
		case goVersionFileInput:
			// Only the setup-go step's own `with:` block can host its version
			// source; a `go-version-file:` line in any other step, job or
			// top-level mapping satisfies no setup-go step. A source naming a
			// file other than go.mod is still the step's own source, so the
			// per-step problem quota is filled, but the wrong file keeps its
			// own message below.
			if setupWithOpen {
				sources++
			}
			if value != goToolchainFile {
				problems = append(problems, fmt.Sprintf(
					"%s:%d: %s: %s, want %s (the file declaring the toolchain version)",
					path, i+1, goVersionFileInput, value, goToolchainFile))
			}
		}
	}
	finishSetupStep(setupStartLine)
	return problems, setupGoSteps
}

// rawLineIndent returns the leading whitespace run of one line, which is the
// only signal a YAML file gives for where a mapping belongs.
func rawLineIndent(line string) string {
	return line[:len(line)-len(strings.TrimLeft(line, " \t"))]
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

// attestationActionsStr and the two permission keys below name the documented
// permission set docker/build-push-action needs when a push requests
// provenance/SBOM attestations: the attestation manifests are signed and
// uploaded with this workflow's OIDC identity, so the job needs id-token: write
// and attestations: write. The reviewed defect was release.yml asking for
// provenance: mode=max and sbom: true on push while the permissions block named
// only contents: read and packages: write, so the README's claim about image
// attestations could not be satisfied on tag push.
const attestationActionStr = "docker/build-push-action"

const (
	idTokenPermission   = "id-token"
	attestPermission    = "attestations"
	writePermissionName = "write"
)

// attestationPermissionsProblems returns one message per workflow under dir
// that pushes images with provenance/SBOM attestations but does not declare
// id-token: write and attestations: write. A workflow that never requests
// attestations is not flagged.
func attestationPermissionsProblems(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var problems []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if ext := filepath.Ext(entry.Name()); ext != ".yml" && ext != ".yaml" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		problems = append(problems, attestationPermissionsProblemsIn(path, string(data))...)
	}
	return problems, nil
}

// attestationPermissionsProblemsIn scans one workflow's text.
func attestationPermissionsProblemsIn(path, text string) []string {
	var problems []string
	attestationInputs := 0
	idToken := false
	attests := false
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			continue
		}
		if strings.Contains(line, attestationActionStr) {
			continue
		}
		key, value, ok := workflowInput(line)
		if !ok {
			continue
		}
		switch key {
		case "provenance", "sbom":
			attestationInputs++
		case idTokenPermission:
			idToken = value == writePermissionName
		case attestPermission:
			attests = value == writePermissionName
		}
	}
	if attestationInputs > 0 && (!idToken || !attests) {
		problems = append(problems, fmt.Sprintf(
			"%s: %d provenance/SBOM attestation inputs but id-token: write=%v, attestations: write=%v; build-push-action's documented push permission set is the pair of those writes",
			path, attestationInputs, idToken, attests))
	}
	return problems
}
