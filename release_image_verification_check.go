package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Release image verification check: the publishing path must run and inspect
// the image it built before it pushes it. The reviewed defect was that
// .github/workflows/release.yml built the image with `docker build --build-arg
// VERSION=<tag>` and then pushed a rebuild with docker/build-push-action while
// running only `go vet` and `go test -count=1 ./...` first. The image-level
// checks live in .github/workflows/ci.yml ("Verify image version output",
// "Verify image OCI labels", "Verify image license metadata"), and ci.yml is
// triggered by pull requests and pushes to main, so a tag push - the only
// release trigger - runs release.yml alone and none of them. The regression the
// task names therefore reached GHCR unverified: a build file that stops
// stamping -X main.version (the binary then reports "dev" or nothing) or a
// LABEL moved into the discarded builder stage (the image then ships no title,
// source, version or license) fails a pull request but was published by a tag.
//
// Go tests can read the build file (dockerfile_check.go) but cannot see the
// built image, so the build-file readers in dockerfile_check.go cannot replace
// running the image; this check instead reads the workflow that publishes and
// requires the pre-push steps that run and inspect it.
// release_image_verification_check_test.go
// (TestPublishingWorkflowVerifiesImageBeforePush) fails when those steps are
// removed, moved after the publishing step, or left commented out.
//
// The same scanner is not spelled against release.yml alone: every workflow that
// pushes an image is read, so a second publishing workflow cannot be added
// without the pre-push verification.

// publishImageAction is the action that publishes images in these workflows. A
// step using it with `push: true` is the point at which the image leaves the
// repository, so everything the job asserts about the image must already have
// run above it.
const publishImageAction = "docker/build-push-action"

// The CI steps the release verification mirrors, named in the failure messages so
// a stripped pre-push check points at the step it dropped.
const (
	ciVerifyVersionStep = "Verify image version output"
	ciVerifyLabelsStep  = "Verify image OCI labels"
	ciVerifyLicenseStep = "Verify image license metadata"
)

// imageVerificationProblems returns one message per workflow under dir that
// would push an image without first running and inspecting the image it built,
// plus the number of workflows that push an image, so a caller can tell "no
// problems" apart from "no publishing path found, so nothing was checked".
func imageVerificationProblems(dir string) ([]string, int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, err
	}

	var problems []string
	publishing := 0
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
		found, publishes := imageVerificationProblemsIn(path, string(data))
		problems = append(problems, found...)
		if publishes {
			publishing++
		}
	}
	sort.Strings(problems)
	return problems, publishing, nil
}

// imageVerificationProblemsIn reports why one workflow would publish an image
// nothing ran or inspected, and whether the workflow is a publishing path at
// all: it must actually push (`docker/build-push-action` with `push: true`), so
// a workflow that only builds or loads an image is not flagged and not counted.
func imageVerificationProblemsIn(path, text string) ([]string, bool) {
	lines := strings.Split(text, "\n")

	pushAt := -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		// A commented-out publishing step publishes nothing, so it is not the
		// point the verification has to precede.
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if !strings.Contains(trimmed, publishImageAction) {
			continue
		}
		if !strings.Contains(stepBlockAt(lines, i), "push: true") {
			continue
		}
		pushAt = i
		break
	}
	if pushAt < 0 {
		return nil, false
	}

	var problems []string
	publishStep := stepBlockAt(lines, pushAt)

	// The publishing step builds the image it pushes, so it needs the tag as its
	// own build arg: build args do not carry over from an earlier step, and a
	// push step without one publishes an image stamped with the build file's
	// default instead of the version the pre-push checks just verified.
	if !strings.Contains(publishStep, "VERSION") {
		problems = append(problems, fmt.Sprintf(
			"%s: the %s push passes no VERSION build arg, so it publishes an image built with the default version rather than the tag the pre-push checks verified",
			path, publishImageAction))
	}

	// The build that produced the image under test. The release job builds the
	// image it later pushes itself (`docker build --build-arg VERSION=<tag>`);
	// without such a build there is nothing local to run or inspect.
	buildAt := -1
	for i := 0; i < pushAt; i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.Contains(trimmed, "docker build") && strings.Contains(trimmed, "--build-arg VERSION") {
			buildAt = i
		}
	}
	if buildAt < 0 {
		return append(problems, fmt.Sprintf(
			"%s: no `docker build --build-arg VERSION=<tag>` step runs before the %s push, so no local image exists to run or inspect before GHCR receives the release",
			path, publishImageAction)), true
	}

	// The pre-push window: the steps between building the image and pushing it,
	// with comment-only lines dropped so a commented-out check reads as the
	// absent check it is. Verification after the push is not verification
	// before it, and lands outside this window.
	var windowLines []string
	for _, line := range lines[buildAt:pushAt] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		windowLines = append(windowLines, line)
	}
	window := strings.Join(windowLines, "\n")

	// The version the binary reports: README documents an unstamped build as
	// `redis-master-label dev` and a release as its tag, and `go test` cannot
	// see the linker stamp, so only running the image proves it.
	if !strings.Contains(window, "docker run") || !strings.Contains(window, "--version") {
		problems = append(problems, fmt.Sprintf(
			"%s: the image built before the push is never run (`docker run ... --version`), so a build file that stops stamping -X main.version is published to GHCR instead of failing the tag push; the release job must mirror CI's %q step",
			path, ciVerifyVersionStep))
	}

	// The image metadata, read from the image rather than from the build file: a
	// LABEL left in the discarded builder stage parses the same in Dockerfile and
	// reaches no image.
	if !strings.Contains(window, "docker inspect") {
		problems = append(problems, fmt.Sprintf(
			"%s: the image built before the push is never inspected (`docker inspect --format '{{json .Config.Labels}}'`), so a LABEL left in the discarded builder stage is published as an image with no metadata; the release job must mirror CI's %q step",
			path, ciVerifyLabelsStep))
	}
	var missingLabels []string
	for _, key := range ociLabelKeys {
		if !strings.Contains(window, key) {
			missingLabels = append(missingLabels, key)
		}
	}
	if len(missingLabels) > 0 {
		problems = append(problems, fmt.Sprintf(
			"%s: the pre-push steps never assert the shipping image's %s label(s), so the release can ship an image missing the metadata the pull-request path requires",
			path, strings.Join(missingLabels, ", ")))
	}

	// The license behind the licenses label: the label is only a truthful claim
	// while the repository grants that license, which is what the CI step reads
	// out of LICENSE before it inspects the label.
	if !strings.Contains(window, licenseFile) {
		problems = append(problems, fmt.Sprintf(
			"%s: the pre-push steps never read the repository's %s grant behind the image's %s label, so a release can advertise a license the repository does not grant; the release job must mirror CI's %q step",
			path, licenseFile, imageLicenseLabel, ciVerifyLicenseStep))
	}

	return problems, true
}

// stepBlockAt returns the text of the step whose `uses:` (or other) line is
// lines[start], up to but excluding the next list item at the same or a
// shallower indentation, so a `with:` block of one step is never read as another
// step's inputs. A top-level key also ends the block.
func stepBlockAt(lines []string, start int) string {
	indent := len(rawLineIndent(lines[start]))
	block := []string{lines[start]}
	for _, line := range lines[start+1:] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			block = append(block, line)
			continue
		}
		lineIndent := len(rawLineIndent(line))
		if lineIndent < indent || (lineIndent == indent && strings.HasPrefix(trimmed, "- ")) {
			break
		}
		block = append(block, line)
	}
	return strings.Join(block, "\n")
}
