package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Workflow Go toolchain check: actions/setup-go reads a single version source.
// When a step sets both `go-version` and `go-version-file`, setup-go warns that
// the file is ignored and installs the pinned version instead, so the explicit
// pin decides the job's toolchain and drifts from go.mod's `go` directive; with
// GOTOOLCHAIN=auto `go` then downloads the toolchain go.mod asks for, which
// means the pinned version is not the one compiling the code. CI therefore
// declares `go-version-file: go.mod` alone (its "Check Go toolchain" step fails
// when the installed toolchain's MAJOR.MINOR differs from the directive), and
// this guard keeps a conflicting pin, a foreign version file or a setup-go step
// with no version source out of every workflow. Commented-out keys are neither
// pins nor version sources, so comment text is stripped before reading.

// workflowDir holds the GitHub Actions workflow definitions.
var workflowDir = filepath.Join(".github", "workflows")

// goModFile is the single place the Go toolchain version is declared; the
// workflows read it through go-version-file instead of pinning a version.
const goModFile = "go.mod"

var (
	// goVersionPinRe matches a `go-version:` key: the explicit pin that wins
	// over go-version-file, and so can drift from the module's go directive.
	goVersionPinRe = regexp.MustCompile(`^\s*go-version\s*:`)
	// goVersionFileRe matches a `go-version-file: <path>` key.
	goVersionFileRe = regexp.MustCompile(`^\s*go-version-file\s*:\s*(\S+)\s*$`)
	// setupGoUsesRe matches the `uses: actions/setup-go@vX` line of a step.
	setupGoUsesRe = regexp.MustCompile(`^\s*(-\s*)?uses\s*:\s*actions/setup-go`)
)

// stripYAMLComment removes a trailing YAML comment from a workflow line while
// keeping a '#' that sits inside a quoted value. A commented-out pin must not
// be read as a pin, and must not count as a step's version source either.
func stripYAMLComment(line string) string {
	var quote byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '"' || c == '\'' {
			quote = c
			continue
		}
		if c == '#' && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t') {
			return line[:i]
		}
	}
	return line
}

// workflowGoToolchainProblem returns why a workflow's Go toolchain declaration
// conflicts with go.mod, or nil when the workflow takes its toolchain from
// go.mod alone.
func workflowGoToolchainProblem(text string) error {
	lines := strings.Split(text, "\n")

	for i, raw := range lines {
		line := stripYAMLComment(raw)
		if goVersionPinRe.MatchString(line) {
			return fmt.Errorf("line %d: %q pins go-version, but actions/setup-go honours one version source: the pin overrides go-version-file and can drift from %s's go directive", i+1, strings.TrimSpace(line), goModFile)
		}
		if m := goVersionFileRe.FindStringSubmatch(line); m != nil {
			if source := strings.Trim(m[1], `"'`); source != goModFile {
				return fmt.Errorf("line %d: go-version-file: %s is not %s, so the job's toolchain can drift from the module's go directive", i+1, m[1], goModFile)
			}
		}
	}

	for i, raw := range lines {
		if !setupGoUsesRe.MatchString(stripYAMLComment(raw)) {
			continue
		}
		if !stepDeclaresGoVersionFile(lines, i) {
			return fmt.Errorf("line %d: actions/setup-go step declares no version source; add go-version-file: %s", i+1, goModFile)
		}
	}
	return nil
}

// stepDeclaresGoVersionFile reports whether the setup-go step whose `uses:` line
// is at index i declares go-version-file. The step body is the block indented at
// least as deep as that line, ending at the next step (a shallower line, or a
// `- ` entry at the same indentation).
func stepDeclaresGoVersionFile(lines []string, i int) bool {
	uses := stripYAMLComment(lines[i])
	indent := len(uses) - len(strings.TrimLeft(uses, " "))
	for _, raw := range lines[i+1:] {
		line := stripYAMLComment(raw)
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		lineIndent := len(line) - len(strings.TrimLeft(line, " "))
		if lineIndent < indent || (lineIndent == indent && strings.HasPrefix(trimmed, "- ")) {
			return false
		}
		if goVersionFileRe.MatchString(line) {
			return true
		}
	}
	return false
}

// goModGoDirective returns the MAJOR.MINOR of the `go` directive in go.mod: the
// version every workflow must take its toolchain from.
func goModGoDirective(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "go" {
			continue
		}
		parts := strings.Split(fields[1], ".")
		if len(parts) < 2 {
			return "", fmt.Errorf("%s: go directive %q is not MAJOR.MINOR", path, fields[1])
		}
		return parts[0] + "." + parts[1], nil
	}
	return "", fmt.Errorf("%s declares no go directive", path)
}
