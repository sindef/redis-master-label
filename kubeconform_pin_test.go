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

	// kubeconformSchemaVersion is the Kubernetes schema series the manifest gate
	// validates against, written as the MAJOR.MINOR.PATCH release whose schemas
	// the manifests are checked with. It is a declaration of its own because
	// kubeconform validates against whatever schema set its release bundles when
	// -kubernetes-version is absent, and that default has no connection to the
	// Kubernetes API this binary is compiled against: go.mod requires
	// k8s.io/api v0.37.1, so the code is written against a newer API than an
	// older release's default schema. The workflow passes this value as
	// -kubernetes-version, and its MAJOR.MINOR must equal go.mod's k8s.io/api
	// MAJOR.MINOR (v0.Y.Z maps to Kubernetes 1.Y), so bumping either side alone
	// fails `go test -count=1 ./...` instead of silently validating the manifests
	// against a schema older than the API the binary uses.
	kubeconformSchemaVersion = "1.37.1"
)

// kubeconformSchemaDeclarationFile names the declaration in failure messages, so
// a drifted workflow flag or a stale schema points at the one place to edit.
const kubeconformSchemaDeclarationFile = "kubeconform_pin_test.go (const kubeconformSchemaVersion)"

// kubeconformSchemaVersionFlag is the kubeconform flag that selects the schema
// series. Without it the validator uses its bundled default, which nothing here
// can compare with anything.
const kubeconformSchemaVersionFlag = "-kubernetes-version"

// kubernetesAPIModule is the module whose go.mod requirement declares the
// Kubernetes API the binary is written and compiled against, and therefore the
// schema series the manifests must be validated against.
const kubernetesAPIModule = "k8s.io/api"

// kubeconformSchemaFlagRe matches the schema flag in an invocation and captures
// its value, accepting both `-kubernetes-version=1.37.1` and
// `-kubernetes-version 1.37.1` (the Go flag package accepts both). The value
// stops at whitespace, a backslash or a quote, so a trailing shell continuation
// is not read as part of the version.
var kubeconformSchemaFlagRe = regexp.MustCompile(`-{1,2}kubernetes-version(?:=|[ \t]+)([^ \t\\"']+)`)

// numericFieldRe matches one all-digit version field, so a value like
// `master` or `latest` cannot be read as a schema series.
var numericFieldRe = regexp.MustCompile(`^[0-9]+$`)

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
// kubeAPIModuleVersion returns the version go.mod requires for kubernetesAPIModule,
// for example "v0.37.1" for `k8s.io/api v0.37.1`. The requirement is the API the
// code is written and compiled against, so it is the source the declared schema
// version is compared with; a missing requirement is an error instead of an
// empty string, because "nothing declared" must not read as agreement.
func kubeAPIModuleVersion(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		for i, field := range fields {
			if field != kubernetesAPIModule {
				continue
			}
			if i+1 >= len(fields) {
				return "", fmt.Errorf("%s names %s with no version", path, kubernetesAPIModule)
			}
			if !strings.HasPrefix(fields[i+1], "v") {
				return "", fmt.Errorf("%s requires %s %q, want a vMAJOR.MINOR.PATCH module version", path, kubernetesAPIModule, fields[i+1])
			}
			return fields[i+1], nil
		}
	}
	return "", fmt.Errorf("%s declares no requirement on %s, so the Kubernetes API the binary compiles against cannot be read from the module file", path, kubernetesAPIModule)
}

// versionMajorMinor returns the MAJOR.MINOR of a dotted version, dropping one
// leading `v` and any suffix after the patch field. A version without at least
// two numeric fields is rejected, so a floating tag cannot stand in for a
// declared schema series.
func versionMajorMinor(version string) (string, error) {
	trimmed := strings.TrimPrefix(strings.TrimSpace(version), "v")
	parts := strings.SplitN(trimmed, ".", 3)
	if len(parts) < 2 || !numericFieldRe.MatchString(parts[0]) || !numericFieldRe.MatchString(parts[1]) {
		return "", fmt.Errorf("%q is not a MAJOR.MINOR[.PATCH] version", version)
	}
	return parts[0] + "." + parts[1], nil
}

