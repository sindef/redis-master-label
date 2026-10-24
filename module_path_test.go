package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// modulePathWant is the module path the repository publishes under: the
// repository path the image's org.opencontainers.image.source label,
// `const releaseImage` in main_test.go and the example manifests all name. The
// pre-fix go.mod declared `github.com/redis-master-label`, which is the import
// path of a different repository, so this value is asserted explicitly rather
// than derived from whatever the files happen to say.
const modulePathWant = "github.com/redis-master-label/redis-master-label"

// The real files must agree: go.mod's module path, the Dockerfile's
// org.opencontainers.image.source label, the pinned release image and both
// example manifests all name the same repository. Nothing else in the suite reads
// the module directive, so a module path that names another repository used to
// pass every check while `go get` and module-derived SBOM/purl metadata pointed
// somewhere else.
func TestGoModModulePathMatchesPublishedRepository(t *testing.T) {
	goModData, err := os.ReadFile(modulePathFile)
	if err != nil {
		t.Fatal(err)
	}
	modulePath, err := modulePathFromGoMod(string(goModData))
	if err != nil {
		t.Fatal(err)
	}
	if modulePath != modulePathWant {
		t.Errorf("%s declares module %q, want %q: the module path is the repository the image source label and the release pin name", modulePathFile, modulePath, modulePathWant)
	}

	dockerfileData, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatal(err)
	}
	imageSource, err := imageSourceValue(string(dockerfileData))
	if err != nil {
		t.Fatal(err)
	}

	pinData, err := os.ReadFile(releasePinFile)
	if err != nil {
		t.Fatal(err)
	}
	releaseImage, err := releaseImageReference(string(pinData))
	if err != nil {
		t.Fatal(err)
	}

	manifestImages, err := manifestImageReferences(manifestDir)
	if err != nil {
		t.Fatal(err)
	}
	if refs := labelerImageReferences(manifestImages); len(refs) != 2 {
		t.Fatalf("labeler image references = %v (%d), want the two example deployments", refs, len(refs))
	}

	for _, problem := range modulePathProblems(modulePath, imageSource, releaseImage, manifestImages) {
		t.Errorf("%s", problem)
	}
}

