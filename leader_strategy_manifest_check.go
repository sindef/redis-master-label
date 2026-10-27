package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// Single-master strategy check: manifests/redis-leader.yaml declares
// `replicas: 1` and no `spec.strategy`, so the Deployment keeps the default
// RollingUpdate strategy. A rollout (a manifest edit or `kubectl rollout
// restart redis-leader`) then creates the replacement pod before terminating
// the old one, and the replacement's redis-server starts with no --replicaof,
// so it becomes a master as well. Both pods carry `app: redis-leader`, which is
// every selector the redis-leader Service and the replica deployment's
// `--replicaof redis-leader.default.svc.cluster.local` resolve through, and both
// labeler sidecars label their own pod `redis-role=master`. The result is two
// live writers and replicas syncing from whichever one the Service routes them
// to, which contradicts README "Kubernetes deployment" (that deployment runs "a
// single Redis master"). Nothing caught it: kubeconform validates the schema,
// where `strategy` is optional, and the other Go manifest readers decode
// containers, credentials, resources and RBAC but never the Deployment's
// rollout strategy.
//
// The reader and check below make the requirement visible to
// TestLeaderDeploymentUsesRecreateStrategy in leader_strategy_manifest_test.go:
// a Deployment whose redis-server starts without --replicaof must run exactly
// one replica and use `strategy: {type: Recreate}`, so at most one pod that can
// become the master exists at any moment. Replica Deployments (redis-server
// with --replicaof) are exempt: they are meant to be rolled out in numbers.

// recreateStrategyType is the only strategy that terminates the old pod before
// the replacement starts. Kubernetes defaults an absent spec.strategy to
// RollingUpdate, which does the opposite.
const recreateStrategyType = "Recreate"

// redisServerBinary is the process name a container has to start to be read as
// a Redis server. Reading the command (not the image) keeps the labeler
// sidecar out: its image reference `ghcr.io/redis-master-label/...` contains
// "redis" as well.
const redisServerBinary = "redis-server"

// redisReplicaFlags are the redis-server flags that make a server a replica of
// another one instead of a writer of its own. --slaveof is the deprecated
// spelling Redis still accepts, so a manifest using it is read the same way.
var redisReplicaFlags = []string{redisReplicaofFlag, "slaveof"}

