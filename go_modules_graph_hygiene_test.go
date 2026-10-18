package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Generated module-graph hygiene: the required `update-go_modules-graph` job
// (`.github/workflows/ci.yml`) runs `go mod graph` and asserts the graph file is
// non-empty. The graph is generated output that matches no source file, so
// nothing the job writes may be committable. The reviewed defect was
// `go mod graph > go_modules_graph.txt`: the step wrote the file into the
// checkout root while .gitignore had no entry for it, so `git add -A` in a
// developer's tree - or any local reproduction of that step - swept a large
// generated dependency graph into a commit as if it were source.
//
// The fix has two halves, and TestModuleGraphStepWritesGraphOutsideCheckout
// fails when either is undone: the step writes the graph outside the checkout
// ($RUNNER_TEMP), and .gitignore covers go_modules_graph.txt for the copy a
// hand-run of the same command leaves in the tree. The non-empty assertion still
// runs, so TestWorkflowsReportModuleGraphCheck keeps its meaning.

// moduleGraphFileName is the name the module-graph job gives the generated graph,
// and the file a developer's local reproduction of the step leaves behind.
const moduleGraphFileName = "go_modules_graph.txt"

// gitignoreFile is the pattern file that decides which working-tree files a
// `git add -A` can pick up.
const gitignoreFile = ".gitignore"

// graphHygieneIgnoredPatterns reads a .gitignore into the set of live patterns
// (comments and blank lines dropped). The reader is deliberately small: the
// patterns this check has to see are file names and simple globs, and
// gitignore's full matching rules (negations, directory-only patterns) would
// hide a missing entry rather than reveal one.
func graphHygieneIgnoredPatterns(path string) (map[string]bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	patterns := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns[line] = true
	}
	return patterns, nil
}

// graphHygieneIgnored reports whether a working-tree-relative path is covered by
// one of the given .gitignore patterns: by its exact path, by its base name, or
// by a glob matching either.
func graphHygieneIgnored(target string, patterns map[string]bool) bool {
	target = strings.TrimPrefix(target, "./")
	base := filepath.Base(target)
	for pattern := range patterns {
		candidate := strings.TrimPrefix(strings.TrimPrefix(pattern, "/"), "./")
		if candidate == target || candidate == base {
			return true
		}
		if matched, err := filepath.Match(candidate, base); err == nil && matched {
			return true
		}
		if matched, err := filepath.Match(candidate, target); err == nil && matched {
			return true
		}
	}
	return false
}

// graphHygieneOutsideCheckout reports whether a redirection target names a place
// outside the checkout: an absolute or home path, a $RUNNER_TEMP/$TMPDIR
// expansion, or the `runner.temp` expression. Anything else is relative to the
// working tree and therefore committable unless .gitignore covers it.
func graphHygieneOutsideCheckout(target string) bool {
	switch {
	case strings.HasPrefix(target, "/"), strings.HasPrefix(target, "~"):
		return true
	case strings.Contains(target, "RUNNER_TEMP"),
		strings.Contains(target, "TMPDIR"),
		strings.Contains(target, "runner.temp"):
		return true
	}
	return false
}

// graphHygieneRedirectTargets returns every path a shell command text redirects
// output to with `>` or `>>`, with surrounding quotes stripped. Comment lines
// are not shell, so they are skipped; /dev/null is kept so a caller can tell a
// discard from a generated file.
func graphHygieneRedirectTargets(command string) []string {
	var targets []string
	for _, line := range strings.Split(command, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		rest := graphHygieneCollapsedExpression(line)
		for {
			i := strings.Index(rest, ">")
			if i < 0 {
				break
			}
			rest = strings.TrimLeft(rest[i+1:], "> \t")
			if rest == "" {
				break
			}
			target := rest
			if j := strings.IndexAny(rest, " \t"); j >= 0 {
				target = rest[:j]
			}
			rest = rest[len(target):]
			target = strings.Trim(target, `"'`)
			if target == "" {
				continue
			}
			targets = append(targets, target)
		}
	}
	return targets
}

