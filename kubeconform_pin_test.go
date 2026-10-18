package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// kubeconform pin check: CI's only schema gate for manifests/ is
// `docker run --rm -v "$PWD":/workdir ghcr.io/yannh/kubeconform:<tag> -strict
// -summary /workdir/manifests` (.github/workflows/ci.yml:99). That tag decides
// whether every manifest in the repository is valid, and nothing compared it
// with anything: Dependabot's docker ecosystem scans only the repository build
// file name (`Dockerfile`), so a container image a workflow runs is never bumped
// by a dependency pull request, and no existing check read the tag -
// workflow_check.go reads setup-go inputs, the module-graph job and attestation
// permissions, and ci_check_comments.py reads setup-go inputs, tabs and GC
// tokens. The pin therefore aged indefinitely and silently: a manifest that only
// breaks under a newer Kubernetes schema (or under a validator bug fix) was never
// caught, and the stale tag looked intentional in review.
//
// The version now has one declared source, kubeconformVersion below, and the
// workflow has to agree with it:
//
//   - kubeconformVersion in this file is the declaration,
//   - every kubeconform invocation in .github/workflows/ must run exactly
//     kubeconformVersion.
//
// Bumping one of the two alone fails `go test -count=1 ./...`, which both CI and
// release.yml run before they build or publish anything, so a validator change
// is a visible edit of the declared constant and the workflow tag instead of a
// silent one, and a second workflow that pins another kubeconform tag cannot be
// added without this check naming it.
const (
	// kubeconformImage is the registry-qualified validator image CI runs. The
	// repository pulls it from GHCR, where kubeconform is published.
	kubeconformImage = "ghcr.io/yannh/kubeconform"

	// kubeconformVersion is the single source of the validator version: the tag
	// the workflows' schema gate must run. Bump it together with that tag.
	kubeconformVersion = "v0.6.7"

	// kubeconformPinnedImage is the exact reference the declaration and the
	// workflows agree on.
	kubeconformPinnedImage = kubeconformImage + ":" + kubeconformVersion
)

// kubeconformImageRe matches one kubeconform image reference and captures its
// tag when it has one: `ghcr.io/yannh/kubeconform:v0.6.7` matches with the tag,
// a reference without a tag matches with an empty capture (the version the
// manifests were validated with cannot be read from the files then), and a
// digest reference (`...@sha256:...`) is reported the same way.
var kubeconformImageRe = regexp.MustCompile(regexp.QuoteMeta(kubeconformImage) + `(?::([0-9A-Za-z._-]+))?`)

// kubeconformVersionRe matches the pin's shape: a strict semver vX.Y.Z, like the
// release tag in main_test.go. A floating `:latest` or an unreviewable digest
// cannot take the pin's place, because neither can be asserted against.
var kubeconformVersionRe = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

// kubeconformPinProblems returns one message per kubeconform invocation in the
// workflows under dir whose image tag is not kubeconformVersion, plus the number
// of invocations seen, so a caller can tell "no problems" from "no gate found,
// so nothing was checked". The gate disappearing or moving to another image must
// not read as agreement.
func kubeconformPinProblems(dir string) ([]string, int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, err
	}

	var problems []string
	invocations := 0
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
		found, count := kubeconformPinProblemsIn(path, string(data))
		problems = append(problems, found...)
		invocations += count
	}
	sort.Strings(problems)
	return problems, invocations, nil
}

