package main

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Module-graph export hygiene: the `update-go_modules-graph` job runs
// `go mod graph` only to prove the graph can be produced from go.mod/go.sum
// alone, but it redirected that output to go_modules_graph.txt in the checkout
// root (.github/workflows/ci.yml:252). The shipped .gitignore listed build
// artifacts, test binaries, coverage output and Go workspace files, but not that
// name, so any developer reproducing the step by hand - or any `git add -A` in
// the same tree - staged the whole dependency graph as if it were source.
//
// The export now goes to the runner's temp directory, outside the working tree,
// and this check keeps it that way:
//
//   - a redirect back into the checkout is only accepted when .gitignore names
//     the file, so the generated graph can never be committed;
//   - the `test -s` assertion must keep checking the file that was written, so
//     the required `update-go_modules-graph` status check cannot stay green over
//     an assertion that reads nothing, or over a job that exports no graph at
//     all.
//
// No compiler can see a workflow file, so the check lives here and runs with
// `go test -count=1 ./...`, which both CI and release.yml run.

// moduleGraphArtifact is the base name of the file the module-graph job
// exports. Only that export (not the file's location) is what this check
// guards: the location is the job's decision, the name is what a stray copy in
// the tree is called.
const moduleGraphArtifact = "go_modules_graph.txt"

// moduleGraphGitignore is the ignore file consulted for an in-tree export, held
// at the repository root, which is also the working directory `go test` runs in.
const moduleGraphGitignore = ".gitignore"

// moduleGraphWord matches one shell word: a quoted string (so a target with
// spaces, as in the `${{ runner.temp }}` expression form, is read whole) or a run
// of non-space characters.
const moduleGraphWord = `("[^"]*"|'[^']*'|\S+)`

// moduleGraphWriteRe matches the export redirect of the module-graph job and
// captures the redirect target: `go mod graph > "$RUNNER_TEMP/x.txt"` and the
// append form `go mod graph >> x.txt` both match.
var moduleGraphWriteRe = regexp.MustCompile(`go mod graph\s*(>>?)\s*` + moduleGraphWord)

// moduleGraphAssertRe matches the non-empty assertion on the exported file and
// captures its target, so the assertion can be compared with what was written.
var moduleGraphAssertRe = regexp.MustCompile(`test -s\s+` + moduleGraphWord)

// shellWord strips the quoting a shell word carries, so a quoted redirect
// target and the quoted assertion on it compare equal: `"$RUNNER_TEMP/x.txt"`
// and '$RUNNER_TEMP/x.txt' both become $RUNNER_TEMP/x.txt.
func shellWord(word string) string {
	word = strings.TrimSpace(word)
	for _, quote := range []string{`"`, `'`} {
		if len(word) >= 2 && strings.HasPrefix(word, quote) && strings.HasSuffix(word, quote) {
			word = word[1 : len(word)-1]
		}
	}
	return word
}

// moduleGraphOutputOutsideCheckout reports whether target names a file outside
// the repository working tree: an absolute path, a parent-relative path, or a
// path under the runner's temp directory (GitHub sets RUNNER_TEMP, and
// `${{ runner.temp }}` is the same directory in expression form). Anything else
// is a path in the checkout, which is what `git add -A` picks up.
func moduleGraphOutputOutsideCheckout(target string) bool {
	switch {
	case target == "":
		return false
	case strings.HasPrefix(target, "/"):
		return true
	case strings.HasPrefix(target, "../"):
		return true
	case strings.Contains(target, "RUNNER_TEMP"):
		return true
	case strings.Contains(strings.ToLower(target), "runner.temp"):
		return true
	}
	return false
}

// moduleGraphIgnoredEntries reads the ignore patterns of one ignore file, one
// entry per line, dropping blank lines and comments and the optional leading
// slash that anchors a pattern to the repository root. A missing file yields no
// entries rather than an error: an unspecified ignore file ignores nothing,
// which is exactly the state that let the export be staged.
func moduleGraphIgnoredEntries(file string) ([]string, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var entries []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		entries = append(entries, strings.TrimPrefix(line, "/"))
	}
	return entries, nil
}

// moduleGraphIgnoredBy reports whether the patterns cover target, matching
// either the target's path or its base name: `.gitignore` entries in this
// repository name files without a directory, and an entry that names the file
// keeps it out of the index wherever it is written.
func moduleGraphIgnoredBy(entries []string, target string) bool {
	base := path.Base(target)
	for _, entry := range entries {
		entry = strings.TrimPrefix(strings.TrimSuffix(entry, "/"), "/")
		if entry == target || entry == base {
			return true
		}
	}
	return false
}

