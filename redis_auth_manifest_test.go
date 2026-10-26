package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Redis authentication regression tests: the example Deployments have to point
// the redis container and the labeler sidecar at the same credential. The
// finding this guards against: both sidecars read REDIS_PASSWORD from
// redis-secret, but redis-server started with no --requirepass, so AUTH failed
// with "ERR Client sent AUTH, but no password is set", ROLE never succeeded,
// /healthz answered 503 and the liveness probe restarted the labeler.

func TestManifestRedisAuthMatchesSidecarCredential(t *testing.T) {
	pods, err := readRedisAuthPods(manifestDir)
	if err != nil {
		t.Fatalf("read example manifests: %v", err)
	}
	if len(pods) != 2 {
		t.Fatalf("read %d Deployments from %s, want 2 (redis-leader.yaml and deployment-example.yaml): the credential check must not pass on a manifest set it never read", len(pods), manifestDir)
	}

	if problems := redisAuthProblems(pods); len(problems) > 0 {
		for _, problem := range problems {
			t.Errorf("%s", problem)
		}
	}

	// Both shapes the check accepts were counted here, so a manifest edited to
	// the unauthenticated shape has to say so in this test's expectations
	// instead of silently passing.
	redisContainers, sidecars := 0, 0
	for _, pod := range pods {
		for _, c := range pod.Containers {
			if strings.Contains(c.Image, sidecarImage) {
				sidecars++
				continue
			}
			if c.Name != "redis" {
				continue
			}
			redisContainers++
			args := append(append([]string{}, c.Command...), c.Args...)
			requirepass, ok := containerFlag(args, redisRequirepassFlag)
			if !ok {
				t.Errorf("%s: redis container starts without --requirepass, so the sidecar's AUTH fails", pod.Manifest)
				continue
			}
			if requirepass != redisAuthFlagExpansion {
				t.Errorf("%s: redis container passes --requirepass %q, want %q so the Secret value is never committed to the manifest", pod.Manifest, requirepass, redisAuthFlagExpansion)
			}
			if _, replica := containerFlag(args, redisReplicaofFlag); replica {
				if masterauth, ok := containerFlag(args, redisMasterauthFlag); !ok || masterauth != redisAuthFlagExpansion {
					t.Errorf("%s: replica passes --masterauth %q (found %v), want %q so replication to the authenticated leader does not stop with NOAUTH", pod.Manifest, masterauth, ok, redisAuthFlagExpansion)
				}
			}
		}
	}
	if redisContainers != 2 || sidecars != 2 {
		t.Fatalf("read %d redis containers and %d %s sidecars across the example Deployments, want 2 of each", redisContainers, sidecars, sidecarImage)
	}
}

func TestManifestRedisAuthMatchesSidecarCredential_MismatchedSecretFails(t *testing.T) {
	password := manifestSecretKeyRef{Name: "redis-secret", Key: "password"}
	other := manifestSecretKeyRef{Name: "redis-secret", Key: "other-key"}

	pod := redisAuthPod{
		Manifest: "fixture.yaml",
		Name:     "fixture",
		Containers: []redisAuthContainer{
			redisAuthFixtureRedis(password, redisAuthFlagExpansion, "", false),
			redisAuthFixtureSidecar(&other),
		},
	}
	problems := redisAuthProblems([]redisAuthPod{pod})
	if len(problems) == 0 {
		t.Fatal("redisAuthProblems accepted a redis container reading a different Secret key than its sidecar: the sidecar's AUTH would fail at runtime")
	}
	if !strings.Contains(strings.Join(problems, "\n"), "other-key") {
		t.Fatalf("problems do not name the mismatched key: %v", problems)
	}
}

