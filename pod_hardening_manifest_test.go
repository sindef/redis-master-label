package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// Per-container restricted Pod Security check: the existing manifest tests
// select containers by the sidecar image (sidecarContainers), so the
// co-located `redis` container of both example deployments was never read.
// The restricted Pod Security Standard is evaluated per container, not per
// pod: a namespace enforcing it rejected the whole pod because that container
// declared no securityContext at all, even though the labeler beside it
// carried the full restricted profile. CI's kubeconform step only validates the
// schema, where securityContext is optional, so nothing caught the gap.
//
// The reader below walks every Deployment under manifests/ and returns every
// container of every pod spec, without filtering by image. The check holds each
// one to the same restricted shape the labeler already met, and holds a Redis
// container whose root filesystem is read-only to a writable /data mount
// backed by a declared pod volume: redis-server writes its RDB snapshot under
// /data and fails at startup without one.

// podSeccompProfile is a securityContext's seccompProfile block, decoded from
// the container and from the pod spec because the restricted standard accepts
// the profile at either level.
type podSeccompProfile struct {
	Type string `json:"type"`
}

// podAllowedSeccompProfileTypes are the profile types the restricted Pod
// Security Standard accepts; Unconfined, an empty type and no profile at all
// are rejected.
var podAllowedSeccompProfileTypes = []string{"RuntimeDefault", "Localhost"}

// podContainerSecurityContext is one container's securityContext: the fields
// restrictedSecurityContextProblem already checks (main_test.go) plus the
// seccompProfile block that reader never decoded.
type podContainerSecurityContext struct {
	containerSecurityContext
	SeccompProfile *podSeccompProfile `json:"seccompProfile"`
}

// podVolumeMount is one entry of a container's volumeMounts list.
type podVolumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	ReadOnly  *bool  `json:"readOnly"`
}

// podVolume is one entry of a pod spec's volumes list; only its name is needed,
// because the /data check asks whether the mount is backed by a declared
// volume, not what that volume is made of.
type podVolume struct {
	Name string `json:"name"`
}

