package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Release tag gate: release.yml publishes whatever tag was pushed
// (`version="${GITHUB_REF_NAME}"`), and no step compared that tag with the
// release image the manifests pin, so a vX.Y.Z tag pushed from a tree still
// pinning the previous release published an image no manifest in the
// repository references. The workflow now runs release_gate.sh before the
// build, and these tests exercise that script for real: matching tags pass,
// every mismatch shape fails, and the workflow keeps both gates ahead of the
// build and the push.

// releaseGateScript is the checker release.yml runs before it builds anything.
const releaseGateScript = "release_gate.sh"

// releaseImageRepoAndTag splits the pinned release image into the
// registry-qualified name and the pinned tag, the two arguments the gate takes.
func releaseImageRepoAndTag(t *testing.T) (repo, tag string) {
	t.Helper()
	i := strings.LastIndex(releaseImage, ":")
	if i <= 0 || i == len(releaseImage)-1 {
		t.Fatalf("releaseImage = %q, want <registry>/<image>:<tag>", releaseImage)
	}
	return releaseImage[:i], releaseImage[i+1:]
}

// runReleaseGate runs the gate script with a real shell and returns its exit
// status and combined output. An empty dir means "use the script's default",
// which is the manifests directory the release workflow runs against.
func runReleaseGate(t *testing.T, dir, tag, image string) (int, string) {
	t.Helper()

	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no POSIX sh available; native CI runs the release gate in release.yml")
	}

	args := []string{releaseGateScript, tag, image}
	if dir != "" {
		args = append(args, dir)
	}
	out, err := exec.Command(shell, args...).CombinedOutput()
	if err == nil {
		return 0, string(out)
	}

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("run %s: %v\n%s", releaseGateScript, err, out)
	}
	return exitErr.ExitCode(), string(out)
}

// The shipped state must pass: the release tag the repository pins is exactly
// the image both example manifests deploy.
func TestReleaseGateAcceptsShippedPin(t *testing.T) {
	repo, tag := releaseImageRepoAndTag(t)

	code, out := runReleaseGate(t, "", tag, repo)
	if code != 0 {
		t.Fatalf("release gate rejected the shipped pin (exit %d):\n%s", code, out)
	}
	if !strings.Contains(out, releaseImage) {
		t.Errorf("output %q does not name the pinned image %q", out, releaseImage)
	}
	if !strings.Contains(out, "2 manifest reference(s)") {
		t.Errorf("output %q does not report the two manifest pins: a gate that found no pin must not pass", out)
	}
}

// The reviewed failure mode: the tag being released differs from the pinned
// image. The gate must fail, and it must name both references so the release
// log says what to bump.
func TestReleaseGateRejectsTagBesideStalePin(t *testing.T) {
	repo, pinnedTag := releaseImageRepoAndTag(t)

	code, out := runReleaseGate(t, "", "v9.9.9", repo)
	if code == 0 {
		t.Fatalf("release gate accepted v9.9.9 while the manifests pin %s:\n%s", releaseImage, out)
	}
	for _, want := range []string{"v9.9.9", releaseImage, pinnedTag} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q does not name %q", out, want)
		}
	}
}

