package main

import (
	"fmt"
	"regexp"
	"strings"
)

// Release tag gate: .github/workflows/release.yml derives the published image
// reference from the tag alone (`version="${GITHUB_REF_NAME}"`), while the
// deployed reference is declared elsewhere - main_test.go's `releaseImage`
// constant pinning what TestManifestsPinVersionedReleaseImage accepts, and the
// example manifests pinning that same reference. Nothing compared the two, so
// pushing `v0.2.0` while the pin still read `v0.1.0` passed vet and the whole
// test suite (the pin agrees with itself, so the manifest test stayed green) and
// published ghcr.io/redis-master-label/redis-master-label:v0.2.0: an image no
// manifest in the repository references, from a commit that need never have
// been a merged pull request. The workflow therefore gates the build on
//
//   - the tag equalling the tag of `releaseImage` and of every manifest sidecar
//     image reference, and
//   - the tagged commit being reachable from main, because Actions has no
//     branch filter for tag pushes and a tag pushed from a branch that never
//     merged starts no pull request check at all.
//
// Both gates have to run before the step that builds or publishes the image, or
// the failure comes after the push it was meant to prevent. No compiler can read
// a workflow file, so the shape is asserted here, in release_tag_gate_test.go,
// which `go test -count=1 ./...` runs in CI and in release.yml before the build.

const (
	// releaseWorkflowFile is the workflow the gates live in.
	releaseWorkflowFile = "release.yml"

	// releaseImageFile declares the pinned release image the gate reads.
	releaseImageFile = "main_test.go"

	// releaseImageDeclPrefix is the Go declaration the workflow's gate extracts
	// the pin with (`sed -n 's/^const releaseImage = "\([^"]*\)"$/\1/p'` on
	// releaseImageFile). releaseImageDeclarationRe is the same expression, so a
	// test can prove the shipped declaration still has the shape the gate reads.
	releaseImageDeclPrefix = "const releaseImage = "

	// fetchDepthInput is the actions/checkout input that decides whether the
	// clone carries enough history for `git merge-base --is-ancestor`.
	fetchDepthInput = "fetch-depth"
)

// releaseImageDeclarationRe reads the pinned release image from
// releaseImageFile exactly as the release gate's sed expression does.
var releaseImageDeclarationRe = regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(releaseImageDeclPrefix) + `"([^"]+)"$`)

// releaseTagFromImage returns the tag part of a registry-qualified image
// reference: the text after the last ':', which is what the release gate's
// `${pinned##*:}` expansion yields.
func releaseTagFromImage(image string) string {
	return image[strings.LastIndex(image, ":")+1:]
}

// releaseWorkflowStep is one step of a workflow's `steps:` list: the line it
// starts at, the indentation of its `- ` item and the lines it owns.
type releaseWorkflowStep struct {
	StartLine int
	Indent    int
	Lines     []string
}

// text returns the step's own lines joined back into text, which is what the
// checks below scan.
func (s releaseWorkflowStep) text() string { return strings.Join(s.Lines, "\n") }

// releaseWorkflowSteps splits a workflow's text into its steps. A step starts at
// a `- ` list item and owns every following line that is blank, a comment or
// indented deeper than that item, so a job key, the next step or a top-level key
// ends it. Nested list items inside a step (a `tags:` list, for example) stay
// with their step.
func releaseWorkflowSteps(text string) []releaseWorkflowStep {
	var steps []releaseWorkflowStep
	var current *releaseWorkflowStep
	flush := func() {
		if current != nil {
			steps = append(steps, *current)
			current = nil
		}
	}
	for i, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		indent := len(rawLineIndent(line))
		if strings.HasPrefix(trimmed, "- ") && (current == nil || indent <= current.Indent) {
			flush()
			current = &releaseWorkflowStep{StartLine: i + 1, Indent: indent}
			current.Lines = append(current.Lines, trimmed)
			continue
		}
		if current == nil {
			continue
		}
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") && indent < current.Indent {
			flush()
			continue
		}
		current.Lines = append(current.Lines, line)
	}
	flush()
	return steps
}

