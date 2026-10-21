package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Release gate: `.github/workflows/release.yml` publishes whatever a vX.Y.Z tag
// names. `version="${GITHUB_REF_NAME}"` was the only source of the published
// reference and no step compared it with the image the manifests pin, so a tag
// pushed while `releaseImage` and the manifests still pinned the previous
// version passed vet and the whole test suite (the pin matched itself) and
// published an image no manifest in the repository references. The README's
// three-file bump was the only thing keeping tag and pin aligned, and a tag
// pushed from a commit that never merged bypassed CI entirely.
//
// release_gate.sh is the gate the workflow runs before anything is built, and
// the tests here cover it twice:
//
//   - the behavior, by running the script against fixture manifests and a
//     fixture git repository (release_gate.sh is the exact file the workflow
//     invokes, so this is not a re-implementation), and
//   - the wiring, by scanning release.yml (releaseGateProblems): both gates
//     must run before the build and push steps, the tag gate must compare
//     `$GITHUB_REF_NAME`, the reachability gate must compare `$GITHUB_SHA`, and
//     the checkout must fetch the history the reachability gate reads.
//
// A gate deleted from the workflow or moved after the build fails
// `go test -count=1 ./...`, which both ci.yml and release.yml run.

// releaseGateScript is the gate the release workflow runs.
const releaseGateScript = "release_gate.sh"

// releaseWorkflowPath is the workflow the gate guards.
const releaseWorkflowPath = ciWorkflowDir + "/release.yml"

// releaseGateMainFetchRefspec is the explicit fetch the reachability gate needs
// so it never depends on how actions/checkout configured its remote.
const releaseGateMainFetchRefspec = "+refs/heads/main:refs/remotes/origin/main"

// releaseImageRepo returns the repository part of a registry-qualified image
// reference: everything before the tag separator, or the whole reference when it
// carries no tag.
func releaseImageRepo(reference string) string {
	if i := strings.LastIndex(reference, ":"); i > strings.LastIndex(reference, "/") {
		return reference[:i]
	}
	return reference
}

// releaseImageTag returns the tag of a registry-qualified image reference, or
// the empty string when it carries none.
func releaseImageTag(reference string) string {
	if i := strings.LastIndex(reference, ":"); i > strings.LastIndex(reference, "/") {
		return reference[i+1:]
	}
	return ""
}

// releaseGateProblems returns one message per way the release workflow fails to
// gate a tag push: no vX.Y.Z tag trigger, no full-history checkout, no explicit
// main fetch, no pinned-image gate, no reachability gate, a gate that compares
// something other than the released tag or commit, or a gate that runs after
// the image is already built or published.
func releaseGateProblems(path, text string) []string {
	var problems []string
	lines := strings.Split(text, "\n")

	hasTagTrigger := false
	fullHistory := false
	fetchesMain := false
	tagGateLine, mainGateLine, buildLine := 0, 0, 0
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if trimmed == "- 'v*.*.*'" || trimmed == `- "v*.*.*"` {
			hasTagTrigger = true
		}
		if trimmed == "fetch-depth: 0" {
			fullHistory = true
		}
		if strings.Contains(trimmed, releaseGateMainFetchRefspec) {
			fetchesMain = true
		}
		// Only a line that invokes the gate script counts: a comment or an
		// unrelated step that merely mentions it is not a gate.
		if tagGateLine == 0 && strings.Contains(line, releaseGateScript+" tag ") {
			tagGateLine = i + 1
			if !strings.Contains(line, `"$GITHUB_REF_NAME"`) {
				problems = append(problems, fmt.Sprintf(
					"%s:%d: the pinned-image gate does not compare the tag being released (`$GITHUB_REF_NAME`), so the published tag can still drift from the pin",
					path, i+1))
			}
		}
		if mainGateLine == 0 && strings.Contains(line, releaseGateScript+" main ") {
			mainGateLine = i + 1
			if !strings.Contains(line, `"$GITHUB_SHA"`) || !strings.Contains(line, "origin/main") {
				problems = append(problems, fmt.Sprintf(
					"%s:%d: the reachability gate does not compare the tagged commit (`$GITHUB_SHA`) against origin/main, so a tag from a commit that never merged can still publish",
					path, i+1))
			}
		}
		if buildLine == 0 && (strings.Contains(line, "docker build") || strings.Contains(line, attestationActionStr)) {
			buildLine = i + 1
		}
	}

	if !hasTagTrigger {
		problems = append(problems, fmt.Sprintf(
			"%s: no v*.*.* tag trigger found, so the release workflow no longer runs on the tags it publishes", path))
	}
	if !fullHistory {
		problems = append(problems, fmt.Sprintf(
			"%s: actions/checkout does not set `fetch-depth: 0`, so the reachability gate runs against a single-commit tag clone with no main history to compare", path))
	}
	if !fetchesMain {
		problems = append(problems, fmt.Sprintf(
			"%s: no step fetches main (%s) before the reachability gate, so the released commit cannot be compared with main", path, releaseGateMainFetchRefspec))
	}
	if tagGateLine == 0 {
		problems = append(problems, fmt.Sprintf(
			"%s: no step runs `%s tag`, so a tag that differs from the image the manifests pin is published unreported", path, releaseGateScript))
	}
	if mainGateLine == 0 {
		problems = append(problems, fmt.Sprintf(
			"%s: no step runs `%s main`, so a tag pushed from a commit that never merged main is published unreported", path, releaseGateScript))
	}
	switch {
	case buildLine == 0:
		problems = append(problems, fmt.Sprintf(
			"%s: no image build or publish step found, so the gates were checked against nothing", path))
	default:
		if tagGateLine > buildLine {
			problems = append(problems, fmt.Sprintf(
				"%s:%d: the pinned-image gate runs after the image is built (%s:%d), so the image is published before the pin is compared", path, tagGateLine, path, buildLine))
		}
		if mainGateLine > buildLine {
			problems = append(problems, fmt.Sprintf(
				"%s:%d: the reachability gate runs after the image is built (%s:%d), so the image is published before reachability is checked", path, mainGateLine, path, buildLine))
		}
	}

	sort.Strings(problems)
	return problems
}

