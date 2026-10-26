package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// Redis authentication check: both example Deployments give the
// redis-master-label sidecar a REDIS_PASSWORD from the redis-secret Secret, so
// go-redis sends AUTH on every connection. Neither example used to configure
// Redis itself with a password: redis-server started bare, replied
// "ERR Client sent AUTH, but no password is set" to the sidecar's AUTH, and
// ROLE never succeeded, so the master pod was never labeled, /healthz answered
// 503 after three failures and the liveness probe restarted the sidecar
// forever. kubeconform validates the manifests against the Kubernetes schemas
// and never compares two containers' credentials, and the readers in
// cmd_manifest_check.go only inspect the sidecar, so nothing caught the drift.
//
// The reader and check below make the contract visible to
// redis_auth_manifest_test.go (TestManifestRedisAuthMatchesSidecarCredential):
// for every example Deployment, a redis container authenticates with exactly
// the Secret reference its co-located sidecar reads, or neither container uses
// a credential at all.
//
// The credential stays out of the pod spec: the redis container gets it through
// an env entry from the Secret and references it as $(REDIS_PASSWORD), which
// the kubelet expands when it starts the container.

// redisAuthEnvVar is the environment variable both containers read the Redis
// credential from. main.go falls back to it when --redis-password is not set.
const redisAuthEnvVar = "REDIS_PASSWORD"

// redisAuthFlagExpansion is how the redis container references its env entry in
// command/args. Kubernetes expands $(VAR) from the container's own env list, so
// the manifest never holds the password text.
const redisAuthFlagExpansion = "$(" + redisAuthEnvVar + ")"

// The redis-server arguments that decide whether the server authenticates
// clients (--requirepass) and how it authenticates to its leader
// (--masterauth).
const (
	redisRequirepassFlag = "requirepass"
	redisMasterauthFlag  = "masterauth"
	redisReplicaofFlag   = "replicaof"
)

// manifestSecretKeyRef is a valueFrom.secretKeyRef: the Secret and key a
// container's env entry reads.
type manifestSecretKeyRef struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

// manifestEnvValueFrom is the valueFrom half of a container env entry.
type manifestEnvValueFrom struct {
	SecretKeyRef *manifestSecretKeyRef `json:"secretKeyRef"`
}

// manifestEnvVar is one container env entry. Value is a pointer so a literal
// (and therefore committed) credential is distinguishable from an absent one.
type manifestEnvVar struct {
	Name      string                `json:"name"`
	Value     *string               `json:"value"`
	ValueFrom *manifestEnvValueFrom `json:"valueFrom"`
}

// redisAuthContainer is one container as far as its Redis credential is
// concerned: who it is, the arguments it starts with and the env it reads.
type redisAuthContainer struct {
	Name    string
	Image   string
	Command []string
	Args    []string
	Env     []manifestEnvVar
}

// redisAuthPod is one Deployment's pod spec, together with the manifest file it
// came from so a failure names that file instead of the test.
type redisAuthPod struct {
	Manifest   string
	Name       string
	Containers []redisAuthContainer
}

