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

// Release tag gate: release.yml is the only place a `vX.Y.Z` tag becomes a
// published image, and `version="${GITHUB_REF_NAME}"` is the only source of the
// published tag. Nothing compared that tag with the image the repository
// deploys: `go vet` and the full test suite pass on `v0.2.0` while the pin is
// still v0.1.0 (TestManifestsPinVersionedReleaseImage only compares the pin with
// manifests/), so the run published an image no manifest references, and the
// README's three-file bump was the only thing keeping tag and pin aligned. A tag
// pushed from a commit that never merged bypassed CI entirely. No compiler and no
// `go test` can see a workflow file, so the workflow itself must carry both
// gates, and this scan (main_test.go:
// TestReleaseWorkflowGatesTagOnPinnedReleaseImage) fails the pull request when a
// workflow that triggers on release tags loses one of them or runs it after the
// image is built.

const (
	// releaseTagTrigger is the push.tags pattern release.yml selects on.
	releaseTagTrigger = "v*.*.*"

	// releaseImageConstName is the pinned reference the release tag must equal:
	// the constant TestManifestsPinVersionedReleaseImage holds manifests/ to.
	releaseImageConstName = "releaseImage"

	// releasePinFile declares that constant.
	releasePinFile = "main_test.go"

	// releaseGateFetchDepth is the checkout depth the ancestry gate needs: a
	// shallow checkout holds neither main nor the history joining it to the tag.
	releaseGateFetchDepth = "fetch-depth: 0"
)

// workflowStep is one item of a job's `steps:` list: the line of its `- name:`
// or `- uses:` item and the text up to the next item. Without a step split the
// scan could not tell whether a gate runs before the build, and text anywhere in
// the file (a comment included) would look like a gate.
type workflowStep struct {
	line int
	body string
}

// workflowSteps splits workflow text into job steps. A step starts at an
// indented list item whose key is name or uses; list items elsewhere (a tags
// pattern, run arguments) start with other keys and are not steps.
func workflowSteps(text string) []workflowStep {
	lines := strings.Split(text, "\n")
	var steps []workflowStep
	start := -1
	closeStep := func(end int) {
		if start >= 0 {
			steps = append(steps, workflowStep{
				line: start + 1,
				body: strings.Join(lines[start:end], "\n"),
			})
		}
		start = -1
	}
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		item := strings.TrimPrefix(trimmed, "- ")
		if item != trimmed && len(line) > len(trimmed) &&
			(strings.HasPrefix(item, "name:") || strings.HasPrefix(item, "uses:")) {
			closeStep(i)
			start = i
		}
	}
	closeStep(len(lines))
	return steps
}

// stepBuildsImage reports whether a step builds or pushes the release image.
// Both markers count: the Docker CLI build in release.yml and the
// build-push-action the publish step uses.
func stepBuildsImage(body string) bool {
	return strings.Contains(body, "docker build") || strings.Contains(body, attestationActionStr)
}

// stepPinsTag reports whether a step compares the tag being released with the
// pinned release image and fails the run when they differ: it reads the
// releaseImage constant out of main_test.go, names the tag GitHub sets for the
// push, and exits non-zero on disagreement. All four must be in the same step,
// because a step that reads the pin without comparing it releases anything.
func stepPinsTag(body string) bool {
	return strings.Contains(body, releaseImageConstName) &&
		strings.Contains(body, releasePinFile) &&
		(strings.Contains(body, "GITHUB_REF_NAME") || strings.Contains(body, "github.ref_name")) &&
		strings.Contains(body, "exit 1")
}

// stepChecksMainAncestry reports whether a step refuses a tag whose commit is
// not on main. `git merge-base --is-ancestor <commit> <main tip>` is that
// answer, so the step must run it against a main ref and fail the run.
func stepChecksMainAncestry(body string) bool {
	return strings.Contains(body, "merge-base") &&
		strings.Contains(body, "--is-ancestor") &&
		strings.Contains(body, "main") &&
		strings.Contains(body, "exit 1")
}

