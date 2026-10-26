package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// Pod-level hardening check: the restricted Pod Security Standard is enforced
// against every container of a pod, but the manifest checks in main_test.go and
// sidecar_hardening_manifest_test.go select containers by the sidecar image.
// That filter is how both example deployments once shipped an unhardened
// `redis` container next to the labeler - no securityContext, uid 0, every
// capability kept, no seccompProfile - while `go test ./...` stayed green:
// kubeconform only validates the schema (where a securityContext is optional)
// and no test ever looked at a container that is not the sidecar.
//
// This file closes that gap at pod level. It decodes every container of every
// Deployment under manifests/ - both `containers` and `initContainers`, not
// only the one matching sidecarImage - and holds each to the restricted shape:
// a non-root user, no privilege escalation,
// a read-only root filesystem, every capability dropped and a RuntimeDefault (or
// Localhost) seccompProfile at container or pod level. It also requires a
// writable volume at /data once a redis container has a read-only root
// filesystem, because redis-server writes its RDB snapshot there and the mount
// is the difference between an admissible pod and one that CrashLoops.
//
// Both example manifests now declare the full restricted profile on every
// container and the /data emptyDir the redis containers need.
// knownUnhardenedContainers is empty and stays shrink-only: a container that
// loses its profile fails the build, and any entry left behind after a fix
// fails until it is deleted.

// restrictedSeccompProfile is a securityContext.seccompProfile block, decoded at
// container level and at pod level (the two levels the standard accepts).
type restrictedSeccompProfile struct {
	Type string `json:"type"`
}

// restrictedCapabilities is the capabilities subset the standard constrains.
type restrictedCapabilities struct {
	Drop []string `json:"drop"`
}

// restrictedContainerSecurity is the securityContext subset the restricted Pod
// Security Standard is enforced on, decoded for every container. It is a second
// decode of the same manifest key as containerSecurityContext in
// cmd_manifest_check.go: that reader is only reached through the sidecar image
// filter and has no seccompProfile field, and both facts are how the redis
// container stayed unchecked.
type restrictedContainerSecurity struct {
	RunAsNonRoot             *bool                     `json:"runAsNonRoot"`
	RunAsUser                *int64                    `json:"runAsUser"`
	RunAsGroup               *int64                    `json:"runAsGroup"`
	AllowPrivilegeEscalation *bool                     `json:"allowPrivilegeEscalation"`
	ReadOnlyRootFilesystem   *bool                     `json:"readOnlyRootFilesystem"`
	Capabilities             restrictedCapabilities    `json:"capabilities"`
	SeccompProfile           *restrictedSeccompProfile `json:"seccompProfile"`
}

// restrictedVolumeMount is one volumeMount of a container.
type restrictedVolumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	ReadOnly  bool   `json:"readOnly"`
}

// restrictedVolume is one volume of a pod spec. Only the name is read: any
// volume type (an emptyDir for a demo, a PersistentVolumeClaim for durable
// snapshots) can back the writable mount, so the check reports a mount whose
// volume is missing rather than prescribing storage.
type restrictedVolume struct {
	Name string `json:"name"`
}

// restrictedContainer is one container of an example pod, with the manifest and
// pod it belongs to so a failure names the file that has to be edited. Init is
// true for an initContainer: the restricted Pod Security Standard is enforced
// on those too, so they are checked with the same rule.
type restrictedContainer struct {
	Manifest        string
	Pod             string
	Name            string
	Image           string
	Init            bool
	SecurityContext *restrictedContainerSecurity
	VolumeMounts    []restrictedVolumeMount
}

// restrictedPod is one Deployment's pod template, reduced to the fields the
// restricted standard decides on.
type restrictedPod struct {
	Manifest       string
	Name           string
	SeccompProfile *restrictedSeccompProfile
	Containers     []restrictedContainer
	Volumes        []restrictedVolume
}

// restrictedContainerSpec is one entry of a pod spec's `containers` or
// `initContainers` list, reduced to the fields the check decides on.
type restrictedContainerSpec struct {
	Name            string                       `json:"name"`
	Image           string                       `json:"image"`
	SecurityContext *restrictedContainerSecurity `json:"securityContext"`
	VolumeMounts    []restrictedVolumeMount      `json:"volumeMounts"`
}

