package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// Resource requests/limits check: the example manifests are what README
// "Kubernetes deployment" tells users to apply, so they teach the shape a
// workload should have. Until now neither Deployment declared a resources block
// on any container (`manifests/deployment-example.yaml` and
// `manifests/redis-leader.yaml` held the only `resources:` references, and they
// were none: the sole match under manifests/ was the RBAC rule in
// `manifests/role.yaml`). Every example pod therefore ran in the BestEffort QoS
// class: with no CPU or memory request the scheduler scores and evicts it first
// under node pressure, and with no limit one replica's Redis - or the labeler's
// 10s poll loop (--check-interval=10s) - can consume a whole node.
//
// Nothing caught it: CI's only schema gate for manifests/ is kubeconform, whose
// schema treats resources as optional, and the existing Go manifest checks read
// args, probes, securityContext and the RBAC chain, never resources. This file
// adds the missing check on top of its own decode (the shared pod spec reader in
// cmd_manifest_check.go does not decode resources at all), and asserts it
// against the manifests as shipped and against fixtures of the real manifest set
// with one edit each, so a container cannot silently lose its requests or limits
// again.
//
// Every container is in scope, not just the labeler: README "Running
// unprivileged" documents that the restricted Pod Security Standard applies to
// every container in the pod, and the same is true of resource sizing - a pod's
// QoS class is decided by all of its containers, so one unset container leaves
// the whole pod BestEffort.

// manifestResourceList is a container's requests or limits, as far as the
// example manifests are concerned. Values stay strings because Kubernetes
// quantities are strings ("100m", "128Mi"); they are parsed below instead of
// being compared as text.
type manifestResourceList struct {
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
}

// manifestResources is one container's resources block. The struct pointer that
// holds it is nil when the manifest declares no block at all, which is the
// deletion this check exists to catch: an absent block and one whose every field
// is empty are both BestEffort, but they are reported differently.
type manifestResources struct {
	Requests manifestResourceList `json:"requests"`
	Limits   manifestResourceList `json:"limits"`
}

// manifestContainer is one container of a pod spec, as far as resource sizing is
// concerned.
type manifestContainer struct {
	Name      string             `json:"name"`
	Image     string             `json:"image"`
	Resources *manifestResources `json:"resources"`
}