// moduleGraphOutputProblems returns one message per problem with the export the
// module-graph jobs under workflowDir write, plus the number of exports seen, so
// a caller can tell "no problems" from "no job exported a graph, so nothing was
// checked". An in-tree export that .gitignore does not cover is reported, as is
// an export the `test -s` assertion does not check.
func moduleGraphOutputProblems(workflowDir, gitignoreFile string) ([]string, int, error) {
	ignored, err := moduleGraphIgnoredEntries(gitignoreFile)
	if err != nil {
		return nil, 0, err
	}

	entries, err := os.ReadDir(workflowDir)
	if err != nil {
		return nil, 0, err
	}

	var problems []string
	exports := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if ext := filepath.Ext(entry.Name()); ext != ".yml" && ext != ".yaml" {
			continue
		}
		file := filepath.Join(workflowDir, entry.Name())
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, 0, err
		}
		found, count := moduleGraphOutputProblemsIn(file, string(data), ignored)
		problems = append(problems, found...)
		exports += count
	}
	sort.Strings(problems)
	return problems, exports, nil
}

// moduleGraphOutputProblemsIn scans one workflow's text for the export of the
// module-graph job and returns the problems found and the number of redirects
// seen. Commented-out lines are skipped: a commented redirect writes nothing, so
// it neither stages an artifact nor proves the graph.
func moduleGraphOutputProblemsIn(file, text string, ignored []string) ([]string, int) {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if !isJobDeclaration(line, moduleGraphJobName) {
			continue
		}

		var problems []string
		var written []string
		asserted := map[string]bool{}
		for j, bodyLine := range strings.Split(jobBody(lines[i+1:]), "\n") {
			at := i + j + 2 // 1-based line number of this body line
			if trimmed := strings.TrimSpace(bodyLine); trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			for _, match := range moduleGraphWriteRe.FindAllStringSubmatch(bodyLine, -1) {
				target := shellWord(match[2])
				if target == "" {
					problems = append(problems, fmt.Sprintf(
						"%s:%d: exports the module graph with a redirect that names no target, so the exported file cannot be checked", file, at))
					continue
				}
				if !moduleGraphOutputOutsideCheckout(target) && !moduleGraphIgnoredBy(ignored, target) {
					problems = append(problems, fmt.Sprintf(
						"%s:%d: writes the module-graph export to %s inside the checkout and %s does not cover it, so a stray export is staged by `git add -A` as if it were source; write it outside the working tree (for example \"$RUNNER_TEMP/%s\") or name it in %s",
						file, at, target, moduleGraphGitignore, moduleGraphArtifact, moduleGraphGitignore))
				}
				written = append(written, target)
			}
			for _, match := range moduleGraphAssertRe.FindAllStringSubmatch(bodyLine, -1) {
				if target := shellWord(match[1]); target != "" {
					asserted[target] = true
				}
			}
		}

		if len(written) == 0 {
			problems = append(problems, fmt.Sprintf(
				"%s:%d: the %s job exports no module graph, so the graph its required status check stands for cannot be inspected",
				file, i+1, moduleGraphJobName))
		}
		for _, target := range written {
			if !asserted[target] {
				problems = append(problems, fmt.Sprintf(
					"%s: the %s job writes %s but no `test -s %s` asserts it is non-empty, so the required check could pass over an empty export",
					file, moduleGraphJobName, target, target))
			}
		}
		return problems, len(written)
	}
	return nil, 0
}

// The reviewed release_hygiene defect: the export landed in the checkout root
// and the ignore file did not cover it. The export must now be written outside
// the working tree (or ignored), and the job must still export a graph, so the
// required status check keeps a file to assert on.
func TestModuleGraphExportIsNotWrittenIntoTheCheckout(t *testing.T) {
	problems, exports, err := moduleGraphOutputProblems(ciWorkflowDir, moduleGraphGitignore)
	if err != nil {
		t.Fatal(err)
	}
	if exports == 0 {
		t.Fatalf("no module-graph export found in %s: the %s job must still write the graph its required status check stands for", ciWorkflowDir, moduleGraphJobName)
	}
	for _, problem := range problems {
		t.Errorf("%s", problem)
	}
}

