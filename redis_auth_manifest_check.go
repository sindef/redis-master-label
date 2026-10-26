package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Redis authentication consistency check: both example Deployments inject a
// Redis credential into the labeler sidecar (REDIS_PASSWORD from the
// redis-secret Secret), and go-redis sends AUTH whenever the password is
// non-empty. The co-located redis containers used to start as plain
// `redis-server`, with no --requirepass, so Redis answered every AUTH with
// "ERR Client sent AUTH, but no password is set": no ROLE call succeeded, the
// label was never applied, /healthz turned 503 after three failures and the
// liveness probe restarted the sidecar. README told the operator to create
// redis-secret with a non-empty value, which is exactly the failing case.
//
// The reader below makes the pod self-consistency visible to
// TestManifestRedisAuthMatchesSidecarCredential in
// redis_auth_manifest_test.go: a Deployment whose labeler reads a Redis
// credential must start every redis container with --requirepass (and, for a
// replica, --masterauth) expanded from that same Secret and key, so the
// credential the sidecar sends is one Redis accepts. A Deployment that wires
// no credential at all stays valid: removing REDIS_PASSWORD from both
// containers is the documented way to run Redis unauthenticated.

// redisPasswordEnv is the env var the binary falls back to when
// --redis-password is not set, and the variable the manifests expand into the
// redis-server flags so the value never lands in the pod spec.
const redisPasswordEnv = "REDIS_PASSWORD"

// redisPasswordExpansion is the Kubernetes env reference redis-server's flags
// carry. The kubelet replaces it at container start; the pod spec keeps only
// the reference, not the credential.
const redisPasswordExpansion = "$(" + redisPasswordEnv + ")"

// redisAuthContainer is one container of one example Deployment as far as the
// Redis credential is concerned: its image, its combined command/args and its
// env list.
type redisAuthContainer struct {
	Manifest string
	Name     string
	Image    string
	Env      []containerEnvVar
	Args     []string
}

// redisPasswordSecretRef returns the Secret name and key a container reads
// REDIS_PASSWORD from, or false when it does not read one from a Secret.
func redisPasswordSecretRef(env []containerEnvVar) (containerSecretKeyRef, bool) {
	for _, e := range env {
		if e.Name != redisPasswordEnv || e.ValueFrom.SecretKeyRef == nil {
			continue
		}
		ref := *e.ValueFrom.SecretKeyRef
		if ref.Name == "" || ref.Key == "" {
			continue
		}
		return ref, true
	}
	return containerSecretKeyRef{}, false
}

// containerFlagValue returns the value a container's command or args give to
// one flag, accepting both --flag=value and --flag value, and the separate
// forms the redis-server command line uses.
func containerFlagValue(args []string, name string) (string, bool) {
	for i, arg := range args {
		flagName, ok := flagNameFromArg(arg)
		if !ok || flagName != name {
			continue
		}
		return flagValue(arg, args, i), true
	}
	return "", false
}

// isRedisContainerImage reports whether a container runs the shipped Redis
// image. The labeler image is matched by sidecarImage elsewhere; this helper
// only has to tell the co-located redis container from other containers.
func isRedisContainerImage(image string) bool {
	return strings.Contains(image, "redis:")
}

