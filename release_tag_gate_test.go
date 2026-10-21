package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The reviewed release_hygiene defect: release.yml derived the published
// reference from the tag alone, so a v0.2.0 tag pushed while `releaseImage` and
// the manifests still pinned v0.1.0 passed vet and the full test suite (the pin
// is self-consistent) and published an image no manifest references. The gates
// that close it must exist in the shipped workflow, and they have to run before
// anything is built or published.
func TestReleaseWorkflowGatesTagOnPinnedImage(t *testing.T) {
	path := filepath.Join(ciWorkflowDir, releaseWorkflowFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	problems, gates := releaseTagGateProblems(path, string(data))
	if gates != 2 {
		t.Fatalf("release gates found in %s = %d, want the tag/pin gate and the reachability gate: without them a tag decides what is published while main_test.go and manifests/ decide what is deployed", path, gates)
	}
	for _, problem := range problems {
		t.Errorf("%s", problem)
	}
}

// The gate extracts the pin with a sed expression over main_test.go's
// declaration, so the declaration has to keep the shape that expression reads.
// A `releaseImage` moved into a const block would leave the gate comparing the
// tag with an empty pin - or, worse, with nothing at all - and no test would
// notice until a release ran.
func TestReleaseImageDeclaredWhereReleaseGateReadsIt(t *testing.T) {
	data, err := os.ReadFile(releaseImageFile)
	if err != nil {
		t.Fatalf("read %s: %v", releaseImageFile, err)
	}

	matches := releaseImageDeclarationRe.FindAllStringSubmatch(string(data), -1)
	if len(matches) != 1 {
		t.Fatalf("%s: %d lines match the release gate's `%s\"...\"` pattern, want exactly 1: the gate reads the pin with `sed -n 's/^%s\"\\([^\"]*\\)\"$/\\1/p'`, so the declaration must keep that shape",
			releaseImageFile, len(matches), releaseImageDeclPrefix, releaseImageDeclPrefix)
	}

	pinned := matches[0][1]
	if pinned != releaseImage {
		t.Fatalf("the declaration the release gate reads is %q, but releaseImage is %q: the gate would compare the tag with the wrong pin", pinned, releaseImage)
	}

	// `${pinned##*:}` in the workflow is the text after the last ':'; the pin has
	// to carry it, and it has to be the reference the manifests deploy.
	if tag := releaseTagFromImage(pinned); !strings.HasPrefix(tag, "v") {
		t.Fatalf("tag %q of the pinned image %q: the gate compares GITHUB_REF_NAME (a vX.Y.Z tag) with this, so the pin must end in its tag", tag, pinned)
	}

	images, sidecars, err := sidecarImages(manifestDir)
	if err != nil {
		t.Fatal(err)
	}
	if sidecars == 0 {
		t.Fatalf("no sidecar container found in %s: the release gate's manifest comparison ran against nothing", manifestDir)
	}
	for _, image := range images {
		if image != pinned {
			t.Errorf("a manifest pins %q while the release gate compares the tag with %q: a release would refuse to publish the image that manifest deploys", image, pinned)
		}
	}
}

// The gate shapes the check has to see. The reviewed pre-fix workflow (no gate
// at all), a gate placed after the build, a gate that leaves the clone shallow,
// and a workflow that compares the tag with the pin but never checks that the
// commit merged must each be reported; the shipped shape must pass.
func TestReleaseTagGateProblems_RegressionFixtures(t *testing.T) {
	const tagGate = `      - name: Verify the tag matches the pinned release image
        shell: bash
        run: |
          tag="${GITHUB_REF_NAME}"
          pinned=$(sed -n 's/^const releaseImage = "\([^"]*\)"$/\1/p' main_test.go)
          if [ -z "$pinned" ]; then
            exit 1
          fi
          pinned_tag="${pinned##*:}"
          if [ "$tag" != "$pinned_tag" ]; then
            echo "tag ${tag} is not ${pinned_tag}" >&2
            exit 1
          fi
          refs=$(grep -rhoE 'image:[[:space:]]+[^[:space:]]*redis-master-label[^[:space:]]*' manifests/ || true)
          if [ "$refs" != "$pinned" ]; then
            echo "manifests/ and releaseImage disagree" >&2
            exit 1
          fi
`
	const mainGate = `      - name: Verify the tag is reachable from main
        shell: bash
        run: |
          base=refs/remotes/origin/main
          git fetch --no-tags origin "+refs/heads/main:${base}"
          if ! git merge-base --is-ancestor HEAD "$base"; then
            echo "not reachable from main" >&2
            exit 1
          fi
`
	const buildStep = `      - name: Build image
        run: |
          version="${GITHUB_REF_NAME}"
          docker build --build-arg VERSION="$version" -t "example:${version}" .
`
	const fullHistoryCheckout = `      - uses: actions/checkout@v7
        with:
          fetch-depth: 0
`
	const shallowCheckout = "      - uses: actions/checkout@v7\n"

	workflow := func(checkout string, steps ...string) string {
		return "name: Release\non:\n  push:\n    tags:\n      - 'v*.*.*'\n\njobs:\n  build-and-publish:\n    runs-on: ubuntu-latest\n    steps:\n" +
			checkout + strings.Join(steps, "")
	}

	tests := []struct {
		name      string
		workflow  string
		wantGates int
		wantIn    []string
	}{
		{
			name:      "reviewed pre-fix shape: tag decides what is published",
			workflow:  workflow(shallowCheckout, buildStep),
			wantGates: 0,
			wantIn:    []string{releaseImageDeclPrefix, "reachable from main"},
		},
		{
			name:      "gates after the build step",
			workflow:  workflow(fullHistoryCheckout, buildStep, tagGate, mainGate),
			wantGates: 2,
			wantIn:    []string{"after the step that builds or publishes the image"},
		},
		{
			name:      "full history not requested",
			workflow:  workflow(shallowCheckout, tagGate, mainGate, buildStep),
			wantGates: 2,
			wantIn:    []string{fetchDepthInput},
		},
		{
			name:      "tag compared with the pin but no reachability gate",
			workflow:  workflow(fullHistoryCheckout, tagGate, buildStep),
			wantGates: 1,
			wantIn:    []string{"reachable from main"},
		},
		{
			name:      "no step builds or publishes anything",
			workflow:  workflow(fullHistoryCheckout, tagGate, mainGate),
			wantGates: 2,
			wantIn:    []string{"no step builds or publishes an image"},
		},
		{
			name:      "shipped shape: both gates before the build",
			workflow:  workflow(fullHistoryCheckout, tagGate, mainGate, buildStep),
			wantGates: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, releaseWorkflowFile)
			if err := os.WriteFile(path, []byte(tt.workflow), 0o644); err != nil {
				t.Fatal(err)
			}

			problems, gates := releaseTagGateProblems(path, tt.workflow)
			if gates != tt.wantGates {
				t.Errorf("gates = %d, want %d", gates, tt.wantGates)
			}
			if len(tt.wantIn) == 0 {
				if len(problems) != 0 {
					t.Fatalf("problems = %v, want none for the shipped gate shape", problems)
				}
				return
			}
			if len(problems) == 0 {
				t.Fatal("workflow that can publish an unpinned or unmerged release accepted, want a problem reported")
			}
			joined := strings.Join(problems, "\n")
			for _, want := range tt.wantIn {
				if !strings.Contains(joined, want) {
					t.Errorf("problems = %v, want one naming %q", problems, want)
				}
			}
		})
	}
}

// releaseTagFromImage is what the gate's `${pinned##*:}` expansion computes, so
// the helper has to read a registry-qualified reference the same way.
func TestReleaseTagFromImage(t *testing.T) {
	tests := []struct {
		image string
		want  string
	}{
		{releaseImage, "v0.1.0"},
		{"ghcr.io/redis-master-label/redis-master-label:v0.2.0", "v0.2.0"},
		{"localhost:5000/redis-master-label:v1.2.3", "v1.2.3"},
	}
	for _, tt := range tests {
		if got := releaseTagFromImage(tt.image); got != tt.want {
			t.Errorf("releaseTagFromImage(%q) = %q, want %q", tt.image, got, tt.want)
		}
	}
}
