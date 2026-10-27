package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// Leader single-master strategy check: manifests/redis-leader.yaml runs the
// Redis master that the replica Deployment points at and that the Service
// selector matches by `app: redis-leader`. Master election in the example is
// that label: the co-located sidecar applies `redis-role=master` whenever its
// redis-server reports the master role, and every pod carrying `app:
// redis-leader` is reachable through the Service.
//
// The Deployment declared replicas: 1 and no spec.strategy, so Kubernetes
// defaulted to RollingUpdate. A rollout - `kubectl rollout restart` included -
// created the replacement pod before terminating the old one. The replacement
// redis-server starts without --replicaof, so it became a master too; both pods
// carried `app: redis-leader` and both sidecars set redis-role=master, so
// replicas split across two writers. `strategy: {type: Recreate}` terminates the
// old pod before the new one starts, which costs a brief write outage but keeps
// exactly one master.
//
// Nothing caught the default: kubeconform validates the schema, where strategy
// is optional, and the manifest readers in cmd_manifest_check.go decoded the
// pod spec only. The reader and check below make the strategy visible to
// leader_strategy_manifest_test.go, which runs against the shipped manifests
// and against fixtures of them. Replica Deployments - a redis-server started
// with --replicaof (or the legacy --slaveof) - stay exempt: they are supposed
// to roll.

// redisSlaveofFlag is the deprecated spelling of --replicaof. Redis still
// accepts it, so a container that starts with it is a replica too and must not
// be asked for the single-master strategy.
const redisSlaveofFlag = "slaveof"

// redisServerBinary is the executable a container has to start to be a Redis
// server. It is matched on the base name so /usr/local/bin/redis-server counts.
const redisServerBinary = "redis-server"

// redisServerRole is how one container participates in the master/replica
// split, as far as its command line tells.
type redisServerRole int

const (
	notRedisServer redisServerRole = iota
	unknownRedisRole
	redisMasterRole
	redisReplicaRole
)

// leaderStrategyContainer is one container of an example Deployment, reduced to
// what decides whether it starts a Redis master.
type leaderStrategyContainer struct {
	Name    string
	Image   string
	Command []string
	Args    []string
}

// deploymentStrategy is the spec.strategy of a Deployment. RollingUpdate stays
// raw because the check only needs to know whether the block is present: a
// Recreate strategy that still carries rollingUpdate parameters is rejected by
// the API server, so the check reports it instead of decoding values.
type deploymentStrategy struct {
	Type          string          `json:"type"`
	RollingUpdate json.RawMessage `json:"rollingUpdate"`
}

// leaderStrategyDeployment is one Deployment document together with the
// manifest file it came from, so a failure names that file instead of the test.
type leaderStrategyDeployment struct {
	Manifest   string
	Name       string
	Replicas   *int32
	Strategy   *deploymentStrategy
	Containers []leaderStrategyContainer
}

// leaderStrategyDocument is the manifest subset the strategy check reads.
type leaderStrategyDocument struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Replicas *int32              `json:"replicas"`
		Strategy *deploymentStrategy `json:"strategy"`
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

// readLeaderStrategyDeployments reads every Deployment under dir. Documents of
// other kinds (the example Service, Role, RoleBinding and ServiceAccount)
// contribute nothing, so the whole example manifest set can be read in one
// pass. A directory that cannot be read is an error, never an empty result: a
// renamed directory must fail the check instead of passing it on no input.
func readLeaderStrategyDeployments(dir string) ([]leaderStrategyDeployment, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var deployments []leaderStrategyDeployment
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
		var doc leaderStrategyDocument
		if err := json.Unmarshal(jsonData, &doc); err != nil {
			return nil, fmt.Errorf("%s: JSON decode: %w", path, err)
		}
		if doc.Kind != deploymentKind {
			continue
		}
		deployment := leaderStrategyDeployment{
			Manifest: entry.Name(),
			Name:     doc.Metadata.Name,
			Replicas: doc.Spec.Replicas,
			Strategy: doc.Spec.Strategy,
		}
		for _, c := range doc.Spec.Template.Spec.Containers {
			deployment.Containers = append(deployment.Containers, leaderStrategyContainer{
				Name:    c.Name,
				Image:   c.Image,
				Command: c.Command,
				Args:    c.Args,
			})
		}
		deployments = append(deployments, deployment)
	}
	return deployments, nil
}

