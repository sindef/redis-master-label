package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// Single-master strategy regression tests: manifests/redis-leader.yaml runs the
// master the replica Deployment and the Service selector point at. The finding
// this guards against: the Deployment declared replicas: 1 and no
// spec.strategy, so a rollout used the default RollingUpdate and started a
// second redis-server (no --replicaof, so a master) beside the old one. Both
// pods carried app: redis-leader and both sidecars set redis-role=master, so
// replicas split across two writers.

func TestManifestLeaderDeploymentPinsRecreateStrategy(t *testing.T) {
	deployments, err := readLeaderStrategyDeployments(manifestDir)
	if err != nil {
		t.Fatalf("read example manifests: %v", err)
	}
	if len(deployments) != 2 {
		t.Fatalf("read %d Deployments from %s, want 2 (redis-leader.yaml and deployment-example.yaml): the strategy check must not pass on a manifest set it never read", len(deployments), manifestDir)
	}

	problems, masters := leaderStrategyProblems(deployments)
	for _, problem := range problems {
		t.Errorf("%s", problem)
	}
	if masters != 1 {
		t.Fatalf("found %d Deployments running a redis-server master, want exactly 1 (the redis-leader Deployment): the replica Deployment must stay exempt and the leader must be checked", masters)
	}

	for _, deployment := range deployments {
		for _, c := range deployment.Containers {
			if redisServerRoleOf(c) != redisMasterRole {
				continue
			}
			if deployment.Strategy == nil || deployment.Strategy.Type != "Recreate" {
				t.Errorf("%s: runs the redis-server master %q without strategy.type: Recreate, so a rollout starts a second master beside it", deployment.Manifest, c.Name)
			}
			if deployment.Replicas == nil || *deployment.Replicas != 1 {
				t.Errorf("%s: runs the redis-server master %q with replicas = %v, want exactly 1: every replica is a master under the same Service selector", deployment.Manifest, c.Name, deployment.Replicas)
			}
		}
	}
}

func leaderReplicas(count int32) *int32 {
	return &count
}

func leaderMasterContainer() leaderStrategyContainer {
	return leaderStrategyContainer{
		Name:    "redis",
		Image:   "redis:7-alpine",
		Command: []string{"redis-server", "--requirepass", "$(REDIS_PASSWORD)"},
	}
}

func leaderReplicaContainer() leaderStrategyContainer {
	return leaderStrategyContainer{
		Name:    "redis",
		Image:   "redis:7-alpine",
		Command: []string{"redis-server", "--replicaof", "redis-leader.default.svc.cluster.local", "6379"},
	}
}

func leaderRecreate() *deploymentStrategy {
	return &deploymentStrategy{Type: "Recreate"}
}

func leaderDeployment(strategy *deploymentStrategy, replicas *int32, containers ...leaderStrategyContainer) leaderStrategyDeployment {
	if replicas == nil {
		replicas = leaderReplicas(1)
	}
	return leaderStrategyDeployment{
		Manifest:   "fixture.yaml",
		Name:       "fixture",
		Replicas:   replicas,
		Strategy:   strategy,
		Containers: containers,
	}
}

func TestLeaderStrategyProblems_RegressionFixtures(t *testing.T) {
	sidecar := leaderStrategyContainer{Name: "redis-master-label", Image: "redis-master-label:v0.1.0"}

	tests := []struct {
		name         string
		deployments  []leaderStrategyDeployment
		wantMasters  int
		wantProblems []string
	}{
		{
			name:        "Recreate with one replica is accepted",
			deployments: []leaderStrategyDeployment{leaderDeployment(leaderRecreate(), leaderReplicas(1), leaderMasterContainer(), sidecar)},
			wantMasters: 1,
		},
		{
			name:         "default strategy is rejected",
			deployments:  []leaderStrategyDeployment{leaderDeployment(nil, leaderReplicas(1), leaderMasterContainer(), sidecar)},
			wantMasters:  1,
			wantProblems: []string{"no spec.strategy", "RollingUpdate", "redis-role=master"},
		},
		{
			name: "explicit RollingUpdate is rejected",
			deployments: []leaderStrategyDeployment{leaderDeployment(
				&deploymentStrategy{Type: "RollingUpdate", RollingUpdate: json.RawMessage(`{"maxSurge":0}`)},
				leaderReplicas(1), leaderMasterContainer(), sidecar)},
			wantMasters:  1,
			wantProblems: []string{`"RollingUpdate"`, "want Recreate"},
		},
		{
			name: "two replicas of a master are rejected",
			deployments: []leaderStrategyDeployment{leaderDeployment(
				leaderRecreate(), leaderReplicas(2), leaderMasterContainer(), sidecar)},
			wantMasters:  1,
			wantProblems: []string{"2 replicas", "split across writers"},
		},
		{
			name: "Recreate with rollingUpdate parameters is rejected",
			deployments: []leaderStrategyDeployment{leaderDeployment(
				&deploymentStrategy{Type: "Recreate", RollingUpdate: json.RawMessage(`{"maxUnavailable":1}`)},
				leaderReplicas(1), leaderMasterContainer(), sidecar)},
			wantMasters:  1,
			wantProblems: []string{"rollingUpdate parameters"},
		},
		{
			name: "a replaced redis entrypoint is rejected",
			deployments: []leaderStrategyDeployment{leaderDeployment(
				nil, leaderReplicas(1),
				leaderStrategyContainer{Name: "redis", Image: "redis:7-alpine", Command: []string{"/bin/sh", "-c", "sleep infinity"}},
				sidecar)},
			wantMasters:  0,
			wantProblems: []string{"cannot tell whether it replicates"},
		},
		{
			name: "a replica Deployment stays exempt",
			deployments: []leaderStrategyDeployment{leaderDeployment(
				nil, leaderReplicas(3), leaderReplicaContainer(), sidecar)},
			wantMasters: 0,
		},
		{
			name: "the legacy --slaveof spelling keeps the replica exempt",
			deployments: []leaderStrategyDeployment{leaderDeployment(
				nil, leaderReplicas(3), leaderStrategyContainer{
					Name:    "redis",
					Image:   "redis:7-alpine",
					Command: []string{"redis-server", "--slaveof", "redis-leader", "6379"},
				}, sidecar)},
			wantMasters: 0,
		},
		{
			name:         "an empty manifest set is rejected",
			deployments:  nil,
			wantMasters:  0,
			wantProblems: []string{"no Deployment documents were read"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			problems, masters := leaderStrategyProblems(test.deployments)
			if masters != test.wantMasters {
				t.Errorf("masters = %d, want %d", masters, test.wantMasters)
			}
			if len(test.wantProblems) == 0 {
				if len(problems) != 0 {
					t.Fatalf("accepted fixture reported problems: %v", problems)
				}
				return
			}
			joined := strings.Join(problems, "\n")
			if len(problems) == 0 {
				t.Fatalf("no problem reported, want one naming %q", test.wantProblems)
			}
			for _, want := range test.wantProblems {
				if !strings.Contains(joined, want) {
					t.Errorf("problems do not mention %q:\n%s", want, joined)
				}
			}
		})
	}
}

func TestReadLeaderStrategyDeployments_MissingDirectoryFails(t *testing.T) {
	_, err := readLeaderStrategyDeployments(filepath.Join(t.TempDir(), "missing"))
	if err == nil {
		t.Fatal("readLeaderStrategyDeployments accepted a missing directory: the strategy check must fail instead of reading nothing")
	}
}