func TestRedisAuthProblems_RegressionFixtures(t *testing.T) {
	password := manifestSecretKeyRef{Name: "redis-secret", Key: "password"}
	otherName := manifestSecretKeyRef{Name: "other-secret", Key: "password"}

	tests := []struct {
		name        string
		containers  func() []redisAuthContainer
		wantProblem bool
	}{
		{
			name: "matching credential is accepted",
			containers: func() []redisAuthContainer {
				return []redisAuthContainer{
					redisAuthFixtureRedis(password, redisAuthFlagExpansion, "", false),
					redisAuthFixtureSidecar(&password),
				}
			},
		},
		{
			name: "unauthenticated Redis and sidecar are accepted together",
			containers: func() []redisAuthContainer {
				return []redisAuthContainer{
					redisAuthFixtureRedis(manifestSecretKeyRef{}, "", "", false),
					redisAuthFixtureSidecar(nil),
				}
			},
		},
		{
			name: "missing --requirepass is rejected",
			containers: func() []redisAuthContainer {
				return []redisAuthContainer{
					redisAuthFixtureRedis(password, "", "", false),
					redisAuthFixtureSidecar(&password),
				}
			},
			wantProblem: true,
		},
		{
			name: "different Secret name is rejected",
			containers: func() []redisAuthContainer {
				return []redisAuthContainer{
					redisAuthFixtureRedis(otherName, redisAuthFlagExpansion, "", false),
					redisAuthFixtureSidecar(&password),
				}
			},
			wantProblem: true,
		},
		{
			name: "redis container without the env entry is rejected",
			containers: func() []redisAuthContainer {
				return []redisAuthContainer{
					redisAuthFixtureRedis(manifestSecretKeyRef{}, redisAuthFlagExpansion, "", false),
					redisAuthFixtureSidecar(&password),
				}
			},
			wantProblem: true,
		},
		{
			name: "literal credential in redis args is rejected",
			containers: func() []redisAuthContainer {
				return []redisAuthContainer{
					redisAuthFixtureRedis(password, "hunter2", "", false),
					redisAuthFixtureSidecar(&password),
				}
			},
			wantProblem: true,
		},
		{
			name: "replica without --masterauth is rejected",
			containers: func() []redisAuthContainer {
				return []redisAuthContainer{
					redisAuthFixtureRedis(password, redisAuthFlagExpansion, "", true),
					redisAuthFixtureSidecar(&password),
				}
			},
			wantProblem: true,
		},
		{
			name: "replica with --masterauth is accepted",
			containers: func() []redisAuthContainer {
				return []redisAuthContainer{
					redisAuthFixtureRedis(password, redisAuthFlagExpansion, redisAuthFlagExpansion, true),
					redisAuthFixtureSidecar(&password),
				}
			},
		},
		{
			name: "replica with a literal --masterauth is rejected",
			containers: func() []redisAuthContainer {
				return []redisAuthContainer{
					redisAuthFixtureRedis(password, redisAuthFlagExpansion, "hunter2", true),
					redisAuthFixtureSidecar(&password),
				}
			},
			wantProblem: true,
		},
		{
			name: "authenticated sidecar against a passwordless Redis is rejected",
			containers: func() []redisAuthContainer {
				return []redisAuthContainer{
					redisAuthFixtureRedis(manifestSecretKeyRef{}, "", "", false),
					redisAuthFixtureSidecar(&password),
				}
			},
			wantProblem: true,
		},
		{
			name: "passwordless sidecar against an authenticated Redis is rejected",
			containers: func() []redisAuthContainer {
				return []redisAuthContainer{
					redisAuthFixtureRedis(password, redisAuthFlagExpansion, "", false),
					redisAuthFixtureSidecar(nil),
				}
			},
			wantProblem: true,
		},
		{
			name: "sidecars reading different Secrets are rejected",
			containers: func() []redisAuthContainer {
				return []redisAuthContainer{
					redisAuthFixtureRedis(password, redisAuthFlagExpansion, "", false),
					redisAuthFixtureSidecar(&password),
					redisAuthFixtureSidecar(&otherName),
				}
			},
			wantProblem: true,
		},
		{
			name: "literal sidecar credential is rejected",
			containers: func() []redisAuthContainer {
				literal := "hunter2"
				return []redisAuthContainer{
					redisAuthFixtureRedis(password, redisAuthFlagExpansion, "", false),
					{Name: "redis-master-label", Image: sidecarImage + ":v0.1.0", Env: []manifestEnvVar{{Name: redisAuthEnvVar, Value: &literal}}},
				}
			},
			wantProblem: true,
		},
		{
			name: "sidecar without a redis container is rejected",
			containers: func() []redisAuthContainer {
				return []redisAuthContainer{redisAuthFixtureSidecar(&password)}
			},
			wantProblem: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			problems := redisAuthProblems([]redisAuthPod{{
				Manifest:   "fixture.yaml",
				Name:       "fixture",
				Containers: tt.containers(),
			}})
			if tt.wantProblem && len(problems) == 0 {
				t.Fatal("redisAuthProblems accepted a fixture the check has to reject")
			}
			if !tt.wantProblem && len(problems) > 0 {
				t.Fatalf("redisAuthProblems rejected a consistent fixture: %v", problems)
			}
		})
	}
}