// kubeAPISchemaMinor maps a k8s.io/* module version to the Kubernetes schema
// series it corresponds to: the k8s.io modules version as v0.Y.Z, and Kubernetes
// releases as 1.Y.Z, so v0.37.1 is Kubernetes 1.37. A module major above zero
// already carries the Kubernetes major.
func kubeAPISchemaMinor(moduleVersion string) (string, error) {
	minor, err := versionMajorMinor(moduleVersion)
	if err != nil {
		return "", fmt.Errorf("%s %q: %v", kubernetesAPIModule, moduleVersion, err)
	}
	parts := strings.SplitN(minor, ".", 2)
	if parts[0] == "0" {
		return "1." + parts[1], nil
	}
	return minor, nil
}

// kubeSchemaVersionProblem returns why the declared validator schema series does
// not match the Kubernetes API go.mod requires, or nil when the two agree. This
// is the comparison the reviewed ci_gap was missing: the validator's schema set
// and the compiled API were never tied together, so a Dependabot bump of
// k8s.io/* landed alone while the manifests kept being validated against older
// schemas, and a schema bump landed alone while the binary kept using a newer
// API.
func kubeSchemaVersionProblem(schemaVersion, moduleVersion string) error {
	want, err := kubeAPISchemaMinor(moduleVersion)
	if err != nil {
		return err
	}
	got, err := versionMajorMinor(schemaVersion)
	if err != nil {
		return fmt.Errorf("%s %q: %v", kubeconformSchemaDeclarationFile, schemaVersion, err)
	}
	if got != want {
		return fmt.Errorf("declared validator schema version %q (Kubernetes %s) does not match %s: %s requires %s (Kubernetes %s); bump %s and the %s value in the workflow in the same change, so the manifests are never validated against a schema older than the API the binary compiles against",
			schemaVersion, got, goToolchainFile, kubernetesAPIModule, moduleVersion, want, kubeconformSchemaDeclarationFile, kubeconformSchemaVersionFlag)
	}
	return nil
}

// kubeconformCommand returns the shell command starting on the given line, with
// its backslash continuations joined, so a flag written on the next line still
// counts as part of the invocation. A command ends at the first line that does
// not end in a backslash.
func kubeconformCommand(lines []string, start int) string {
	var command strings.Builder
	for i := start; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		continues := strings.HasSuffix(line, "\\")
		command.WriteString(strings.TrimSpace(strings.TrimSuffix(line, "\\")))
		command.WriteString(" ")
		if !continues {
			break
		}
	}
	return command.String()
}

// kubeconformSchemaFlagProblemsIn returns one problem per kubeconform invocation
// in one workflow's text that does not pass kubeconformSchemaVersionFlag=
// kubeconformSchemaVersion, plus the number of invocations seen so a caller can
// tell "no problems" from "no gate found". A commented-out invocation is skipped
// because it neither validates nor pins anything. A missing flag is a problem on
// its own: kubeconform then validates against the schema set its release bundles,
// which is exactly the undeclared default this check exists to remove.
func kubeconformSchemaFlagProblemsIn(path, text string) ([]string, int) {
	lines := strings.Split(text, "\n")
	var problems []string
	invocations := 0
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if !kubeconformImageRe.MatchString(line) {
			continue
		}
		invocations++
		matches := kubeconformSchemaFlagRe.FindAllStringSubmatch(kubeconformCommand(lines, i), -1)
		if len(matches) == 0 {
			problems = append(problems, fmt.Sprintf(
				"%s:%d: runs %s without %s, so the manifests are validated against whatever schema set that validator release bundles and nothing compares it with %s; pass %s=%s",
				path, i+1, kubeconformImage, kubeconformSchemaVersionFlag, kubernetesAPIModule, kubeconformSchemaVersionFlag, kubeconformSchemaVersion))
			continue
		}
		for _, match := range matches {
			value := strings.Trim(match[1], `"'`)
			if value == kubeconformSchemaVersion {
				continue
			}
			problems = append(problems, fmt.Sprintf(
				"%s:%d: runs %s with %s=%s, but the declared schema version is %s (%s); bump both in the same change",
				path, i+1, kubeconformImage, kubeconformSchemaVersionFlag, value, kubeconformSchemaVersion, kubeconformSchemaDeclarationFile))
		}
	}
	return problems, invocations
}

// kubeconformSchemaFlagProblems scans every workflow under dir, so a second
// workflow that validates manifests with another schema version is caught too,
// and returns the total invocation count with the problems sorted.
func kubeconformSchemaFlagProblems(dir string) ([]string, int, error) {
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
		found, count := kubeconformSchemaFlagProblemsIn(path, string(data))
		problems = append(problems, found...)
		invocations += count
	}
	sort.Strings(problems)
	return problems, invocations, nil
}

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

