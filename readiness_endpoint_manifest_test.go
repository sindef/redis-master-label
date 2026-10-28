package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The shipped leader pod is the only endpoint of the redis-leader Service that
// every replica's --replicaof points at, so its labeler must not decide whether
// the pod stays an endpoint. The reviewed manifests declared a readinessProbe on
// /healthz there: a labeler that answers 503 after three consecutive ROLE
// failures made the kubelet mark the whole pod NotReady, the API server removed
// the Service's only endpoint, and all three replicas stalled while Redis was
// healthy. Only the livenessProbe stays, and this test fails if a readinessProbe
// is added back or the livenessProbe is dropped.
func TestManifestServiceBackedLabelerKeepsReadinessOutOfPodEndpoint(t *testing.T) {
	labelers, err := serviceBackedLabelers(manifestDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(labelers) != 1 {
		t.Fatalf("service-backed labeler containers = %d, want exactly the one in the redis-leader pod: the check must read the Service selector and the pod labels, not nothing: %+v", len(labelers), labelers)
	}
	labeler := labelers[0]
	if labeler.Manifest != "redis-leader.yaml" || labeler.Deployment != "redis-leader" || labeler.Container != "redis-master-label" {
		t.Fatalf("service-backed labeler = %s/%s/%s, want redis-leader.yaml/redis-leader/redis-master-label", labeler.Manifest, labeler.Deployment, labeler.Container)
	}
	if got := strings.Join(labeler.Services, ", "); got != "redis-leader" {
		t.Fatalf("labeler pod is selected by Services %q, want the redis-leader Service the replicas replicate from", got)
	}
	if labeler.Readiness != nil {
		t.Errorf("%s/%s/%s: readinessProbe declared, want none: pod readiness is the AND of every container's readiness, so a 503 from /healthz removes the only redis-leader endpoint and stalls every replica", labeler.Manifest, labeler.Deployment, labeler.Container)
	}
	if labeler.Liveness == nil {
		t.Errorf("%s/%s/%s: livenessProbe missing, want one polling %s: it restarts a wedged labeler without gating readiness", labeler.Manifest, labeler.Deployment, labeler.Container, healthPath)
	}
	if err := labelerEndpointProblem(labeler); err != nil {
		t.Errorf("%v", err)
	}

	problems, checked, err := labelerEndpointProblems(manifestDir)
	if err != nil {
		t.Fatal(err)
	}
	if checked != 1 {
		t.Fatalf("checked labelers = %d, want 1", checked)
	}
	for _, problem := range problems {
		t.Errorf("%s", problem)
	}
}

const (
	serviceBackedFixtureRedis = "      - name: redis\n" +
		"        image: redis:7-alpine\n"
	serviceBackedFixtureLivenessProbe = "        livenessProbe:\n" +
		"          httpGet:\n" +
		"            path: /healthz\n" +
		"            port: health\n"
	serviceBackedFixtureReadinessProbe = "        readinessProbe:\n" +
		"          httpGet:\n" +
		"            path: /healthz\n" +
		"            port: health\n"
)

// serviceBackedFixtureLabeler returns a labeler container snippet with the
// probe blocks under test appended.
func serviceBackedFixtureLabeler(probes string) string {
	return "      - name: redis-master-label\n" +
		"        image: ghcr.io/redis-master-label/redis-master-label:v0.1.0\n" +
		"        ports:\n" +
		"        - name: health\n" +
		"          containerPort: 8080\n" +
		probes
}

// serviceBackedFixtureService returns a Service document selecting the given pod
// labels. An empty selector is written as the empty map, which selects no pod.
func serviceBackedFixtureService(name string, selector map[string]string) string {
	document := "apiVersion: v1\nkind: Service\nmetadata:\n  name: " + name + "\nspec:\n"
	if len(selector) == 0 {
		return document + "  selector: {}\n  ports:\n  - name: redis\n    port: 6379\n    targetPort: 6379\n"
	}
	document += "  selector:\n"
	keys := make([]string, 0, len(selector))
	for key := range selector {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		document += "    " + key + ": " + selector[key] + "\n"
	}
	return document + "  ports:\n  - name: redis\n    port: 6379\n    targetPort: 6379\n"
}

// serviceBackedFixtureDeployment returns a Deployment document with the given
// pod labels and container snippets.
func serviceBackedFixtureDeployment(name string, labels map[string]string, containers ...string) string {
	document := "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: " + name + "\nspec:\n  template:\n    metadata:\n      labels:\n"
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		document += "        " + key + ": " + labels[key] + "\n"
	}
	return document + "    spec:\n      containers:\n" + strings.Join(containers, "")
}