// The ignore file is the second line of defence: reproducing the step by hand in
// a working tree writes the same file name into the checkout, so the name must
// be ignored even though CI now writes it elsewhere.
func TestGitignoreCoversModuleGraphArtifact(t *testing.T) {
	entries, err := moduleGraphIgnoredEntries(moduleGraphGitignore)
	if err != nil {
		t.Fatal(err)
	}
	if !moduleGraphIgnoredBy(entries, moduleGraphArtifact) {
		t.Fatalf("%s does not list %s: a hand-run of the module-graph step would leave a generated dependency graph untracked, which `git add -A` commits as if it were source", moduleGraphGitignore, moduleGraphArtifact)
	}
}

// The failure modes: the reviewed in-tree export with no ignore entry, an
// in-tree export an ignore entry does cover, the shipped runner-temp export, the
// absolute and parent-relative forms, a missing export, and an export whose
// non-empty assertion checks a different file or is gone. The unmutated fixture
// must pass, so every reported problem is the edit and not a check that always
// complains.
func TestModuleGraphOutputProblems_RegressionFixtures(t *testing.T) {
	graphStep := func(body string) string {
		return "jobs:\n" +
			"  " + moduleGraphJobName + ":\n" +
			"    name: " + moduleGraphJobName + "\n" +
			"    runs-on: ubuntu-latest\n" +
			"    steps:\n" +
			"      - uses: actions/checkout@v7\n" +
			"      - name: Update go_modules graph\n" +
			"        run: |\n" + body
	}
	export := "          go mod graph > %s\n"
	assert := "          test -s %s\n"
	runnerTemp := `"$RUNNER_TEMP/` + moduleGraphArtifact + `"`

	tests := []struct {
		name        string
		workflow    string
		gitignore   string
		wantExports int
		wantProblem string
	}{
		{
			name:        "reviewed shape: exported into the checkout with no ignore entry",
			workflow:    graphStep(fmt.Sprintf(export, moduleGraphArtifact) + fmt.Sprintf(assert, moduleGraphArtifact)),
			gitignore:   "# Build artifacts\n*.exe\n*.test\n",
			wantExports: 1,
			wantProblem: "inside the checkout",
		},
		{
			name:        "in-tree export the ignore file covers",
			workflow:    graphStep(fmt.Sprintf(export, moduleGraphArtifact) + fmt.Sprintf(assert, moduleGraphArtifact)),
			gitignore:   "# Build artifacts\n*.exe\n" + moduleGraphArtifact + "\n",
			wantExports: 1,
		},
		{
			name:        "shipped shape: exported to the runner temp directory",
			workflow:    graphStep(fmt.Sprintf(export, runnerTemp) + fmt.Sprintf(assert, runnerTemp)),
			wantExports: 1,
		},
		{
			name:        "expression form of the runner temp directory",
			workflow:    graphStep(fmt.Sprintf(export, `"${{ runner.temp }}/`+moduleGraphArtifact+`"`) + fmt.Sprintf(assert, `"${{ runner.temp }}/`+moduleGraphArtifact+`"`)),
			wantExports: 1,
		},
		{
			name:        "absolute path outside the checkout",
			workflow:    graphStep(fmt.Sprintf(export, "/tmp/"+moduleGraphArtifact) + fmt.Sprintf(assert, "/tmp/"+moduleGraphArtifact)),
			wantExports: 1,
		},
		{
			name:        "parent-relative path outside the checkout",
			workflow:    graphStep(fmt.Sprintf(export, "../"+moduleGraphArtifact) + fmt.Sprintf(assert, "../"+moduleGraphArtifact)),
			wantExports: 1,
		},
		{
			name:        "append redirect into the checkout",
			workflow:    graphStep(fmt.Sprintf("          go mod graph >> %s\n", moduleGraphArtifact) + fmt.Sprintf(assert, moduleGraphArtifact)),
			wantExports: 1,
			wantProblem: "inside the checkout",
		},
		{
			name:        "subdirectory of the checkout not covered by an ignore entry",
			workflow:    graphStep(fmt.Sprintf(export, "out/"+moduleGraphArtifact) + fmt.Sprintf(assert, "out/"+moduleGraphArtifact)),
			gitignore:   "# Build artifacts\n*.exe\n",
			wantExports: 1,
			wantProblem: "inside the checkout",
		},
		{
			name:        "no export at all",
			workflow:    graphStep("          echo no graph here\n"),
			wantExports: 0,
			wantProblem: "exports no module graph",
		},
		{
			name:        "export only in a comment",
			workflow:    graphStep("          # go mod graph > " + moduleGraphArtifact + "\n"),
			wantExports: 0,
			wantProblem: "exports no module graph",
		},
		{
			name:        "non-empty assertion checks another file",
			workflow:    graphStep(fmt.Sprintf(export, runnerTemp) + fmt.Sprintf(assert, `"$RUNNER_TEMP/other.txt"`)),
			wantExports: 1,
			wantProblem: "no `test -s",
		},
		{
			name:        "no non-empty assertion",
			workflow:    graphStep(fmt.Sprintf(export, runnerTemp)),
			wantExports: 1,
			wantProblem: "no `test -s",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "ci.yml"), []byte(tt.workflow), 0o644); err != nil {
				t.Fatal(err)
			}
			if tt.gitignore != "" {
				if err := os.WriteFile(filepath.Join(dir, moduleGraphGitignore), []byte(tt.gitignore), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			problems, exports, err := moduleGraphOutputProblems(dir, filepath.Join(dir, moduleGraphGitignore))
			if err != nil {
				t.Fatal(err)
			}
			if exports != tt.wantExports {
				t.Fatalf("exports = %d, want %d", exports, tt.wantExports)
			}
			if tt.wantProblem == "" {
				if len(problems) != 0 {
					t.Fatalf("problems = %v, want none", problems)
				}
				return
			}
			if len(problems) == 0 {
				t.Fatal("workflow leaving the generated export stageable accepted, want a problem reported")
			}
			if !strings.Contains(strings.Join(problems, "\n"), tt.wantProblem) {
				t.Fatalf("problems = %v, want one naming %q", problems, tt.wantProblem)
			}
		})
	}
}

