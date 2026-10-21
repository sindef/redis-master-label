package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// release_hygiene tag gate: `.github/workflows/release.yml` publishes on any
// `v*.*.*` tag, and `version="${GITHUB_REF_NAME}"` used to be the only source of
// the published tag. Nothing compared it with the release image the repository
// pins (`releaseImage` in main_test.go, which TestManifestsPinVersionedReleaseImage
// holds equal to the two example manifests), so pushing `v0.2.0` from a tree that
// still pinned `v0.1.0` passed vet and the full suite - the pin and the manifests
// agreed with each other, and neither was compared with the tag - and published
// `ghcr.io/redis-master-label/redis-master-label:v0.2.0`, an image no manifest in
// the repository deploys. A tag naming a commit that never merged reached the
// same publish path while bypassing CI entirely.
//
// The workflow now carries a gate before the build that reads the pin from
// main_test.go, checks the manifests deploy it, fails when the tag differs, and
// fails when the tagged commit is not an ancestor of main. A YAML trigger cannot
// express reachability, so the check lives in the workflow's first step.
//
// No compiler or `go test` invocation can see a workflow file, so the shape is
// asserted here (TestReleaseWorkflowGatesTagOnPinnedImage) as well as in the
// workflow itself. Dropping the gate, moving it after the build, or leaving only
// one of its two checks fails `go test -count=1 ./...`, which both CI and
// release.yml run.

// releaseTagTriggerRe matches the tags-list entry that makes a workflow a
// tag-release workflow: the `- 'v*.*.*'` item under `on.push.tags`.
var releaseTagTriggerRe = regexp.MustCompile(`(?m)^[ \t]*-[ \t]*['"]?v\*\.\*\.\*['"]?[ \t]*$`)

// workflowRunBlock is one `run:` step body: the shell text it executes and the
// line of its `run:` key, which is what proves the gate comes before the build.
type workflowRunBlock struct {
	line int
	text string
}

// workflowRunBlocks returns every `run:` body in a workflow, in file order. A
// block scalar (`run: |`) is the run of lines indented deeper than its `run:`
// key; an inline `run: <command>` is the command itself. Comment lines are
// dropped from the body, because a commented-out command never runs and must not
// satisfy a gate requirement.
func workflowRunBlocks(text string) []workflowRunBlock {
	lines := strings.Split(text, "\n")
	var blocks []workflowRunBlock
	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(trimmed, "run:") {
			continue
		}
		indent := len(rawLineIndent(lines[i]))
		inline := strings.TrimSpace(strings.TrimPrefix(trimmed, "run:"))
		switch inline {
		case "", "|", "|-", "|+", ">", ">-", ">+":
			var body []string
			j := i + 1
			for ; j < len(lines); j++ {
				if strings.TrimSpace(lines[j]) == "" {
					body = append(body, "")
					continue
				}
				if len(rawLineIndent(lines[j])) <= indent {
					break
				}
				body = append(body, lines[j])
			}
			blocks = append(blocks, workflowRunBlock{line: i + 1, text: shellCode(body)})
			i = j - 1
		default:
			blocks = append(blocks, workflowRunBlock{line: i + 1, text: inline})
		}
	}
	return blocks
}