// redisServerRoleOf classifies one container by the redis-server invocation in
// its command and args. A container named redis whose entrypoint is not
// redis-server reports unknownRedisRole rather than being skipped: replacing the
// entrypoint must not hide a master from the strategy check. Arguments are
// matched with containerFlag, so `--replicaof host port` and
// `--replicaof=host:port` both count.
func redisServerRoleOf(c leaderStrategyContainer) redisServerRole {
	args := append(append([]string{}, c.Command...), c.Args...)
	start := -1
	for i, arg := range args {
		if filepath.Base(arg) == redisServerBinary {
			start = i
			break
		}
	}
	if start < 0 {
		if c.Name == "redis" {
			return unknownRedisRole
		}
		return notRedisServer
	}
	serverArgs := args[start+1:]
	for _, flag := range []string{redisReplicaofFlag, redisSlaveofFlag} {
		if _, ok := containerFlag(serverArgs, flag); ok {
			return redisReplicaRole
		}
	}
	return redisMasterRole
}

// rollingUpdateParameters reports whether a Recreate strategy still carries a
// rollingUpdate block. Kubernetes rejects that combination at admission, so a
// manifest that keeps it never reaches the cluster.
func rollingUpdateParameters(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}

// leaderStrategyProblems returns one message per way a Deployment that runs a
// Redis master can end up with two masters, plus the number of such
// Deployments read, so a caller can tell "exactly one leader was checked" from
// "no leader was found". Replica Deployments contribute no problems and are not
// counted.
func leaderStrategyProblems(deployments []leaderStrategyDeployment) (problems []string, masters int) {
	if len(deployments) == 0 {
		return []string{"no Deployment documents were read from manifests/: the single-master strategy check cannot pass on a manifest set it never read"}, 0
	}

	for _, deployment := range deployments {
		where := fmt.Sprintf("%s/%s", deployment.Manifest, deployment.Name)
		masterContainer := ""
		for _, c := range deployment.Containers {
			switch redisServerRoleOf(c) {
			case unknownRedisRole:
				problems = append(problems, fmt.Sprintf("%s/%s: the redis container does not start redis-server, so the check cannot tell whether it replicates; start redis-server in the container's command (with --replicaof for a replica) or the single-master rule cannot be verified", where, c.Name))
			case redisMasterRole:
				if masterContainer == "" {
					masterContainer = c.Name
				}
			}
		}
		if masterContainer == "" {
			continue
		}
		masters++

		replicas := int32(1)
		if deployment.Replicas != nil {
			replicas = *deployment.Replicas
		}
		if replicas > 1 {
			problems = append(problems, fmt.Sprintf("%s: runs %d replicas of the redis-server master in container %s (it starts without --replicaof); every replica becomes its own master under the same Service selector and labels itself redis-role=master, so clients split across writers. Keep the leader at one replica", where, replicas, masterContainer))
		}

		if deployment.Strategy == nil {
			problems = append(problems, fmt.Sprintf("%s: declares no spec.strategy, so Kubernetes defaults to RollingUpdate; a rollout (kubectl rollout restart included) creates the replacement master before the old pod terminates, and both label themselves redis-role=master. Declare strategy.type: Recreate", where))
			continue
		}
		if deployment.Strategy.Type != "Recreate" {
			problems = append(problems, fmt.Sprintf("%s: spec.strategy.type = %q, want Recreate; RollingUpdate brings up a second redis-server master beside the old one during a rollout, so replicas split across two writers", where, deployment.Strategy.Type))
			continue
		}
		if rollingUpdateParameters(deployment.Strategy.RollingUpdate) {
			problems = append(problems, fmt.Sprintf("%s: spec.strategy.type = Recreate also carries rollingUpdate parameters, which the API server rejects; remove the rollingUpdate block", where))
		}
	}
	return problems, masters
}
