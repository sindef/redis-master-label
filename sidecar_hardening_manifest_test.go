package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// seccompProfile and runAsGroup check: the example manifests' labeler
// securityContext claims to satisfy the restricted Pod Security Standard
// (README "Running unprivileged"), which requires a seccomp profile of
// RuntimeDefault or Localhost and pins the group the process runs as. Neither
// claim was enforced. The shared containerSecurityContext reader in
// cmd_manifest_check.go has no seccompProfile field at all, and json.Unmarshal
// drops unknown keys silently, so a manifest whose profile was deleted reads
// back byte-identically to one that still has it; runAsGroup is decoded there
// but no check ever reads it back. Deleting either field from either example
// manifest therefore kept `go test ./...` (and therefore CI) green while the
// pods stopped satisfying the standard the README advertises, because the
// "Validate manifests" step runs kubeconform, which validates the Kubernetes
// schema only: that schema accepts an absent seccompProfile and any type.
//
// This file adds the missing validation on top of the existing readers. The
// seccompProfile is decoded here (at container level and at pod level, the two
// levels the standard accepts) and runAsGroup is compared with the gid the
// shipping image's own account runs with. Every check is asserted twice: against
// the manifests as shipped and against fixtures of the real manifest set with
// the field deleted or changed, so the field cannot be dropped again without
// failing `go test ./...`. The other restricted fields (runAsNonRoot,
// allowPrivilegeEscalation, readOnlyRootFilesystem, capabilities.drop) keep being
// checked by restrictedSecurityContextProblem in main_test.go, which the fixture
// test below also runs.

// labelerSeccompProfile is the seccompProfile block of a container's or a pod's
// securityContext. Type decides which syscalls the container may make: the
// restricted Pod Security Standard requires RuntimeDefault or Localhost, and a
// profile that is declared nowhere leaves the container on the runtime's
// default, which is Unconfined on most runtimes.
type labelerSeccompProfile struct {
	Type string `json:"type"`
}

// labelerAllowedSeccompProfileTypes are the seccompProfile types the restricted
// Pod Security Standard accepts. Unconfined, an empty type and no profile at all
// are rejected by labelerSeccompProfileProblem.
var labelerAllowedSeccompProfileTypes = []string{"RuntimeDefault", "Localhost"}

// labelerSecurityContextSpec is the securityContext subset this file decodes
// from a manifest. It is a second decode of the same key the header comment
// describes: the shared reader in cmd_manifest_check.go cannot report a
// seccompProfile because its struct has no field for it.
type labelerSecurityContextSpec struct {
	SeccompProfile *labelerSeccompProfile `json:"seccompProfile"`
}

// labelerContainerSpec is one container of a pod spec, as far as the labeler's
// securityContext is concerned.
type labelerContainerSpec struct {
	Name            string                      `json:"name"`
	Image           string                      `json:"image"`
	SecurityContext *labelerSecurityContextSpec `json:"securityContext"`
}