// shellCode joins a run body without its comment-only lines.
func shellCode(body []string) string {
	var kept []string
	for _, line := range body {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// releaseTagGateProblemsIn scans one workflow's text for the tag gate. The
// second result reports whether the workflow releases on a `v*.*.*` tag at all,
// so a caller can tell "no problems" from "no release workflow found, so nothing
// was checked".
func releaseTagGateProblemsIn(path, text string) ([]string, bool) {
	if !releaseTagTriggerRe.MatchString(text) {
		return nil, false
	}

	var problems []string
	blocks := workflowRunBlocks(text)

	// The pin gate: one step that reads main_test.go's releaseImage, checks the
	// manifests deploy it, and fails when the released tag differs.
	gateLine := 0
	for _, block := range blocks {
		if strings.Contains(block.text, "releaseImage") {
			gateLine = block.line
			at := fmt.Sprintf("%s:%d", path, block.line)
			if !strings.Contains(block.text, "main_test.go") {
				problems = append(problems, at+": reads releaseImage without reading main_test.go, so the released tag is compared with a value copied into the workflow instead of the pin the repository declares")
			}
			if !strings.Contains(block.text, "manifests") {
				problems = append(problems, at+": compares the released tag with releaseImage but never with the image references in manifests/, so a pin no manifest deploys would pass")
			}
			if !strings.Contains(block.text, "GITHUB_REF_NAME") && !strings.Contains(block.text, "github.ref_name") {
				problems = append(problems, at+": reads releaseImage without reading the released tag (GITHUB_REF_NAME), so nothing is compared")
			}
			if !strings.Contains(block.text, "!=") || !strings.Contains(block.text, "exit 1") {
				problems = append(problems, at+": reads releaseImage but does not fail (a `!=` test and `exit 1`) when the released tag differs")
			}
			break
		}
	}
	if gateLine == 0 {
		problems = append(problems, fmt.Sprintf(
			"%s: no step reads the pinned release image (main_test.go's releaseImage), so the workflow publishes whatever tag it is given even when no manifest deploys that image",
			path))
	}

	// The reachability gate: the tagged commit must be an ancestor of main.
	ancestryLine := 0
	for _, block := range blocks {
		if strings.Contains(block.text, "merge-base") && strings.Contains(block.text, "--is-ancestor") {
			ancestryLine = block.line
			if !strings.Contains(block.text, "main") {
				problems = append(problems, fmt.Sprintf(
					"%s:%d: checks ancestry with git merge-base but names no main branch, so a tag from an unmerged commit can still publish",
					path, block.line))
			}
			if !strings.Contains(block.text, "exit 1") {
				problems = append(problems, fmt.Sprintf(
					"%s:%d: checks ancestry with git merge-base but never fails (exit 1) when the tagged commit is unreachable from main",
					path, block.line))
			}
			break
		}
	}
	if ancestryLine == 0 {
		problems = append(problems, fmt.Sprintf(
			"%s: no step checks the tagged commit is reachable from main (`git merge-base --is-ancestor`), so a tag pushed from a commit that never merged publishes an image CI never saw",
			path))
	}

	// Both gates must run before anything is built or published: a gate after
	// the build can still publish on a failure path that ignores it, and the
	// reviewed defect was no gate at all.
	buildLine := 0
	for _, block := range blocks {
		if strings.Contains(block.text, "docker build") && (buildLine == 0 || block.line < buildLine) {
			buildLine = block.line
		}
	}
	for i, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.Contains(line, attestationActionStr) && (buildLine == 0 || i+1 < buildLine) {
			buildLine = i + 1
		}
	}
	if buildLine == 0 {
		problems = append(problems, fmt.Sprintf(
			"%s: releases on a tag but builds or publishes no image (no `docker build` and no %s), so the gate's position cannot be checked",
			path, attestationActionStr))
	}
	for _, gate := range []struct {
		name string
		line int
	}{{"pinned-image", gateLine}, {"reachability", ancestryLine}} {
		if gate.line == 0 || buildLine == 0 {
			continue
		}
		if gate.line > buildLine {
			problems = append(problems, fmt.Sprintf(
				"%s:%d: the %s gate runs after the build at %s:%d, so an image is built from a tag no manifest deploys or from a commit main never received",
				path, gate.line, gate.name, path, buildLine))
		}
	}

	return problems, true
}

// releaseTagGateProblems scans every workflow under dir and returns one message
// per problem, plus the number of tag-release workflows seen.
func releaseTagGateProblems(dir string) ([]string, int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, err
	}

	var problems []string
	workflows := 0
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
		found, releases := releaseTagGateProblemsIn(path, string(data))
		problems = append(problems, found...)
		if releases {
			workflows++
		}
	}
	return problems, workflows, nil
}

// The shipped release workflow must carry both halves of the gate before its
// build step: the tag/pin comparison and the reachability check. This test is what
// fails a pull request that removes either one, because no other check reads the
// release workflow's ordering.
func TestReleaseWorkflowGatesTagOnPinnedImage(t *testing.T) {
	problems, workflows, err := releaseTagGateProblems(ciWorkflowDir)
	if err != nil {
		t.Fatal(err)
	}
	if workflows == 0 {
		t.Fatalf("no workflow under %s releases on a vX.Y.Z tag: the release gate ran against nothing", ciWorkflowDir)
	}
	for _, problem := range problems {
		t.Errorf("%s", problem)
	}
}