// releaseGateShell returns the POSIX shell that runs the gate, so the tests
// execute the same script the workflow does instead of a paraphrase of it.
func releaseGateShell(t *testing.T) string {
	t.Helper()
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh in PATH: the release gate cannot be executed here")
	}
	return shell
}

// runReleaseGate runs release_gate.sh with args in dir and returns its combined
// output and exit code.
func runReleaseGate(t *testing.T, dir string, args ...string) (string, int) {
	t.Helper()
	script, err := filepath.Abs(releaseGateScript)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(releaseGateShell(t), append([]string{script}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("run %s %v: %v\n%s", releaseGateScript, args, err, out)
	}
	return string(out), exitErr.ExitCode()
}

// writeReleaseGateManifests writes one manifest file per image reference into
// dir, so a fixture spells out the pins the gate has to read.
func writeReleaseGateManifests(t *testing.T, dir string, images ...string) {
	t.Helper()
	for i, image := range images {
		name := fmt.Sprintf("deployment-%d.yaml", i)
		text := "apiVersion: apps/v1\nkind: Deployment\nspec:\n  template:\n    spec:\n      containers:\n" +
			"      - name: redis-master-label\n        image: " + image + "\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// gitIn runs one git command in dir and returns its trimmed output.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{
		"-c", "user.name=release-gate-test",
		"-c", "user.email=release-gate-test@example.com",
		"-c", "init.defaultBranch=main",
	}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// The release workflow must keep both gates, in order, before it builds
// anything: the shape release.yml has today passes, so the fixtures below prove
// that each removed or reordered gate is what fails.
func TestReleaseWorkflowGatesTagAgainstPinnedImage(t *testing.T) {
	data, err := os.ReadFile(releaseWorkflowPath)
	if err != nil {
		t.Fatalf("read %s: %v", releaseWorkflowPath, err)
	}
	problems := releaseGateProblems(releaseWorkflowPath, string(data))
	if len(problems) != 0 {
		t.Fatalf("%s does not gate the tag it publishes:\n%s", releaseWorkflowPath, strings.Join(problems, "\n"))
	}
}

// The failure modes the scan has to see, and the shipped shape it must accept.
func TestReleaseGateProblems_RegressionFixtures(t *testing.T) {
	const head = "name: Release\n" +
		"on:\n" +
		"  push:\n" +
		"    tags:\n" +
		"      - 'v*.*.*'\n" +
		"\n" +
		"jobs:\n" +
		"  build-and-publish:\n" +
		"    steps:\n" +
		"      - uses: actions/checkout@v7\n" +
		"        with:\n" +
		"          fetch-depth: 0\n"
	const mainGate = "      - name: Require the tag to be reachable from main\n" +
		"        run: |\n" +
		"          git fetch --no-tags origin " + releaseGateMainFetchRefspec + "\n" +
		"          sh release_gate.sh main \"$GITHUB_SHA\" origin/main\n"
	const tagGate = "      - name: Require the tag to match the pinned release image\n" +
		"        run: |\n" +
		"          sh release_gate.sh tag \"$GITHUB_REF_NAME\" \"${{ steps.image.outputs.image }}\" manifests\n"
	const build = "      - name: Build image\n" +
		"        run: |\n" +
		"          docker build -t app .\n"

	tests := []struct {
		name        string
		workflow    string
		wantProblem string
	}{
		{
			name:     "both gates before the build",
			workflow: head + mainGate + tagGate + build,
		},
		{
			name:        "pinned-image gate removed",
			workflow:    head + mainGate + build,
			wantProblem: "no step runs `release_gate.sh tag`",
		},
		{
			name:        "reachability gate removed",
			workflow:    head + tagGate + build,
			wantProblem: "no step runs `release_gate.sh main`",
		},
		{
			name:        "pinned-image gate after the build",
			workflow:    head + mainGate + build + tagGate,
			wantProblem: "runs after the image is built",
		},
		{
			name:        "reachability gate after the build",
			workflow:    head + tagGate + build + mainGate,
			wantProblem: "runs after the image is built",
		},
		{
			name:        "checkout without full history",
			workflow:    strings.Replace(head, "          fetch-depth: 0\n", "", 1) + mainGate + tagGate + build,
			wantProblem: "fetch-depth: 0",
		},
		{
			name:        "no explicit main fetch",
			workflow:    head + strings.Replace(mainGate, "          git fetch --no-tags origin "+releaseGateMainFetchRefspec+"\n", "", 1) + tagGate + build,
			wantProblem: "no step fetches main",
		},
		{
			name:        "pinned-image gate compares a literal tag",
			workflow:    head + mainGate + strings.Replace(tagGate, `"$GITHUB_REF_NAME"`, "v0.1.0", 1) + build,
			wantProblem: "$GITHUB_REF_NAME",
		},
		{
			name:        "reachability gate compares another ref",
			workflow:    head + strings.Replace(mainGate, "origin/main", "origin/dev", 1) + tagGate + build,
			wantProblem: "origin/main",
		},
		{
			name:        "no tag trigger",
			workflow:    strings.Replace(head, "      - 'v*.*.*'\n", "", 1) + mainGate + tagGate + build,
			wantProblem: "no v*.*.* tag trigger",
		},
		{
			name:        "gates present but nothing is built",
			workflow:    head + mainGate + tagGate,
			wantProblem: "no image build or publish",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			problems := releaseGateProblems(releaseWorkflowPath, tt.workflow)
			if tt.wantProblem == "" {
				if len(problems) != 0 {
					t.Fatalf("problems = %v, want none", problems)
				}
				return
			}
			if len(problems) == 0 {
				t.Fatal("workflow that would publish an ungated tag accepted, want a problem")
			}
			if !strings.Contains(strings.Join(problems, "\n"), tt.wantProblem) {
				t.Fatalf("problems = %v, want one naming %q", problems, tt.wantProblem)
			}
		})
	}
}

// The reviewed failure end to end: the tag being released must be the tag the
// manifests pin. Run against the manifests this repository ships, so the pin the
// release workflow reads is the one the tests pin, not a copy.
func TestReleaseGateTagMatchesShippedPin(t *testing.T) {
	pinnedTag := releaseImageTag(releaseImage)
	if pinnedTag == "" {
		t.Fatalf("releaseImage = %q carries no tag", releaseImage)
	}
	repo := releaseImageRepo(releaseImage)

	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	out, code := runReleaseGate(t, dir, "tag", pinnedTag, repo, manifestDir)
	if code != 0 {
		t.Fatalf("releasing the pinned tag %q failed the gate (exit %d):\n%s", pinnedTag, code, out)
	}

	// The reviewed shape: a newer tag than the manifests pin.
	newer := "v9.9.9"
	if pinnedTag == newer {
		newer = "v9.9.8"
	}
	out, code = runReleaseGate(t, dir, "tag", newer, repo, manifestDir)
	if code == 0 {
		t.Fatalf("releasing %q while the manifests pin %q passed the gate:\n%s", newer, pinnedTag, out)
	}
	if !strings.Contains(out, newer) || !strings.Contains(out, releaseImage) {
		t.Fatalf("gate output %q does not name both the released tag %q and the pinned reference %q", out, newer, releaseImage)
	}
}

// The gate's own behavior on fixture manifests: matching pins pass, a drifted
// pin, an image no manifest references, manifests that disagree and a missing
// manifest directory all fail.
func TestReleaseGateTag_RegressionFixtures(t *testing.T) {
	const image = "ghcr.io/example/app"

	tests := []struct {
		name        string
		tag         string
		image       string
		pins        []string
		noManifests bool
		wantExit    int
		wantMessage string
	}{
		{
			name:  "both deployments pin the released tag",
			tag:   "v0.2.0",
			image: image,
			pins:  []string{image + ":v0.2.0", image + ":v0.2.0"},
		},
		{
			name:        "release tag newer than the pin",
			tag:         "v0.2.0",
			image:       image,
			pins:        []string{image + ":v0.1.0"},
			wantExit:    1,
			wantMessage: "but the tag being released is v0.2.0",
		},
		{
			name:        "release tag older than the pin",
			tag:         "v0.1.0",
			image:       image,
			pins:        []string{image + ":v0.2.0"},
			wantExit:    1,
			wantMessage: "but the tag being released is v0.1.0",
		},
		{
			name:        "manifests disagree on the pin",
			tag:         "v0.2.0",
			image:       image,
			pins:        []string{image + ":v0.2.0", image + ":v0.1.0"},
			wantExit:    1,
			wantMessage: "v0.1.0",
		},
		{
			name:        "no manifest references the published image",
			tag:         "v0.2.0",
			image:       image,
			pins:        []string{"redis:7-alpine"},
			wantExit:    1,
			wantMessage: "nothing deploys",
		},
		{
			name:        "manifest directory missing",
			tag:         "v0.2.0",
			image:       image,
			noManifests: true,
			wantExit:    1,
			wantMessage: "missing",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			manifests := filepath.Join(dir, manifestDir)
			if tt.noManifests {
				manifests = filepath.Join(dir, "missing")
			} else if err := os.Mkdir(manifests, 0o755); err != nil {
				t.Fatal(err)
			}
			writeReleaseGateManifests(t, manifests, tt.pins...)

			out, code := runReleaseGate(t, dir, "tag", tt.tag, tt.image, manifests)
			if code != tt.wantExit {
				t.Fatalf("exit = %d, want %d:\n%s", code, tt.wantExit, out)
			}
			if tt.wantMessage != "" && !strings.Contains(out, tt.wantMessage) {
				t.Fatalf("output %q does not mention %q", out, tt.wantMessage)
			}
		})
	}
}

