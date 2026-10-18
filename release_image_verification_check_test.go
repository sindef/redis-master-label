package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// prePushVerificationSteps is the shape release.yml keeps: the image built above
// is run for the version it reports and inspected for its OCI labels and the
// license behind them, all before the publishing step.
var prePushVerificationSteps = strings.Join([]string{
	"      - name: Verify image version output",
	"        run: |",
	"          version=\"${GITHUB_REF_NAME}\"",
	"          image=\"ghcr.io/example/redis-master-label:$version\"",
	"          got=$(docker run --rm \"$image\" --version)",
	"          want=\"redis-master-label $version\"",
	"          [ \"$got\" = \"$want\" ] || exit 1",
	"",
	"      - name: Verify image OCI labels",
	"        run: |",
	"          labels=$(docker inspect --format '{{json .Config.Labels}}' \"$image\")",
	"          for key in \\",
	"            org.opencontainers.image.title \\",
	"            org.opencontainers.image.description \\",
	"            org.opencontainers.image.source \\",
	"            org.opencontainers.image.licenses \\",
	"            org.opencontainers.image.version; do",
	"            printf '%s' \"$labels\" | grep -q \"\\\"$key\\\":\\\"[^\\\"]\" || exit 1",
	"          done",
	"",
	"      - name: Verify image license metadata",
	"        run: |",
	"          test -f LICENSE || exit 1",
	"          got=$(docker inspect -f '{{ index .Config.Labels \"org.opencontainers.image.licenses\" }}' \"$image\")",
	"          [ \"$got\" = MIT ] || exit 1",
	"",
}, "\n")

// publishingWorkflow builds a workflow around prePush: it builds the image with
// --build-arg VERSION, runs whatever prePush holds, then publishes the image
// with docker/build-push-action. Only prePush varies between fixtures, so the
// pre-push check is what decides the result.
func publishingWorkflow(prePush string) string {
	return strings.Join([]string{
		"name: Release",
		"on:",
		"  push:",
		"    tags:",
		"      - 'v*.*.*'",
		"jobs:",
		"  build-and-publish:",
		"    runs-on: ubuntu-latest",
		"    steps:",
		"      - uses: actions/checkout@v7",
		"      - name: Build image",
		"        run: |",
		"          version=\"${GITHUB_REF_NAME}\"",
		"          docker build --build-arg VERSION=\"$version\" -t \"ghcr.io/example/redis-master-label:$version\" .",
		prePush +
			"      - name: Publish image",
		"        uses: docker/build-push-action@v7",
		"        with:",
		"          push: true",
		"          build-args: VERSION=${{ github.ref_name }}",
		"",
	}, "\n")
}

// The reviewed ci_gap: .github/workflows/release.yml is the only publishing
// path, and ci.yml - which holds the image checks - never runs for a tag push,
// so the tag push has to run and inspect the image itself before it publishes
// it. The shipped workflows must pass and at least one of them must publish:
// with no publishing workflow found the check would pass vacuously.
func TestPublishingWorkflowVerifiesImageBeforePush(t *testing.T) {
	problems, publishing, err := imageVerificationProblems(ciWorkflowDir)
	if err != nil {
		t.Fatal(err)
	}
	if publishing == 0 {
		t.Fatalf("no workflow in %s pushes an image with %s, so the pre-push verification check ran against nothing", ciWorkflowDir, publishImageAction)
	}
	for _, problem := range problems {
		t.Errorf("%s", problem)
	}
}