// The fixture shapes around the shipped manifests: a manifest set is pinned
// once, so a matching pin passes while every other shape the gate can meet
// fails instead of publishing an unreferenced image.
func TestReleaseGateManifestFixtures(t *testing.T) {
	const (
		image = "ghcr.io/example/app"
		tag   = "v0.2.0"
	)

	tests := []struct {
		name       string
		files      map[string]string
		tag        string
		image      string
		noDir      bool
		wantExit   int
		wantOutput string
	}{
		{
			name:       "matching pin",
			files:      map[string]string{"deployment.yaml": image + ":" + tag},
			tag:        tag,
			image:      image,
			wantOutput: image + ":" + tag,
		},
		{
			name:       "pin on an older release",
			files:      map[string]string{"deployment.yaml": image + ":v0.1.0"},
			tag:        tag,
			image:      image,
			wantExit:   1,
			wantOutput: "does not match the pinned release image",
		},
		{
			name:       "manifests pin two different tags",
			files:      map[string]string{"a.yaml": image + ":v0.1.0", "b.yaml": image + ":" + tag},
			tag:        tag,
			image:      image,
			wantExit:   1,
			wantOutput: image + ":v0.1.0",
		},
		{
			name:       "no manifest references the image",
			files:      map[string]string{"deployment.yaml": "redis:7-alpine"},
			tag:        tag,
			image:      image,
			wantExit:   1,
			wantOutput: "nothing deploys",
		},
		{
			name:       "a floating tag is not the pin",
			files:      map[string]string{"deployment.yaml": image + ":latest"},
			tag:        tag,
			image:      image,
			wantExit:   1,
			wantOutput: "does not match the pinned release image",
		},
		{
			name:       "a digest reference is not the pin",
			files:      map[string]string{"deployment.yaml": image + "@sha256:0000000000000000000000000000000000000000000000000000000000000000"},
			tag:        tag,
			image:      image,
			wantExit:   1,
			wantOutput: "nothing deploys",
		},
		{
			name:       "pin only in a comment",
			files:      map[string]string{"deployment.yaml": "# image: " + image + ":" + tag},
			tag:        tag,
			image:      image,
			wantExit:   1,
			wantOutput: "nothing deploys",
		},
		{
			name:       "quoted pin",
			files:      map[string]string{"deployment.yaml": `"` + image + ":" + tag + `"`},
			tag:        tag,
			image:      image,
			wantOutput: image + ":" + tag,
		},
		{
			name:       "tag is not semver",
			files:      map[string]string{"deployment.yaml": image + ":" + tag},
			tag:        "latest",
			image:      image,
			wantExit:   1,
			wantOutput: "is not vMAJOR.MINOR.PATCH",
		},
		{
			name:     "expected image carries a tag",
			files:    map[string]string{"deployment.yaml": image + ":" + tag},
			tag:      tag,
			image:    image + ":" + tag,
			wantExit: 2,
		},
		{
			name:       "manifest directory does not exist",
			tag:        tag,
			image:      image,
			noDir:      true,
			wantExit:   1,
			wantOutput: "does not exist",
		},
		{
			name:       "manifest directory holds no manifests",
			files:      map[string]string{},
			tag:        tag,
			image:      image,
			wantExit:   1,
			wantOutput: "holds no YAML manifests",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "manifests")
			if !tt.noDir {
				if err := os.Mkdir(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			for name, ref := range tt.files {
				writeReleaseManifest(t, dir, name, ref)
			}

			code, out := runReleaseGate(t, dir, tt.tag, tt.image)
			if code != tt.wantExit {
				t.Fatalf("exit = %d, want %d:\n%s", code, tt.wantExit, out)
			}
			if tt.wantOutput != "" && !strings.Contains(out, tt.wantOutput) {
				t.Fatalf("output %q does not contain %q", out, tt.wantOutput)
			}
		})
	}
}

