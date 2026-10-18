package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The workflow checker's unittest suite, wired into the build that gates the
// pull request.
//
// test_workflow_checks.py asserts ci_check_comments.py's own rules against
// fixture text (a setup-go step with no version source, a second version source
// beside go-version-file: go.mod, a tab character, a leftover GC token in a run
// block) and then re-runs the checker over the workflows this repository ships,
// so a shipped workflow that starts violating a rule fails there too. Until now
// nothing referenced the file: the CI workflow's only Python steps ran
// `python3 ci_check_comments.py` and `python3 harness.py`, and the only Python
// invocation in a Go test ran harness.py. Its cases were therefore dead
// coverage - the file could be deleted, or a case could be broken, and every
// check stayed green.
//
// Two gates run it now, so neither a removed command nor a changed gate can
// retire it silently: a CI job of its own (.github/workflows/python-checks.yml,
// which runs `python3 test_workflow_checks.py` on every push and pull request),
// and the Go tests below, because `go test -count=1 ./...` is what the
// build-and-test job runs before the build and what release.yml runs before
// publishing. A failing case exits non-zero, Python reports a missing entry point
// as an error, and an entry point left without cases is caught by its own "Ran 0
// tests" summary. The interpreter lookup matches main_test.go's harness.py
// runner (python3, then python), so the suite runs wherever that check runs; the
// existence check runs before the lookup, so a deleted suite fails even where no
// Python is installed.

// pythonWorkflowCheckSuite is the unittest entry point for the offline workflow
// checker: the file whose cases must reach the build.
const pythonWorkflowCheckSuite = "test_workflow_checks.py"

// pythonCheckInterpreter returns the interpreter the repository's Python checks
// run with, preferring python3, or "" when neither is installed.
func pythonCheckInterpreter() string {
	for _, candidate := range []string{"python3", "python"} {
		if found, err := exec.LookPath(candidate); err == nil {
			return found
		}
	}
	return ""
}

// runPythonCheckSuite executes one repository Python check with the interpreter
// and returns its combined output. A non-zero exit is an error, so a failing
// assertion inside the suite fails the Go test that ran it.
func runPythonCheckSuite(python, script string) ([]byte, error) {
	return exec.Command(python, script).CombinedOutput()
}

// unittestRanTests reads unittest's "Ran N tests in Xs" summary. It reports
// ok=false when the summary is absent (the entry point never reached unittest),
// and N=0 for a suite whose cases were deleted or renamed - both are failures
// for a caller that needs the cases to have run.
func unittestRanTests(output string) (int, bool) {
	for _, line := range strings.Split(output, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "Ran ")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		// unittest writes "Ran 1 test" for a single case and "Ran 9 tests"
		// otherwise, so both forms have to be read.
		if len(fields) < 2 || (fields[1] != "test" && fields[1] != "tests") {
			continue
		}
		count, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		return count, true
	}
	return 0, false
}

// The summary reader, exercised without an interpreter so its edge shapes stay
// covered wherever `go test ./...` runs: unittest's singular form for one case,
// its 0-case form (an entry point emptied of cases), and output that never
// reached unittest at all.
func TestUnittestRanTestsSummary(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   int
		wantOK bool
	}{
		{
			name:   "several cases",
			output: "..........\n----------------------------------------------------------------------\nRan 9 tests in 0.002s\n\nOK\n",
			want:   9,
			wantOK: true,
		},
		{
			name:   "one case",
			output: ".\n----------------------------------------------------------------------\nRan 1 test in 0.001s\n\nOK\n",
			want:   1,
			wantOK: true,
		},
		{
			name:   "no cases left",
			output: "----------------------------------------------------------------------\nRan 0 tests in 0.000s\n\nOK\n",
			want:   0,
			wantOK: true,
		},
		{
			name:   "no unittest summary",
			output: "python3: can't open file 'test_workflow_checks.py': [Errno 2] No such file or directory\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			count, ok := unittestRanTests(tt.output)
			if count != tt.want || ok != tt.wantOK {
				t.Fatalf("unittestRanTests(%q) = %d, %v; want %d, %v", tt.output, count, ok, tt.want, tt.wantOK)
			}
		})
	}
}