// labelerPodSpecManifest holds the pod-level securityContext, which a container
// inherits when it declares no seccompProfile of its own, and the containers.
type labelerPodSpecManifest struct {
	Spec struct {
		Template struct {
			Spec struct {
				SecurityContext *labelerSecurityContextSpec `json:"securityContext"`
				Containers      []labelerContainerSpec      `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
}

// labelerSeccompWiring is the seccomp wiring of one labeler container: the
// profile it declares itself, the pod-level profile it falls back to, and where
// it came from so a failure names the right deployment.
type labelerSeccompWiring struct {
	Manifest                string
	Name                    string
	ContainerSeccompProfile *labelerSeccompProfile
	PodSeccompProfile       *labelerSeccompProfile
}

// labelerSeccompProfiles returns the seccomp wiring of every labeler container
// across manifests/*.yaml, walking the same manifest set (and tolerating the
// same non-Pod-spec documents) as sidecarContainers. The container is selected
// by the sidecar image, exactly like the other manifest checks, so the
// co-located redis container is not read as the labeler.
func labelerSeccompProfiles(dir string) ([]labelerSeccompWiring, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var wiring []labelerSeccompWiring
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := filepath.Ext(entry.Name())
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		jsonData, err := utilyaml.ToJSON(data)
		if err != nil {
			return nil, fmt.Errorf("%s: YAML to JSON: %w", path, err)
		}
		var doc labelerPodSpecManifest
		if err := json.Unmarshal(jsonData, &doc); err != nil {
			return nil, fmt.Errorf("%s: JSON decode: %w", path, err)
		}
		podSecurityContext := doc.Spec.Template.Spec.SecurityContext
		for _, c := range doc.Spec.Template.Spec.Containers {
			if !strings.Contains(c.Image, sidecarImage) {
				continue
			}
			container := labelerSeccompWiring{Manifest: entry.Name(), Name: c.Name}
			if c.SecurityContext != nil {
				container.ContainerSeccompProfile = c.SecurityContext.SeccompProfile
			}
			if podSecurityContext != nil {
				container.PodSeccompProfile = podSecurityContext.SeccompProfile
			}
			wiring = append(wiring, container)
		}
	}
	return wiring, nil
}

// labelerSeccompProfileProblem returns why a labeler container does not satisfy
// the restricted Pod Security Standard's seccomp requirement, or nil when it
// does. The profile in effect is the container's own when it declares one - a
// container-level profile overrides a pod-level one in the API - and the
// pod-level profile it inherits otherwise. Both levels are accepted because that
// is how the standard is written; a profile declared at neither level is the
// deletion this check exists to catch.
func labelerSeccompProfileProblem(w labelerSeccompWiring) error {
	profile, level := w.ContainerSeccompProfile, "container"
	if profile == nil {
		profile, level = w.PodSeccompProfile, "pod"
	}
	if profile == nil {
		return fmt.Errorf("no seccompProfile on the container or on its pod: the container keeps the runtime's default profile (Unconfined on most runtimes), which the restricted Pod Security Standard forbids; declare seccompProfile.type RuntimeDefault on the container or on the pod spec")
	}
	if profile.Type == "" {
		return fmt.Errorf("%s-level seccompProfile declares no type, want %s per the restricted Pod Security Standard", level, strings.Join(labelerAllowedSeccompProfileTypes, " or "))
	}
	for _, allowed := range labelerAllowedSeccompProfileTypes {
		if profile.Type == allowed {
			return nil
		}
	}
	return fmt.Errorf("%s-level seccompProfile.type = %q, want %s per the restricted Pod Security Standard (Unconfined is the profile the standard forbids)", level, profile.Type, strings.Join(labelerAllowedSeccompProfileTypes, " or "))
}

// labelerImageGid returns the gid the shipping image's account runs with, read
// from the build file's final stage USER line (`USER <uid>:<gid>`). The final
// stage is the shipping image, so this is the group the image's own account
// uses and the group the example manifests pin with runAsGroup. A USER without a
// group, or with a non-numeric one, is an error rather than a silent zero: the
// manifests cannot be held to a gid the build file does not name.
func labelerImageGid(buildFile string) (int64, error) {
	user, _, err := dockerfileFinalStageUser(buildFile)
	if err != nil {
		return 0, err
	}
	if user == "" {
		return 0, fmt.Errorf("%s: the final stage selects no USER, so the gid the image runs with cannot be read", buildFile)
	}
	_, group, found := strings.Cut(user, ":")
	if !found || group == "" {
		return 0, fmt.Errorf("%s: final stage USER %q names no group, so the gid the image runs with cannot be read; select the account with USER <uid>:<gid>", buildFile, user)
	}
	gid, err := strconv.ParseInt(group, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: final stage USER %q has no numeric gid: %v", buildFile, user, err)
	}
	return gid, nil
}

// labelerRunAsGroupProblem returns why a labeler container's runAsGroup does not
// pin the gid the shipping image's account runs with, or nil when it does.
// runAsGroup was decoded by the shared reader but asserted nowhere, so a
// manifest could lose it and leave the process on the image or runtime's default
// primary group (gid 0 with a root default) while runAsNonRoot still looked
// satisfied.
func labelerRunAsGroupProblem(sc *containerSecurityContext, gid int64) error {
	if sc == nil {
		return fmt.Errorf("no securityContext, so runAsGroup is not pinned to the image gid %d", gid)
	}
	if sc.RunAsGroup == nil {
		return fmt.Errorf("runAsGroup is not pinned, want the image gid %d: without it the process runs with the image or runtime's default primary group", gid)
	}
	if *sc.RunAsGroup != gid {
		return fmt.Errorf("runAsGroup = %d, want the image gid %d the final build stage selects", *sc.RunAsGroup, gid)
	}
	return nil
}

// Both example deployments must be held to the seccomp profile the restricted
// Pod Security Standard requires, at container or pod level. The profile was
// outside the decoded securityContext until now, so deleting it left every test
// green while the standard the README claims stopped being met.
func TestManifestLabelerSidecarsDeclareSeccompProfile(t *testing.T) {
	wiring, err := labelerSeccompProfiles(manifestDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(wiring) == 0 {
		t.Fatal("no sidecar container found in manifests: the seccompProfile check ran against nothing")
	}
	if len(wiring) != 2 {
		t.Fatalf("sidecar containers = %d, want the two example deployments", len(wiring))
	}
	for _, w := range wiring {
		if err := labelerSeccompProfileProblem(w); err != nil {
			t.Errorf("%s/%s: %v", w.Manifest, w.Name, err)
		}
	}
}

// Both example deployments must pin runAsGroup to the gid the shipping image's
// account runs with, the same agreement TestManifestRunAsUserMatchesImageUser
// requires for the uid: runAsNonRoot is satisfied by any non-root uid, while the
// group the process actually runs with is decided by runAsGroup (or the image's
// default when it is absent).
func TestManifestLabelerSidecarsRunAsGroupMatchesImageGid(t *testing.T) {
	gid, err := labelerImageGid(dockerfilePath)
	if err != nil {
		t.Fatal(err)
	}
	if gid == 0 {
		t.Errorf("%s: the shipping image's account runs with gid 0, want an unprivileged gid: runAsGroup cannot pin a non-root group then", dockerfilePath)
	}

	containers, err := sidecarContainers(manifestDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(containers) == 0 {
		t.Fatal("no sidecar container found in manifests: the runAsGroup check ran against nothing")
	}
	if len(containers) != 2 {
		t.Fatalf("sidecar containers = %d, want the two example deployments", len(containers))
	}
	for _, c := range containers {
		if err := labelerRunAsGroupProblem(c.SecurityContext, gid); err != nil {
			t.Errorf("%s/%s: %v", c.Manifest, c.Name, err)
		}
	}
}

// The reader has to see what the manifest text says: a profile declared on the
// container is read back, a profile declared on the pod spec is read back, the
// container-level one wins over the pod-level one, a pod-level profile an
// override cannot save is still reported, and a document without a Pod spec (the
// Service, Role, RoleBinding, ServiceAccount) contributes no sidecar instead of
// being counted. Each fixture gets a directory of its own, so only the manifest
// under test is read.
func TestLabelerSeccompProfiles_RegressionFixtures(t *testing.T) {
	root := t.TempDir()
	dir := func(name string) string { return filepath.Join(root, name) }

	// one reads a fixture directory and requires exactly one sidecar.
	one := func(t *testing.T, name string) labelerSeccompWiring {
		t.Helper()
		wiring, err := labelerSeccompProfiles(dir(name))
		if err != nil {
			t.Fatal(err)
		}
		if len(wiring) != 1 {
			t.Fatalf("sidecar containers = %d, want 1: the labeler is selected by the sidecar image", len(wiring))
		}
		return wiring[0]
	}

	neither := "apiVersion: apps/v1\nkind: Deployment\nspec:\n  template:\n    spec:\n      containers:\n      - name: redis\n        image: redis:7-alpine\n      - name: redis-master-label\n        image: ghcr.io/redis-master-label/redis-master-label:v0.1.0\n"
	writeManifestFile(t, dir("neither"), "deployment.yaml", neither)
	wiring := one(t, "neither")
	if wiring.ContainerSeccompProfile != nil || wiring.PodSeccompProfile != nil {
		t.Fatalf("wiring = %+v, want no profile for a manifest that declares none", wiring)
	}

	containerLevel := "apiVersion: apps/v1\nkind: Deployment\nspec:\n  template:\n    spec:\n      containers:\n      - name: redis-master-label\n        image: ghcr.io/redis-master-label/redis-master-label:v0.1.0\n        securityContext:\n          seccompProfile:\n            type: RuntimeDefault\n"
	writeManifestFile(t, dir("container"), "deployment.yaml", containerLevel)
	wiring = one(t, "container")
	if wiring.ContainerSeccompProfile == nil || wiring.ContainerSeccompProfile.Type != "RuntimeDefault" {
		t.Fatalf("container profile = %+v, want the manifest's RuntimeDefault", wiring.ContainerSeccompProfile)
	}
	if wiring.PodSeccompProfile != nil {
		t.Errorf("pod profile = %+v, want none: this manifest declares it on the container", wiring.PodSeccompProfile)
	}

	podLevel := "apiVersion: apps/v1\nkind: Deployment\nspec:\n  template:\n    spec:\n      securityContext:\n        seccompProfile:\n          type: Localhost\n          localhostProfile: profiles/labeler.json\n      containers:\n      - name: redis-master-label\n        image: ghcr.io/redis-master-label/redis-master-label:v0.1.0\n"
	writeManifestFile(t, dir("pod"), "deployment.yaml", podLevel)
	wiring = one(t, "pod")
	if wiring.PodSeccompProfile == nil || wiring.PodSeccompProfile.Type != "Localhost" {
		t.Fatalf("pod profile = %+v, want the manifest's Localhost", wiring.PodSeccompProfile)
	}
	if wiring.ContainerSeccompProfile != nil {
		t.Errorf("container profile = %+v, want none: this manifest declares the profile on the pod", wiring.ContainerSeccompProfile)
	}
	if err := labelerSeccompProfileProblem(wiring); err != nil {
		t.Errorf("a pod-level Localhost profile = %v, want it accepted: the standard accepts the profile at this level", err)
	}

	// A pod-level profile only applies where the container declares none: the
	// container-level RuntimeDefault stays the profile in effect even beside a
	// pod-level Unconfined.
	both := strings.Replace(containerLevel, "      containers:\n", "      securityContext:\n        seccompProfile:\n          type: Unconfined\n      containers:\n", 1)
	writeManifestFile(t, dir("both"), "deployment.yaml", both)
	wiring = one(t, "both")
	if wiring.ContainerSeccompProfile == nil || wiring.PodSeccompProfile == nil {
		t.Fatalf("wiring = %+v, want both the container and the pod profile of this manifest", wiring)
	}
	if err := labelerSeccompProfileProblem(wiring); err != nil {
		t.Errorf("a container-level RuntimeDefault beside a pod-level Unconfined = %v, want it accepted: the container-level profile is what the kubelet applies", err)
	}

	// An Unconfined profile inherited from the pod must be reported: with no
	// profile of its own, that is the profile the container ends up with.
	inherited := strings.Replace(podLevel, "          type: Localhost\n", "          type: Unconfined\n", 1)
	writeManifestFile(t, dir("inherited"), "deployment.yaml", inherited)
	if err := labelerSeccompProfileProblem(one(t, "inherited")); err == nil {
		t.Error("a labeler inheriting an Unconfined profile from its pod was accepted, want a problem")
	}

	// A document with no Pod spec is not a sidecar, and walking a missing
	// directory is an error rather than an empty pass.
	writeManifestFile(t, dir("neither"), "service.yaml", "apiVersion: v1\nkind: Service\nmetadata:\n  name: redis-leader\n")
	if sidecars, err := labelerSeccompProfiles(dir("neither")); err != nil || len(sidecars) != 1 {
		t.Fatalf("sidecars = %d entries, err = %v, want the one sidecar and no Service", len(sidecars), err)
	}
	if _, err := labelerSeccompProfiles(filepath.Join(root, "missing")); err == nil {
		t.Error("missing manifest directory accepted, want a read error")
	}
}

// The failure modes of the seccomp rule: the profile the manifest lost, the
// Unconfined profile the standard forbids, an empty type, a type that is not one
// of the two allowed ones and a whitespace-different spelling must each be
// reported, while RuntimeDefault and Localhost must pass at either level.
func TestLabelerSeccompProfileProblem_RegressionFixtures(t *testing.T) {
	tests := []struct {
		name        string
		wiring      labelerSeccompWiring
		wantErr     bool
		wantInError string
	}{
		{
			name:   "container RuntimeDefault",
			wiring: labelerSeccompWiring{Name: "redis-master-label", ContainerSeccompProfile: &labelerSeccompProfile{Type: "RuntimeDefault"}},
		},
		{
			name:   "container Localhost",
			wiring: labelerSeccompWiring{Name: "redis-master-label", ContainerSeccompProfile: &labelerSeccompProfile{Type: "Localhost"}},
		},
		{
			name:   "pod-level RuntimeDefault inherited by the container",
			wiring: labelerSeccompWiring{Name: "redis-master-label", PodSeccompProfile: &labelerSeccompProfile{Type: "RuntimeDefault"}},
		},
		{
			name:   "pod-level Localhost inherited by the container",
			wiring: labelerSeccompWiring{Name: "redis-master-label", PodSeccompProfile: &labelerSeccompProfile{Type: "Localhost"}},
		},
		{
			name: "container RuntimeDefault overrides a pod-level Unconfined",
			wiring: labelerSeccompWiring{
				Name:                    "redis-master-label",
				ContainerSeccompProfile: &labelerSeccompProfile{Type: "RuntimeDefault"},
				PodSeccompProfile:       &labelerSeccompProfile{Type: "Unconfined"},
			},
		},
		{
			name:        "profile deleted at both levels",
			wiring:      labelerSeccompWiring{Name: "redis-master-label"},
			wantErr:     true,
			wantInError: "no seccompProfile",
		},
		{
			name:        "container profile Unconfined",
			wiring:      labelerSeccompWiring{Name: "redis-master-label", ContainerSeccompProfile: &labelerSeccompProfile{Type: "Unconfined"}},
			wantErr:     true,
			wantInError: "Unconfined",
		},
		{
			name:        "pod-level profile Unconfined",
			wiring:      labelerSeccompWiring{Name: "redis-master-label", PodSeccompProfile: &labelerSeccompProfile{Type: "Unconfined"}},
			wantErr:     true,
			wantInError: "pod-level",
		},
		{
			name:        "profile declared with no type",
			wiring:      labelerSeccompWiring{Name: "redis-master-label", ContainerSeccompProfile: &labelerSeccompProfile{}},
			wantErr:     true,
			wantInError: "declares no type",
		},
		{
			name:        "profile type in another case",
			wiring:      labelerSeccompWiring{Name: "redis-master-label", ContainerSeccompProfile: &labelerSeccompProfile{Type: "runtimedefault"}},
			wantErr:     true,
			wantInError: "RuntimeDefault or Localhost",
		},
		{
			name:        "profile type with trailing whitespace",
			wiring:      labelerSeccompWiring{Name: "redis-master-label", ContainerSeccompProfile: &labelerSeccompProfile{Type: "RuntimeDefault "}},
			wantErr:     true,
			wantInError: "RuntimeDefault or Localhost",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := labelerSeccompProfileProblem(tt.wiring)
			if (err != nil) != tt.wantErr {
				t.Fatalf("labelerSeccompProfileProblem(%+v) error = %v, wantErr %v", tt.wiring, err, tt.wantErr)
			}
			if tt.wantInError != "" && (err == nil || !strings.Contains(err.Error(), tt.wantInError)) {
				t.Fatalf("error = %v, want it to mention %q", err, tt.wantInError)
			}
		})
	}
}

// The failure modes of the runAsGroup rule: an absent securityContext, an
// absent runAsGroup (the deletion that used to pass), gid 0 and any gid other
// than the image's must be reported, while the image gid itself must pass.
func TestLabelerRunAsGroupProblem_RegressionFixtures(t *testing.T) {
	const imageGid = int64(10001)

	contextWithGroup := func(gid *int64) *containerSecurityContext {
		return &containerSecurityContext{RunAsGroup: gid}
	}
	group := func(gid int64) *int64 { return &gid }

	tests := []struct {
		name        string
		sc          *containerSecurityContext
		wantErr     bool
		wantInError string
	}{
		{name: "runAsGroup equals the image gid", sc: contextWithGroup(group(imageGid))},
		{name: "no securityContext", sc: nil, wantErr: true, wantInError: "runAsGroup is not pinned"},
		{name: "runAsGroup deleted", sc: contextWithGroup(nil), wantErr: true, wantInError: "runAsGroup is not pinned"},
		{name: "runAsGroup 0", sc: contextWithGroup(group(0)), wantErr: true, wantInError: "want the image gid 10001"},
		{name: "runAsGroup names another gid", sc: contextWithGroup(group(9999)), wantErr: true, wantInError: "want the image gid 10001"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := labelerRunAsGroupProblem(tt.sc, imageGid)
			if (err != nil) != tt.wantErr {
				t.Fatalf("labelerRunAsGroupProblem(%+v, %d) error = %v, wantErr %v", tt.sc, imageGid, err, tt.wantErr)
			}
			if tt.wantInError != "" && (err == nil || !strings.Contains(err.Error(), tt.wantInError)) {
				t.Fatalf("error = %v, want it to mention %q", err, tt.wantInError)
			}
		})
	}
}

// The image gid reader must see the build file: a USER naming a group is read
// back, while a USER without a group, a non-numeric group, an empty final stage
// and a missing file are errors rather than a silent zero (a zero would be
// indistinguishable from the root group, the very thing runAsGroup pins).
func TestLabelerImageGid_RegressionFixtures(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		writeBuildFile(t, path, content)
		return path
	}

	path := write("user-with-group.Dockerfile", "FROM alpine:3.24\nRUN addgroup -S -g 10001 labeler && adduser -S -u 10001 -G labeler -H labeler\nUSER 10001:10001\n")
	if gid, err := labelerImageGid(path); err != nil || gid != 10001 {
		t.Fatalf("gid = %d, err = %v, want 10001 from USER 10001:10001", gid, err)
	}

	path = write("user-without-group.Dockerfile", "FROM alpine:3.24\nUSER 10001\n")
	if _, err := labelerImageGid(path); err == nil {
		t.Error("USER without a group accepted, want an error: the gid the image runs with cannot be read")
	}

	path = write("user-named.Dockerfile", "FROM alpine:3.24\nUSER labeler:labeler\n")
	if _, err := labelerImageGid(path); err == nil {
		t.Error("USER with a non-numeric group accepted, want an error")
	}

	path = write("no-user.Dockerfile", "FROM alpine:3.24\n")
	if _, err := labelerImageGid(path); err == nil {
		t.Error("build file whose final stage selects no USER accepted, want an error")
	}

	if _, err := labelerImageGid(filepath.Join(dir, "missing.Dockerfile")); err == nil {
		t.Error("missing build file accepted, want a read error")
	}
}

// The regression the reviewed gap describes, at manifest level: deleting
// seccompProfile or runAsGroup from the example manifests, changing the profile
// type to Unconfined, or pointing runAsGroup at another gid must each be
// reported, while moving the profile to the pod spec (which the standard
// accepts) and the manifests as shipped must pass. Every fixture is a copy of
// the real manifest set with one edit, so the edit is what the check caught, and
// the shared restrictedSecurityContextProblem is run over the same fixtures so
// the other restricted fields stay enforced here too.
func TestManifestLabelerHardeningFields_FailureModes(t *testing.T) {
	gid, err := labelerImageGid(dockerfilePath)
	if err != nil {
		t.Fatal(err)
	}

	removeLine := func(line string) func(file, text string) string {
		return func(file, text string) string { return strings.ReplaceAll(text, line, "") }
	}
	replaceLine := func(old, new string) func(file, text string) string {
		return func(file, text string) string { return strings.ReplaceAll(text, old, new) }
	}

	tests := []struct {
		name        string
		mutate      func(file, text string) string
		wantProblem string
	}{
		{
			// The exact field deletion that used to keep the suite green.
			name:        "seccompProfile deleted from the container",
			mutate:      removeLine("          seccompProfile:\n            type: RuntimeDefault\n"),
			wantProblem: "seccompProfile",
		},
		{
			name:        "runAsGroup deleted from the container",
			mutate:      removeLine("          runAsGroup: 10001\n"),
			wantProblem: "runAsGroup",
		},
		{
			name:        "seccompProfile type Unconfined",
			mutate:      replaceLine("            type: RuntimeDefault\n", "            type: Unconfined\n"),
			wantProblem: "Unconfined",
		},
		{
			name:        "seccompProfile type in another case",
			mutate:      replaceLine("            type: RuntimeDefault\n", "            type: runtimedefault\n"),
			wantProblem: "RuntimeDefault or Localhost",
		},
		{
			name:        "seccompProfile declared with no type",
			mutate:      replaceLine("            type: RuntimeDefault\n", "            type: \"\"\n"),
			wantProblem: "seccompProfile",
		},
		{
			name:        "runAsGroup 0",
			mutate:      replaceLine("          runAsGroup: 10001\n", "          runAsGroup: 0\n"),
			wantProblem: "runAsGroup",
		},
		{
			name:        "runAsGroup names another gid",
			mutate:      replaceLine("          runAsGroup: 10001\n", "          runAsGroup: 9999\n"),
			wantProblem: "want the image gid",
		},
		{
			// Both levels are valid for the restricted standard: the profile
			// has to be declared somewhere, not necessarily on the container.
			name: "seccompProfile moved to the pod spec",
			mutate: func(file, text string) string {
				text = strings.Replace(text, "          seccompProfile:\n            type: RuntimeDefault\n", "", 1)
				return strings.Replace(text, "    spec:\n      serviceAccountName:", "    spec:\n      securityContext:\n        seccompProfile:\n          type: RuntimeDefault\n      serviceAccountName:", 1)
			},
		},
		{
			name: "unmutated manifests",
		},
	}

	problemsIn := func(t *testing.T, dir string) []string {
		t.Helper()
		var problems []string

		// The other restricted fields keep their existing checker.
		containers, err := sidecarContainers(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(containers) != 2 {
			t.Fatalf("sidecar containers = %d, want the two example deployments", len(containers))
		}
		for _, c := range containers {
			if err := restrictedSecurityContextProblem(c.SecurityContext); err != nil {
				problems = append(problems, fmt.Sprintf("%s/%s: %v", c.Manifest, c.Name, err))
			}
			if err := labelerRunAsGroupProblem(c.SecurityContext, gid); err != nil {
				problems = append(problems, fmt.Sprintf("%s/%s: %v", c.Manifest, c.Name, err))
			}
		}

		wiring, err := labelerSeccompProfiles(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(wiring) != 2 {
			t.Fatalf("sidecar containers = %d, want the two example deployments", len(wiring))
		}
		for _, w := range wiring {
			if err := labelerSeccompProfileProblem(w); err != nil {
				problems = append(problems, fmt.Sprintf("%s/%s: %v", w.Manifest, w.Name, err))
			}
		}

		return problems
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeManifestSet(t, dir, tt.mutate)

			problems := problemsIn(t, dir)
			if tt.wantProblem == "" {
				if len(problems) != 0 {
					t.Fatalf("manifest set reported: %v, want none", problems)
				}
				return
			}
			if len(problems) == 0 {
				t.Fatalf("a labeler securityContext that lost %s was accepted, want a problem reported", tt.wantProblem)
			}
			if !strings.Contains(strings.Join(problems, "\n"), tt.wantProblem) {
				t.Fatalf("problems = %v, want one naming %q", problems, tt.wantProblem)
			}
		})
	}

	// The manifests as shipped must pass every check above, so the fixtures
	// prove the edit is what was caught rather than a checker that always
	// complains.
	shipped := t.TempDir()
	writeManifestSet(t, shipped, nil)
	if problems := problemsIn(t, shipped); len(problems) != 0 {
		t.Fatalf("manifest set as shipped reported: %v, want none", problems)
	}
}

// writeManifestFile writes one manifest into dir (creating it), so a fixture can
// spell out only the manifest it needs.
func writeManifestFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