// The readers have to see what the files say, and must not invent agreement: the
// quoted and unquoted label forms, the module directive with a trailing comment
// and a tag or digest on an image reference are all read back, while a missing
// label, a module directive without a path and a reference without a registry
// host are errors.
func TestModulePathReaders_RegressionFixtures(t *testing.T) {
	t.Run("module directive", func(t *testing.T) {
		tests := []struct {
			name        string
			content     string
			want        string
			wantInError string
		}{
			{name: "the declared path", content: "module github.com/redis-master-label/redis-master-label\n\ngo 1.27.0\n", want: modulePathWant},
			{name: "quoted path", content: "module \"github.com/redis-master-label/redis-master-label\"\n", want: modulePathWant},
			{name: "a comment before the directive", content: "// module github.com/ignored\nmodule github.com/redis-master-label/redis-master-label // trailing\n", want: modulePathWant},
			{name: "no module directive", content: "go 1.27.0\n", wantInError: "no module directive"},
			{name: "module directive without a path", content: "module\n", wantInError: "declares no path"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				got, err := modulePathFromGoMod(tt.content)
				if tt.wantInError != "" {
					if err == nil || !strings.Contains(err.Error(), tt.wantInError) {
						t.Fatalf("error = %v, want one mentioning %q", err, tt.wantInError)
					}
					return
				}
				if err != nil || got != tt.want {
					t.Fatalf("modulePathFromGoMod() = %q, %v, want %q", got, err, tt.want)
				}
			})
		}
	})

	t.Run("image source label", func(t *testing.T) {
		tests := []struct {
			name        string
			dockerfile  string
			want        string
			wantInError string
		}{
			{name: "quoted label", dockerfile: "LABEL org.opencontainers.image.source=\"https://github.com/redis-master-label/redis-master-label\"\n", want: "https://github.com/redis-master-label/redis-master-label"},
			{name: "unquoted label", dockerfile: "LABEL org.opencontainers.image.source=https://github.com/redis-master-label/redis-master-label\n", want: "https://github.com/redis-master-label/redis-master-label"},
			{name: "label continued on the next line", dockerfile: "LABEL org.opencontainers.image.source=\"https://github.com/a/b\" \\\n      org.opencontainers.image.licenses=\"MIT\"\n", want: "https://github.com/a/b"},
			{name: "no label", dockerfile: "LABEL org.opencontainers.image.licenses=\"MIT\"\n", wantInError: "declares no org.opencontainers.image.source label"},
			{name: "empty label", dockerfile: "LABEL org.opencontainers.image.source=\"\"\n", wantInError: "carries no value"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				got, err := imageSourceValue(tt.dockerfile)
				if tt.wantInError != "" {
					if err == nil || !strings.Contains(err.Error(), tt.wantInError) {
						t.Fatalf("error = %v, want one mentioning %q", err, tt.wantInError)
					}
					return
				}
				if err != nil || got != tt.want {
					t.Fatalf("imageSourceValue() = %q, %v, want %q", got, err, tt.want)
				}
			})
		}
	})

	t.Run("repository paths", func(t *testing.T) {
		host, path, err := repositoryPathFromURL("https://github.com/redis-master-label/redis-master-label/")
		if err != nil || host != "github.com" || path != "redis-master-label/redis-master-label" {
			t.Fatalf("repositoryPathFromURL() = %q, %q, %v, want github.com and redis-master-label/redis-master-label", host, path, err)
		}
		if _, _, err := repositoryPathFromURL("redis-master-label"); err == nil {
			t.Error("a bare name accepted as a repository URL, want an error")
		}

		for _, ref := range []string{
			"ghcr.io/redis-master-label/redis-master-label:v0.1.0",
			"ghcr.io/redis-master-label/redis-master-label",
			"ghcr.io/redis-master-label/redis-master-label@sha256:0000000000000000000000000000000000000000000000000000000000000000",
		} {
			if got, err := repositoryPathFromImageRef(ref); err != nil || got != "redis-master-label/redis-master-label" {
				t.Errorf("repositoryPathFromImageRef(%q) = %q, %v, want redis-master-label/redis-master-label", ref, got, err)
			}
		}
		if _, err := repositoryPathFromImageRef("redis-master-label:v0.1.0"); err == nil {
			t.Error("a reference without a registry host accepted, want an error")
		}
	})

	t.Run("release pin", func(t *testing.T) {
		got, err := releaseImageReference("const releaseImage = \"ghcr.io/redis-master-label/redis-master-label:v0.1.0\"\n")
		if err != nil || got != "ghcr.io/redis-master-label/redis-master-label:v0.1.0" {
			t.Fatalf("releaseImageReference() = %q, %v, want the pinned reference", got, err)
		}
		if _, err := releaseImageReference("const other = \"x\"\n"); err == nil {
			t.Error("source without the releaseImage constant accepted, want an error")
		}
	})
}