// restrictedPodDocument is the manifest subset restrictedExamplePods decodes.
type restrictedPodDocument struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Template struct {
			Spec struct {
				SecurityContext *struct {
					SeccompProfile *restrictedSeccompProfile `json:"seccompProfile"`
				} `json:"securityContext"`
				InitContainers []restrictedContainerSpec `json:"initContainers"`
				Containers     []restrictedContainerSpec `json:"containers"`
				Volumes        []restrictedVolume        `json:"volumes"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
}

// restrictedExamplePods returns every Deployment document across manifests/*.yaml
// with all of its containers - init containers first, then the regular ones -
// walking the same manifest set (and tolerating the same non-Pod-spec
// documents) as sidecarContainers. Unlike that reader it does not filter by
// image, which is exactly the gap this check exists to close.
func restrictedExamplePods(dir string) ([]restrictedPod, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var pods []restrictedPod
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
		var doc restrictedPodDocument
		if err := json.Unmarshal(jsonData, &doc); err != nil {
			return nil, fmt.Errorf("%s: JSON decode: %w", path, err)
		}
		if doc.Kind != deploymentKind {
			continue
		}
		pod := restrictedPod{
			Manifest: entry.Name(),
			Name:     doc.Metadata.Name,
			Volumes:  doc.Spec.Template.Spec.Volumes,
		}
		if sc := doc.Spec.Template.Spec.SecurityContext; sc != nil {
			pod.SeccompProfile = sc.SeccompProfile
		}
		for _, list := range []struct {
			init       bool
			containers []restrictedContainerSpec
		}{
			{init: true, containers: doc.Spec.Template.Spec.InitContainers},
			{init: false, containers: doc.Spec.Template.Spec.Containers},
		} {
			for _, c := range list.containers {
				pod.Containers = append(pod.Containers, restrictedContainer{
					Manifest:        entry.Name(),
					Pod:             doc.Metadata.Name,
					Name:            c.Name,
					Image:           c.Image,
					Init:            list.init,
					SecurityContext: c.SecurityContext,
					VolumeMounts:    c.VolumeMounts,
				})
			}
		}
		pods = append(pods, pod)
	}
	return pods, nil
}

// containerKey names one container across the manifest set, uniquely enough for
// a baseline entry: file, pod, container and, for an init container, its list.
// Kubernetes lets an init container and an app container share a name, so the
// list has to be part of the key or one could hide behind the other.
func containerKey(c restrictedContainer) string {
	if c.Init {
		return fmt.Sprintf("%s/%s/init:%s", c.Manifest, c.Pod, c.Name)
	}
	return fmt.Sprintf("%s/%s/%s", c.Manifest, c.Pod, c.Name)
}

// allowedRestrictedSeccompTypes are the seccompProfile types the restricted Pod
// Security Standard accepts. Unconfined, an empty type and no profile at all are
// rejected.
var allowedRestrictedSeccompTypes = []string{"RuntimeDefault", "Localhost"}

// restrictedContainerProblem returns why a container does not satisfy the
// restricted Pod Security Standard, or nil when it does. The pod-level
// seccompProfile is accepted as the fallback for a container that declares no
// profile of its own, because that is how the standard is written. Every other
// field is read from the container itself: the standard is enforced per
// container, so a pod-level block cannot make an unhardened container pass.
func restrictedContainerProblem(c restrictedContainer, podProfile *restrictedSeccompProfile) error {
	where := containerKey(c)
	sc := c.SecurityContext
	if sc == nil {
		return fmt.Errorf("%s: declares no securityContext, so the container runs with the image defaults (uid 0 for a root image), can gain privileges, keeps every capability and writes its root filesystem. The restricted Pod Security Standard is enforced per container, so the labeler's securityContext does not cover it: declare the same restricted block on this container", where)
	}
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		return fmt.Errorf("%s: runAsNonRoot must be true: without it the restricted Pod Security Standard rejects the pod and the container runs as uid 0 when the image sets no USER", where)
	}
	if sc.RunAsUser != nil && *sc.RunAsUser == 0 {
		return fmt.Errorf("%s: runAsUser = 0 contradicts runAsNonRoot and is rejected by the restricted Pod Security Standard", where)
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		return fmt.Errorf("%s: allowPrivilegeEscalation must be false: an unset field keeps the runtime default (true), so a setuid binary could gain privileges", where)
	}
	if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		return fmt.Errorf("%s: readOnlyRootFilesystem must be true: a writable root filesystem lets the container modify its image (and redis writes its snapshots outside a mounted volume)", where)
	}
	if !dropsAllCapabilities(sc.Capabilities.Drop) {
		return fmt.Errorf("%s: capabilities.drop must include \"ALL\": a kept capability is a kernel power the container did not need", where)
	}

	profile, level := sc.SeccompProfile, "container"
	if profile == nil {
		profile, level = podProfile, "pod"
	}
	if profile == nil {
		return fmt.Errorf("%s: no seccompProfile on the container or on its pod: the container keeps the runtime's default profile (Unconfined on most runtimes), which the restricted Pod Security Standard forbids; declare seccompProfile.type RuntimeDefault", where)
	}
	if profile.Type == "" {
		return fmt.Errorf("%s: %s-level seccompProfile declares no type, want %s per the restricted Pod Security Standard", where, level, strings.Join(allowedRestrictedSeccompTypes, " or "))
	}
	for _, allowed := range allowedRestrictedSeccompTypes {
		if profile.Type == allowed {
			return nil
		}
	}
	return fmt.Errorf("%s: %s-level seccompProfile.type = %q, want %s per the restricted Pod Security Standard (Unconfined is the profile the standard forbids)", where, level, profile.Type, strings.Join(allowedRestrictedSeccompTypes, " or "))
}

// redisSnapshotDir is where redis-server writes its RDB snapshots: the official
// redis image's WORKDIR and the directory its default `dir` points at.
const redisSnapshotDir = "/data"

// isRedisServerContainer reports whether a container runs the co-located Redis,
// selected by its image reference. The labeler's image contains
// "redis-master-label" but not "redis:", so the sidecar is not misread as Redis.
func isRedisServerContainer(c restrictedContainer) bool {
	return strings.Contains(c.Image, "redis:")
}

// redisSnapshotStorageProblem returns why a redis container cannot persist its
// snapshot despite a read-only root filesystem, or nil when it can. redis-server
// writes under /data, so readOnlyRootFilesystem: true must be paired with a
// writable mount there; otherwise the server fails at startup with a read-only
// filesystem error and the pod that the hardening block made admissible
// CrashLoops. A container whose root filesystem is still writable needs no
// mount, so this rule only bites once the hardening is in place.
func redisSnapshotStorageProblem(c restrictedContainer, volumes []restrictedVolume) error {
	if !isRedisServerContainer(c) {
		return nil
	}
	if c.SecurityContext == nil || c.SecurityContext.ReadOnlyRootFilesystem == nil || !*c.SecurityContext.ReadOnlyRootFilesystem {
		return nil
	}
	where := containerKey(c)

	var mount *restrictedVolumeMount
	for i := range c.VolumeMounts {
		if c.VolumeMounts[i].MountPath == redisSnapshotDir {
			mount = &c.VolumeMounts[i]
		}
	}
	if mount == nil {
		return fmt.Errorf("%s: no volumeMount at %s, but redis-server writes its RDB snapshot there and the root filesystem is read-only: mount an emptyDir (or a PersistentVolumeClaim) at %s, or the server exits at startup with a read-only filesystem error", where, redisSnapshotDir, redisSnapshotDir)
	}
	if mount.ReadOnly {
		return fmt.Errorf("%s: the %s volumeMount is read-only, but redis-server writes its snapshot there: remove readOnly: true from the mount", where, redisSnapshotDir)
	}
	for _, volume := range volumes {
		if volume.Name == mount.Name {
			return nil
		}
	}
	return fmt.Errorf("%s: volumeMount %q at %s names no volume in the pod spec: declare the volume (an emptyDir for a demo, a PersistentVolumeClaim for durable data) or the pod fails to be admitted", where, mount.Name, redisSnapshotDir)
}

// knownUnhardenedContainers records the example containers that still do not
// satisfy the restricted Pod Security Standard, keyed by containerKey. No
// container is recorded now: both example manifests declare the restricted
// profile on every container. The list stays because it is shrink-only and
// guards the fix - it fails the build the moment a container loses its
// hardening while still listed, and a listed container that becomes hardened
// fails until its entry is deleted, so an entry cannot outlive the gap it
// records.
var knownUnhardenedContainers = map[string]string{}

// unhardenedContainerProblems returns one message per container of the given pods
// that does not satisfy the restricted Pod Security Standard and is not recorded
// in the baseline, plus one message per baseline entry that no longer matches an
// unhardened container. It is the guard both the shipped manifests and the
// fixtures run through, so a checker that always agrees cannot pass.
func unhardenedContainerProblems(pods []restrictedPod, baseline map[string]string) []string {
	var problems []string
	matched := make(map[string]bool, len(baseline))

	for _, pod := range pods {
		for _, c := range pod.Containers {
			key := containerKey(c)
			problem := restrictedContainerProblem(c, pod.SeccompProfile)
			recorded, listed := baseline[key]
			if !listed {
				if problem != nil {
					problems = append(problems, problem.Error())
				}
				continue
			}
			matched[key] = true
			if problem == nil {
				problems = append(problems, fmt.Sprintf("%s satisfies the restricted Pod Security Standard now, so it must not stay in knownUnhardenedContainers (recorded as: %s): delete the entry, the list is shrink-only", key, recorded))
			}
		}
	}

	keys := make([]string, 0, len(baseline))
	for key := range baseline {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if matched[key] {
			continue
		}
		problems = append(problems, fmt.Sprintf("knownUnhardenedContainers lists %s, which is not an unhardened container of any example pod (recorded as: %s): a stale entry would hide a container that was renamed or deleted", key, baseline[key]))
	}
	return problems
}

// Every container of every example pod must satisfy the restricted Pod Security
// Standard, not only the labeler. Both example manifests declare the full
// restricted profile on the co-located redis containers as well, so a namespace
// enforcing the standard accepts the pods. A newly unhardened container, or a
// stray baseline entry, fails here.
func TestExampleManifestsEveryContainerMeetsRestrictedStandard(t *testing.T) {
	pods, err := restrictedExamplePods(manifestDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(pods) != 2 {
		t.Fatalf("Deployment documents across manifests = %d, want the two example deployments: the check must read both", len(pods))
	}
	containers := 0
	for _, pod := range pods {
		containers += len(pod.Containers)
	}
	if containers != 4 {
		t.Fatalf("containers across the example deployments = %d, want 4 (redis and redis-master-label in each): the restricted standard is enforced per container, not per sidecar", containers)
	}

	for _, problem := range unhardenedContainerProblems(pods, knownUnhardenedContainers) {
		t.Errorf("%s", problem)
	}
}

// Both redis containers keep a writable mount for their snapshots once their
// root filesystem is read-only, so the hardening the standard requires does not
// stop redis-server from writing its RDB file.
func TestExampleManifestsMountWritableRedisData(t *testing.T) {
	pods, err := restrictedExamplePods(manifestDir)
	if err != nil {
		t.Fatal(err)
	}
	redisContainers := 0
	for _, pod := range pods {
		for _, c := range pod.Containers {
			if !isRedisServerContainer(c) {
				continue
			}
			redisContainers++
			if problem := redisSnapshotStorageProblem(c, pod.Volumes); problem != nil {
				t.Errorf("%s", problem)
			}
		}
	}
	if redisContainers != 2 {
		t.Fatalf("redis containers across the example deployments = %d, want the two co-located redis containers: the snapshot check must read both", redisContainers)
	}
}

// restrictedBoolPtr and restrictedInt64Ptr let a fixture start from a container
// that satisfies the standard and change one field at a time.
func restrictedBoolPtr(v bool) *bool    { return &v }
func restrictedInt64Ptr(v int64) *int64 { return &v }

// restrictedFixtureContainer returns a container that satisfies the restricted
// Pod Security Standard, so a fixture isolates the one field a test removes.
func restrictedFixtureContainer(manifest, pod, name, image string) restrictedContainer {
	return restrictedContainer{
		Manifest: manifest,
		Pod:      pod,
		Name:     name,
		Image:    image,
		SecurityContext: &restrictedContainerSecurity{
			RunAsNonRoot:             restrictedBoolPtr(true),
			RunAsUser:                restrictedInt64Ptr(10001),
			RunAsGroup:               restrictedInt64Ptr(10001),
			AllowPrivilegeEscalation: restrictedBoolPtr(false),
			ReadOnlyRootFilesystem:   restrictedBoolPtr(true),
			Capabilities:             restrictedCapabilities{Drop: []string{"ALL"}},
			SeccompProfile:           &restrictedSeccompProfile{Type: "RuntimeDefault"},
		},
	}
}

// The failure modes of the per-container rule: every restricted field is
// required, at container level, with the pod-level seccompProfile as the only
// fallback the standard allows.
func TestRestrictedContainerProblem_RegressionFixtures(t *testing.T) {
	const manifest, pod = "deployment-example.yaml", "redis-with-labeler"
	base := func() restrictedContainer {
		return restrictedFixtureContainer(manifest, pod, "redis", "redis:7-alpine")
	}

	tests := []struct {
		name        string
		mutate      func(c *restrictedContainer)
		podProfile  *restrictedSeccompProfile
		wantProblem string
	}{
		{name: "restricted container", mutate: func(c *restrictedContainer) {}},
		{
			name:        "no securityContext",
			mutate:      func(c *restrictedContainer) { c.SecurityContext = nil },
			wantProblem: "declares no securityContext",
		},
		{
			name:        "runAsNonRoot absent",
			mutate:      func(c *restrictedContainer) { c.SecurityContext.RunAsNonRoot = nil },
			wantProblem: "runAsNonRoot must be true",
		},
		{
			name:        "runAsNonRoot false",
			mutate:      func(c *restrictedContainer) { c.SecurityContext.RunAsNonRoot = restrictedBoolPtr(false) },
			wantProblem: "runAsNonRoot must be true",
		},
		{
			name:        "runAsUser root",
			mutate:      func(c *restrictedContainer) { c.SecurityContext.RunAsUser = restrictedInt64Ptr(0) },
			wantProblem: "runAsUser = 0",
		},
		{
			name:        "allowPrivilegeEscalation absent",
			mutate:      func(c *restrictedContainer) { c.SecurityContext.AllowPrivilegeEscalation = nil },
			wantProblem: "allowPrivilegeEscalation must be false",
		},
		{
			name:        "allowPrivilegeEscalation true",
			mutate:      func(c *restrictedContainer) { c.SecurityContext.AllowPrivilegeEscalation = restrictedBoolPtr(true) },
			wantProblem: "allowPrivilegeEscalation must be false",
		},
		{
			name:        "readOnlyRootFilesystem absent",
			mutate:      func(c *restrictedContainer) { c.SecurityContext.ReadOnlyRootFilesystem = nil },
			wantProblem: "readOnlyRootFilesystem must be true",
		},
		{
			name:        "readOnlyRootFilesystem false",
			mutate:      func(c *restrictedContainer) { c.SecurityContext.ReadOnlyRootFilesystem = restrictedBoolPtr(false) },
			wantProblem: "readOnlyRootFilesystem must be true",
		},
		{
			name:        "capabilities kept",
			mutate:      func(c *restrictedContainer) { c.SecurityContext.Capabilities.Drop = nil },
			wantProblem: "capabilities.drop must include",
		},
		{
			name:        "only some capabilities dropped",
			mutate:      func(c *restrictedContainer) { c.SecurityContext.Capabilities.Drop = []string{"NET_BIND_SERVICE"} },
			wantProblem: "capabilities.drop must include",
		},
		{
			name:        "seccompProfile absent",
			mutate:      func(c *restrictedContainer) { c.SecurityContext.SeccompProfile = nil },
			wantProblem: "no seccompProfile on the container or on its pod",
		},
		{
			name: "seccompProfile Unconfined",
			mutate: func(c *restrictedContainer) {
				c.SecurityContext.SeccompProfile = &restrictedSeccompProfile{Type: "Unconfined"}
			},
			wantProblem: "Unconfined is the profile the standard forbids",
		},
		{
			name: "seccompProfile without type",
			mutate: func(c *restrictedContainer) {
				c.SecurityContext.SeccompProfile = &restrictedSeccompProfile{}
			},
			wantProblem: "declares no type",
		},
		{
			name:       "pod-level RuntimeDefault covers the container",
			mutate:     func(c *restrictedContainer) { c.SecurityContext.SeccompProfile = nil },
			podProfile: &restrictedSeccompProfile{Type: "RuntimeDefault"},
		},
		{
			name:       "pod-level Localhost profile is accepted",
			mutate:     func(c *restrictedContainer) { c.SecurityContext.SeccompProfile = nil },
			podProfile: &restrictedSeccompProfile{Type: "Localhost"},
		},
		{
			name:        "pod-level Unconfined profile is rejected",
			mutate:      func(c *restrictedContainer) { c.SecurityContext.SeccompProfile = nil },
			podProfile:  &restrictedSeccompProfile{Type: "Unconfined"},
			wantProblem: "Unconfined is the profile the standard forbids",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			container := base()
			tt.mutate(&container)
			err := restrictedContainerProblem(container, tt.podProfile)
			if tt.wantProblem == "" {
				if err != nil {
					t.Fatalf("problem = %v, want none: a container that satisfies the restricted standard must be accepted", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("container losing %q was accepted, want a problem reported", tt.wantProblem)
			}
			if !strings.Contains(err.Error(), tt.wantProblem) {
				t.Fatalf("problem = %q, want it to name %q", err, tt.wantProblem)
			}
		})
	}
}

// The baseline guard is what lets the hardened pods be required: it must report
// an unhardened container that is not listed, accept a listed one, and refuse to
// keep an entry that no longer describes a gap.
func TestKnownUnhardenedBaseline_RegressionFixtures(t *testing.T) {
	const manifest, pod = "deployment-example.yaml", "redis-with-labeler"
	key := manifest + "/" + pod + "/redis"

	unhardenedRedis := restrictedContainer{
		Manifest: manifest,
		Pod:      pod,
		Name:     "redis",
		Image:    "redis:7-alpine",
	}
	hardenedRedis := restrictedFixtureContainer(manifest, pod, "redis", "redis:7-alpine")
	hardenedLabeler := restrictedFixtureContainer(manifest, pod, "redis-master-label", "ghcr.io/redis-master-label/redis-master-label:v0.1.0")
	unhardenedLabeler := hardenedLabeler
	unhardenedLabeler.SecurityContext = nil

	pods := func(containers ...restrictedContainer) []restrictedPod {
		return []restrictedPod{{Manifest: manifest, Name: pod, Containers: containers}}
	}

	tests := []struct {
		name        string
		pods        []restrictedPod
		baseline    map[string]string
		wantProblem string
	}{
		{
			name:        "unhardened container without a baseline entry",
			pods:        pods(unhardenedRedis, hardenedLabeler),
			wantProblem: "declares no securityContext",
		},
		{
			name:     "unhardened container recorded in the baseline",
			pods:     pods(unhardenedRedis, hardenedLabeler),
			baseline: map[string]string{key: "no securityContext"},
		},
		{
			name:        "baseline entry left behind after the container was hardened",
			pods:        pods(hardenedRedis, hardenedLabeler),
			baseline:    map[string]string{key: "no securityContext"},
			wantProblem: "must not stay in knownUnhardenedContainers",
		},
		{
			name:        "baseline entry naming no container",
			pods:        pods(hardenedLabeler),
			baseline:    map[string]string{key: "no securityContext"},
			wantProblem: "not an unhardened container of any example pod",
		},
		{
			name:        "a second container loses hardening",
			pods:        pods(unhardenedRedis, unhardenedLabeler),
			baseline:    map[string]string{key: "no securityContext"},
			wantProblem: "redis-master-label: declares no securityContext",
		},
		{
			name:     "every container hardened needs no baseline",
			pods:     pods(hardenedRedis, hardenedLabeler),
			baseline: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			problems := unhardenedContainerProblems(tt.pods, tt.baseline)
			if tt.wantProblem == "" {
				if len(problems) != 0 {
					t.Fatalf("problems = %v, want none", problems)
				}
				return
			}
			if len(problems) == 0 {
				t.Fatalf("want a problem naming %q, got none", tt.wantProblem)
			}
			if !strings.Contains(strings.Join(problems, "\n"), tt.wantProblem) {
				t.Fatalf("problems = %v, want one naming %q", problems, tt.wantProblem)
			}
		})
	}
}

// redisSnapshotStorageProblem decides whether a read-only redis container can
// still write its RDB file.
func TestRedisSnapshotStorageProblem_RegressionFixtures(t *testing.T) {
	const manifest, pod = "deployment-example.yaml", "redis-with-labeler"
	writableRoot := func() restrictedContainer {
		c := restrictedFixtureContainer(manifest, pod, "redis", "redis:7-alpine")
		c.SecurityContext.ReadOnlyRootFilesystem = nil
		return c
	}
	readOnlyRoot := func() restrictedContainer {
		return restrictedFixtureContainer(manifest, pod, "redis", "redis:7-alpine")
	}
	withMount := func(c restrictedContainer, mount restrictedVolumeMount) restrictedContainer {
		c.VolumeMounts = []restrictedVolumeMount{mount}
		return c
	}

	tests := []struct {
		name        string
		container   restrictedContainer
		volumes     []restrictedVolume
		wantProblem string
	}{
		{
			name:      "writable root filesystem needs no mount",
			container: writableRoot(),
		},
		{
			name:        "read-only root filesystem without a mount",
			container:   readOnlyRoot(),
			wantProblem: "no volumeMount at /data",
		},
		{
			name:        "read-only snapshot mount",
			container:   withMount(readOnlyRoot(), restrictedVolumeMount{Name: "redis-data", MountPath: redisSnapshotDir, ReadOnly: true}),
			volumes:     []restrictedVolume{{Name: "redis-data"}},
			wantProblem: "is read-only",
		},
		{
			name:        "mount without a volume",
			container:   withMount(readOnlyRoot(), restrictedVolumeMount{Name: "redis-data", MountPath: redisSnapshotDir}),
			wantProblem: "names no volume in the pod spec",
		},
		{
			name:      "writable mount backed by an emptyDir",
			container: withMount(readOnlyRoot(), restrictedVolumeMount{Name: "redis-data", MountPath: redisSnapshotDir}),
			volumes:   []restrictedVolume{{Name: "redis-data"}},
		},
		{
			name:      "the labeler does not snapshot",
			container: restrictedFixtureContainer(manifest, pod, "redis-master-label", "ghcr.io/redis-master-label/redis-master-label:v0.1.0"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := redisSnapshotStorageProblem(tt.container, tt.volumes)
			if tt.wantProblem == "" {
				if err != nil {
					t.Fatalf("problem = %v, want none", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want a problem naming %q, got none", tt.wantProblem)
			}
			if !strings.Contains(err.Error(), tt.wantProblem) {
				t.Fatalf("problem = %q, want it to name %q", err, tt.wantProblem)
			}
		})
	}
}

// The reader must return every container of the pod, not only the sidecar. This
// fixture recreates the reviewed pre-fix shape - a hardened labeler beside a
// redis container that declares no securityContext - and a reader that filtered
// by image reported nothing for the redis container. This is the regression the
// reviewed finding describes.
func TestRestrictedExamplePods_ReportsEveryContainerOfThePod(t *testing.T) {
	dir := t.TempDir()
	writeRestrictedFixtureManifest(t, dir, "deployment.yaml",
		restrictedFixtureDeployment("redis-leader",
			"      - name: redis\n        image: redis:7-alpine\n",
			restrictedFixtureLabelerContainer))

	pods, err := restrictedExamplePods(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(pods) != 1 || len(pods[0].Containers) != 2 {
		t.Fatalf("pods = %d, containers = %d, want the one Deployment and both of its containers", len(pods), len(pods[0].Containers))
	}
	for _, c := range pods[0].Containers {
		problem := restrictedContainerProblem(c, pods[0].SeccompProfile)
		if c.Name == "redis" {
			if problem == nil {
				t.Fatal("the unhardened redis container was accepted, want the pod-level check to report it")
			}
			if !strings.Contains(problem.Error(), "declares no securityContext") {
				t.Fatalf("problem = %q, want it to name the missing securityContext", problem)
			}
			continue
		}
		if problem != nil {
			t.Errorf("the hardened %s container = %v, want it accepted", c.Name, problem)
		}
	}
}

// Init containers are containers to the restricted Pod Security Standard, so a
// pod whose app containers are hardened but whose init container is not is
// rejected. The reader has to return them or the guard misses that container the
// same way the image-filtered sidecar checks missed the redis container.
func TestRestrictedExamplePods_ChecksInitContainers(t *testing.T) {
	dir := t.TempDir()
	writeRestrictedFixtureManifest(t, dir, "deployment.yaml",
		"apiVersion: apps/v1\n"+
			"kind: Deployment\n"+
			"metadata:\n"+
			"  name: with-init\n"+
			"spec:\n"+
			"  template:\n"+
			"    spec:\n"+
			"      initContainers:\n"+
			"      - name: setup\n"+
			"        image: busybox:1.36\n"+
			"      containers:\n"+
			restrictedFixtureLabelerContainer)

	pods, err := restrictedExamplePods(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(pods) != 1 || len(pods[0].Containers) != 2 {
		t.Fatalf("pods = %d, containers = %d, want the one Deployment and its init plus app container", len(pods), len(pods[0].Containers))
	}
	if !pods[0].Containers[0].Init || pods[0].Containers[0].Name != "setup" {
		t.Fatalf("first container = %+v, want the init container setup read before the app containers", pods[0].Containers[0])
	}
	problem := restrictedContainerProblem(pods[0].Containers[0], pods[0].SeccompProfile)
	if problem == nil {
		t.Fatal("an init container without a securityContext was accepted, want the pod-level check to report it")
	}
	if !strings.Contains(problem.Error(), "declares no securityContext") {
		t.Fatalf("problem = %q, want it to name the missing securityContext", problem)
	}
	if !strings.Contains(problem.Error(), "init:setup") {
		t.Fatalf("problem = %q, want it to name the init container so the failing list is unambiguous", problem)
	}
	if problem := restrictedContainerProblem(pods[0].Containers[1], pods[0].SeccompProfile); problem != nil {
		t.Errorf("the hardened %s container = %v, want it accepted", pods[0].Containers[1].Name, problem)
	}
}

// A pod-level seccompProfile covers a container that declares none; a document
// without a Pod spec contributes no container; and a missing directory is an
// error rather than an empty, agreeing read.
func TestRestrictedExamplePods_SeccompFallbackAndNonPodDocuments(t *testing.T) {
	root := t.TempDir()

	podLevel := "apiVersion: apps/v1\n" +
		"kind: Deployment\n" +
		"metadata:\n" +
		"  name: pod-level\n" +
		"spec:\n" +
		"  template:\n" +
		"    spec:\n" +
		"      securityContext:\n" +
		"        seccompProfile:\n" +
		"          type: RuntimeDefault\n" +
		"      containers:\n" +
		"      - name: labeler\n" +
		"        image: ghcr.io/redis-master-label/redis-master-label:v0.1.0\n" +
		"        securityContext:\n" +
		"          runAsNonRoot: true\n" +
		"          allowPrivilegeEscalation: false\n" +
		"          readOnlyRootFilesystem: true\n" +
		"          capabilities:\n" +
		"            drop:\n" +
		"            - ALL\n"

	dir := filepath.Join(root, "pod-level")
	writeRestrictedFixtureManifest(t, dir, "deployment.yaml", podLevel)

	pods, err := restrictedExamplePods(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(pods) != 1 || len(pods[0].Containers) != 1 {
		t.Fatalf("pods = %d, containers = %d, want 1 and 1", len(pods), len(pods[0].Containers))
	}
	if pods[0].SeccompProfile == nil || pods[0].SeccompProfile.Type != "RuntimeDefault" {
		t.Fatalf("pod seccompProfile = %+v, want the manifest's RuntimeDefault", pods[0].SeccompProfile)
	}
	if err := restrictedContainerProblem(pods[0].Containers[0], pods[0].SeccompProfile); err != nil {
		t.Errorf("a container covered by its pod's RuntimeDefault profile = %v, want it accepted: the standard accepts the profile at either level", err)
	}

	inherited := strings.Replace(podLevel, "          type: RuntimeDefault\n", "          type: Unconfined\n", 1)
	dir = filepath.Join(root, "inherited")
	writeRestrictedFixtureManifest(t, dir, "deployment.yaml", inherited)
	pods, err = restrictedExamplePods(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := restrictedContainerProblem(pods[0].Containers[0], pods[0].SeccompProfile); err == nil {
		t.Error("a container inheriting an Unconfined profile from its pod was accepted, want a problem")
	}

	writeRestrictedFixtureManifest(t, dir, "service.yaml", "apiVersion: v1\nkind: Service\nmetadata:\n  name: redis-leader\n")
	pods, err = restrictedExamplePods(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(pods) != 1 || len(pods[0].Containers) != 1 {
		t.Fatalf("pods = %d, containers = %d, want the one Deployment and its one container", len(pods), len(pods[0].Containers))
	}

	if _, err := restrictedExamplePods(filepath.Join(root, "missing")); err == nil {
		t.Error("missing manifest directory accepted, want a read error")
	}
}

// restrictedFixtureLabelerContainer is a hardened labeler container, in the
// manifest text shape the example deployments use.
const restrictedFixtureLabelerContainer = "      - name: redis-master-label\n" +
	"        image: ghcr.io/redis-master-label/redis-master-label:v0.1.0\n" +
	"        securityContext:\n" +
	"          runAsNonRoot: true\n" +
	"          runAsUser: 10001\n" +
	"          runAsGroup: 10001\n" +
	"          allowPrivilegeEscalation: false\n" +
	"          readOnlyRootFilesystem: true\n" +
	"          capabilities:\n" +
	"            drop:\n" +
	"            - ALL\n" +
	"          seccompProfile:\n" +
	"            type: RuntimeDefault\n"

// restrictedFixtureDeployment assembles a Deployment document from container
// snippets, so a fixture spells out only the container it is about.
func restrictedFixtureDeployment(name string, containers ...string) string {
	return "apiVersion: apps/v1\n" +
		"kind: Deployment\n" +
		"metadata:\n" +
		"  name: " + name + "\n" +
		"spec:\n" +
		"  template:\n" +
		"    spec:\n" +
		"      containers:\n" +
		strings.Join(containers, "")
}

// writeRestrictedFixtureManifest writes one fixture manifest into dir (creating
// it), so a fixture spells out only the document it needs.
func writeRestrictedFixtureManifest(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