// redisAuthProblems walks every Deployment under dir and returns one message
// per inconsistency between the Redis credential a labeler reads and the
// authentication the co-located redis containers configure. It also returns
// the number of Deployments, redis containers and Secret-backed sidecar
// credentials read, so a caller can tell "every pod is consistent" from "no
// pod was read".
func redisAuthProblems(dir string) (problems []string, deployments, redisContainers, sidecarCredentials int, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, 0, 0, fmt.Errorf("read %s: %w", dir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := filepath.Ext(entry.Name())
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		doc, err := readPodSpecManifest(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, deployments, redisContainers, sidecarCredentials, fmt.Errorf("parse manifest: %w", err)
		}
		if doc.Kind != deploymentKind {
			continue
		}
		deployments++

		var sidecarRefs []containerSecretKeyRef
		var redis []redisAuthContainer
		for _, c := range doc.Spec.Template.Spec.Containers {
			args := append(append([]string{}, c.Command...), c.Args...)
			if strings.Contains(c.Image, sidecarImage) {
				if ref, ok := redisPasswordSecretRef(c.Env); ok {
					sidecarRefs = append(sidecarRefs, ref)
					sidecarCredentials++
				}
				continue
			}
			if !isRedisContainerImage(c.Image) {
				continue
			}
			redisContainers++
			redis = append(redis, redisAuthContainer{
				Manifest: entry.Name(),
				Name:     c.Name,
				Image:    c.Image,
				Env:      c.Env,
				Args:     args,
			})
		}

		for _, r := range redis {
			where := fmt.Sprintf("%s/%s", r.Manifest, r.Name)
			ref, readsPassword := redisPasswordSecretRef(r.Env)
			requirepass, hasRequirepass := containerFlagValue(r.Args, "requirepass")
			masterauth, hasMasterauth := containerFlagValue(r.Args, "masterauth")
			_, replicating := containerFlagValue(r.Args, "replicaof")

			if len(sidecarRefs) == 0 {
				if readsPassword || hasRequirepass {
					problems = append(problems, fmt.Sprintf(
						"%s: requires a Redis password (REDIS_PASSWORD/--requirepass), but the redis-master-label sidecar of the same pod reads no credential: the sidecar connects without AUTH and Redis answers every ROLE call with NOAUTH, so no label is applied and /healthz turns 503. Either wire REDIS_PASSWORD for both containers from the same Secret (valueFrom.secretKeyRef) as README \"Kubernetes deployment\" documents, or remove the password from both",
						where))
				}
				continue
			}

			want := sidecarRefs[0]
			if !readsPassword {
				problems = append(problems, fmt.Sprintf(
					"%s: reads no REDIS_PASSWORD from a Secret while the redis-master-label sidecar of the same pod reads %s/%s: start Redis from the same Secret so the AUTH the sidecar sends is accepted, for example env REDIS_PASSWORD from secretKeyRef %s/%s plus `--requirepass %s`",
					where, want.Name, want.Key, want.Name, want.Key, redisPasswordExpansion))
			} else if ref != want {
				problems = append(problems, fmt.Sprintf(
					"%s: reads REDIS_PASSWORD from Secret %s/%s, but the redis-master-label sidecar of the same pod reads %s/%s: Redis and the sidecar must share one credential, or every ROLE call fails AUTH and /healthz turns 503",
					where, ref.Name, ref.Key, want.Name, want.Key))
			}

			if !hasRequirepass {
				problems = append(problems, fmt.Sprintf(
					"%s: starts redis-server without --requirepass while the redis-master-label sidecar sends AUTH from Secret %s/%s: Redis answers \"ERR Client sent AUTH, but no password is set\", so no label is applied and /healthz turns 503. Add `--requirepass %s` so the same Secret authenticates the sidecar",
					where, want.Name, want.Key, redisPasswordExpansion))
			} else if requirepass != redisPasswordExpansion {
				problems = append(problems, fmt.Sprintf(
					"%s: --requirepass = %q, want the env expansion %q: a literal credential in args is recorded in the pod spec, while the Secret reference keeps it out and makes Redis accept the sidecar's AUTH",
					where, requirepass, redisPasswordExpansion))
			}

			if replicating && !hasMasterauth {
				problems = append(problems, fmt.Sprintf(
					"%s: is a replica (--replicaof) but has no --masterauth while the leader requires the password from Secret %s/%s: replication stops with NOAUTH. Add `--masterauth %s` from the same Secret",
					where, want.Name, want.Key, redisPasswordExpansion))
			} else if hasMasterauth && masterauth != redisPasswordExpansion {
				problems = append(problems, fmt.Sprintf(
					"%s: --masterauth = %q, want the env expansion %q: the replica must authenticate to the leader with the same Secret the sidecar reads",
					where, masterauth, redisPasswordExpansion))
			}
		}
	}

	return problems, deployments, redisContainers, sidecarCredentials, nil
}