// hardenedPodManifest is the pod-spec subset the per-container check reads: the
// document kind and name, the pod-level securityContext, every container with
// its securityContext and volumeMounts, and the declared volumes.
type hardenedPodManifest struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Template struct {
			Spec struct {
				SecurityContext *struct {
					SeccompProfile *podSeccompProfile `json:"seccompProfile"`
				} `json:"securityContext"`
				Containers []struct {
					Name            string                       `json:"name"`
					Image           string                       `json:"image"`
					SecurityContext *podContainerSecurityContext `json:"securityContext"`
					VolumeMounts    []podVolumeMount             `json:"volumeMounts"`
				} `json:"containers"`
				Volumes []podVolume `json:"volumes"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
}

// hardenedPodContainer is one container of one Deployment, with everything the
// per-container restricted check needs and the names that make a failure point
// at the YAML.
type hardenedPodContainer struct {
	Manifest          string
	Deployment        string
	Name              string
	Image             string
	SecurityContext   *podContainerSecurityContext
	PodSeccompProfile *podSeccompProfile
	VolumeMounts      []podVolumeMount
	PodVolumes        []podVolume
}

// sharedSecurityContext exposes the container securityContext in the shape
// restrictedSecurityContextProblem reads, or nil when the container declares
// none.
func (c hardenedPodContainer) sharedSecurityContext() *containerSecurityContext {
	if c.SecurityContext == nil {
		return nil
	}
	return &c.SecurityContext.containerSecurityContext
}

// podContainers returns every container of every Deployment across
// manifests/*.yaml, plus the number of Deployment documents read so a caller
// can tell "every container is hardened" from "no Deployment was read".
// Documents without a pod spec (the Service, Role, RoleBinding,
// ServiceAccount) contribute nothing. Unlike sidecarContainers it does not
// filter by image: restricted Pod Security is per container.
func podContainers(dir string) (containers []hardenedPodContainer, deployments int, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, fmt.Errorf("read %s: %w", dir, err)
	}
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
			return nil, 0, err
		}
		jsonData, err := utilyaml.ToJSON(data)
		if err != nil {
			return nil, 0, fmt.Errorf("%s: YAML to JSON: %w", path, err)
		}
		var doc hardenedPodManifest
		if err := json.Unmarshal(jsonData, &doc); err != nil {
			return nil, 0, fmt.Errorf("%s: JSON decode: %w", path, err)
		}
		if doc.Kind != deploymentKind {
			continue
		}
		deployments++

		// The pod-level profile is the fallback a container inherits when it
		// declares none of its own.
		var podSeccomp *podSeccompProfile
		if doc.Spec.Template.Spec.SecurityContext != nil {
			podSeccomp = doc.Spec.Template.Spec.SecurityContext.SeccompProfile
		}

		for _, c := range doc.Spec.Template.Spec.Containers {
			containers = append(containers, hardenedPodContainer{
				Manifest:          entry.Name(),
				Deployment:        doc.Metadata.Name,
				Name:              c.Name,
				Image:             c.Image,
				SecurityContext:   c.SecurityContext,
				PodSeccompProfile: podSeccomp,
				VolumeMounts:      c.VolumeMounts,
				PodVolumes:        doc.Spec.Template.Spec.Volumes,
			})
		}
	}
	return containers, deployments, nil
}

// restrictedSeccompProblem returns why a container does not satisfy the
// restricted standard's seccomp requirement, or nil when it does. The profile
// in effect is the container's own when declared (it overrides a pod-level one
// in the API) and the pod-level profile otherwise.
func restrictedSeccompProblem(containerProfile, podProfile *podSeccompProfile) error {
	profile, level := containerProfile, "container"
	if profile == nil {
		profile, level = podProfile, "pod"
	}
	if profile == nil {
		return fmt.Errorf("no seccompProfile on the container or on its pod: the container keeps the runtime's default profile (Unconfined on most runtimes), which the restricted Pod Security Standard forbids; declare seccompProfile.type RuntimeDefault on the container or on the pod spec")
	}
	for _, allowed := range podAllowedSeccompProfileTypes {
		if profile.Type == allowed {
			return nil
		}
	}
	if profile.Type == "" {
		return fmt.Errorf("%s-level seccompProfile declares no type, want %s per the restricted Pod Security Standard", level, strings.Join(podAllowedSeccompProfileTypes, " or "))
	}
	return fmt.Errorf("%s-level seccompProfile.type = %q, want %s per the restricted Pod Security Standard", level, profile.Type, strings.Join(podAllowedSeccompProfileTypes, " or "))
}

// writableDataVolumeProblem returns why a container whose root filesystem is
// read-only cannot persist Redis data, or nil when it is not a Redis container,
// writes nothing, or mounts a writable volume at /data. redis-server writes its
// RDB snapshot to /data (the redis image's working directory), so a read-only
// root filesystem without that mount stops the container at startup.
func writableDataVolumeProblem(c hardenedPodContainer) error {
	if !strings.Contains(c.Image, "redis") || strings.Contains(c.Image, sidecarImage) {
		return nil
	}
	if c.SecurityContext == nil || c.SecurityContext.ReadOnlyRootFilesystem == nil || !*c.SecurityContext.ReadOnlyRootFilesystem {
		return nil
	}
	declared := make(map[string]bool, len(c.PodVolumes))
	for _, volume := range c.PodVolumes {
		declared[volume.Name] = true
	}
	for _, mount := range c.VolumeMounts {
		if mount.MountPath != "/data" {
			continue
		}
		if mount.ReadOnly != nil && *mount.ReadOnly {
			continue
		}
		if !declared[mount.Name] {
			return fmt.Errorf("volumeMount %q at /data is not backed by a declared pod volume: redis-server writes its RDB snapshot there, so add a volumes entry with that name", mount.Name)
		}
		return nil
	}
	return fmt.Errorf("readOnlyRootFilesystem is true but no writable volume is mounted at /data: redis-server writes its RDB snapshot there and cannot start without one; add a volumeMount at /data backed by a pod volume (an emptyDir is enough)")
}

// restrictedPodContainerProblem returns why one container of an example
// Deployment does not meet the restricted Pod Security Standard, or nil when it
// does. Every field the standard evaluates per container is required here,
// including the fields restrictedSecurityContextProblem already checks, because
// a pod-level block cannot satisfy a per-container field.
func restrictedPodContainerProblem(c hardenedPodContainer) error {
	if err := restrictedSecurityContextProblem(c.sharedSecurityContext()); err != nil {
		return err
	}
	if c.SecurityContext.RunAsUser == nil || *c.SecurityContext.RunAsUser == 0 {
		return fmt.Errorf("runAsUser must pin a non-zero uid: runAsNonRoot alone leaves the process on whatever non-root uid the image defaults to")
	}
	if c.SecurityContext.RunAsGroup == nil || *c.SecurityContext.RunAsGroup == 0 {
		return fmt.Errorf("runAsGroup must pin a non-zero gid: without it the process keeps the image or runtime's default primary group")
	}
	if err := restrictedSeccompProblem(c.SecurityContext.SeccompProfile, c.PodSeccompProfile); err != nil {
		return err
	}
	return writableDataVolumeProblem(c)
}

// Both example deployments must satisfy the restricted Pod Security Standard on
// every container, not only on the labeler sidecar: a namespace enforcing the
// standard rejects the whole pod for one unhardened container, and the README
// tells users to apply these manifests with `kubectl apply`. This is the check
// the sidecar-image-filtered test could not make.
func TestManifestEveryContainerMeetsRestrictedStandard(t *testing.T) {
	containers, deployments, err := podContainers(manifestDir)
	if err != nil {
		t.Fatal(err)
	}
	if deployments != 2 {
		t.Fatalf("Deployment documents = %d, want the two example deployments", deployments)
	}
	if len(containers) != 4 {
		t.Fatalf("containers = %d, want the redis and labeler container of each example deployment", len(containers))
	}

	perDeployment := make(map[string]int)
	for _, c := range containers {
		perDeployment[c.Deployment]++
		if err := restrictedPodContainerProblem(c); err != nil {
			t.Errorf("%s/%s (deployment %q, image %q): %v", c.Manifest, c.Name, c.Deployment, c.Image, err)
		}
	}
	for deployment, count := range perDeployment {
		if count < 2 {
			t.Errorf("deployment %q: %d containers read, want its redis and labeler containers", deployment, count)
		}
	}

	// The hardening must stay identical across containers of one pod: a pod
	// whose redis container and labeler run as different uids is a manifest
	// mistake the profile fields alone would not catch.
	for _, c := range containers {
		if c.SecurityContext == nil || c.SecurityContext.RunAsUser == nil {
			continue
		}
		for _, other := range containers {
			if other.Deployment != c.Deployment || other.Name == c.Name || other.SecurityContext == nil || other.SecurityContext.RunAsUser == nil {
				continue
			}
			if *other.SecurityContext.RunAsUser != *c.SecurityContext.RunAsUser {
				t.Errorf("%s: %s runs as uid %d but %s runs as uid %d: every container of an example pod uses the same restricted profile", c.Manifest, c.Name, *c.SecurityContext.RunAsUser, other.Name, *other.SecurityContext.RunAsUser)
			}
		}
	}
}

// podHardeningFixture is one restricted Deployment. The per-container failure
// fixtures below delete or change exactly one field of it, so the field the
// check reports is the one the fixture removed.
const podHardeningFixture = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: example
spec:
  template:
    spec:
      containers:
      - name: redis
        image: redis:7-alpine
        volumeMounts:
        - name: redis-data
          mountPath: /data
        securityContext:
          runAsNonRoot: true
          runAsUser: 10001
          runAsGroup: 10001
          allowPrivilegeEscalation: false
          readOnlyRootFilesystem: true
          capabilities:
            drop:
            - ALL
          seccompProfile:
            type: RuntimeDefault
      volumes:
      - name: redis-data
        emptyDir: {}
`

// problemsInFixture writes the fixture into a directory of its own, runs the
// reader and the per-container check, and returns the problems found.
func problemsInFixture(t *testing.T, content string) []string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "deployment.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	containers, _, err := podContainers(dir)
	if err != nil {
		t.Fatal(err)
	}
	var problems []string
	for _, c := range containers {
		if problem := restrictedPodContainerProblem(c); problem != nil {
			problems = append(problems, fmt.Sprintf("%s/%s: %v", c.Manifest, c.Name, problem))
		}
	}
	return problems
}

// The failure modes of the per-container rule: every restricted field dropped
// or flipped, a seccomp profile deleted at both levels, and a Redis container
// with a read-only root filesystem but no writable /data mount. Each fixture is
// the restricted shape with one edit, and the unmutated fixture must pass, so
// the edit is what the check caught.
func TestRestrictedPodContainerProblem_RegressionFixtures(t *testing.T) {
	remove := func(snippet string) func(string) string {
		return func(text string) string { return strings.ReplaceAll(text, snippet, "") }
	}
	replace := func(old, new string) func(string) string {
		return func(text string) string { return strings.ReplaceAll(text, old, new) }
	}

	tests := []struct {
		name        string
		mutate      func(string) string
		wantProblem string
	}{
		{
			name: "container as restricted",
		},
		{
			name:        "securityContext deleted",
			mutate:      remove("        securityContext:\n          runAsNonRoot: true\n          runAsUser: 10001\n          runAsGroup: 10001\n          allowPrivilegeEscalation: false\n          readOnlyRootFilesystem: true\n          capabilities:\n            drop:\n            - ALL\n          seccompProfile:\n            type: RuntimeDefault\n"),
			wantProblem: "no securityContext",
		},
		{
			name:        "runAsNonRoot false",
			mutate:      replace("          runAsNonRoot: true\n", "          runAsNonRoot: false\n"),
			wantProblem: "runAsNonRoot",
		},
		{
			name:        "runAsUser deleted",
			mutate:      remove("          runAsUser: 10001\n"),
			wantProblem: "runAsUser",
		},
		{
			name:        "runAsUser 0",
			mutate:      replace("          runAsUser: 10001\n", "          runAsUser: 0\n"),
			wantProblem: "runAsUser",
		},
		{
			name:        "runAsGroup deleted",
			mutate:      remove("          runAsGroup: 10001\n"),
			wantProblem: "runAsGroup",
		},
		{
			name:        "runAsGroup 0",
			mutate:      replace("          runAsGroup: 10001\n", "          runAsGroup: 0\n"),
			wantProblem: "runAsGroup",
		},
		{
			name:        "allowPrivilegeEscalation true",
			mutate:      replace("          allowPrivilegeEscalation: false\n", "          allowPrivilegeEscalation: true\n"),
			wantProblem: "allowPrivilegeEscalation",
		},
		{
			name:        "readOnlyRootFilesystem false",
			mutate:      replace("          readOnlyRootFilesystem: true\n", "          readOnlyRootFilesystem: false\n"),
			wantProblem: "readOnlyRootFilesystem",
		},
		{
			name:        "capabilities kept",
			mutate:      replace("          capabilities:\n            drop:\n            - ALL\n", "          capabilities:\n            add:\n            - NET_ADMIN\n"),
			wantProblem: "capabilities.drop",
		},
		{
			name:        "seccompProfile deleted at both levels",
			mutate:      remove("          seccompProfile:\n            type: RuntimeDefault\n"),
			wantProblem: "no seccompProfile",
		},
		{
			name:        "seccompProfile Unconfined",
			mutate:      replace("            type: RuntimeDefault\n", "            type: Unconfined\n"),
			wantProblem: "RuntimeDefault or Localhost",
		},
		{
			name:        "seccompProfile declared with no type",
			mutate:      replace("            type: RuntimeDefault\n", "            type: \"\"\n"),
			wantProblem: "no type",
		},
		{
			// The standard accepts the profile on the pod spec, which the
			// container inherits when it declares none of its own.
			name: "seccompProfile on the pod spec",
			mutate: func(text string) string {
				text = strings.Replace(text, "          seccompProfile:\n            type: RuntimeDefault\n", "", 1)
				return strings.Replace(text, "      containers:\n", "      securityContext:\n        seccompProfile:\n          type: RuntimeDefault\n      containers:\n", 1)
			},
		},
		{
			name:        "writable /data mount deleted",
			mutate:      remove("        volumeMounts:\n        - name: redis-data\n          mountPath: /data\n"),
			wantProblem: "/data",
		},
		{
			name:        "/data mounted elsewhere",
			mutate:      replace("          mountPath: /data\n", "          mountPath: /tmp\n"),
			wantProblem: "/data",
		},
		{
			name:        "/data mount read-only",
			mutate:      replace("          mountPath: /data\n", "          mountPath: /data\n          readOnly: true\n"),
			wantProblem: "/data",
		},
		{
			name:        "/data mount names an undeclared volume",
			mutate:      replace("        - name: redis-data\n", "        - name: other-data\n"),
			wantProblem: "not backed by a declared pod volume",
		},
		{
			name:        "pod volume deleted",
			mutate:      remove("      volumes:\n      - name: redis-data\n        emptyDir: {}\n"),
			wantProblem: "not backed by a declared pod volume",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content := podHardeningFixture
			if tt.mutate != nil {
				content = tt.mutate(content)
			}
			problems := problemsInFixture(t, content)
			if tt.wantProblem == "" {
				if len(problems) != 0 {
					t.Fatalf("fixture reported: %v, want none", problems)
				}
				return
			}
			if len(problems) == 0 {
				t.Fatalf("a container that lost %s was accepted, want a problem reported", tt.wantProblem)
			}
			if !strings.Contains(strings.Join(problems, "\n"), tt.wantProblem) {
				t.Fatalf("problems = %v, want one naming %q", problems, tt.wantProblem)
			}
		})
	}
}

