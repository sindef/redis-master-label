package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Single-master strategy regression tests: the leader Deployment runs one
// redis-server with no --replicaof, and the pod gets redis-role=master from its
// sidecar. It used to declare only `replicas: 1` and no `spec.strategy`, so it
// kept the default RollingUpdate: a rollout (or `kubectl rollout restart`)
// created the replacement pod before deleting the old one, both pods started a
// master, both carried app: redis-leader (the Service selector and the replica
// deployment's --replicaof target), and the cluster had two writers with
// replicas split across them.
func TestLeaderDeploymentUsesRecreateStrategy(t *testing.T) {
	deployments, err := readLeaderDeployments(manifestDir)
	if err != nil {
		t.Fatalf("read example manifests: %v", err)
	}
	if len(deployments) != 2 {
		t.Fatalf("read %d Deployments from %s, want 2 (redis-leader.yaml and deployment-example.yaml): the strategy check must not pass on a manifest set it never read", len(deployments), manifestDir)
	}

	problems, leaders := singleWriterDeploymentProblems(deployments)
	for _, problem := range problems {
		t.Errorf("%s", problem)
	}
	if leaders != 1 {
		t.Fatalf("single-master Deployments found = %d, want exactly 1 (redis-leader): with none found the check proves nothing, and with more than one the example has several writers", leaders)
	}

	// Read the pinned fields back explicitly, so a reader that stopped decoding
	// strategy or replicas fails here instead of quietly reporting no problem.
	leader := leadersByName(deployments)["redis-leader"]
	if leader == nil {
		t.Fatalf("no Deployment named redis-leader in %s: found %v", manifestDir, deploymentNames(deployments))
	}
	if leader.StrategyType != recreateStrategyType {
		t.Errorf("redis-leader strategy = %q, want %q", leader.StrategyType, recreateStrategyType)
	}
	if leader.Replicas == nil || *leader.Replicas != 1 {
		t.Errorf("redis-leader replicas = %v, want 1: the leader is a single writer", leader.Replicas)
	}
	if leader.RollingUpdate {
		t.Errorf("redis-leader declares spec.strategy.rollingUpdate, which the API server rejects next to type: %s", recreateStrategyType)
	}
	if len(leader.MasterRedisContainers) != 1 || leader.MasterRedisContainers[0] != "redis" {
		t.Errorf("redis-leader master containers = %v, want exactly [redis]: the pod's redis-server has to start without --replicaof", leader.MasterRedisContainers)
	}
}

// leadersByName indexes Deployments by their manifest's Deployment name.
func leadersByName(deployments []leaderDeployment) map[string]*leaderDeployment {
	byName := make(map[string]*leaderDeployment, len(deployments))
	for i := range deployments {
		byName[deployments[i].Name] = &deployments[i]
	}
	return byName
}

// deploymentNames lists the Deployment names a manifest set declared, so a
// failure says what was read instead of only what was missing.
func deploymentNames(deployments []leaderDeployment) []string {
	names := make([]string, 0, len(deployments))
	for _, d := range deployments {
		names = append(names, d.Name)
	}
	return names
}

// leaderStrategyFixture is one single-master Deployment in the shape the fix
// requires: one replica, Recreate, and a redis-server with no --replicaof.
const leaderStrategyFixture = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: redis-leader
spec:
  replicas: 1
  strategy:
    type: Recreate
  selector:
    matchLabels:
      app: redis-leader
  template:
    metadata:
      labels:
        app: redis-leader
    spec:
      containers:
      - name: redis
        image: redis:7-alpine
        command:
        - redis-server
        - --requirepass
        - $(REDIS_PASSWORD)
`

// replicaDeploymentFixture is a replica Deployment: it replicates from the
// leader, so RollingUpdate (the default) and several replicas are correct and
// must not be reported.
const replicaDeploymentFixture = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: redis-with-labeler
spec:
  replicas: 3
  selector:
    matchLabels:
      app: redis
  template:
    metadata:
      labels:
        app: redis
    spec:
      containers:
      - name: redis
        image: redis:7-alpine
        command:
        - redis-server
        - --replicaof
        - "redis-leader.default.svc.cluster.local"
        - "6379"
`