// The failure modes the scan has to see, each a workflow shaped like
// release.yml: the pre-fix workflow with no gate, a gate missing the
// reachability check, a gate placed after the build, a gate comparing the tag
// with a value copied into the workflow instead of the pin declaration, and a
// gate that never reads manifests/. The gated shape must pass, so the fixtures
// prove the edit is what was caught.
func TestReleaseTagGateProblems_RegressionFixtures(t *testing.T) {
	const trigger = "name: Release\non:\n  push:\n    tags:\n      - 'v*.*.*'\n\njobs:\n  build-and-publish:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@v7\n"

	const gate = `      - name: Check released tag matches the pinned release image
        shell: bash
        run: |
          pinned=$(sed -nE 's/.*const releaseImage = "([^"]+)".*/\1/p' main_test.go | head -n 1)
          deployed=$(grep -hoE 'ghcr\.io/redis-master-label/redis-master-label:[^[:space:]"]+' manifests/*.yaml | sort -u)
          if [ "$deployed" != "$pinned" ]; then
            echo "manifests deploy $deployed, pin is $pinned" >&2
            exit 1
          fi
          if [ "${GITHUB_REF_NAME}" != "${pinned##*:}" ]; then
            echo "tag ${GITHUB_REF_NAME} is not the pinned release image ${pinned}" >&2
            exit 1
          fi
`

	const ancestry = `      - name: Check the tag is reachable from main
        run: |
          git fetch --no-tags origin "+refs/heads/main:refs/remotes/origin/main"
          if ! git merge-base --is-ancestor HEAD refs/remotes/origin/main; then
            echo "tag ${GITHUB_REF_NAME} is not reachable from main" >&2
            exit 1
          fi
`

	const build = `      - name: Build image
        run: |
          docker build -t example .
`

	const publish = `      - uses: ` + attestationActionStr + `@v7
        with:
          push: true
`

	tests := []struct {
		name         string
		workflow     string
		wantReleases bool
		wantProblem  string
	}{
		{
			name:         "the gated shape this repository ships",
			workflow:     trigger + gate + ancestry + build + publish,
			wantReleases: true,
		},
		{
			// The reviewed shape: the tag alone decided what was published.
			name:         "pre-fix shape: no gate at all",
			workflow:     trigger + build + publish,
			wantReleases: true,
			wantProblem:  "no step reads the pinned release image",
		},
		{
			name:         "no reachability check",
			workflow:     trigger + gate + build + publish,
			wantReleases: true,
			wantProblem:  "reachable from main",
		},
		{
			name:         "gate after the build",
			workflow:     trigger + build + gate + ancestry + publish,
			wantReleases: true,
			wantProblem:  "runs after the build",
		},
		{
			name:         "tag compared with a value copied into the workflow",
			workflow:     trigger + strings.Replace(gate, "main_test.go", "expected-image.txt", 1) + ancestry + build + publish,
			wantReleases: true,
			wantProblem:  "without reading main_test.go",
		},
		{
			name:         "gate that never reads the manifests",
			workflow:     trigger + strings.Replace(gate, "manifests", "elsewhere", -1) + ancestry + build + publish,
			wantReleases: true,
			wantProblem:  "never with the image references in manifests/",
		},
		{
			name:         "gate that never reads the released tag",
			workflow:     trigger + strings.Replace(gate, "GITHUB_REF_NAME", "PINNED_TAG", -1) + ancestry + build + publish,
			wantReleases: true,
			wantProblem:  "without reading the released tag",
		},
		{
			name:         "gate that never fails",
			workflow:     trigger + strings.Replace(gate, "exit 1", "true", -1) + ancestry + build + publish,
			wantReleases: true,
			wantProblem:  "does not fail",
		},
		{
			// A pull-request workflow is not a release workflow, so it carries
			// no tag gate and must not be flagged by this scan.
			name:         "workflow that does not release on a tag",
			workflow:     "name: CI\non:\n  pull_request:\n\njobs:\n  go:\n    runs-on: ubuntu-latest\n    steps:\n      - name: Build\n        run: docker build -t example .\n",
			wantReleases: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "release.yml"), []byte(tt.workflow), 0o644); err != nil {
				t.Fatal(err)
			}

			problems, workflows, err := releaseTagGateProblems(dir)
			if err != nil {
				t.Fatal(err)
			}
			if (workflows > 0) != tt.wantReleases {
				t.Fatalf("tag-release workflows = %d, want %v", workflows, tt.wantReleases)
			}
			if tt.wantProblem == "" {
				if len(problems) != 0 {
					t.Fatalf("problems = %v, want none", problems)
				}
				return
			}
			if len(problems) == 0 {
				t.Fatal("workflow without a working release gate accepted, want a problem reported")
			}
			if !strings.Contains(strings.Join(problems, "\n"), tt.wantProblem) {
				t.Fatalf("problems = %v, want one naming %q", problems, tt.wantProblem)
			}
		})
	}
}

// The scan reads every workflow in the directory, so a second tag-release
// workflow cannot be added without a gate of its own, and a missing directory is
// an error rather than an empty pass.
func TestReleaseTagGateProblems_CoversEveryWorkflowFile(t *testing.T) {
	dir := t.TempDir()
	ungated := "name: Release\non:\n  push:\n    tags:\n      - 'v*.*.*'\n\njobs:\n  build:\n    runs-on: ubuntu-latest\n    steps:\n      - name: Build\n        run: docker build -t example .\n"
	for _, name := range []string{"release.yml", "mirror-release.yaml"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(ungated), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	problems, workflows, err := releaseTagGateProblems(dir)
	if err != nil {
		t.Fatal(err)
	}
	if workflows != 2 {
		t.Fatalf("tag-release workflows = %d, want both files read", workflows)
	}
	joined := strings.Join(problems, "\n")
	for _, name := range []string{"release.yml", "mirror-release.yaml"} {
		if !strings.Contains(joined, name) {
			t.Errorf("problems %v do not name %s", problems, name)
		}
	}

	if _, _, err := releaseTagGateProblems(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing workflow directory accepted, want a read error")
	}
}

// The gate's own shell must survive the offline workflow scan CI runs: no tab
// characters (YAML forbids them) and setup-go untouched. Its commands are the
// ones the fixtures above assert on, so a rewrite of the step that keeps the
// assertions but breaks the scan fails here.
func TestReleaseWorkflowTagGateStaysScanClean(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(ciWorkflowDir, "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, "\t") {
		t.Error("release.yml contains a tab character")
	}
	if !strings.Contains(text, "merge-base --is-ancestor") {
		t.Error("release.yml no longer checks the tagged commit is an ancestor of main")
	}
}