// leaderStrategyManifest is the manifest subset the strategy check reads: the
// Deployment metadata, its replica count and rollout strategy, and the pod
// spec's container commands. A pointer for Replicas keeps an absent field (which
// Kubernetes defaults to 1) distinguishable from an explicit 0 or 2, and
// RollingUpdate stays raw JSON so its presence is readable without decoding a
// shape the check does not otherwise need.
type leaderStrategyManifest struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Replicas *int32 `json:"replicas"`
		Strategy struct {
			Type          string          `json:"type"`
			RollingUpdate json.RawMessage `json:"rollingUpdate"`
		} `json:"strategy"`
		Template struct {
			Spec struct {
				Containers []struct {
					Name    string   `json:"name"`
					Image   string   `json:"image"`
					Command []string `json:"command"`
					Args    []string `json:"args"`
				} `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
}

// leaderDeployment is one Deployment as far as its rollout strategy is
// concerned: where it came from, how many replicas it runs, the strategy it
// declares, and which of its containers start Redis as a master (no
// --replicaof) or as a replica.
type leaderDeployment struct {
	Manifest string
	Name     string
	// Replicas is the declared replica count, or nil when the manifest leaves
	// it out and Kubernetes defaults it to 1.
	Replicas *int32
	// StrategyType is spec.strategy.type as written; an empty value means the
	// manifest declares no strategy and Kubernetes applies RollingUpdate.
	StrategyType string
	// RollingUpdate reports a spec.strategy.rollingUpdate block. Recreate
	// rejects any such parameters at admission.
	RollingUpdate bool
	// MasterRedisContainers names the containers whose redis-server starts
	// without --replicaof, so every pod of this Deployment is a writer.
	MasterRedisContainers []string
	// UnreadableRedisContainers names containers called "redis" that do not
	// start redis-server from their command, so this check cannot tell whether
	// they replicate. They are reported instead of silently skipped.
	UnreadableRedisContainers []string
}

// readLeaderDeployments parses every Deployment document under dir into the
// subset above. Documents of other kinds (the example Service, the RBAC
// documents) contribute nothing, so the whole example manifest set can be read
// in one pass.
func readLeaderDeployments(dir string) ([]leaderDeployment, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var deployments []leaderDeployment
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
		var doc leaderStrategyManifest
		if err := json.Unmarshal(jsonData, &doc); err != nil {
			return nil, fmt.Errorf("%s: JSON decode: %w", path, err)
		}
		if doc.Kind != deploymentKind {
			continue
		}
		deployment := leaderDeployment{
			Manifest:     entry.Name(),
			Name:         doc.Metadata.Name,
			Replicas:     doc.Spec.Replicas,
			StrategyType: doc.Spec.Strategy.Type,
			RollingUpdate: len(strings.TrimSpace(string(doc.Spec.Strategy.RollingUpdate))) > 0 &&
				strings.TrimSpace(string(doc.Spec.Strategy.RollingUpdate)) != "null",
		}
		for _, c := range doc.Spec.Template.Spec.Containers {
			args := append(append([]string{}, c.Command...), c.Args...)
			if !startsRedisServer(args) {
				if c.Name == "redis" {
					deployment.UnreadableRedisContainers = append(deployment.UnreadableRedisContainers, c.Name)
				}
				continue
			}
			if _, replica := redisReplicaFlag(args); replica {
				continue
			}
			deployment.MasterRedisContainers = append(deployment.MasterRedisContainers, c.Name)
		}
		deployments = append(deployments, deployment)
	}
	return deployments, nil
}

// startsRedisServer reports whether a container's command starts redis-server,
// accepting a path prefix (`/usr/local/bin/redis-server`) the way an explicit
// command may carry one.
func startsRedisServer(args []string) bool {
	if len(args) == 0 {
		return false
	}
	return filepath.Base(args[0]) == redisServerBinary
}

// redisReplicaFlag returns the first replication flag the container passes
// (--replicaof or its deprecated --slaveof spelling) and whether it passes one.
func redisReplicaFlag(args []string) (string, bool) {
	for _, name := range redisReplicaFlags {
		if _, found := containerFlag(args, name); found {
			return name, true
		}
	}
	return "", false
}

// singleWriterDeploymentProblems returns one message per way a Deployment that
// starts Redis as a master allows a second master to exist at the same time,
// plus the number of such Deployments read so a caller can tell "every
// single-master Deployment is pinned" from "no single-master Deployment was
// found".
func singleWriterDeploymentProblems(deployments []leaderDeployment) (problems []string, leaders int) {
	for _, d := range deployments {
		for _, container := range d.UnreadableRedisContainers {
			problems = append(problems, fmt.Sprintf("%s: Deployment %q container %q is named redis but does not start %s in its command, so this check cannot tell whether it starts as a master or as a replica. Declare command: [%s, ...] so the rollout strategy this check requires is provable",
				d.Manifest, d.Name, container, redisServerBinary, redisServerBinary))
		}
		if len(d.MasterRedisContainers) == 0 {
			continue
		}
		leaders++

		if d.Replicas != nil && *d.Replicas != 1 {
			problems = append(problems, fmt.Sprintf("%s: Deployment %q declares replicas: %d, but its redis-server starts without --replicaof, so every replica is a Redis master and the redis-leader Service selects all of them. Run exactly one replica",
				d.Manifest, d.Name, *d.Replicas))
		}

		switch {
		case d.StrategyType == "":
			problems = append(problems, fmt.Sprintf("%s: Deployment %q declares no spec.strategy, so Kubernetes applies the default RollingUpdate: %s. Set spec.strategy.type: %s so the old pod is terminated before the replacement starts",
				d.Manifest, d.Name, singleWriterConsequence(), recreateStrategyType))
		case d.StrategyType != recreateStrategyType:
			problems = append(problems, fmt.Sprintf("%s: Deployment %q declares spec.strategy.type: %q, but its redis-server starts without --replicaof, so %s. Set spec.strategy.type: %s so the old pod is terminated before the replacement starts",
				d.Manifest, d.Name, d.StrategyType, singleWriterConsequence(), recreateStrategyType))
		case d.RollingUpdate:
			problems = append(problems, fmt.Sprintf("%s: Deployment %q sets spec.strategy.type: %s together with spec.strategy.rollingUpdate parameters: the API server rejects that combination (Recreate has no rolling update), so the Deployment is refused instead of rolled out. Delete the rollingUpdate block",
				d.Manifest, d.Name, recreateStrategyType))
		}
	}
	return problems, leaders
}

// singleWriterConsequence describes what a rolling replacement of a
// single-master Redis Deployment does, so a failure says why the default
// strategy is wrong here rather than only which field to edit.
func singleWriterConsequence() string {
	return "a rollout (a manifest edit or `kubectl rollout restart` included) creates the replacement pod before terminating the old one, and the replacement's redis-server starts with no --replicaof as well, so two pods labelled app: redis-leader are masters at the same time: the redis-leader Service routes the replicas across both and both labeler sidecars set redis-role=master"
}