// The reported failure mode at manifest level: an absent strategy (RollingUpdate
// by default), an explicit RollingUpdate, more than one replica of a master, and
// Recreate combined with rollingUpdate parameters must each be reported, while
// the pinned leader and a RollingUpdate replica Deployment are accepted.
func TestSingleWriterDeploymentProblems_RegressionFixtures(t *testing.T) {
	dir := t.TempDir()
	write := func(name, text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("leader.yaml", leaderStrategyFixture)
	write("replicas.yaml", replicaDeploymentFixture)
	write("leader-service.yaml", "apiVersion: v1\nkind: Service\nmetadata:\n  name: redis-leader\nspec:\n  selector:\n    app: redis-leader\n")

	deployments, err := readLeaderDeployments(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(deployments) != 2 {
		t.Fatalf("Deployments read = %d, want 2: the Service document contributes none", len(deployments))
	}
	problems, leaders := singleWriterDeploymentProblems(deployments)
	if leaders != 1 {
		t.Fatalf("single-master Deployments = %d, want 1: the replica Deployment replicates (--replicaof) and must stay exempt", leaders)
	}
	if len(problems) != 0 {
		t.Fatalf("pinned manifest set rejected: %v", problems)
	}

	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "no strategy, the reported defect",
			text: strings.Replace(leaderStrategyFixture, "  strategy:\n    type: Recreate\n", "", 1),
			want: "declares no spec.strategy",
		},
		{
			name: "explicit RollingUpdate",
			text: strings.Replace(leaderStrategyFixture, "type: Recreate", "type: RollingUpdate", 1),
			want: "RollingUpdate",
		},
		{
			name: "two replicas of a master",
			text: strings.Replace(leaderStrategyFixture, "replicas: 1", "replicas: 2", 1),
			want: "replicas: 2",
		},
		{
			name: "Recreate with rollingUpdate parameters",
			text: strings.Replace(leaderStrategyFixture, "  strategy:\n    type: Recreate\n", "  strategy:\n    type: Recreate\n    rollingUpdate:\n      maxSurge: 0\n", 1),
			want: "rollingUpdate",
		},
		{
			name: "redis command replaced by an entrypoint",
			text: strings.Replace(leaderStrategyFixture, "        command:\n        - redis-server\n        - --requirepass\n        - $(REDIS_PASSWORD)\n", "", 1),
			want: "does not start redis-server",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			write("leader.yaml", tt.text)
			deployments, err := readLeaderDeployments(dir)
			if err != nil {
				t.Fatal(err)
			}
			problems, _ := singleWriterDeploymentProblems(deployments)
			if len(problems) == 0 {
				t.Fatalf("manifest accepted, want a problem naming %q", tt.want)
			}
			joined := strings.Join(problems, "\n")
			if !strings.Contains(joined, tt.want) {
				t.Fatalf("problems = %q, want one naming %q", joined, tt.want)
			}
			if !strings.Contains(joined, "leader.yaml") {
				t.Errorf("problems = %q, want them to name the manifest file", joined)
			}
		})
	}

	// Every report has to name the Deployment, otherwise the fix is a hunt
	// through the manifest set.
	write("leader.yaml", strings.Replace(leaderStrategyFixture, "type: Recreate", "type: RollingUpdate", 1))
	deployments, err = readLeaderDeployments(dir)
	if err != nil {
		t.Fatal(err)
	}
	problems, _ = singleWriterDeploymentProblems(deployments)
	if len(problems) != 1 || !strings.Contains(problems[0], `"redis-leader"`) {
		t.Fatalf("problems = %v, want exactly one naming the redis-leader Deployment", problems)
	}
}

// A manifest directory that cannot be read is an error, not an empty set: a
// renamed directory must not make the strategy check pass by reading nothing.
func TestReadLeaderDeployments_MissingDirFails(t *testing.T) {
	if _, err := readLeaderDeployments(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("missing manifest directory accepted, want an error")
	}
}
