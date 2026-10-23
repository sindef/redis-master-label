package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every container in both example Deployments must declare cpu and memory
// requests and limits. Before this check neither manifest had a resources
// block at all, so the pods ran BestEffort: the scheduler had no size to pack
// them by, they were evicted first under node pressure, and one replica's
// redis-server or the labeler's 10s poll loop could take a whole node.
// kubeconform accepts an absent resources block, so nothing failed.
func TestManifestContainersDeclareResources(t *testing.T) {
	problems, deployments, containers, err := deploymentResourceProblems(manifestDir)
	if err != nil {
		t.Fatal(err)
	}
	if deployments != 2 {
		t.Fatalf("Deployment documents across manifests = %d, want the two example deployments: the sizing check must read both", deployments)
	}
	if containers != 4 {
		t.Fatalf("containers across the example deployments = %d, want 4 (redis and redis-master-label in each): sizing applies to every container in the pod, not only the sidecar", containers)
	}
	for _, problem := range problems {
		t.Errorf("%s", problem)
	}
}

// The reported failure mode at value level: an absent block, a missing half and
// an impossible quantity must each be reported, and the sized shape accepted.
func TestContainerResourceProblem_RegressionFixtures(t *testing.T) {
	sized := func(mutate func(*containerResources)) *containerResources {
		resources := containerResources{
			Requests: resourceRequirements{CPU: "100m", Memory: "128Mi"},
			Limits:   resourceRequirements{CPU: "500m", Memory: "256Mi"},
		}
		if mutate != nil {
			mutate(&resources)
		}
		return &resources
	}

	if err := containerResourceProblem("deployment-example.yaml", "redis", sized(nil)); err != nil {
		t.Fatalf("sized container rejected: %v", err)
	}

	tests := []struct {
		name      string
		resources *containerResources
		want      string
	}{
		{
			name:      "no resources block",
			resources: nil,
			want:      "BestEffort",
		},
		{
			name:      "cpu request missing",
			resources: sized(func(r *containerResources) { r.Requests.CPU = "" }),
			want:      "resources.requests.cpu is missing",
		},
		{
			name:      "memory request missing",
			resources: sized(func(r *containerResources) { r.Requests.Memory = "" }),
			want:      "resources.requests.memory is missing",
		},
		{
			name:      "cpu limit missing",
			resources: sized(func(r *containerResources) { r.Limits.CPU = "" }),
			want:      "resources.limits.cpu is missing",
		},
		{
			name:      "memory limit missing",
			resources: sized(func(r *containerResources) { r.Limits.Memory = "" }),
			want:      "resources.limits.memory is missing",
		},
		{
			name:      "zero cpu request",
			resources: sized(func(r *containerResources) { r.Requests.CPU = "0" }),
			want:      "greater than zero",
		},
		{
			name:      "negative memory limit",
			resources: sized(func(r *containerResources) { r.Limits.Memory = "-1Mi" }),
			want:      "greater than zero",
		},
		{
			name:      "unparseable memory limit",
			resources: sized(func(r *containerResources) { r.Limits.Memory = "lots" }),
			want:      "not a valid Kubernetes quantity",
		},
		{
			name:      "limit below request",
			resources: sized(func(r *containerResources) { r.Limits.CPU = "50m" }),
			want:      "below",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := containerResourceProblem("deployment-example.yaml", "redis", tt.resources)
			if err == nil {
				t.Fatal("unsized container accepted, want a problem reported")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %q, want it to name %q", err, tt.want)
			}
			if !strings.Contains(err.Error(), "deployment-example.yaml/redis") {
				t.Errorf("error = %q, want it to name the manifest and container", err)
			}
		})
	}
}

// sizedDeploymentFixture is one Deployment whose two containers both declare
// requests and limits, the shape both example manifests now have.
const sizedDeploymentFixture = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: redis-leader
spec:
  template:
    spec:
      containers:
      - name: redis
        image: redis:7-alpine
        resources:
          requests:
            cpu: 100m
            memory: 128Mi
          limits:
            cpu: 500m
            memory: 256Mi
      - name: redis-master-label
        image: ghcr.io/redis-master-label/redis-master-label:v0.1.0
        resources:
          requests:
            cpu: 10m
            memory: 32Mi
          limits:
            cpu: 100m
            memory: 64Mi
`

// labelerResourcesBlock is the labeler half of the fixture, removed by the
// regression case to reproduce one unsized container in an otherwise sized
// pod.
const labelerResourcesBlock = `        resources:
          requests:
            cpu: 10m
            memory: 32Mi
          limits:
            cpu: 100m
            memory: 64Mi
`

// The same failure mode at manifest level: one container without a resources
// block must be reported by the reader, and the rest of the manifest set must
// keep being read. A Service document in the directory contributes no
// Deployment, so the count cannot be inflated by a non-workload manifest.
func TestDeploymentResourceProblems_MissingResourcesFails(t *testing.T) {
	dir := t.TempDir()
	write := func(name, text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("deployment.yaml", sizedDeploymentFixture)
	write("leader-service.yaml", "apiVersion: v1\nkind: Service\nmetadata:\n  name: redis-leader\n")

	problems, deployments, containers, err := deploymentResourceProblems(dir)
	if err != nil {
		t.Fatal(err)
	}
	if deployments != 1 || containers != 2 {
		t.Fatalf("deployments = %d, containers = %d, want 1 and 2", deployments, containers)
	}
	if len(problems) != 0 {
		t.Fatalf("sized manifest set rejected: %v", problems)
	}

	write("deployment.yaml", strings.Replace(sizedDeploymentFixture, labelerResourcesBlock, "", 1))
	problems, deployments, containers, err = deploymentResourceProblems(dir)
	if err != nil {
		t.Fatal(err)
	}
	if deployments != 1 || containers != 2 {
		t.Fatalf("deployments = %d, containers = %d, want 1 and 2", deployments, containers)
	}
	if len(problems) != 1 {
		t.Fatalf("problems = %v, want exactly the unsized labeler container", problems)
	}
	if !strings.Contains(problems[0], "deployment.yaml/redis-master-label") {
		t.Errorf("problem = %q, want it to name deployment.yaml/redis-master-label", problems[0])
	}

	// A limit below its request is rejected at admission, so it must be
	// reported here rather than passing as "sized".
	write("deployment.yaml", strings.Replace(sizedDeploymentFixture, "cpu: 500m", "cpu: 10m", 1))
	problems, _, _, err = deploymentResourceProblems(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 1 || !strings.Contains(problems[0], "below") {
		t.Fatalf("problems = %v, want the limit-below-request failure", problems)
	}
}