// manifestPodSpecManifest holds just the containers of a pod spec. Manifests
// without a Pod spec (the Service, Role, RoleBinding, ServiceAccount, and the
// rolebinding) contribute no container instead of being misread.
type manifestPodSpecManifest struct {
	Spec struct {
		Template struct {
			Spec struct {
				Containers []manifestContainer `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
}

// manifestContainerResources is one container with the manifest file it came
// from and the resources block it declares, so a failure names the right
// deployment and container.
type manifestContainerResources struct {
	Manifest  string
	Name      string
	Resources *manifestResources
}

// podContainerResources returns every container of every pod spec across
// manifests/*.yaml, walking the same manifest set as the other manifest checks
// and tolerating the same non-Pod-spec documents. Unlike sidecarContainers it
// does not select by image: a pod's QoS class is set by all of its containers,
// so the co-located redis container has to declare resources too.
func podContainerResources(dir string) ([]manifestContainerResources, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var containers []manifestContainerResources
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
		var doc manifestPodSpecManifest
		if err := json.Unmarshal(jsonData, &doc); err != nil {
			return nil, fmt.Errorf("%s: JSON decode: %w", path, err)
		}
		for _, c := range doc.Spec.Template.Spec.Containers {
			containers = append(containers, manifestContainerResources{
				Manifest:  entry.Name(),
				Name:      c.Name,
				Resources: c.Resources,
			})
		}
	}
	sort.Slice(containers, func(i, j int) bool {
		if containers[i].Manifest != containers[j].Manifest {
			return containers[i].Manifest < containers[j].Manifest
		}
		return containers[i].Name < containers[j].Name
	})
	return containers, nil
}

// resourceQuantityRule is one of the four quantity fields a container has to
// declare: which field it is, the value the manifest wrote and whether it is a
// request. A request decides the QoS class and the capacity the scheduler
// reserves; a limit caps what the container can consume.
type resourceQuantityRule struct {
	field   string
	value   string
	request bool
}

// containerResourceProblems returns one message per way a container's resource
// block falls short of what the examples are supposed to teach, or nil when it
// declares requests and limits for CPU and memory with the request at or below
// the limit.
func containerResourceProblems(c manifestContainerResources) []string {
	if c.Resources == nil {
		return []string{fmt.Sprintf("%s/%s: declares no resources, so Kubernetes places the pod in the BestEffort QoS class: the scheduler scores and evicts it first under node pressure, and with no limit its process can consume the whole node; declare resources.requests.cpu, resources.requests.memory, resources.limits.cpu and resources.limits.memory", c.Manifest, c.Name)}
	}

	rules := []resourceQuantityRule{
		{"requests.cpu", c.Resources.Requests.CPU, true},
		{"requests.memory", c.Resources.Requests.Memory, true},
		{"limits.cpu", c.Resources.Limits.CPU, false},
		{"limits.memory", c.Resources.Limits.Memory, false},
	}

	var problems []string
	parsed := make(map[string]resource.Quantity, len(rules))
	for _, rule := range rules {
		if strings.TrimSpace(rule.value) == "" {
			if rule.request {
				problems = append(problems, fmt.Sprintf("%s/%s: declares no resources.%s, so the pod is not given the request the scheduler reserves capacity for and lands in the BestEffort QoS class; declare it", c.Manifest, c.Name, rule.field))
			} else {
				problems = append(problems, fmt.Sprintf("%s/%s: declares no resources.%s, so the container is capped by nothing and can consume the whole node; declare it", c.Manifest, c.Name, rule.field))
			}
			continue
		}
		quantity, err := resource.ParseQuantity(rule.value)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s/%s: resources.%s = %q is not a Kubernetes quantity: %v", c.Manifest, c.Name, rule.field, rule.value, err))
			continue
		}
		parsed[rule.field] = quantity
	}

	// A request above its limit is rejected by the API server, so the manifest
	// would not apply at all; the examples must not teach that shape either.
	for _, pair := range [][2]string{{"requests.cpu", "limits.cpu"}, {"requests.memory", "limits.memory"}} {
		request, requestOK := parsed[pair[0]]
		limit, limitOK := parsed[pair[1]]
		if !requestOK || !limitOK {
			continue
		}
		if request.Cmp(limit) > 0 {
			problems = append(problems, fmt.Sprintf("%s/%s: resources.%s = %s is above resources.%s = %s, so the container can never be admitted: Kubernetes rejects a request above its limit", c.Manifest, c.Name, pair[0], request.String(), pair[1], limit.String()))
		}
	}

	return problems
}

// Both example Deployments must declare requests and limits for every container,
// not only the labeler sidecar: a pod's QoS class is decided by all of its
// containers, so the redis container missing resources leaves the whole pod
// BestEffort. The count is asserted so the check cannot pass against a manifest
// set the example containers silently disappeared from.
func TestManifestContainersDeclareResources(t *testing.T) {
	containers, err := podContainerResources(manifestDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(containers) == 0 {
		t.Fatal("no container found in manifests: the resource check ran against nothing")
	}
	if len(containers) != 4 {
		t.Fatalf("containers = %d, want the four example containers (the redis and labeler containers of the two example Deployments)", len(containers))
	}
	for _, c := range containers {
		for _, problem := range containerResourceProblems(c) {
			t.Errorf("%s", problem)
		}
	}
}

// The reader has to see what the manifests say: containers and their resources
// are read back, sorted by manifest and name, and a document with no Pod spec
// (the Service, Role, RoleBinding, ServiceAccount) contributes no container
// instead of being counted. Walking a missing directory is an error rather than
// an empty pass.
func TestPodContainerResources_RegressionFixtures(t *testing.T) {
	dir := t.TempDir()
	manifest := "apiVersion: apps/v1\nkind: Deployment\nspec:\n  template:\n    spec:\n      containers:\n" +
		"      - name: redis\n        image: redis:7-alpine\n        resources:\n          requests:\n            cpu: 100m\n            memory: 128Mi\n          limits:\n            cpu: 500m\n            memory: 512Mi\n" +
		"      - name: redis-master-label\n        image: ghcr.io/redis-master-label/redis-master-label:v0.1.0\n"
	if err := os.WriteFile(filepath.Join(dir, "deployment.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "service.yaml"), []byte("apiVersion: v1\nkind: Service\nmetadata:\n  name: redis-leader\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	containers, err := podContainerResources(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(containers) != 2 {
		t.Fatalf("containers = %d, want the two Deployment containers and no Service container: %+v", len(containers), containers)
	}
	if containers[0].Name != "redis" || containers[0].Manifest != "deployment.yaml" {
		t.Fatalf("containers[0] = %+v, want the redis container of deployment.yaml", containers[0])
	}
	if containers[0].Resources == nil || containers[0].Resources.Requests.CPU != "100m" || containers[0].Resources.Limits.Memory != "512Mi" {
		t.Fatalf("redis resources = %+v, want the requests and limits the manifest declares", containers[0].Resources)
	}
	if containers[1].Name != "redis-master-label" {
		t.Fatalf("containers[1] = %+v, want the labeler container", containers[1])
	}
	if containers[1].Resources != nil {
		t.Errorf("labeler resources = %+v, want none: this manifest declares no block", containers[1].Resources)
	}

	if _, err := podContainerResources(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing manifest directory accepted, want a read error")
	}
}

// The failure modes of the rule: no block at all, a block without requests, a
// block without limits, an empty field, a value that is not a quantity and a
// request above its limit must each be reported, while a complete block must
// pass. The four quantities are checked independently, so dropping one is
// enough.
func TestContainerResourceProblems_RegressionFixtures(t *testing.T) {
	complete := &manifestResources{
		Requests: manifestResourceList{CPU: "100m", Memory: "128Mi"},
		Limits:   manifestResourceList{CPU: "500m", Memory: "512Mi"},
	}

	tests := []struct {
		name        string
		resources   *manifestResources
		wantProblem string
	}{
		{name: "complete block", resources: complete},
		{name: "request equal to limit", resources: &manifestResources{
			Requests: manifestResourceList{CPU: "100m", Memory: "128Mi"},
			Limits:   manifestResourceList{CPU: "100m", Memory: "128Mi"},
		}},
		{name: "no resources block", resources: nil, wantProblem: "BestEffort"},
		{name: "requests missing", resources: &manifestResources{Limits: complete.Limits}, wantProblem: "declares no resources.requests.cpu"},
		{name: "limits missing", resources: &manifestResources{Requests: complete.Requests}, wantProblem: "declares no resources.limits.memory"},
		{name: "cpu request empty", resources: &manifestResources{
			Requests: manifestResourceList{CPU: "  ", Memory: "128Mi"},
			Limits:   complete.Limits,
		}, wantProblem: "declares no resources.requests.cpu"},
		{name: "memory limit unparseable", resources: &manifestResources{
			Requests: complete.Requests,
			Limits:   manifestResourceList{CPU: "500m", Memory: "512 megabytes"},
		}, wantProblem: "is not a Kubernetes quantity"},
		{name: "cpu request above its limit", resources: &manifestResources{
			Requests: manifestResourceList{CPU: "1", Memory: "128Mi"},
			Limits:   manifestResourceList{CPU: "100m", Memory: "512Mi"},
		}, wantProblem: "above resources.limits.cpu"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			problems := containerResourceProblems(manifestContainerResources{Manifest: "deployment.yaml", Name: "redis", Resources: tt.resources})
			if tt.wantProblem == "" {
				if len(problems) != 0 {
					t.Fatalf("problems = %v, want none", problems)
				}
				return
			}
			if len(problems) == 0 {
				t.Fatalf("a resource block missing %s was accepted, want a problem reported", tt.wantProblem)
			}
			if !strings.Contains(strings.Join(problems, "\n"), tt.wantProblem) {
				t.Fatalf("problems = %v, want one mentioning %q", problems, tt.wantProblem)
			}
		})
	}
}

// The regression the finding describes, at manifest level: deleting the
// resources block or one of its four quantities, or raising a request above its
// limit, must each be reported, while the manifests as shipped must pass. Every
// fixture is a copy of the real manifest set with one edit, so the edit is what
// the check caught.
func TestManifestContainerResources_FailureModes(t *testing.T) {
	// The registered resources block of the redis container: 100m/128Mi
	// requests and 500m/512Mi limits, indented as in the manifests.
	const redisBlock = "        resources:\n          requests:\n            cpu: 100m\n            memory: 128Mi\n          limits:\n            cpu: 500m\n            memory: 512Mi\n"
	const labelerLimits = "          limits:\n            cpu: 100m\n            memory: 128Mi\n"

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
			// The exact deletion the finding describes: the redis container's
			// block is gone in both Deployments.
			name:        "resources block deleted from the redis container",
			mutate:      removeLine(redisBlock),
			wantProblem: "declares no resources",
		},
		{
			name:        "resources replaced by an empty block",
			mutate:      replaceLine(redisBlock, "        resources: {}\n"),
			wantProblem: "declares no resources.requests.cpu",
		},
		{
			name:        "limits deleted from the labeler container",
			mutate:      removeLine(labelerLimits),
			wantProblem: "declares no resources.limits.cpu",
		},
		{
			name:        "requests deleted from the labeler container",
			mutate:      removeLine("          requests:\n            cpu: 10m\n            memory: 32Mi\n"),
			wantProblem: "declares no resources.requests.cpu",
		},
		{
			name:        "redis cpu request above its limit",
			mutate:      replaceLine("          limits:\n            cpu: 500m\n", "          limits:\n            cpu: 50m\n"),
			wantProblem: "above resources.limits.cpu",
		},
		{
			name:        "redis memory request unparseable",
			mutate:      replaceLine("          requests:\n            cpu: 100m\n            memory: 128Mi\n", "          requests:\n            cpu: 100m\n            memory: 128 megabytes\n"),
			wantProblem: "is not a Kubernetes quantity",
		},
		{
			name: "unmutated manifests",
		},
	}

	problemsIn := func(t *testing.T, dir string) []string {
		t.Helper()
		containers, err := podContainerResources(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(containers) != 4 {
			t.Fatalf("containers = %d, want the four example containers", len(containers))
		}
		var problems []string
		for _, c := range containers {
			for _, problem := range containerResourceProblems(c) {
				problems = append(problems, problem)
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
				t.Fatalf("a container whose resources lost %s was accepted, want a problem reported", tt.wantProblem)
			}
			if !strings.Contains(strings.Join(problems, "\n"), tt.wantProblem) {
				t.Fatalf("problems = %v, want one mentioning %q", problems, tt.wantProblem)
			}
		})
	}

	// The manifests as shipped must pass, so the fixtures prove the edit is what
	// was caught rather than a checker that always complains.
	shipped := t.TempDir()
	writeManifestSet(t, shipped, nil)
	if problems := problemsIn(t, shipped); len(problems) != 0 {
		t.Fatalf("manifest set as shipped reported: %v, want none", problems)
	}
}