// kubeconformPinProblemsIn scans one workflow's text line by line, so a failure
// names the line of the tag. A commented-out invocation is not counted: a
// commented `docker run` never reaches CI, so it neither pins nor validates
// anything.
func kubeconformPinProblemsIn(path, text string) ([]string, int) {
	var problems []string
	invocations := 0
	for i, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		for _, match := range kubeconformImageRe.FindAllStringSubmatch(line, -1) {
			invocations++
			tag := match[1]
			switch {
			case tag == kubeconformVersion:
			case tag == "":
				problems = append(problems, fmt.Sprintf(
					"%s:%d: runs %s with no version tag, so the validator version the manifests are checked with cannot be read from the files; pin %s",
					path, i+1, kubeconformImage, kubeconformPinnedImage))
			default:
				problems = append(problems, fmt.Sprintf(
					"%s:%d: runs %s:%s, but the declared validator version is %s (%s); bump the tag and the kubeconformVersion constant in the same change",
					path, i+1, kubeconformImage, tag, kubeconformVersion, kubeconformDeclarationFile))
			}
		}
	}
	return problems, invocations
}

// kubeconformDeclarationFile is where the validator version is declared, named in
// the failure messages so a stale or drifted tag points at the one place to edit.
const kubeconformDeclarationFile = "kubeconform_pin_test.go (const kubeconformVersion)"

// The reviewed ci_gap: the kubeconform tag was a hard-coded pin that nothing
// read, so the validator aged indefinitely with no signal in review. Every
// workflow's kubeconform invocation must run exactly kubeconformVersion, and the
// invocation must still be there at all: `go test -count=1 ./...` (run by CI and
// by release.yml) fails when either side is edited alone.
func TestKubeconformValidatorPinMatchesWorkflow(t *testing.T) {
	problems, invocations, err := kubeconformPinProblems(ciWorkflowDir)
	if err != nil {
		t.Fatal(err)
	}
	if invocations == 0 {
		t.Fatalf("no %s invocation found in %s: the schema gate for %s/ is gone, so a manifest that breaks the Kubernetes schema would pass CI unreported", kubeconformImage, ciWorkflowDir, manifestDir)
	}
	for _, problem := range problems {
		t.Errorf("%s", problem)
	}
}

// The pin's shape: a strict semver vX.Y.Z, so a floating tag or a digest cannot
// replace it and leave the validator version unreadable from the files.
func TestKubeconformVersionPinnedIsSemver(t *testing.T) {
	if !kubeconformVersionRe.MatchString(kubeconformVersion) {
		t.Fatalf("kubeconformVersion = %q, want vMAJOR.MINOR.PATCH: a floating or digest reference cannot be asserted against", kubeconformVersion)
	}
}

// The declaration has to cover the whole workflow set, so a second workflow that
// carries its own kubeconform tag is caught too: the scan reads every workflow
// under the directory, not just the one the gate lives in today.
func TestKubeconformPinProblems_CoversEveryWorkflowFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, image string) {
		text := "jobs:\n  go:\n    steps:\n      - name: Validate manifests\n        run: |\n          docker run --rm -v \"$PWD\":/workdir " + image + " \\\n            -strict -summary /workdir/manifests\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("ci.yml", kubeconformPinnedImage)
	write("second.yml", kubeconformImage+":v0.6.6")

	problems, invocations, err := kubeconformPinProblems(dir)
	if err != nil {
		t.Fatal(err)
	}
	if invocations != 2 {
		t.Fatalf("invocations = %d, want both workflows read", invocations)
	}
	if len(problems) != 1 || !strings.Contains(problems[0], "second.yml") {
		t.Fatalf("problems = %v, want exactly the second workflow's drift reported", problems)
	}

	if _, _, err := kubeconformPinProblems(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing workflow directory accepted, want a read error")
	}
}