// graphHygieneCollapsedExpression joins a `${{ ... }}` GitHub expression into a
// single token by dropping the spaces it is written with, so
// `${{ runner.temp }}/graph.txt` is read as one redirection target instead of
// being cut at the expression's first space.
func graphHygieneCollapsedExpression(line string) string {
	var b strings.Builder
	inExpression := false
	for i := 0; i < len(line); i++ {
		switch {
		case !inExpression && strings.HasPrefix(line[i:], "${{"):
			inExpression = true
			b.WriteString("${{")
			i += 2
		case inExpression && strings.HasPrefix(line[i:], "}}"):
			inExpression = false
			b.WriteString("}}")
			i++
		case inExpression && line[i] == ' ':
			// Drop the padding the expression is written with.
		default:
			b.WriteByte(line[i])
		}
	}
	return b.String()
}

// graphHygieneProblems returns one message per redirection target that lands
// inside the checkout without a .gitignore pattern covering it, plus one message
// when the text redirects nothing at all, so a caller can tell "no problems"
// apart from "the redirection was dropped and nothing was checked".
func graphHygieneProblems(command string, ignored map[string]bool) []string {
	targets := graphHygieneRedirectTargets(command)
	if len(targets) == 0 {
		return []string{"no output redirection found, so no generated file path could be checked"}
	}
	var problems []string
	for _, target := range targets {
		if target == os.DevNull || graphHygieneOutsideCheckout(target) {
			continue
		}
		if graphHygieneIgnored(target, ignored) {
			continue
		}
		problems = append(problems, fmt.Sprintf(
			"generated file %q is written inside the checkout and %s does not cover it, so `git add -A` can commit generated output; write it outside the tree (for example under $RUNNER_TEMP) or ignore it",
			target, gitignoreFile))
	}
	sort.Strings(problems)
	return problems
}

// graphHygieneJobBody returns the body of the module-graph job in one workflow's
// text. It reuses the job reader the required-status-check guard uses, so both
// checks look at the same job.
func graphHygieneJobBody(text string) (string, bool) {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if isJobDeclaration(line, moduleGraphJobName) {
			return jobBody(lines[i+1:]), true
		}
	}
	return "", false
}

// graphHygieneOutsideTargets returns the redirection targets that land outside
// the checkout, so a caller can require that a generated file is actually
// written out of the tree instead of merely being ignored.
func graphHygieneOutsideTargets(command string) []string {
	var outside []string
	for _, target := range graphHygieneRedirectTargets(command) {
		if target != os.DevNull && graphHygieneOutsideCheckout(target) {
			outside = append(outside, target)
		}
	}
	return outside
}

// The real repository: the required module-graph job must not be able to leave a
// committable graph file, and .gitignore must cover the name for a hand-run of
// `go mod graph > go_modules_graph.txt`. The shipped shape is the strongest of
// the accepted ones - the graph goes to $RUNNER_TEMP, outside the checkout - so
// this test also fails if the step starts writing into the working tree again
// (even if the ignore entry would cover it).
func TestModuleGraphStepWritesGraphOutsideCheckout(t *testing.T) {
	patterns, err := graphHygieneIgnoredPatterns(gitignoreFile)
	if err != nil {
		t.Fatalf("%s: %v", gitignoreFile, err)
	}
	if !graphHygieneIgnored(moduleGraphFileName, patterns) {
		t.Errorf("%s does not ignore %s: a local `go mod graph > %s` leaves a generated dependency graph that `git add -A` commits as if it were source",
			gitignoreFile, moduleGraphFileName, moduleGraphFileName)
	}

	workflowPath := filepath.Join(ciWorkflowDir, "ci.yml")
	data, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatal(err)
	}
	body, ok := graphHygieneJobBody(string(data))
	if !ok {
		t.Fatalf("%s: no %s job to inspect", workflowPath, moduleGraphJobName)
	}
	for _, problem := range graphHygieneProblems(body, patterns) {
		t.Errorf("%s: %s", workflowPath, problem)
	}
	if outside := graphHygieneOutsideTargets(body); len(outside) == 0 {
		t.Errorf("%s: the %s job writes no generated file outside the checkout, so the graph it produces can only be prevented from being committed by an ignore entry",
			workflowPath, moduleGraphJobName)
	}
}