// The failure modes the check has to see: the reviewed release.yml (a push with
// nothing run or inspected in between), verification placed after the publishing
// step instead of before it, verification that only lives in comments, a
// publishing path that builds no local image, and pre-push steps that assert
// only part of the metadata. The verified shape must be quiet, so the fixtures
// prove the missing piece is what was reported.
func TestImageVerificationProblems_RegressionFixtures(t *testing.T) {
	versionOnly := strings.Join([]string{
		"      - name: Verify image version output",
		"        run: docker run --rm \"$image\" --version",
		"",
	}, "\n")

	versionAndLabels := strings.Join([]string{
		versionOnly,
		"      - name: Verify image OCI labels",
		"        run: |",
		"          labels=$(docker inspect --format '{{json .Config.Labels}}' \"$image\")",
		"          for key in org.opencontainers.image.title org.opencontainers.image.description org.opencontainers.image.source org.opencontainers.image.licenses org.opencontainers.image.version; do",
		"            printf '%s' \"$labels\" | grep -q \"$key\" || exit 1",
		"          done",
		"",
	}, "\n")

	commentedOut := strings.Join([]string{
		"      # Verify image version output stopped running here:",
		"      #   docker run --rm \"$image\" --version",
		"      # Verify image OCI labels stopped running here:",
		"      #   docker inspect --format '{{json .Config.Labels}}' \"$image\"",
		"      #   org.opencontainers.image.title org.opencontainers.image.description",
		"      #   org.opencontainers.image.source org.opencontainers.image.licenses",
		"      #   org.opencontainers.image.version LICENSE",
		"",
	}, "\n")

	noLocalBuild := strings.Join([]string{
		"name: Release",
		"jobs:",
		"  build-and-publish:",
		"    runs-on: ubuntu-latest",
		"    steps:",
		"      - uses: actions/checkout@v7",
		"      - name: Publish image",
		"        uses: docker/build-push-action@v7",
		"        with:",
		"          push: true",
		"          build-args: VERSION=${{ github.ref_name }}",
		"",
	}, "\n")

	buildOnlyBuildx := strings.Join([]string{
		"name: Release",
		"jobs:",
		"  build:",
		"    runs-on: ubuntu-latest",
		"    steps:",
		"      - uses: docker/build-push-action@v7",
		"        with:",
		"          push: false",
		"          load: true",
		"",
	}, "\n")

	commentedOutPush := "name: Release\n" +
		"jobs:\n" +
		"  build-and-publish:\n" +
		"    steps:\n" +
		"      # - uses: docker/build-push-action@v7\n" +
		"      #   with:\n" +
		"      #     push: true\n"

	noVersionBuildArg := strings.Replace(
		publishingWorkflow(prePushVerificationSteps),
		"          build-args: VERSION=${{ github.ref_name }}\n", "", 1)

	tests := []struct {
		name         string
		workflow     string
		wantPublish  bool
		wantProblem  string
		wantProblems int
	}{
		{
			name:        "push step that passes no version build arg",
			workflow:    noVersionBuildArg,
			wantPublish: true,
			wantProblem: "no VERSION build arg",
		},
		{
			name:        "reviewed pre-fix shape: the built image is pushed unrun and uninspected",
			workflow:    publishingWorkflow(""),
			wantPublish: true,
			// All three assertions are missing; the version one is asserted by
			// name here and the others by the fixtures below.
			wantProblem:  "--version",
			wantProblems: 4,
		},
		{
			name:        "verification only after the publishing step",
			workflow:    publishingWorkflow("") + prePushVerificationSteps,
			wantPublish: true,
			wantProblem: "--version",
		},
		{
			name:        "verification that only lives in comments",
			workflow:    publishingWorkflow(commentedOut),
			wantPublish: true,
			wantProblem: "--version",
		},
		{
			name:        "the image is run but its labels are never inspected",
			workflow:    publishingWorkflow(versionOnly),
			wantPublish: true,
			wantProblem: "docker inspect",
		},
		{
			name:        "the labels are inspected but the license grant is never read",
			workflow:    publishingWorkflow(versionAndLabels),
			wantPublish: true,
			wantProblem: "LICENSE",
		},
		{
			name:        "the labels are inspected without one of the five OCI labels",
			workflow:    publishingWorkflow(strings.Replace(prePushVerificationSteps, "            org.opencontainers.image.description \\\n", "", 1)),
			wantPublish: true,
			wantProblem: "org.opencontainers.image.description",
		},
		{
			name:        "complete verification before the push",
			workflow:    publishingWorkflow(prePushVerificationSteps),
			wantPublish: true,
		},
		{
			name:        "publishing path with no local build to verify",
			workflow:    noLocalBuild,
			wantPublish: true,
			wantProblem: "no `docker build --build-arg VERSION",
		},
		{
			name:        "buildx step that only loads an image publishes nothing",
			workflow:    buildOnlyBuildx,
			wantPublish: false,
		},
		{
			name:        "publishing step that only lives in a comment",
			workflow:    commentedOutPush,
			wantPublish: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "release.yml")
			if err := os.WriteFile(path, []byte(tt.workflow), 0o644); err != nil {
				t.Fatal(err)
			}

			problems, publishes := imageVerificationProblemsIn(path, tt.workflow)
			if publishes != tt.wantPublish {
				t.Fatalf("publishes = %v, want %v", publishes, tt.wantPublish)
			}
			if tt.wantProblem == "" {
				if len(problems) != 0 {
					t.Fatalf("problems = %v, want none", problems)
				}
			} else {
				if len(problems) == 0 {
					t.Fatal("publishing workflow without the pre-push image verification accepted, want a problem reported")
				}
				if !strings.Contains(strings.Join(problems, "\n"), tt.wantProblem) {
					t.Fatalf("problems = %v, want one naming %q", problems, tt.wantProblem)
				}
			}
			if tt.wantProblems != 0 && len(problems) != tt.wantProblems {
				t.Errorf("problems = %v, want %d of them", problems, tt.wantProblems)
			}
		})
	}
}

// The scanner reads every workflow file, not only the first one it finds: a
// second publishing workflow without the pre-push verification is reported
// beside a verified one, and the count covers both, so the check cannot pass by
// finding one correct publishing path.
func TestImageVerificationProblems_CoversEveryPublishingWorkflow(t *testing.T) {
	dir := t.TempDir()
	for name, text := range map[string]string{
		"release.yml": publishingWorkflow(prePushVerificationSteps),
		"second.yml":  publishingWorkflow(""),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	problems, publishing, err := imageVerificationProblems(dir)
	if err != nil {
		t.Fatal(err)
	}
	if publishing != 2 {
		t.Fatalf("publishing workflows = %d, want both read", publishing)
	}
	if len(problems) == 0 {
		t.Fatal("unverified second publishing workflow accepted, want a problem reported")
	}
	for _, problem := range problems {
		if !strings.Contains(problem, "second.yml") {
			t.Errorf("problem %q does not name the unverified publishing workflow", problem)
		}
	}

	if _, _, err := imageVerificationProblems(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing workflow directory accepted, want a read error")
	}
}