// The failure modes the check has to see: a tag that drifted from the declared
// version in either direction, a floating tag, a reference with no tag, a digest
// reference, and an invocation that only lives in a comment. The shipped shape
// must pass, so the fixtures prove the edit is what was caught.
func TestKubeconformPinProblems_RegressionFixtures(t *testing.T) {
	validateStep := func(image string) string {
		return "jobs:\n" +
			"  go:\n" +
			"    steps:\n" +
			"      - name: Validate manifests\n" +
			"        run: |\n" +
			"          docker run --rm -v \"$PWD\":/workdir " + image + " \\\n" +
			"            -strict -summary /workdir/manifests\n"
	}

	tests := []struct {
		name            string
		workflow        string
		wantInvocations int
		wantProblem     string
	}{
		{
			name:            "the pin CI runs",
			workflow:        validateStep(kubeconformPinnedImage),
			wantInvocations: 1,
		},
		{
			name:            "stale tag beside the declared version",
			workflow:        validateStep(kubeconformImage + ":v0.6.6"),
			wantInvocations: 1,
			wantProblem:     "v0.6.6",
		},
		{
			name:            "newer tag beside the declared version",
			workflow:        validateStep(kubeconformImage + ":v0.7.0"),
			wantInvocations: 1,
			wantProblem:     kubeconformVersion,
		},
		{
			name:            "floating tag",
			workflow:        validateStep(kubeconformImage + ":latest"),
			wantInvocations: 1,
			wantProblem:     "latest",
		},
		{
			name:            "reference without a tag",
			workflow:        validateStep(kubeconformImage),
			wantInvocations: 1,
			wantProblem:     "no version tag",
		},
		{
			name:            "digest reference",
			workflow:        validateStep(kubeconformImage + "@sha256:0000000000000000000000000000000000000000000000000000000000000000"),
			wantInvocations: 1,
			wantProblem:     "no version tag",
		},
		{
			name:            "invocation only in a comment",
			workflow:        "jobs:\n  go:\n    steps:\n      # docker run --rm -v \"$PWD\":/workdir " + kubeconformImage + ":v0.5.0 -strict -summary /workdir/manifests\n",
			wantInvocations: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "ci.yml"), []byte(tt.workflow), 0o644); err != nil {
				t.Fatal(err)
			}

			problems, invocations, err := kubeconformPinProblems(dir)
			if err != nil {
				t.Fatal(err)
			}
			if invocations != tt.wantInvocations {
				t.Fatalf("invocations = %d, want %d", invocations, tt.wantInvocations)
			}
			if tt.wantProblem == "" {
				if len(problems) != 0 {
					t.Fatalf("problems = %v, want none", problems)
				}
				return
			}
			if len(problems) == 0 {
				t.Fatal("validator reference that disagrees with the declared version accepted, want a problem reported")
			}
			if !strings.Contains(strings.Join(problems, "\n"), tt.wantProblem) {
				t.Fatalf("problems = %v, want one naming %q", problems, tt.wantProblem)
			}
		})
	}
}

// The scan is per directory, not per first match: a drifted workflow is read
// even beside the correct one, and the caller's invocation count covers both
// files, so the check cannot pass by finding one correct invocation.
func TestKubeconformPinProblems_AcrossWorkflowFiles(t *testing.T) {
	dir := t.TempDir()
	pinned := "jobs:\n  go:\n    steps:\n      - name: Validate manifests\n        run: |\n          docker run --rm -v \"$PWD\":/workdir " + kubeconformPinnedImage + " \\\n            -strict -summary /workdir/manifests\n"
	stale := "jobs:\n  release:\n    steps:\n      - name: Validate manifests\n        run: |\n          docker run --rm -v \"$PWD\":/workdir " + kubeconformImage + ":v0.7.0 \\\n            -strict -summary /workdir/manifests\n"
	for name, text := range map[string]string{"ci.yml": pinned, "release.yml": stale} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	problems, invocations, err := kubeconformPinProblems(dir)
	if err != nil {
		t.Fatal(err)
	}
	if invocations != 2 {
		t.Fatalf("invocations = %d, want the two validator invocations", invocations)
	}
	if len(problems) != 1 {
		t.Fatalf("problems = %v, want exactly the drifted workflow reported", problems)
	}
	if !strings.Contains(problems[0], "release.yml") {
		t.Fatalf("problem %q does not name the drifted workflow", problems[0])
	}

	if _, _, err := kubeconformPinProblems(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing workflow directory accepted, want a read error")
	}
}
