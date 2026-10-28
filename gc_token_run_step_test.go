package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The leftover-GC-token rule of the offline workflow checker, pinned for both
// step shapes.
//
// The reviewed defect: ci_check_comments.py scanned run commands with
// `run: \|?\n((?:[ \t]+[^\n]*\n)*)`, which needs a newline straight after
// `run:`. Block scalars matched, inline steps (`run: gc-strace ./binary`,
// `- run: go build ./... && gcDirty ...`) matched nothing, so a leftover token
// moved onto the inline form stopped being reported while the shipped
// block-scalar fixtures stayed green.
//
// Two gates below cover it, so neither shape can regress silently:
//   - gcTokenRunStepSuite is the unittest file with the released-fixture cases:
//     an inline run step carrying each token must be reported, the block-scalar
//     shapes must keep being reported, and a step name or comment must stay
//     invisible to the scan. `go test -count=1 ./...` runs it, and the same
//     build step runs ci_check_comments.py itself.
//   - TestGCTokenCheckerFixtureFiles runs the checker as CI runs it - as a
//     script over a file path - on fixture workflows, so the exit status and
//     the reported message are asserted end to end rather than on the internal
//     helper alone. The pre-fix failure mode (an inline step reported as clean)
//     fails here.

const gcTokenRunStepSuite = "test_workflow_run_step_checks.py"

// TestGCTokenRunStepSuitePasses executes the inline-run-step fixtures. Like the
// existing suite wiring it fails when the file is missing, when a case fails,
// and when the file was emptied of cases.
func TestGCTokenRunStepSuitePasses(t *testing.T) {
	if _, err := os.Stat(gcTokenRunStepSuite); err != nil {
		t.Fatalf("%s is missing: %v; the checker's inline `run:` step shapes are unasserted again", gcTokenRunStepSuite, err)
	}

	python := pythonCheckInterpreter()
	if python == "" {
		t.Skip("no local python interpreter, so the inline run step fixtures cannot run here; native CI provides python3")
	}

	out, err := runPythonCheckSuite(python, gcTokenRunStepSuite)
	if err != nil {
		if count, ok := unittestRanTests(string(out)); unittestNoTestsExit(err) || (ok && count == 0) {
			t.Fatalf("%s ran 0 tests: the inline run step cases were deleted or renamed, so an inline `run:` step carrying a leftover GC token would pass again:\n%s", gcTokenRunStepSuite, out)
		}
		t.Fatalf("%s %s: %v\n%s", python, gcTokenRunStepSuite, err, out)
	}
	count, ok := unittestRanTests(string(out))
	if !ok {
		t.Fatalf("no unittest summary in the output of %s, so nothing proves its cases ran:\n%s", gcTokenRunStepSuite, out)
	}
	if count == 0 {
		t.Fatalf("%s ran 0 tests: the inline run step cases were deleted or renamed", gcTokenRunStepSuite)
	}
}

// TestGCTokenCheckerFixtureFiles runs ci_check_comments.py the way CI does, as
// `python3 ci_check_comments.py <workflow>`, over the fixture shapes that hid
// the defect. The pre-fix checker exited 0 (and printed the "scan ok" line) for
// every inline fixture below, so these cases fail against it.
func TestGCTokenCheckerFixtureFiles(t *testing.T) {
	python := pythonCheckInterpreter()
	if python == "" {
		t.Skip("no local python interpreter, so the checker cannot be executed over fixtures here; native CI provides python3")
	}

	cases := []struct {
		name     string
		workflow string
		wantFail bool
		wantIn   string
	}{
		{
			name: "inline run step carrying a token",
			workflow: "jobs:\n" +
				"  go:\n" +
				"    steps:\n" +
				"      - name: Build\n" +
				"        run: gc-strace ./binary\n",
			wantFail: true,
			wantIn:   "gc-strace",
		},
		{
			name: "inline list item step carrying a token",
			workflow: "jobs:\n" +
				"  go:\n" +
				"    steps:\n" +
				"      - run: go build ./... && gcDirty ./binary\n",
			wantFail: true,
			wantIn:   "gcDirty",
		},
		{
			name: "block scalar carrying a token",
			workflow: "jobs:\n" +
				"  go:\n" +
				"    steps:\n" +
				"      - name: Build\n" +
				"        run: |\n" +
				"          gc-strace ./binary\n",
			wantFail: true,
			wantIn:   "gc-strace",
		},
		{
			name: "clean inline step",
			workflow: "jobs:\n" +
				"  go:\n" +
				"    steps:\n" +
				"      - name: Build\n" +
				"        run: go build ./...\n",
			wantFail: false,
		},
	}

	dir := t.TempDir()
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(tt.name, " ", "-")+".yml")
			if err := os.WriteFile(path, []byte(tt.workflow), 0o644); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command(python, "ci_check_comments.py", path).CombinedOutput()
			text := string(out)
			if tt.wantFail {
				if err == nil {
					t.Fatalf("checker passed a workflow it must report (%s), so the token is invisible to the scan:\n%s", tt.name, text)
				}
				if !strings.Contains(text, "gc-related step text") || !strings.Contains(text, tt.wantIn) {
					t.Fatalf("checker failed the workflow without naming the leftover token %q:\n%s", tt.wantIn, text)
				}
				return
			}
			if err != nil {
				t.Fatalf("checker rejected a clean workflow: %v\n%s", err, text)
			}
			if strings.Contains(text, "gc-related step text") {
				t.Fatalf("checker reported a leftover token in a clean workflow:\n%s", text)
			}
		})
	}
}