// The reader must return every container of a pod, not just the ones whose
// image matches the sidecar, and non-Pod documents must contribute nothing
// instead of being counted. A missing manifest directory is an error, so an
// empty pass cannot be mistaken for a hardened manifest set.
func TestPodContainers_ReaderFixtures(t *testing.T) {
	const mixedDeployment = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: mixed
spec:
  template:
    spec:
      containers:
      - name: redis
        image: redis:7-alpine
        volumeMounts:
        - name: redis-data
          mountPath: /data
        securityContext:
          runAsNonRoot: true
          runAsUser: 10001
          runAsGroup: 10001
          allowPrivilegeEscalation: false
          readOnlyRootFilesystem: true
          capabilities:
            drop:
            - ALL
          seccompProfile:
            type: RuntimeDefault
      - name: helper
        image: busybox:1.36
      volumes:
      - name: redis-data
        emptyDir: {}
`
	dir := t.TempDir()
	writeManifestFile(t, dir, "deployment.yaml", mixedDeployment)
	writeManifestFile(t, dir, "service.yaml", "apiVersion: v1\nkind: Service\nmetadata:\n  name: redis-leader\n")

	containers, deployments, err := podContainers(dir)
	if err != nil {
		t.Fatal(err)
	}
	if deployments != 1 {
		t.Fatalf("deployments = %d, want 1: non-Pod documents contribute nothing", deployments)
	}
	if len(containers) != 2 {
		t.Fatalf("containers = %d, want the redis and helper container: the reader must not filter by sidecar image", len(containers))
	}

	// The hardened redis container beside the unhardened helper must pass, so
	// the one reported problem names the container that actually lost its
	// profile rather than any container the reader happened to return.
	problems := problemsInFixture(t, mixedDeployment)
	if len(problems) != 1 || !strings.Contains(problems[0], "/helper") {
		t.Fatalf("problems = %v, want the unhardened helper container reported: the redis container beside it is hardened", problems)
	}

	if _, _, err := podContainers(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing manifest directory accepted, want a read error")
	}
}