// releaseTagGateProblems returns one message per problem in the workflows under
// dir that trigger on release tags, plus the number of such workflows, so a
// caller can tell "no problems" from "no release workflow found, so nothing was
// checked".
func releaseTagGateProblems(dir string) ([]string, int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, err
	}

	var problems []string
	releaseWorkflows := 0
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
		found, release := releaseTagGateProblemsIn(path, string(data))
		problems = append(problems, found...)
		if release {
			releaseWorkflows++
		}
	}
	sort.Strings(problems)
	return problems, releaseWorkflows, nil
}

// releaseTagGateProblemsIn scans one workflow's text. A workflow that never
// triggers on release tags is not flagged: it publishes nothing on a tag push.
func releaseTagGateProblemsIn(path, text string) ([]string, bool) {
	triggers := 0
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.Contains(trimmed, releaseTagTrigger) {
			triggers++
		}
	}
	if triggers == 0 {
		return nil, false
	}

	steps := workflowSteps(text)
	buildLine := 0
	for _, step := range steps {
		if stepBuildsImage(step.body) {
			buildLine = step.line
			break
		}
	}

	var problems []string
	pinGateLine := 0
	ancestryLine := 0
	for _, step := range steps {
		if pinGateLine == 0 && stepPinsTag(step.body) {
			pinGateLine = step.line
		}
		if ancestryLine == 0 && stepChecksMainAncestry(step.body) {
			ancestryLine = step.line
		}
	}

	switch {
	case pinGateLine == 0:
		problems = append(problems, fmt.Sprintf(
			"%s: triggers on release tags (%s) but no step compares the tag with the %s constant in %s and fails on a mismatch, so a tag that no manifest pins publishes an image nothing deploys",
			path, releaseTagTrigger, releaseImageConstName, releasePinFile))
	case buildLine != 0 && pinGateLine > buildLine:
		problems = append(problems, fmt.Sprintf(
			"%s:%d: the tag/pin gate runs after the image is built at line %d, so the release is published before the tag is compared with the pin",
			path, pinGateLine, buildLine))
	}

	switch {
	case ancestryLine == 0:
		problems = append(problems, fmt.Sprintf(
			"%s: triggers on release tags (%s) but no step refuses a tag whose commit is not reachable from main, so a tag pushed from a commit that never merged is released",
			path, releaseTagTrigger))
	case buildLine != 0 && ancestryLine > buildLine:
		problems = append(problems, fmt.Sprintf(
			"%s:%d: the main-reachability gate runs after the image is built at line %d, so the release is published before the tag is checked",
			path, ancestryLine, buildLine))
	}

	// `git merge-base --is-ancestor` needs the history joining the tag to main,
	// and actions/checkout is shallow by default. Without the full history the
	// ancestry gate either cannot resolve the commit or answers from a partial
	// graph, which is the bypass it exists to close.
	if !strings.Contains(text, releaseGateFetchDepth) {
		problems = append(problems, fmt.Sprintf(
			"%s: checkout does not request %q, so `git merge-base --is-ancestor` cannot tell whether the tagged commit is on main",
			path, releaseGateFetchDepth))
	}
	return problems, true
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

// Module-graph output location: `go mod graph > go_modules_graph.txt` writes
// the generated dependency graph into the checkout root, where `git status`
// reports it as an untracked file and `git add -A` sweeps it into a commit as if
// the large graph were source. The reviewed defect was exactly that shape: the
// `update-go_modules-graph` job redirected the graph into the working tree and
// .gitignore did not cover the name. CI now writes it into `$RUNNER_TEMP`, and
// .gitignore lists the name so a local replay of the older command is
// uncommittable too. No compiler sees a workflow file or a .gitignore, so the
// check lives here (main_test.go:
// TestModuleGraphOutputStaysOutOfWorkingTree).

// moduleGraphFileName is the name `go mod graph` is redirected to.
const moduleGraphFileName = "go_modules_graph.txt"