// The reviewed ci_gap: CI's manifest gate ran kubeconform with no
// -kubernetes-version, so the schema set was whatever that validator release
// bundles, while go.mod compiles against k8s.io/api v0.37.1. Nothing compared
// the two: a k8s.io/* bump changed the API alone, and a validator bump could
// relax the gate alone. The declared schema version and the workflow flag have to
// agree with each other and with go.mod's k8s.io/api MAJOR.MINOR, so either edit
// alone fails `go test -count=1 ./...`.
func TestKubeconformSchemaVersionMatchesKubernetesAPI(t *testing.T) {
	moduleVersion, err := kubeAPIModuleVersion(goToolchainFile)
	if err != nil {
		t.Fatalf("%v: the schema gate cannot be compared with the API the binary compiles against", err)
	}
	if err := kubeSchemaVersionProblem(kubeconformSchemaVersion, moduleVersion); err != nil {
		t.Errorf("%v", err)
	}
}

// The declaration has to be a fixed release series, so a floating value cannot
// replace it and leave "which schema the manifests were validated against"
// unreadable from the files.
func TestKubeconformSchemaVersionPinnedIsSemver(t *testing.T) {
	if _, err := versionMajorMinor(kubeconformSchemaVersion); err != nil {
		t.Fatalf("%s = %q: %v", kubeconformSchemaDeclarationFile, kubeconformSchemaVersion, err)
	}
}