// A missing argument is a usage error, not a pass: the gate must never read an
// empty tag as "nothing to compare".
func TestReleaseGate_UsageErrors(t *testing.T) {
	dir := t.TempDir()
	for name, args := range map[string][]string{
		"no arguments":             nil,
		"unknown mode":             {"publish", "v0.2.0"},
		"tag mode without tag":     {"tag"},
		"tag mode without image":   {"tag", "v0.2.0"},
		"main mode without commit": {"main"},
	} {
		t.Run(name, func(t *testing.T) {
			out, code := runReleaseGate(t, dir, args...)
			if code == 0 {
				t.Fatalf("args %v accepted (exit 0):\n%s", args, out)
			}
			if !strings.Contains(out, "usage:") {
				t.Fatalf("output %q does not print the usage", out)
			}
		})
	}
}

// The reachability gate on a fixture repository: a commit on main passes, a
// commit on an unmerged branch fails (the "tag pushed from a commit that never
// merged bypasses CI" shape), and a missing main ref or commit fails instead of
// passing by default.
func TestReleaseGateMain_ReachabilityFixtures(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git in PATH: the reachability gate cannot be executed here")
	}

	dir := t.TempDir()
	gitIn(t, dir, "init", "-q", "-b", "main")
	gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "main commit")
	onMain := gitIn(t, dir, "rev-parse", "HEAD")

	gitIn(t, dir, "checkout", "-q", "-b", "unmerged")
	if err := os.WriteFile(filepath.Join(dir, "side.txt"), []byte("side\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", "side.txt")
	gitIn(t, dir, "commit", "-q", "-m", "unmerged commit")
	unmerged := gitIn(t, dir, "rev-parse", "HEAD")

	tests := []struct {
		name        string
		commit      string
		ref         string
		wantExit    int
		wantMessage string
	}{
		{
			name:   "commit on main",
			commit: onMain,
			ref:    "main",
		},
		{
			name:        "commit on an unmerged branch",
			commit:      unmerged,
			ref:         "main",
			wantExit:    1,
			wantMessage: "never merged",
		},
		{
			name:        "main ref absent from the checkout",
			commit:      onMain,
			ref:         "origin/main",
			wantExit:    1,
			wantMessage: "fetch main first",
		},
		{
			name:        "commit absent from the checkout",
			commit:      strings.Repeat("0", 40),
			ref:         "main",
			wantExit:    1,
			wantMessage: "is not in this checkout",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, code := runReleaseGate(t, dir, "main", tt.commit, tt.ref)
			if code != tt.wantExit {
				t.Fatalf("exit = %d, want %d:\n%s", code, tt.wantExit, out)
			}
			if tt.wantMessage != "" && !strings.Contains(out, tt.wantMessage) {
				t.Fatalf("output %q does not mention %q", out, tt.wantMessage)
			}
		})
	}
}