// The rule has to be read from the manifests, not assumed: a Service selector
// decides whose readiness gates an endpoint. The pre-fix shape must be reported,
// the chosen shape accepted, and a labeler in a pod no Service selects stays
// exempt because its readiness removes no endpoint this repository defines.
func TestLabelerEndpointProblems_RegressionFixtures(t *testing.T) {
	leaderLabels := map[string]string{"app": "redis-leader"}
	replicaLabels := map[string]string{"app": "redis"}
	leaderService := serviceBackedFixtureService("redis-leader", leaderLabels)

	tests := []struct {
		name             string
		files            map[string]string
		wantChecked      int
		wantProblemCount int
		wantInProblems   []string
	}{
		{
			// The reviewed manifests: readiness and liveness on the leader's
			// labeler, whose pod is the Service's only endpoint.
			name: "reviewed leader declares a readinessProbe",
			files: map[string]string{
				"redis-leader-service.yaml": leaderService,
				"redis-leader.yaml": serviceBackedFixtureDeployment("redis-leader", leaderLabels,
					serviceBackedFixtureRedis, serviceBackedFixtureLabeler(serviceBackedFixtureLivenessProbe+serviceBackedFixtureReadinessProbe)),
			},
			wantChecked:      1,
			wantProblemCount: 1,
			wantInProblems:   []string{"readinessProbe", "redis-leader"},
		},
		{
			name: "leader keeps only the livenessProbe",
			files: map[string]string{
				"redis-leader-service.yaml": leaderService,
				"redis-leader.yaml": serviceBackedFixtureDeployment("redis-leader", leaderLabels,
					serviceBackedFixtureRedis, serviceBackedFixtureLabeler(serviceBackedFixtureLivenessProbe)),
			},
			wantChecked: 1,
		},
		{
			name: "leader labeler lost its livenessProbe too",
			files: map[string]string{
				"redis-leader-service.yaml": leaderService,
				"redis-leader.yaml": serviceBackedFixtureDeployment("redis-leader", leaderLabels,
					serviceBackedFixtureRedis, serviceBackedFixtureLabeler("")),
			},
			wantChecked:      1,
			wantProblemCount: 1,
			wantInProblems:   []string{"no livenessProbe"},
		},
		{
			name: "replica pod with both probes backs no Service",
			files: map[string]string{
				"redis-leader-service.yaml": leaderService,
				"deployment-example.yaml": serviceBackedFixtureDeployment("redis-with-labeler", replicaLabels,
					serviceBackedFixtureRedis, serviceBackedFixtureLabeler(serviceBackedFixtureLivenessProbe+serviceBackedFixtureReadinessProbe)),
			},
			wantChecked: 0,
		},
		{
			name: "Service without a selector selects no pod",
			files: map[string]string{
				"redis-leader-service.yaml": serviceBackedFixtureService("redis-leader", nil),
				"redis-leader.yaml": serviceBackedFixtureDeployment("redis-leader", leaderLabels,
					serviceBackedFixtureRedis, serviceBackedFixtureLabeler(serviceBackedFixtureLivenessProbe+serviceBackedFixtureReadinessProbe)),
			},
			wantChecked: 0,
		},
		{
			name: "selector names a label the pod does not carry",
			files: map[string]string{
				"redis-leader-service.yaml": serviceBackedFixtureService("redis-leader", map[string]string{"app": "redis-leader", "tier": "cache"}),
				"redis-leader.yaml": serviceBackedFixtureDeployment("redis-leader", leaderLabels,
					serviceBackedFixtureRedis, serviceBackedFixtureLabeler(serviceBackedFixtureLivenessProbe+serviceBackedFixtureReadinessProbe)),
			},
			wantChecked: 0,
		},
		{
			name: "every matching Service is named",
			files: map[string]string{
				"redis-leader-service.yaml": leaderService,
				"canary-service.yaml":       serviceBackedFixtureService("redis-leader-canary", leaderLabels),
				"redis-leader.yaml": serviceBackedFixtureDeployment("redis-leader", leaderLabels,
					serviceBackedFixtureRedis, serviceBackedFixtureLabeler(serviceBackedFixtureLivenessProbe+serviceBackedFixtureReadinessProbe)),
			},
			wantChecked:      1,
			wantProblemCount: 1,
			wantInProblems:   []string{"redis-leader, redis-leader-canary"},
		},
		{
			name: "only the labeler container is checked",
			files: map[string]string{
				"redis-leader-service.yaml": leaderService,
				"redis-leader.yaml": serviceBackedFixtureDeployment("redis-leader", leaderLabels,
					serviceBackedFixtureRedis, serviceBackedFixtureLabeler(serviceBackedFixtureLivenessProbe)),
			},
			wantChecked: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, content := range tt.files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			problems, checked, err := labelerEndpointProblems(dir)
			if err != nil {
				t.Fatalf("labelerEndpointProblems(%s) error = %v", dir, err)
			}
			if checked != tt.wantChecked {
				t.Fatalf("checked labelers = %d, want %d: %+v", checked, tt.wantChecked, problems)
			}
			if len(problems) != tt.wantProblemCount {
				t.Fatalf("problems = %v, want %d", problems, tt.wantProblemCount)
			}
			joined := strings.Join(problems, "\n")
			for _, want := range tt.wantInProblems {
				if !strings.Contains(joined, want) {
					t.Errorf("problems = %v, want one naming %q", problems, want)
				}
			}
		})
	}
}

// A renamed or emptied manifests/ must fail instead of reporting no labeler.
func TestServiceBackedLabelers_MissingDirectoryFails(t *testing.T) {
	if _, err := serviceBackedLabelers(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing manifest directory accepted, want a read error")
	}
}