func TestRedisAuthProblems_NoDeploymentsReadIsReported(t *testing.T) {
	problems := redisAuthProblems(nil)
	if len(problems) != 1 {
		t.Fatalf("redisAuthProblems(nil) = %v, want one problem: an empty manifest set must not read as agreement", problems)
	}
	if !strings.Contains(problems[0], manifestDir) {
		t.Fatalf("problem does not name the manifest directory: %q", problems[0])
	}
}

func TestReadRedisAuthPods_DecodesEnvAndArgs(t *testing.T) {
	dir := t.TempDir()
	manifest := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: fixture
spec:
  template:
    spec:
      containers:
      - name: redis
        image: redis:7-alpine
        env:
        - name: REDIS_PASSWORD
          valueFrom:
            secretKeyRef:
              name: redis-secret
              key: password
        command:
        - redis-server
        - --requirepass
        - $(REDIS_PASSWORD)
      - name: redis-master-label
        image: ghcr.io/redis-master-label/redis-master-label:v0.1.0
`
	path := filepath.Join(dir, "fixture.yaml")
	if err := os.WriteFile(path, []byte(manifest), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	pods, err := readRedisAuthPods(dir)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if len(pods) != 1 {
		t.Fatalf("read %d pods, want 1", len(pods))
	}
	if pods[0].Name != "fixture" {
		t.Fatalf("pod name = %q, want fixture", pods[0].Name)
	}
	if len(pods[0].Containers) != 2 {
		t.Fatalf("read %d containers, want 2", len(pods[0].Containers))
	}

	redis := pods[0].Containers[0]
	ref, present, fromSecret := containerSecretRef(redis, redisAuthEnvVar)
	if !present || !fromSecret || ref != (manifestSecretKeyRef{Name: "redis-secret", Key: "password"}) {
		t.Fatalf("decoded secretKeyRef = %+v (present %v, fromSecret %v), want redis-secret/password from the Secret", ref, present, fromSecret)
	}
	args := append(append([]string{}, redis.Command...), redis.Args...)
	if value, ok := containerFlag(args, redisRequirepassFlag); !ok || value != redisAuthFlagExpansion {
		t.Fatalf("decoded --requirepass = %q (found %v), want %q", value, ok, redisAuthFlagExpansion)
	}
}

func TestReadRedisAuthPods_MissingDirectoryFails(t *testing.T) {
	if _, err := readRedisAuthPods(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("readRedisAuthPods accepted a missing manifest directory: the check would prove nothing")
	}
}

// redisAuthFixtureRedis builds a redis container fixture: an optional Secret
// reference in its env, and the given --requirepass/--masterauth arguments (an
// empty value omits the argument, as an unconfigured Redis would).
func redisAuthFixtureRedis(ref manifestSecretKeyRef, requirepass, masterauth string, replica bool) redisAuthContainer {
	c := redisAuthContainer{Name: "redis", Image: "redis:7-alpine", Command: []string{"redis-server"}}
	if replica {
		c.Command = append(c.Command, "--replicaof", "redis-leader.default.svc.cluster.local", "6379")
	}
	if ref != (manifestSecretKeyRef{}) {
		c.Env = append(c.Env, manifestEnvVar{
			Name:      redisAuthEnvVar,
			ValueFrom: &manifestEnvValueFrom{SecretKeyRef: &ref},
		})
	}
	if requirepass != "" {
		c.Command = append(c.Command, "--requirepass", requirepass)
	}
	if masterauth != "" {
		c.Command = append(c.Command, "--masterauth", masterauth)
	}
	return c
}

// redisAuthFixtureSidecar builds a labeler sidecar fixture reading
// REDIS_PASSWORD from ref, or carrying no credential when ref is nil.
func redisAuthFixtureSidecar(ref *manifestSecretKeyRef) redisAuthContainer {
	c := redisAuthContainer{
		Name:  "redis-master-label",
		Image: "ghcr.io/redis-master-label/redis-master-label:v0.1.0",
	}
	if ref != nil {
		c.Env = append(c.Env, manifestEnvVar{
			Name:      redisAuthEnvVar,
			ValueFrom: &manifestEnvValueFrom{SecretKeyRef: ref},
		})
	}
	return c
}
