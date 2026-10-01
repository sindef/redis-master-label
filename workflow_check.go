package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Go toolchain single-source check: actions/setup-go honours one version
// source. With both `go-version` and `go-version-file` set it warns that the
// file is ignored, so the explicit pin — not go.mod's `go` directive — decided
// the job's toolchain, and it could drift from the version the code actually
// declares. Any setup-go step must therefore declare go-version-file: go.mod
// and no go-version pin. Comments are not pins.

const (
	workflowSetupGoUses     = "uses: actions/setup-go"
	workflowGoVersionKey    = "go-version:"
	workflowGoVersionFileK  = "go-version-file:"
	workflowWantVersionFile = "go.mod"
)

// workflowToolchainProblems scans every workflow under .github/workflows and
// reports setup-go declarations that conflict with go.mod as the single
// toolchain source.
func workflowToolchainProblems() []string {
	return workflowDirToolchainProblems(".github/workflows", workflowWantVersionFile)
}

// goModGoDirective returns the raw `go` directive value, or "" when missing.
func goModGoDirective(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(noInlineComment(line))
		if len(fields) >= 2 && fields[0] == "go" && !strings.Contains(line, "module") {
			return fields[1]
		}
	}
	return ""
}

// workflowDirToolchainProblems scans every *.yml/*.yaml file in dir; fixtures
// pass a temp dir directly.
func workflowDirToolchainProblems(dir, wantVersionFile string) []string {
	var problems []string
	for _, pattern := range []string{"*.yml", "*.yaml"} {
		matches, _ := filepath.Glob(filepath.Join(dir, pattern))
		for _, path := range matches {
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			problems = append(problems, setupGoStepProblems(path, string(data), wantVersionFile)...)
		}
	}
	return problems
}

// setupGoStepProblems inspects one workflow file's setup-go steps. Each step
// is covered by `- uses:`/`- name:` bullets; the `with:` block is indented
// deeper, so any line back at or above the bullet's column ends the step.
// A commented-out pin (`# go-version: "1.23"`) is not a pin.
func setupGoStepProblems(path, content, wantVersionFile string) []string {
	var problems []string
	lines := strings.Split(content, "\n")
	for i, raw := range lines {
		trimmed := strings.TrimSpace(noInlineComment(raw))
		if !strings.Contains(trimmed, workflowSetupGoUses) {
			continue
		}
		usesIndent := len(raw) - len(strings.TrimLeft(raw, " \t"))

		var sawVersionSource bool
		for j := i + 1; j < len(lines); j++ {
			innerTrim := strings.TrimSpace(noInlineComment(lines[j]))
			thisIndent := len(lines[j]) - len(strings.TrimLeft(lines[j], " \t"))
			if innerTrim == "" {
				continue
			}
			if thisIndent <= usesIndent {
				break
			}
			if v, ok := hasValue(innerTrim, workflowGoVersionFileK); ok {
				sawVersionSource = true
				if v != wantVersionFile {
					problems = append(problems, fmt.Sprintf("%s: setup-go declares go-version-file %q, want %q", path, v, wantVersionFile))
				}
			}
			if v, ok := hasValue(innerTrim, workflowGoVersionKey); ok {
				problems = append(problems, fmt.Sprintf("%s: setup-go declares go-version %q; use only go-version-file: %s so go.mod stays the single toolchain declaration", path, v, wantVersionFile))
				sawVersionSource = true
			}
		}
		if !sawVersionSource {
			problems = append(problems, fmt.Sprintf("%s: setup-go step declares no Go version source (go-version-file: %s required)", path, wantVersionFile))
		}
	}
	return problems
}

// hasValue returns the scalar value after `key ` (quoted or bare); a trailing
// comment was already stripped by noInlineComment.
func hasValue(trimmed, key string) (string, bool) {
	idx := strings.Index(trimmed, key)
	if idx < 0 {
		return "", false
	}
	rest := strings.TrimSpace(trimmed[idx+len(key):])
	if rest == "" {
		return "", false
	}
	rest = strings.Trim(rest, "\"'")
	return rest, true
}

// noInlineComment strips a first `#` that is not inside quotes, so
// `# go-version: "1.23"` (and any other commented pin) carries no value.
func noInlineComment(line string) string {
	inQuote := byte(0)
	for i := 0; i < len(line); i++ {
		c := line[i]
		if inQuote != 0 {
			if c == inQuote {
				inQuote = 0
			}
			continue
		}
		if c == '"' || c == '\'' {
			inQuote = c
			continue
		}
		if c == '#' {
			return strings.TrimSpace(line[:i])
		}
	}
	return strings.TrimSpace(line)
}