// writeReleaseManifest writes one minimal manifest whose container declares the
// given image reference, so the fixture edits nothing but the pin the gate
// reads.
func writeReleaseManifest(t *testing.T, dir, name, imageRef string) {
	t.Helper()
	text := "apiVersion: apps/v1\n" +
		"kind: Deployment\n" +
		"spec:\n" +
		"  template:\n" +
		"    spec:\n" +
		"      containers:\n" +
		"      - name: app\n" +
		"        image: " + imageRef + "\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The gate only guards releases while release.yml runs it, so the workflow
// itself is asserted: both gates sit ahead of the build and the push, the
// ancestry check fetches main and refuses a tag outside it, and the checkout is
// deep enough to answer the ancestry question.
func TestReleaseWorkflowGatesTagBeforePublishing(t *testing.T) {
	path := filepath.Join(ciWorkflowDir, "release.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	lines := strings.Split(text, "\n")

	lineOf := func(needle string) int {
		for i, line := range lines {
			if strings.Contains(line, needle) {
				return i + 1
			}
		}
		return -1
	}

	gate := lineOf("sh " + releaseGateScript)
	if gate < 0 {
		t.Fatalf("%s never runs %s: a vX.Y.Z tag would publish whatever the tag says, whatever the manifests pin", path, releaseGateScript)
	}
	if !strings.Contains(lines[gate-1], "GITHUB_REF_NAME") {
		t.Errorf("%s:%d does not pass the pushed tag (%s) to %s", path, gate, "GITHUB_REF_NAME", releaseGateScript)
	}
	if !strings.Contains(lines[gate-1], "steps.image.outputs.image") {
		t.Errorf("%s:%d does not pass the published image name to %s, so the gate cannot tell which image the manifests must pin", path, gate, releaseGateScript)
	}

	ancestor := lineOf("merge-base --is-ancestor")
	if ancestor < 0 {
		t.Fatalf("%s does not check that the tagged commit is reachable from main: a tag pushed from an unmerged branch would skip CI and still publish", path)
	}
	if lineOf("git fetch --no-tags origin main") < 0 {
		t.Errorf("%s checks ancestry but never fetches main, so the check cannot see main", path)
	}

	build := lineOf("name: Build image")
	if build < 0 {
		t.Fatalf("%s: no 'Build image' step found; the gate must run before the image is built", path)
	}
	push := lineOf("uses: " + attestationActionStr)
	if push < 0 {
		t.Fatalf("%s: no %s step found; the gate must run before the image is pushed", path, attestationActionStr)
	}
	if gate > build || ancestor > build {
		t.Errorf("%s: gate lines (tag=%d, ancestry=%d) must precede the build (line %d)", path, gate, ancestor, build)
	}
	if gate > push || ancestor > push {
		t.Errorf("%s: gate lines (tag=%d, ancestry=%d) must precede the push (line %d)", path, gate, ancestor, push)
	}

	// The ancestry check needs history, and the tag checkout is otherwise
	// shallow.
	checkout := lineOf("uses: actions/checkout@")
	if checkout < 0 {
		t.Fatalf("%s: no checkout step found", path)
	}
	found := false
	for _, line := range lines[checkout:] {
		if strings.HasPrefix(line, "      - ") {
			break
		}
		if strings.TrimSpace(line) == "fetch-depth: 0" {
			found = true
		}
	}
	if !found {
		t.Errorf("%s: the checkout does not set fetch-depth: 0, so the ancestry check cannot see main's history", path)
	}
}

// The gate must stay runnable where the workflow runs it: the script exists, is
// a POSIX sh script, and refuses an unusable command line instead of passing.
func TestReleaseGateScriptContract(t *testing.T) {
	_, tag := releaseImageRepoAndTag(t)

	info, err := os.Stat(releaseGateScript)
	if err != nil {
		t.Fatalf("%s: %v", releaseGateScript, err)
	}
	if info.IsDir() {
		t.Fatalf("%s is a directory", releaseGateScript)
	}

	data, err := os.ReadFile(releaseGateScript)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), "#!/bin/sh") {
		t.Errorf("%s does not declare a POSIX sh shebang", releaseGateScript)
	}
	if strings.Contains(string(data), "\t") {
		t.Errorf("%s contains a tab character", releaseGateScript)
	}

	if code, out := runReleaseGate(t, "", tag, ""); code != 2 {
		t.Errorf("release gate with no expected image exited %d, want 2:\n%s", code, out)
	}

	code, out := runReleaseGate(t, "", "1.2.3", "ghcr.io/example/app")
	if code != 1 {
		t.Errorf("release gate with tag 1.2.3 exited %d, want 1:\n%s", code, out)
	}
	if !strings.Contains(out, "vMAJOR.MINOR.PATCH") {
		t.Errorf("output %q does not explain the tag shape it wants", out)
	}
}