// releaseTagGateProblems returns one message per way the release workflow fails
// to gate the published tag on the pinned image and on main, plus the number of
// gates found, so a caller can tell "no problems" from "no gate exists, so
// nothing was checked".
func releaseTagGateProblems(path, text string) ([]string, int) {
	steps := releaseWorkflowSteps(text)

	tagGate := -1
	mainGate := -1
	checkout := -1
	publish := -1
	for i, step := range steps {
		body := step.text()
		if publish < 0 && publishesReleaseImage(body) {
			publish = i
		}
		if tagGate < 0 && comparesTagWithPinnedImage(body) {
			tagGate = i
		}
		if mainGate < 0 && provesTagReachableFromMain(body) {
			mainGate = i
		}
		if checkout < 0 && strings.Contains(body, "actions/checkout") {
			checkout = i
		}
	}

	var problems []string
	if publish < 0 {
		problems = append(problems, fmt.Sprintf(
			"%s: no step builds or publishes an image, so the release gates cannot be placed before one and the tag check ran against nothing",
			path))
	}

	gates := 0
	if tagGate < 0 {
		problems = append(problems, fmt.Sprintf(
			"%s: no step compares GITHUB_REF_NAME with the `%s` declared in %s and with the image references in manifests/, so a tag that disagrees with the pin publishes an image no manifest deploys",
			path, releaseImageDeclPrefix, releaseImageFile))
	} else {
		gates++
		if publish >= 0 && tagGate > publish {
			problems = append(problems, fmt.Sprintf(
				"%s:%d: the tag/releaseImage gate runs after the step that builds or publishes the image (%s:%d); it has to fail before anything is built",
				path, steps[tagGate].StartLine, path, steps[publish].StartLine))
		}
	}

	if mainGate < 0 {
		problems = append(problems, fmt.Sprintf(
			"%s: no step proves the tagged commit is reachable from main (`git merge-base --is-ancestor`), so a tag pushed from a branch that never merged releases with no pull request check behind it",
			path))
	} else {
		gates++
		if publish >= 0 && mainGate > publish {
			problems = append(problems, fmt.Sprintf(
				"%s:%d: the reachability gate runs after the step that builds or publishes the image (%s:%d); it has to fail before anything is built",
				path, steps[mainGate].StartLine, path, steps[publish].StartLine))
		}
	}

	switch {
	case checkout < 0:
		problems = append(problems, fmt.Sprintf(
			"%s: no actions/checkout step found, so the release workflow cannot read the pin it has to compare the tag with",
			path))
	case !declaresFullHistory(steps[checkout].text()):
		problems = append(problems, fmt.Sprintf(
			"%s:%d: the actions/checkout step declares no `%s: 0`, so the ancestry check runs on a shallow clone that cannot prove the tag is reachable from main",
			path, steps[checkout].StartLine, fetchDepthInput))
	}

	return problems, gates
}

// publishesReleaseImage reports whether a step's text builds or pushes the
// release image, which is what the gates must precede.
func publishesReleaseImage(body string) bool {
	for _, marker := range []string{"docker build", "docker/build-push-action"} {
		if strings.Contains(body, marker) {
			return true
		}
	}
	return false
}

// comparesTagWithPinnedImage reports whether a step's text compares the tag
// being released with the pinned reference and with the manifests, and fails on
// a mismatch: it reads GITHUB_REF_NAME, extracts the pin from main_test.go's
// declaration, reads manifests/, and exits non-zero when either disagrees.
func comparesTagWithPinnedImage(body string) bool {
	return strings.Contains(body, "GITHUB_REF_NAME") &&
		strings.Contains(body, releaseImageFile) &&
		strings.Contains(body, releaseImageDeclPrefix) &&
		strings.Contains(body, "manifests") &&
		(strings.Contains(body, "!=") || strings.Contains(body, " -ne ")) &&
		strings.Contains(body, "exit 1")
}

// provesTagReachableFromMain reports whether a step's text proves the tagged
// commit is an ancestor of main and fails when it is not.
func provesTagReachableFromMain(body string) bool {
	return strings.Contains(body, "merge-base") &&
		strings.Contains(body, "--is-ancestor") &&
		strings.Contains(body, "refs/heads/main") &&
		strings.Contains(body, "exit 1")
}

// declaresFullHistory reports whether a checkout step's text asks for a full
// clone (`fetch-depth: 0`) rather than the default single-commit fetch, which
// cannot answer whether the tag is an ancestor of main.
func declaresFullHistory(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, value, ok := workflowInput(trimmed)
		if ok && key == fetchDepthInput && value == "0" {
			return true
		}
	}
	return false
}