// The failure modes of the scan: the reviewed defect (the graph written into the
// checkout root with no ignore entry) must be reported, every accepted shape
// must pass, and a dropped or commented-out redirection must not read as
// "nothing to check".
func TestGraphHygieneRedirectScan_Fixtures(t *testing.T) {
	ignored := map[string]bool{moduleGraphFileName: true, "coverage*.txt": true}

	tests := []struct {
		name          string
		command       string
		patterns      map[string]bool
		wantProblem   bool
		wantSubstring string
	}{
		{
			name:          "reviewed defect: graph in the checkout root, not ignored",
			command:       "go mod graph > go_modules_graph.txt\n",
			wantProblem:   true,
			wantSubstring: moduleGraphFileName,
		},
		{
			name:        "in-tree path the scan accepts because .gitignore covers it",
			command:     "go mod graph > go_modules_graph.txt\n",
			patterns:    ignored,
			wantProblem: false,
		},
		{
			name:          "append redirection names the same file",
			command:       "go mod graph >> go_modules_graph.txt\n",
			wantProblem:   true,
			wantSubstring: moduleGraphFileName,
		},
		{
			name:        "written under a $RUNNER_TEMP fallback",
			command:     "go mod graph > \"${RUNNER_TEMP:-/tmp}/go_modules_graph.txt\"\n",
			wantProblem: false,
		},
		{
			name:        "written under the runner temp expression",
			command:     "go mod graph > \"${{ runner.temp }}/go_modules_graph.txt\"\n",
			wantProblem: false,
		},
		{
			name:        "written under an absolute temp path",
			command:     "go mod graph > /tmp/go_modules_graph.txt\n",
			wantProblem: false,
		},
		{
			name:          "subdirectory is still inside the checkout",
			command:       "go mod graph > out/go_modules_graph.txt\n",
			wantProblem:   true,
			wantSubstring: "out/go_modules_graph.txt",
		},
		{
			name:        "subdirectory covered by the base-name pattern",
			command:     "go mod graph > out/go_modules_graph.txt\n",
			patterns:    ignored,
			wantProblem: false,
		},
		{
			name:        "discard to /dev/null is not a generated file",
			command:     "go list -m -json -e all > /dev/null\n",
			wantProblem: false,
		},
		{
			name:          "no redirection at all is reported",
			command:       "go mod graph\n",
			wantProblem:   true,
			wantSubstring: "no output redirection",
		},
		{
			name:          "commented-out redirection is not read as a shell command",
			command:       "# go mod graph > go_modules_graph.txt\n",
			wantProblem:   true,
			wantSubstring: "no output redirection",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			problems := graphHygieneProblems(tt.command, tt.patterns)
			if !tt.wantProblem {
				if len(problems) != 0 {
					t.Fatalf("%q: %d problems, want none: %v", tt.command, len(problems), problems)
				}
				return
			}
			if len(problems) == 0 {
				t.Fatalf("%q: no problem reported, want one mentioning %q", tt.command, tt.wantSubstring)
			}
			if !strings.Contains(problems[0], tt.wantSubstring) {
				t.Fatalf("%q: problem %q does not mention %q", tt.command, problems[0], tt.wantSubstring)
			}
		})
	}
}

// A missing .gitignore, or one that lists only comments, must not read as "the
// file is ignored": the reader has to report the empty pattern set.
func TestGraphHygieneIgnoredPatterns_Fixtures(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gitignore")
	if err := os.WriteFile(path, []byte("# only a comment\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	patterns, err := graphHygieneIgnoredPatterns(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(patterns) != 0 {
		t.Fatalf("comment-only file yielded patterns %v, want none", patterns)
	}
	if graphHygieneIgnored(moduleGraphFileName, patterns) {
		t.Fatalf("%s reported as ignored by a comment-only file", moduleGraphFileName)
	}
	if _, err := graphHygieneIgnoredPatterns(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("a missing .gitignore was read without error")
	}
}