// redisAuthManifest is the manifest subset the credential check reads.
type redisAuthManifest struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Template struct {
			Spec struct {
				Containers []struct {
					Name    string           `json:"name"`
					Image   string           `json:"image"`
					Command []string         `json:"command"`
					Args    []string         `json:"args"`
					Env     []manifestEnvVar `json:"env"`
				} `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
}

// readRedisAuthPod parses one manifest into the Redis credential subset above.
func readRedisAuthPod(path, manifest string) (redisAuthPod, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return redisAuthPod{}, err
	}
	jsonData, err := utilyaml.ToJSON(data)
	if err != nil {
		return redisAuthPod{}, fmt.Errorf("%s: YAML to JSON: %w", path, err)
	}
	var doc redisAuthManifest
	if err := json.Unmarshal(jsonData, &doc); err != nil {
		return redisAuthPod{}, fmt.Errorf("%s: JSON decode: %w", path, err)
	}
	pod := redisAuthPod{Manifest: manifest, Name: doc.Metadata.Name}
	if doc.Kind != deploymentKind {
		return pod, nil
	}
	for _, c := range doc.Spec.Template.Spec.Containers {
		pod.Containers = append(pod.Containers, redisAuthContainer{
			Name:    c.Name,
			Image:   c.Image,
			Command: c.Command,
			Args:    c.Args,
			Env:     c.Env,
		})
	}
	return pod, nil
}

// readRedisAuthPods reads every Deployment under dir and returns its pod spec.
// Documents of other kinds (the example Service) contribute no containers, so
// the whole example manifest set can be read in one pass.
func readRedisAuthPods(dir string) ([]redisAuthPod, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var pods []redisAuthPod
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := filepath.Ext(entry.Name())
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		pod, err := readRedisAuthPod(filepath.Join(dir, entry.Name()), entry.Name())
		if err != nil {
			return nil, fmt.Errorf("parse manifest: %w", err)
		}
		if len(pod.Containers) == 0 {
			continue
		}
		pods = append(pods, pod)
	}
	return pods, nil
}

// containerFlag returns the value of a redis-server flag in one container's
// command/args. Both forms redis-server accepts are read: `--flag value` and
// `--flag=value`.
func containerFlag(args []string, name string) (value string, found bool) {
	for i, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		trimmed := strings.TrimLeft(arg, "-")
		if trimmed == name {
			if i+1 < len(args) {
				return args[i+1], true
			}
			return "", true
		}
		if value, ok := strings.CutPrefix(trimmed, name+"="); ok {
			return value, true
		}
	}
	return "", false
}

// containerSecretRef returns the Secret reference a container's env entry reads,
// and whether the entry exists at all. A literal value (value instead of
// valueFrom) reports the entry as present without a reference, because that
// shape commits the credential to the pod spec.
func containerSecretRef(c redisAuthContainer, envName string) (ref manifestSecretKeyRef, present, fromSecret bool) {
	for _, e := range c.Env {
		if e.Name != envName {
			continue
		}
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			return *e.ValueFrom.SecretKeyRef, true, true
		}
		return manifestSecretKeyRef{}, true, false
	}
	return manifestSecretKeyRef{}, false, false
}

// redisAuthProblems returns one message per way the example Deployments' Redis
// authentication and their sidecar's credential disagree, or nil when every
// Deployment is consistent. A Deployment is consistent in one of two shapes:
// the redis container authenticates with exactly the Secret reference its
// sidecar reads (and a replica also authenticates to its leader), or neither
// container carries a credential and Redis runs unauthenticated.
func redisAuthProblems(pods []redisAuthPod) []string {
	if len(pods) == 0 {
		return []string{fmt.Sprintf("no example Deployments read from %s: the Redis credential check proved nothing", manifestDir)}
	}

	var problems []string
	for _, pod := range pods {
		var sidecars, redisContainers []redisAuthContainer
		for _, c := range pod.Containers {
			switch {
			case strings.Contains(c.Image, sidecarImage):
				sidecars = append(sidecars, c)
			case c.Name == "redis":
				redisContainers = append(redisContainers, c)
			}
		}
		if len(sidecars) == 0 {
			problems = append(problems, fmt.Sprintf("%s: deployment %q runs no %s sidecar, so no credential can match its Redis", pod.Manifest, pod.Name, sidecarImage))
			continue
		}
		if len(redisContainers) == 0 {
			problems = append(problems, fmt.Sprintf("%s: deployment %q runs no co-located redis container, so the sidecar's credential has nothing to authenticate to", pod.Manifest, pod.Name))
			continue
		}

		expected, expectedPresent, sidecarProblems := sidecarRedisCredential(pod, sidecars)
		problems = append(problems, sidecarProblems...)
		if len(sidecarProblems) > 0 {
			continue
		}

		for _, redis := range redisContainers {
			problems = append(problems, redisContainerAuthProblems(pod, redis, expected, expectedPresent)...)
		}
	}
	return problems
}

// sidecarRedisCredential returns the Secret reference every labeler sidecar of a
// pod reads REDIS_PASSWORD from, and whether the pod uses authentication at all.
// Sidecars that disagree, or a sidecar that carries the credential as a literal
// value, are reported instead: the redis container could not match both.
func sidecarRedisCredential(pod redisAuthPod, sidecars []redisAuthContainer) (ref manifestSecretKeyRef, present bool, problems []string) {
	var firstName string
	for _, c := range sidecars {
		got, envPresent, fromSecret := containerSecretRef(c, redisAuthEnvVar)
		if !envPresent {
			if present {
				problems = append(problems, fmt.Sprintf("%s: sidecar %q sets no %s env, but sidecar %q does: the two sidecars would connect to the same Redis differently", pod.Manifest, c.Name, redisAuthEnvVar, firstName))
			}
			continue
		}
		if !fromSecret {
			problems = append(problems, fmt.Sprintf("%s: sidecar %q sets %s without valueFrom.secretKeyRef: a literal credential lands in the pod spec, where anyone with pod read access sees it", pod.Manifest, c.Name, redisAuthEnvVar))
			continue
		}
		if !present {
			ref, present, firstName = got, true, c.Name
			continue
		}
		if got != ref {
			problems = append(problems, fmt.Sprintf("%s: sidecar %q reads %s from Secret %q key %q, but sidecar %q reads Secret %q key %q: the redis container can match only one", pod.Manifest, firstName, redisAuthEnvVar, ref.Name, ref.Key, c.Name, got.Name, got.Key))
		}
	}
	return ref, present, problems
}

// redisContainerAuthProblems returns one message per way a redis container's
// authentication disagrees with what the sidecar sends, or nil when it matches.
func redisContainerAuthProblems(pod redisAuthPod, redis redisAuthContainer, expected manifestSecretKeyRef, expectedPresent bool) []string {
	where := fmt.Sprintf("%s: redis container %q", pod.Manifest, redis.Name)
	args := append(append([]string{}, redis.Command...), redis.Args...)

	ref, envPresent, fromSecret := containerSecretRef(redis, redisAuthEnvVar)
	requirepass, hasRequirepass := containerFlag(args, redisRequirepassFlag)
	masterauth, hasMasterauth := containerFlag(args, redisMasterauthFlag)
	_, isReplica := containerFlag(args, redisReplicaofFlag)

	var problems []string
	if !expectedPresent {
		// The accepted unauthenticated shape: neither container holds a
		// credential and Redis accepts unauthenticated clients.
		if envPresent {
			problems = append(problems, fmt.Sprintf("%s sets %s but the %s sidecar does not: the sidecar connects unauthenticated and Redis rejects it with NOAUTH", where, redisAuthEnvVar, sidecarImage))
		}
		if hasRequirepass {
			problems = append(problems, fmt.Sprintf("%s starts with --requirepass %q but the %s sidecar sends no credential: AUTH would fail, so drop both or supply the Secret to both", where, requirepass, sidecarImage))
		}
		if hasMasterauth {
			problems = append(problems, fmt.Sprintf("%s starts with --masterauth %q but the %s sidecar sends no credential: keep the two containers consistent", where, masterauth, sidecarImage))
		}
		return problems
	}

	if !envPresent {
		problems = append(problems, fmt.Sprintf("%s sets no %s env, but the %s sidecar reads it from Secret %q key %q: Redis accepts unauthenticated clients while the sidecar sends AUTH, so Redis replies %q and ROLE never succeeds", where, redisAuthEnvVar, sidecarImage, expected.Name, expected.Key, "ERR Client sent AUTH, but no password is set"))
	} else if !fromSecret {
		problems = append(problems, fmt.Sprintf("%s sets %s without valueFrom.secretKeyRef: a literal credential lands in the pod spec", where, redisAuthEnvVar))
	} else if ref != expected {
		problems = append(problems, fmt.Sprintf("%s reads %s from Secret %q key %q, but the %s sidecar reads Secret %q key %q: the sidecar's AUTH cannot succeed against a Redis configured with another password", where, redisAuthEnvVar, ref.Name, ref.Key, sidecarImage, expected.Name, expected.Key))
	}

	if !hasRequirepass {
		problems = append(problems, fmt.Sprintf("%s starts without --requirepass: Redis then has no password while the %s sidecar sends AUTH from Secret %q key %q, so Redis replies %q and ROLE never succeeds", where, sidecarImage, expected.Name, expected.Key, "ERR Client sent AUTH, but no password is set"))
	} else if requirepass != redisAuthFlagExpansion {
		problems = append(problems, fmt.Sprintf("%s passes --requirepass %q, want %q: the credential has to come from the container's own %s env entry so it never lands in the pod spec", where, requirepass, redisAuthFlagExpansion, redisAuthEnvVar))
	}

	if isReplica {
		if !hasMasterauth {
			problems = append(problems, fmt.Sprintf("%s replicates (--replicaof) without --masterauth: the leader requires a password, so replication stops with NOAUTH", where))
		} else if masterauth != redisAuthFlagExpansion {
			problems = append(problems, fmt.Sprintf("%s passes --masterauth %q, want %q: the credential has to come from the container's own %s env entry", where, masterauth, redisAuthFlagExpansion, redisAuthEnvVar))
		}
	}

	return problems
}