// The comparison itself: the module path must equal the repository the image
// source label, the release pin and the manifests name. The pre-fix module path
// is the regression fixture, and a surface that cannot be read is reported rather
// than skipped.
func TestModulePathProblems_RegressionFixtures(t *testing.T) {
	const (
		goodSource  = "https://github.com/redis-master-label/redis-master-label"
		goodRelease = "ghcr.io/redis-master-label/redis-master-label:v0.1.0"
	)
	goodManifests := []string{"redis:7-alpine", goodRelease}

	tests := []struct {
		name          string
		modulePath    string
		imageSource   string
		releaseImage  string
		manifestImage []string
		wantProblem   string
	}{
		{
			name:          "the shipped shape",
			modulePath:    modulePathWant,
			imageSource:   goodSource,
			releaseImage:  goodRelease,
			manifestImage: goodManifests,
		},
		{
			// The exact declaration the check exists to catch.
			name:          "module path without the repository segment",
			modulePath:    "github.com/redis-master-label",
			imageSource:   goodSource,
			releaseImage:  goodRelease,
			manifestImage: goodManifests,
			wantProblem:   modulePathWant,
		},
		{
			name:          "module path naming another owner",
			modulePath:    "github.com/someone-else/redis-master-label",
			imageSource:   goodSource,
			releaseImage:  goodRelease,
			manifestImage: goodManifests,
			wantProblem:   modulePathWant,
		},
		{
			name:          "no module path declared",
			modulePath:    "",
			imageSource:   goodSource,
			releaseImage:  goodRelease,
			manifestImage: goodManifests,
			wantProblem:   "declares no module path",
		},
		{
			name:          "image source names another repository",
			modulePath:    modulePathWant,
			imageSource:   "https://github.com/someone-else/redis-master-label",
			releaseImage:  goodRelease,
			manifestImage: []string{goodRelease},
			wantProblem:   "github.com/someone-else/redis-master-label",
		},
		{
			name:          "no image source label read",
			modulePath:    modulePathWant,
			imageSource:   "",
			releaseImage:  goodRelease,
			manifestImage: goodManifests,
			wantProblem:   "org.opencontainers.image.source",
		},
		{
			name:          "release pin names another repository",
			modulePath:    modulePathWant,
			imageSource:   goodSource,
			releaseImage:  "ghcr.io/someone-else/redis-master-label:v0.1.0",
			manifestImage: goodManifests,
			wantProblem:   "release image",
		},
		{
			name:          "no release pin read",
			modulePath:    modulePathWant,
			imageSource:   goodSource,
			releaseImage:  "",
			manifestImage: goodManifests,
			wantProblem:   "no releaseImage reference",
		},
		{
			name:          "manifest names another repository",
			modulePath:    modulePathWant,
			imageSource:   goodSource,
			releaseImage:  goodRelease,
			manifestImage: []string{"ghcr.io/someone-else/redis-master-label:v0.1.0"},
			wantProblem:   "manifest image",
		},
		{
			name:          "no labeler image in the manifests",
			modulePath:    modulePathWant,
			imageSource:   goodSource,
			releaseImage:  goodRelease,
			manifestImage: []string{"redis:7-alpine"},
			wantProblem:   "no redis-master-label image reference",
		},
		{
			name:          "no manifests read at all",
			modulePath:    modulePathWant,
			imageSource:   goodSource,
			releaseImage:  goodRelease,
			manifestImage: nil,
			wantProblem:   "no redis-master-label image reference",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			problems := modulePathProblems(tt.modulePath, tt.imageSource, tt.releaseImage, tt.manifestImage)
			if tt.wantProblem == "" {
				if len(problems) != 0 {
					t.Fatalf("problems = %v, want none", problems)
				}
				return
			}
			if len(problems) == 0 {
				t.Fatalf("module path %q beside %q was accepted, want a problem naming %q", tt.modulePath, tt.imageSource, tt.wantProblem)
			}
			if !strings.Contains(strings.Join(problems, "\n"), tt.wantProblem) {
				t.Fatalf("problems = %v, want one naming %q", problems, tt.wantProblem)
			}
		})
	}
}

// The reader walks the manifest set, so a manifest added later is read too, and a
// missing directory is an error rather than an empty pass.
func TestManifestImageReferences_RegressionFixtures(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"deployment.yaml": "spec:\n  template:\n    spec:\n      containers:\n      - image: ghcr.io/redis-master-label/redis-master-label:v0.1.0\n",
		"leader.yaml":     "spec:\n  template:\n    spec:\n      containers:\n      - image: \"redis:7-alpine\"\n",
		"service.yaml":    "apiVersion: v1\nkind: Service\nmetadata:\n  name: redis-leader\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	images, err := manifestImageReferences(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 2 {
		t.Fatalf("images = %v, want the two container images and no Service", images)
	}
	if refs := labelerImageReferences(images); len(refs) != 1 || refs[0] != "ghcr.io/redis-master-label/redis-master-label:v0.1.0" {
		t.Fatalf("labeler refs = %v, want the labeler image only", refs)
	}

	if _, err := manifestImageReferences(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing manifest directory accepted, want a read error")
	}
}
