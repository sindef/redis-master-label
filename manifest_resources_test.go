package main

import (
	"os"
	"strings"
	"testing"
)

// The reviewed gap, at manifest level: neither example Deployment declared a
// resources block, so both pods ran at the BestEffort QoS class and taught that
// shape to everyone applying the manifests README "Kubernetes deployment"
// lists. Every container of both Deployments must declare requests and limits
// for cpu and memory, and each pod must come out better than BestEffort.
func TestManifestContainersDeclareResourceRequestsAndLimits(t *testing.T) {
	containers, err := containerResourceSpecs(manifestDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(containers) == 0 {
		t.Fatal("no container found in manifests: the resource check ran against nothing")
	}
	if len(containers) != 4 {
		t.Fatalf("containers across manifests = %d, want the two example Deployments' four containers (two each)", len(containers))
	}

	for _, c := range containers {
		for _, problem := range containerResourceProblems(c) {
			t.Errorf("%s", problem)
		}
	}

	byManifest := map[string][]containerResourceSpec{}
	for _, c := range containers {
		byManifest[c.Manifest] = append(byManifest[c.Manifest], c)
	}
	if len(byManifest) != 2 {
		t.Fatalf("manifests declaring containers = %d, want the two example Deployments", len(byManifest))
	}

	for manifest, pod := range byManifest {
		switch class := podQoSClass(pod); class {
		case qosClassBestEffort, qosClassUnknown:
			t.Errorf("%s: pod QoS class = %s, want Burstable or Guaranteed: a pod whose containers declare no request has nothing for the scheduler to score or reserve, so it is evicted first under node pressure, and one without limits can consume the node (README \"Resource sizing\")", manifest, class)
		}
	}
}

// The manifests teach the shape, and the README is where the numbers are
// explained: the declared pair is only documented if the section that names it
// exists, so removing "Resource sizing" (or the QoS class it explains) must
// fail the pull request rather than leave users to infer the sizing rule.
func TestREADMEDocumentsResourceSizing(t *testing.T) {
	data, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	readme := string(data)
	for _, want := range []string{"### Resource sizing", "BestEffort", "resources.requests", "resources.limits"} {
		if !strings.Contains(readme, want) {
			t.Errorf("README.md does not mention %q; the resource sizing rule the example manifests follow has to be stated there", want)
		}
	}
}

// The failure modes at manifest level. Every fixture is a copy of the real
// manifest set with one edit, so the edit is what the check caught: the missing
// resources block the finding reported, a requests-only or limits-only block,
// one resource pair dropped, a limit below its request and a quantity the API
// cannot parse. The unmutated set must produce no problem at all.
func TestManifestContainersDeclareResourceRequestsAndLimits_FailureModes(t *testing.T) {
	const (
		redisResources = "        resources:\n" +
			"          requests:\n" +
			"            cpu: 100m\n" +
			"            memory: 128Mi\n" +
			"          limits:\n" +
			"            cpu: 500m\n" +
			"            memory: 256Mi\n"
		labelerRequests = "          requests:\n" +
			"            cpu: 10m\n" +
			"            memory: 32Mi\n"
		labelerLimits = "          limits:\n" +
			"            cpu: 100m\n" +
			"            memory: 64Mi\n"
	)

	tests := []struct {
		name        string
		mutate      func(file, text string) string
		wantProblem string
	}{
		{
			// The shape both example Deployments shipped: no resources block.
			name: "resources block deleted from the redis containers",
			mutate: func(file, text string) string {
				return strings.ReplaceAll(text, redisResources, "")
			},
			wantProblem: "declares no resources block",
		},
		{
			name: "resources block deleted from the labeler sidecars",
			mutate: func(file, text string) string {
				return strings.ReplaceAll(text, labelerRequests+labelerLimits, "")
			},
			wantProblem: "declares no resources block",
		},
		{
			name: "only the limits declared on the labeler",
			mutate: func(file, text string) string {
				return strings.ReplaceAll(text, labelerRequests, "")
			},
			wantProblem: "resources.requests.cpu is missing",
		},
		{
			name: "only the requests declared on the labeler",
			mutate: func(file, text string) string {
				return strings.ReplaceAll(text, labelerLimits, "")
			},
			wantProblem: "resources.limits.cpu is missing",
		},
		{
			name: "memory request dropped from the labeler",
			mutate: func(file, text string) string {
				return strings.ReplaceAll(text, "            memory: 32Mi\n", "")
			},
			wantProblem: "resources.requests.memory is missing",
		},
		{
			// Only the labeler's own limit block is moved below its request;
			// the redis containers keep theirs.
			name: "labeler cpu limit below its request",
			mutate: func(file, text string) string {
				return strings.ReplaceAll(text, labelerLimits, "          limits:\n            cpu: 5m\n            memory: 64Mi\n")
			},
			wantProblem: "is below",
		},
		{
			name: "quantity the API cannot parse",
			mutate: func(file, text string) string {
				return strings.ReplaceAll(text, "            memory: 32Mi\n", "            memory: 32 MB\n")
			},
			wantProblem: "is not a Kubernetes quantity",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeManifestSet(t, dir, tt.mutate)

			containers, err := containerResourceSpecs(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(containers) != 4 {
				t.Fatalf("containers = %d, want the two example Deployments' four containers: the fixture must edit the real manifest set", len(containers))
			}

			var problems []string
			for _, c := range containers {
				problems = append(problems, containerResourceProblems(c)...)
			}
			if len(problems) == 0 {
				t.Fatal("a manifest set that lost a declared resource pair was accepted, want a problem reported")
			}
			if !strings.Contains(strings.Join(problems, "\n"), tt.wantProblem) {
				t.Fatalf("problems = %v, want one naming %q", problems, tt.wantProblem)
			}
		})
	}

	// The real manifests as shipped must pass, so the fixtures above prove the
	// edit is what was caught rather than a checker that always complains.
	shipped := t.TempDir()
	writeManifestSet(t, shipped, nil)
	containers, err := containerResourceSpecs(shipped)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range containers {
		if problems := containerResourceProblems(c); len(problems) != 0 {
			t.Errorf("manifest set as shipped reported %s/%s: %v", c.Manifest, c.Name, problems)
		}
	}
}

// The rule one container at a time: the declared pair passes, whether the limit
// sits above the request or exactly on it (the Guaranteed shape), while a
// missing block, a missing half, a missing resource pair, a limit below its
// request and an unparseable quantity are each reported.
func TestContainerResourceProblems_RegressionFixtures(t *testing.T) {
	container := func(requests, limits map[string]string) containerResourceSpec {
		return containerResourceSpec{
			Manifest: "deployment.yaml",
			Name:     "redis-master-label",
			Resources: containerResourceRequirements{
				Requests: requests,
				Limits:   limits,
			},
		}
	}

	tests := []struct {
		name         string
		container    containerResourceSpec
		wantProblems []string
	}{
		{
			name:      "request below the limit",
			container: container(map[string]string{"cpu": "10m", "memory": "32Mi"}, map[string]string{"cpu": "100m", "memory": "64Mi"}),
		},
		{
			name:      "request equal to the limit",
			container: container(map[string]string{"cpu": "100m", "memory": "128Mi"}, map[string]string{"cpu": "100m", "memory": "128Mi"}),
		},
		{
			// 0.1 CPU is 100m: the same amount written differently must count
			// as equal, because the API compares quantities by value.
			name:      "same amount written in another form",
			container: container(map[string]string{"cpu": "0.1", "memory": "128Mi"}, map[string]string{"cpu": "100m", "memory": "128Mi"}),
		},
		{
			name:      "pair for an extra resource beside the required two",
			container: container(map[string]string{"cpu": "10m", "memory": "32Mi", "ephemeral-storage": "1Gi"}, map[string]string{"cpu": "100m", "memory": "64Mi", "ephemeral-storage": "2Gi"}),
		},
		{
			name:         "no resources block",
			container:    container(nil, nil),
			wantProblems: []string{"declares no resources block", "BestEffort"},
		},
		{
			name:         "limits only",
			container:    container(nil, map[string]string{"cpu": "100m", "memory": "64Mi"}),
			wantProblems: []string{"resources.requests.cpu is missing", "resources.requests.memory is missing"},
		},
		{
			name:         "requests only",
			container:    container(map[string]string{"cpu": "10m", "memory": "32Mi"}, nil),
			wantProblems: []string{"resources.limits.cpu is missing", "resources.limits.memory is missing"},
		},
		{
			name:         "cpu pair only, memory declared nowhere",
			container:    container(map[string]string{"cpu": "10m"}, map[string]string{"cpu": "100m"}),
			wantProblems: []string{"resources.memory is declared neither as a request nor as a limit"},
		},
		{
			name:         "memory limit below the request",
			container:    container(map[string]string{"cpu": "10m", "memory": "64Mi"}, map[string]string{"cpu": "100m", "memory": "32Mi"}),
			wantProblems: []string{"resources.limits.memory = \"32Mi\" is below resources.requests.memory = \"64Mi\"", "API rejects"},
		},
		{
			name:         "ephemeral-storage limit below its request",
			container:    container(map[string]string{"cpu": "10m", "memory": "32Mi", "ephemeral-storage": "2Gi"}, map[string]string{"cpu": "100m", "memory": "64Mi", "ephemeral-storage": "1Gi"}),
			wantProblems: []string{"resources.limits.ephemeral-storage", "is below"},
		},
		{
			name:         "requested cpu, limited memory",
			container:    container(map[string]string{"cpu": "10m"}, map[string]string{"memory": "64Mi"}),
			wantProblems: []string{"resources.requests.memory is missing", "resources.limits.cpu is missing"},
		},
		{
			name:         "quantity with a space before the unit",
			container:    container(map[string]string{"cpu": "10m", "memory": "32 MB"}, map[string]string{"cpu": "100m", "memory": "64Mi"}),
			wantProblems: []string{"resources.requests.memory = \"32 MB\" is not a Kubernetes quantity"},
		},
		{
			name:         "empty quantity",
			container:    container(map[string]string{"cpu": "10m", "memory": "32Mi"}, map[string]string{"cpu": "100m", "memory": ""}),
			wantProblems: []string{"resources.limits.memory = \"\" is not a Kubernetes quantity"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			problems := containerResourceProblems(tt.container)
			if len(tt.wantProblems) == 0 {
				if len(problems) != 0 {
					t.Fatalf("problems = %v, want none", problems)
				}
				return
			}
			if len(problems) == 0 {
				t.Fatal("container accepted, want a problem reported")
			}
			joined := strings.Join(problems, "\n")
			for _, want := range tt.wantProblems {
				if !strings.Contains(joined, want) {
					t.Errorf("problems = %v, want one naming %q", problems, want)
				}
			}
		})
	}
}

// The pod-level consequence the finding describes: the QoS class of the whole
// pod, as the API computes it from every container's requests and limits. The
// class is what the scheduler and the kubelet act on, so the example
// Deployments must come out better than BestEffort and a reader that found no
// container must not pass as one that declared nothing.
func TestPodQoSClass(t *testing.T) {
	container := func(name string, requests, limits map[string]string) containerResourceSpec {
		return containerResourceSpec{
			Manifest:  "deployment.yaml",
			Name:      name,
			Resources: containerResourceRequirements{Requests: requests, Limits: limits},
		}
	}
	burstable := container("redis", map[string]string{"cpu": "100m", "memory": "128Mi"}, map[string]string{"cpu": "500m", "memory": "256Mi"})
	guaranteed := container("redis", map[string]string{"cpu": "100m", "memory": "128Mi"}, map[string]string{"cpu": "100m", "memory": "128Mi"})
	bestEffort := container("redis", nil, nil)

	tests := []struct {
		name       string
		containers []containerResourceSpec
		want       string
	}{
		{
			name:       "the reviewed shape: no container declares anything",
			containers: []containerResourceSpec{bestEffort, bestEffort},
			want:       qosClassBestEffort,
		},
		{
			name:       "requests below the limits",
			containers: []containerResourceSpec{burstable},
			want:       qosClassBurstable,
		},
		{
			name:       "requests only",
			containers: []containerResourceSpec{container("redis", map[string]string{"cpu": "100m", "memory": "128Mi"}, nil)},
			want:       qosClassBurstable,
		},
		{
			name:       "limits only: the API defaults the requests to them",
			containers: []containerResourceSpec{container("redis", nil, map[string]string{"cpu": "100m", "memory": "128Mi"})},
			want:       qosClassGuaranteed,
		},
		{
			name:       "requests equal to the limits",
			containers: []containerResourceSpec{guaranteed},
			want:       qosClassGuaranteed,
		},
		{
			name:       "cpu limited and memory not",
			containers: []containerResourceSpec{container("redis", map[string]string{"cpu": "100m"}, map[string]string{"cpu": "100m"})},
			want:       qosClassBurstable,
		},
		{
			name:       "request for a resource no limit caps",
			containers: []containerResourceSpec{container("redis", map[string]string{"cpu": "100m", "memory": "128Mi", "ephemeral-storage": "1Gi"}, map[string]string{"cpu": "100m", "memory": "128Mi"})},
			want:       qosClassBurstable,
		},
		{
			// The pod class is the worst of its containers: one container with
			// no request is enough for the scheduler to treat the pod as
			// evictable first, whatever the others declare.
			name:       "one container declares nothing beside a burstable one",
			containers: []containerResourceSpec{burstable, bestEffort},
			want:       qosClassBurstable,
		},
		{
			name:       "one container declares nothing beside a guaranteed-shaped one",
			containers: []containerResourceSpec{guaranteed, bestEffort},
			want:       qosClassBurstable,
		},
		{
			name:       "no container found",
			containers: nil,
			want:       qosClassUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := podQoSClass(tt.containers); got != tt.want {
				t.Fatalf("podQoSClass(%+v) = %q, want %q", tt.containers, got, tt.want)
			}
		})
	}
}