// moduleGraphOutputProblems returns one message per `go mod graph` redirect in
// the workflows under dir whose target lands inside the repository working tree
// while the repository's .gitignore does not ignore it, plus the number of
// redirect targets it read, so a caller can tell "no problems" apart from "no
// redirect found, so nothing was checked". A shell-expanded target
// (`$RUNNER_TEMP/...`), an absolute path and a path .gitignore covers are all
// accepted: none of them can reach a commit from the checkout.
func moduleGraphOutputProblems(dir string) ([]string, int, error) {
	root := filepath.Dir(dir)
	// The workflows live in <repo>/.github/workflows, so the repository root -
	// and its .gitignore - is two levels up from the workflow directory.
	if filepath.Base(root) == ".github" {
		root = filepath.Dir(root)
	}
	ignored, err := loadGitignore(filepath.Join(root, ".gitignore"))
	if err != nil {
		return nil, 0, err
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, err
	}

	var problems []string
	targets := 0
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
		for i, line := range strings.Split(string(data), "\n") {
			target, ok := moduleGraphRedirectTarget(line)
			if !ok {
				continue
			}
			targets++
			if strings.HasPrefix(target, "$") || strings.HasPrefix(target, "~") || strings.HasPrefix(target, "/") {
				continue
			}
			if ignored(target) {
				continue
			}
			problems = append(problems, fmt.Sprintf(
				"%s:%d: writes the module graph to %q inside the repository working tree and .gitignore does not ignore it, so `git status` reports a generated file and `git add -A` commits the graph as source; redirect it outside the checkout (for example $RUNNER_TEMP) or list %q in .gitignore",
				path, i+1, target, moduleGraphFileName))
		}
	}
	sort.Strings(problems)
	return problems, targets, nil
}

// moduleGraphRedirectTarget returns the file a `go mod graph` command line
// redirects to. Comment lines are skipped: a commented-out command is not run,
// so it writes nothing. A command that writes no file (a pipe, a bare print)
// reports no target.
func moduleGraphRedirectTarget(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") || !strings.Contains(trimmed, "go mod graph") {
		return "", false
	}
	after := trimmed[strings.Index(trimmed, "go mod graph")+len("go mod graph"):]
	tokens := strings.Fields(after)
	for i, token := range tokens {
		if !strings.HasPrefix(token, ">") {
			continue
		}
		target := strings.TrimLeft(token, ">")
		if target == "" {
			// A separate `>` token: the target is the next word.
			if i+1 < len(tokens) {
				return strings.Trim(tokens[i+1], `"'`), true
			}
			return "", false
		}
		if strings.HasPrefix(target, "&") {
			// File-descriptor duplication such as `2>&1` writes no file.
			continue
		}
		return strings.Trim(target, `"'`), true
	}
	return "", false
}

// loadGitignore returns a matcher for the repository's .gitignore rules: a path
// is ignored when the last rule matching it (by relative path, basename or glob)
// is not a `!` negation. A repository without .gitignore ignores nothing.
func loadGitignore(path string) (func(rel string) bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return func(string) bool { return false }, nil
		}
		return nil, err
	}

	type rule struct {
		pattern string
		negate  bool
	}
	var rules []rule
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		negate := strings.HasPrefix(trimmed, "!")
		if negate {
			trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, "!"))
		}
		trimmed = strings.TrimSuffix(strings.TrimPrefix(trimmed, "/"), "/")
		if trimmed == "" {
			continue
		}
		rules = append(rules, rule{pattern: trimmed, negate: negate})
	}

	return func(rel string) bool {
		rel = strings.TrimPrefix(filepath.ToSlash(rel), "./")
		ignored := false
		for _, r := range rules {
			if !gitignoreRuleMatches(r.pattern, rel) {
				continue
			}
			ignored = !r.negate
		}
		return ignored
	}, nil
}

// gitignoreRuleMatches reports whether one .gitignore pattern matches rel, a
// slash-separated path relative to the repository root. The patterns this
// repository uses are plain names, so a pattern matches its own path, a basename
// on any level and - with a wildcard - the same two shapes through filepath.Match.
func gitignoreRuleMatches(pattern, rel string) bool {
	if pattern == rel || pattern == filepath.Base(rel) {
		return true
	}
	if !strings.ContainsAny(pattern, "*?[") {
		return false
	}
	if ok, err := filepath.Match(pattern, rel); err == nil && ok {
		return true
	}
	if ok, err := filepath.Match(pattern, filepath.Base(rel)); err == nil && ok {
		return true
	}
	return false
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