// Every kubeconform invocation in the shipped workflows must pass the declared
// schema version, and the gate must still exist at all: a removed step or a
// missing flag must not read as agreement.
func TestKubeconformWorkflowDeclaresSchemaVersion(t *testing.T) {
	problems, invocations, err := kubeconformSchemaFlagProblems(ciWorkflowDir)
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

// The failure modes the comparison has to see: the module bumped alone, the
// declared schema bumped alone, and values that are not version series at all.
// The shipped pair must pass, so the fixtures prove the edit is what was caught.
func TestKubeSchemaVersionProblem_RegressionFixtures(t *testing.T) {
	tests := []struct {
		name          string
		schemaVersion string
		moduleVersion string
		wantProblem   string
	}{
		{
			name:          "the declared pair the repository ships",
			schemaVersion: "1.37.1",
			moduleVersion: "v0.37.1",
		},
		{
			name:          "module bumped alone",
			schemaVersion: "1.37.1",
			moduleVersion: "v0.38.1",
			wantProblem:   "Kubernetes 1.38",
		},
		{
			name:          "schema bumped alone",
			schemaVersion: "1.36.1",
			moduleVersion: "v0.37.1",
			wantProblem:   "Kubernetes 1.36",
		},
		{
			name:          "schema newer than the module",
			schemaVersion: "1.38.1",
			moduleVersion: "v0.37.1",
			wantProblem:   kubeconformSchemaDeclarationFile,
		},
		{
			name:          "module major already carries the Kubernetes release",
			schemaVersion: "2.1.0",
			moduleVersion: "v2.1.4",
		},
		{
			name:          "floating schema version",
			schemaVersion: "latest",
			moduleVersion: "v0.37.1",
			wantProblem:   "not a MAJOR.MINOR",
		},
		{
			name:          "schema series without a patch field is readable",
			schemaVersion: "1.37",
			moduleVersion: "v0.37.1",
		},
		{
			name:          "floating module version",
			schemaVersion: "1.37.1",
			moduleVersion: "master",
			wantProblem:   kubernetesAPIModule,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := kubeSchemaVersionProblem(tt.schemaVersion, tt.moduleVersion)
			if tt.wantProblem == "" {
				if err != nil {
					t.Fatalf("agreeing versions rejected: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("schema %s beside %s %s accepted, want a problem reported", tt.schemaVersion, kubernetesAPIModule, tt.moduleVersion)
			}
			if !strings.Contains(err.Error(), tt.wantProblem) {
				t.Fatalf("problem = %q, want one naming %q", err, tt.wantProblem)
			}
		})
	}

	// The go.mod reader itself: a requirement that is not there must fail closed
	// rather than compare against an empty version.
	dir := t.TempDir()
	if _, err := kubeAPIModuleVersion(filepath.Join(dir, "go.mod")); err == nil {
		t.Error("missing module file accepted, want a read error")
	}
	noAPI := filepath.Join(dir, "without-api.mod")
	if err := os.WriteFile(noAPI, []byte("module example.com/x\n\ngo 1.27.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := kubeAPIModuleVersion(noAPI); err == nil {
		t.Errorf("%s accepted with no %s requirement, want an error", noAPI, kubernetesAPIModule)
	}
}

// The workflow shapes the flag scan has to see: the shipped continuation-line
// invocation, both flag spellings, a missing flag, a drifted value, a flag that
// only lives in a comment, and a commented-out invocation. A missing workflow
// directory must fail instead of scanning nothing.
func TestKubeconformSchemaFlagProblems_RegressionFixtures(t *testing.T) {
	step := func(flag string) string {
		return "jobs:\n" +
			"  go:\n" +
			"    steps:\n" +
			"      - name: Validate manifests\n" +
			"        run: |\n" +
			"          docker run --rm -v \"$PWD\":/workdir " + kubeconformPinnedImage + " \\\n" +
			"            " + flag + "\n" +
			"            -strict -summary /workdir/manifests\n"
	}

	tests := []struct {
		name            string
		workflow        string
		wantInvocations int
		wantProblem     string
	}{
		{
			name:            "the declared flag CI passes",
			workflow:        step(kubeconformSchemaVersionFlag + "=" + kubeconformSchemaVersion),
			wantInvocations: 1,
		},
		{
			name:            "space separated flag form",
			workflow:        step(kubeconformSchemaVersionFlag + " " + kubeconformSchemaVersion),
			wantInvocations: 1,
		},
		{
			name:            "no schema flag, so the validator's default decides",
			workflow:        step("-strict -summary /workdir/manifests"),
			wantInvocations: 1,
			wantProblem:     "without -kubernetes-version",
		},
		{
			name:            "stale schema version",
			workflow:        step(kubeconformSchemaVersionFlag + "=1.36.1"),
			wantInvocations: 1,
			wantProblem:     "1.36.1",
		},
		{
			name:            "floating schema version",
			workflow:        step(kubeconformSchemaVersionFlag + "=latest"),
			wantInvocations: 1,
			wantProblem:     "latest",
		},
		{
			name: "flag only in a comment beside the invocation",
			workflow: "jobs:\n  go:\n    steps:\n      - name: Validate manifests\n" +
				"        run: |\n" +
				"          # " + kubeconformSchemaVersionFlag + "=" + kubeconformSchemaVersion + "\n" +
				"          docker run --rm -v \"$PWD\":/workdir " + kubeconformPinnedImage + " -strict -summary /workdir/manifests\n",
			wantInvocations: 1,
			wantProblem:     "without -kubernetes-version",
		},
		{
			name:            "invocation only in a comment",
			workflow:        "jobs:\n  go:\n    steps:\n      # docker run --rm " + kubeconformPinnedImage + " " + kubeconformSchemaVersionFlag + "=1.36.1 -strict /workdir/manifests\n",
			wantInvocations: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "ci.yml"), []byte(tt.workflow), 0o644); err != nil {
				t.Fatal(err)
			}

			problems, invocations, err := kubeconformSchemaFlagProblems(dir)
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
				t.Fatalf("invocation without %q accepted, want a problem reported", tt.wantProblem)
			}
			if !strings.Contains(strings.Join(problems, "\n"), tt.wantProblem) {
				t.Fatalf("problems = %v, want one naming %q", problems, tt.wantProblem)
			}
		})
	}

	if _, _, err := kubeconformSchemaFlagProblems(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing workflow directory accepted, want a read error")
	}
}

// A drifted workflow is read even beside the correct one, and the invocation
// count covers both files, so the check cannot pass by finding one correct
// invocation.
func TestKubeconformSchemaFlagProblems_AcrossWorkflowFiles(t *testing.T) {
	dir := t.TempDir()
	pinned := "jobs:\n  go:\n    steps:\n      - name: Validate manifests\n        run: |\n          docker run --rm -v \"$PWD\":/workdir " + kubeconformPinnedImage + " \\\n            " + kubeconformSchemaVersionFlag + "=" + kubeconformSchemaVersion + "\n            -strict -summary /workdir/manifests\n"
	stale := "jobs:\n  go:\n    steps:\n      - name: Validate manifests\n        run: |\n          docker run --rm -v \"$PWD\":/workdir " + kubeconformPinnedImage + " \\\n            " + kubeconformSchemaVersionFlag + "=1.35.0\n            -strict -summary /workdir/manifests\n"
	for name, text := range map[string]string{"ci.yml": pinned, "release.yml": stale} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	problems, invocations, err := kubeconformSchemaFlagProblems(dir)
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
}