// The suite has to exist and its cases have to run: deleting the file, breaking
// a case, or emptying it of cases all fail this test, which CI runs through
// `go test -count=1 ./...` on every push and pull request.
func TestWorkflowChecksPythonSuitePasses(t *testing.T) {
	if _, err := os.Stat(pythonWorkflowCheckSuite); err != nil {
		t.Fatalf("%s is missing: %v; the offline workflow checker's cases are gone, so a checker rule that stopped firing, a shipped workflow that violates one, or a leftover GC token can no longer fail a pull request", pythonWorkflowCheckSuite, err)
	}

	python := pythonCheckInterpreter()
	if python == "" {
		t.Skip("no local python interpreter, so the workflow checker's unittest suite cannot be executed here; native CI provides python3")
	}

	out, err := runPythonCheckSuite(python, pythonWorkflowCheckSuite)
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", python, pythonWorkflowCheckSuite, err, out)
	}
	count, ok := unittestRanTests(string(out))
	if !ok {
		t.Fatalf("no unittest summary in the output of %s, so nothing proves its cases ran:\n%s", pythonWorkflowCheckSuite, out)
	}
	if count == 0 {
		t.Fatalf("%s ran 0 tests: its cases were deleted or renamed, so the suite no longer guards the checker or the workflows this repository ships", pythonWorkflowCheckSuite)
	}
}

// The runner's failure modes, so the wiring above cannot pass vacuously: a
// passing suite is read as its case count, a broken case exits non-zero, a
// deleted script cannot be executed, and an entry point emptied of cases is
// distinguishable from one that ran its cases.
func TestRunPythonCheckSuite_RegressionFixtures(t *testing.T) {
	python := pythonCheckInterpreter()
	if python == "" {
		t.Skip("no local python interpreter, so the workflow checker suite runner cannot be exercised here; native CI provides python3")
	}

	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	passing := write("passing_suite.py", "import unittest\n\n\nclass FixtureTests(unittest.TestCase):\n    def test_passes(self):\n        self.assertEqual(1 + 1, 2)\n\n\nif __name__ == \"__main__\":\n    unittest.main()\n")
	empty := write("empty_suite.py", "import unittest\n\n\nif __name__ == \"__main__\":\n    unittest.main()\n")
	failing := write("failing_suite.py", "import unittest\n\n\nclass FixtureTests(unittest.TestCase):\n    def test_breaks(self):\n        self.assertEqual(1 + 1, 3)\n\n\nif __name__ == \"__main__\":\n    unittest.main()\n")

	// The shipped shape: the cases run and the entry point exits 0.
	out, err := runPythonCheckSuite(python, passing)
	if err != nil {
		t.Fatalf("passing suite: %v\n%s", err, out)
	}
	count, ok := unittestRanTests(string(out))
	if !ok || count != 1 {
		t.Fatalf("passing suite reported %d tests (summary read=%v), want the 1 case counted", count, ok)
	}

	// One broken case is what a pull request must not survive.
	if out, err := runPythonCheckSuite(python, failing); err == nil {
		t.Fatalf("suite with a failing case exited 0, so a broken case could not fail the build:\n%s", out)
	}

	// An entry point whose cases were removed exits 0 with "Ran 0 tests"; the
	// summary reader is what keeps that from reading as a pass.
	out, err = runPythonCheckSuite(python, empty)
	if err != nil {
		t.Fatalf("empty suite: %v\n%s", err, out)
	}
	if count, ok := unittestRanTests(string(out)); !ok || count != 0 {
		t.Fatalf("empty suite reported %d tests (summary read=%v), want its 0-test summary detected", count, ok)
	}

	// A deleted entry point is an execution error, not a silent pass.
	if out, err := runPythonCheckSuite(python, filepath.Join(dir, "deleted_suite.py")); err == nil {
		t.Fatalf("missing suite script exited 0, so deleting the checks would not fail the build:\n%s", out)
	}
}