// The scan reads containment and ignore coverage from the paths themselves, so
// the helpers must not misread a file that is merely named like an export of
// another directory, or an ignore file that only comments the name.
func TestModuleGraphOutputHelpers(t *testing.T) {
	targets := []struct {
		target string
		want   bool
	}{
		{target: "go_modules_graph.txt"},
		{target: "out/go_modules_graph.txt"},
		{target: `"$RUNNER_TEMP/` + moduleGraphArtifact + `"`, want: true},
		{target: "${RUNNER_TEMP}/x.txt", want: true},
		{target: "${{ runner.temp }}/x.txt", want: true},
		{target: "/tmp/x.txt", want: true},
		{target: "../x.txt", want: true},
		{target: ""},
	}
	for _, tt := range targets {
		if got := moduleGraphOutputOutsideCheckout(shellWord(tt.target)); got != tt.want {
			t.Errorf("moduleGraphOutputOutsideCheckout(%q) = %v, want %v", tt.target, got, tt.want)
		}
	}

	if moduleGraphIgnoredBy([]string{"# " + moduleGraphArtifact, "*.exe"}, moduleGraphArtifact) {
		t.Error("a commented ignore entry counted as coverage of the export")
	}
	if !moduleGraphIgnoredBy([]string{"/" + moduleGraphArtifact}, moduleGraphArtifact) {
		t.Error("a root-anchored ignore entry did not count as coverage of the export")
	}

	if entries, err := moduleGraphIgnoredEntries(filepath.Join(t.TempDir(), "missing")); err != nil || len(entries) != 0 {
		t.Errorf("entries = %v, err = %v, want no entries and no error for a missing ignore file", entries, err)
	}
}

// The scan is per directory, not per first match: a second workflow that exports
// the graph into the checkout is read beside the shipped one, and the missing
// workflow directory is an error rather than a silent pass.
func TestModuleGraphOutputProblems_AcrossWorkflowFiles(t *testing.T) {
	dir := t.TempDir()
	body := func(target string) string {
		return "jobs:\n" +
			"  " + moduleGraphJobName + ":\n" +
			"    runs-on: ubuntu-latest\n" +
			"    steps:\n" +
			"      - name: Update go_modules graph\n" +
			"        run: |\n" +
			"          go mod graph > " + target + "\n" +
			"          test -s " + target + "\n"
	}
	for name, target := range map[string]string{
		"ci.yml":     `"$RUNNER_TEMP/` + moduleGraphArtifact + `"`,
		"second.yml": moduleGraphArtifact,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body(target)), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	problems, exports, err := moduleGraphOutputProblems(dir, filepath.Join(dir, moduleGraphGitignore))
	if err != nil {
		t.Fatal(err)
	}
	if exports != 2 {
		t.Fatalf("exports = %d, want both workflows read", exports)
	}
	if len(problems) != 1 || !strings.Contains(problems[0], "second.yml") {
		t.Fatalf("problems = %v, want exactly the second workflow's in-tree export reported", problems)
	}

	if _, _, err := moduleGraphOutputProblems(filepath.Join(dir, "missing"), filepath.Join(dir, moduleGraphGitignore)); err == nil {
		t.Error("missing workflow directory accepted, want a read error")
	}
}
