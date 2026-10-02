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

// Module-graph status check: GitHub keys a required status check on the name of
// the job that reported it, not on the steps that did the work. Branch
// protection on this repository requires the `update-go_modules-graph` check, so
// the module-graph gate has to stay a job of its own: folding its commands into
// another job (or renaming the job while reshuffling the workflow) keeps running
// them while the required context stops reporting, and every pull request then
// waits on a check that can no longer arrive. The reviewed conflict was exactly
// that: the job was gone from .github/workflows/ci.yml while the check was still
// required. No compiler can see a workflow file, so the check lives here
// (main_test.go: TestWorkflowsReportModuleGraphCheck) and the job itself runs in
// CI on every push and pull request.

// moduleGraphJobName is the job name - and therefore the status-check context -
// the module-graph gate must keep reporting under.
const moduleGraphJobName = "update-go_modules-graph"

// moduleGraphJobProblems returns one message per workflow under dir that
// declares the module-graph job without rebuilding the graph and asserting it is
// non-empty, plus the number of workflows declaring that job, so a caller can
// tell "no problems" from "no job declared, so the check ran against nothing".
// The workflow set as a whole must declare the job exactly once: it is the
// status check pull requests wait for.
func moduleGraphJobProblems(dir string) ([]string, int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, err
	}

	var problems []string
	jobs := 0
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
			return nil, 0, err
		}
		found, declared := moduleGraphJobProblemsIn(path, string(data))
		problems = append(problems, found...)
		if declared {
			jobs++
		}
	}

	switch {
	case jobs == 0:
		problems = append(problems, fmt.Sprintf(
			"%s: no workflow declares the %s job, so the required %s status check never reports and pull requests wait for a check that cannot arrive; the graph commands must run in a job of that name",
			dir, moduleGraphJobName, moduleGraphJobName))
	case jobs > 1:
		problems = append(problems, fmt.Sprintf(
			"%s: %d workflows declare the %s job, so the same status check reports more than once per commit",
			dir, jobs, moduleGraphJobName))
	}
	sort.Strings(problems)
	return problems, jobs, nil
}

// moduleGraphJobProblemsIn scans one workflow's text for the module-graph job.
func moduleGraphJobProblemsIn(path, text string) ([]string, bool) {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if !isJobDeclaration(line, moduleGraphJobName) {
			continue
		}

		body := jobBody(lines[i+1:])
		var problems []string
		if !strings.Contains(body, "go mod graph") {
			problems = append(problems, fmt.Sprintf(
				"%s:%d: the %s job does not run `go mod graph`, so the graph its status check stands for is never rebuilt",
				path, i+1, moduleGraphJobName))
		}
		if !strings.Contains(body, "test -s") {
			problems = append(problems, fmt.Sprintf(
				"%s:%d: the %s job does not assert the graph file is non-empty (`test -s`), so an empty graph would report the required check green",
				path, i+1, moduleGraphJobName))
		}
		return problems, true
	}
	return nil, false
}

// isJobDeclaration reports whether a line is the YAML job key of the named job:
// an indented `name:` line, not a comment and not a top-level key (a top-level
// key is a workflow input, and a commented-out job reports nothing).
func isJobDeclaration(line, name string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed != name+":" || strings.HasPrefix(trimmed, "#") {
		return false
	}
	return len(line) > len(trimmed)
}

// jobBody returns the text of a job body: everything after the job key up to the
// next top-level key. Job bodies in these workflows are indented, so the first
// unindented line ends the job.
func jobBody(lines []string) string {
	var body []string
	for _, line := range lines {
		if line != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			break
		}
		body = append(body, line)
	}
	return strings.Join(body, "\n")
}
