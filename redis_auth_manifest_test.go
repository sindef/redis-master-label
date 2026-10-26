package main

import (
	"strings"
	"testing"
)

// The reported infra_drift: both example pods injected REDIS_PASSWORD into the
// labeler sidecar but started the co-located redis-server as a bare
// `redis-server` with no --requirepass. go-redis sends AUTH for a non-empty
// password, Redis answers "ERR Client sent AUTH, but no password is set", so
// no ROLE call succeeded, no label was applied, /healthz turned 503 after three
// failures and the liveness probe restarted the sidecar. README told the
// operator to create the Secret with a non-empty password, which is exactly
// the broken case.
func TestManifestRedisAuthMatchesSidecarCredential(t *testing.T) {
	problems, deployments, redisContainers, sidecarCredentials, err := redisAuthProblems(manifestDir)
	if err != nil {
		t.Fatal(err)
	}
	if deployments != 2 {
		t.Fatalf("Deployment documents across manifests = %d, want the two example deployments", deployments)
	}
	if redisContainers != 2 {
		t.Fatalf("redis containers across the example deployments = %d, want the leader and the replica", redisContainers)
	}
	if sidecarCredentials != 2 {
		t.Fatalf("labeler containers reading REDIS_PASSWORD from a Secret = %d, want both example sidecars", sidecarCredentials)
	}
	for _, problem := range problems {
		t.Errorf("%s", problem)
	}
}

// redisPasswordEnvBlock is the Secret-backed env entry both containers of both
// example deployments carry. Removing it is how a fixture reproduces a pod
// that reads no Redis credential.
const redisPasswordEnvBlock = `        - name: REDIS_PASSWORD
          valueFrom:
            secretKeyRef:
              name: redis-secret
              key: password
`

const (
	leaderRequirepassBlock = "        - --requirepass\n        - $(REDIS_PASSWORD)\n"
	replicaMasterauthBlock = "        - --masterauth\n        - $(REDIS_PASSWORD)\n"
)

// The failure modes at manifest level, each a copy of the shipped manifest set
// with one edit, plus the unmutated set that proves the edit is what the check
// caught. Removing the credential from both containers is the documented
// unauthenticated shape and must stay accepted.
func TestRedisAuthProblems_RegressionFixtures(t *testing.T) {
	tests := []struct {
		name                    string
		mutate                  func(file, text string) string
		wantProblem             string
		wantQuiet               bool
		wantSidecarCredentials  int
		wantProblemManifestPart string
	}{
		{
			name: "leader starts redis-server without --requirepass",
			mutate: func(file, text string) string {
				if file != "redis-leader.yaml" {
					return text
				}
				return strings.Replace(text, leaderRequirepassBlock, "", 1)
			},
			wantProblem:             "--requirepass",
			wantSidecarCredentials:  2,
			wantProblemManifestPart: "redis-leader.yaml/redis",
		},
		{
			name: "replica has no --masterauth for the authenticated leader",
			mutate: func(file, text string) string {
				if file != "deployment-example.yaml" {
					return text
				}
				return strings.Replace(text, replicaMasterauthBlock, "", 1)
			},
			wantProblem:             "--masterauth",
			wantSidecarCredentials:  2,
			wantProblemManifestPart: "deployment-example.yaml/redis",
		},
		{
			name: "redis container reads a different Secret than the sidecar",
			mutate: func(file, text string) string {
				// The redis container's env entry comes first; the sidecar's
				// copy keeps redis-secret so the two disagree.
				return strings.Replace(text, "              name: redis-secret\n", "              name: other-secret\n", 1)
			},
			wantProblem:             "other-secret",
			wantSidecarCredentials:  2,
			wantProblemManifestPart: "redis-leader.yaml/redis",
		},
		{
			name: "redis container loses its REDIS_PASSWORD env entry",
			mutate: func(file, text string) string {
				return strings.Replace(text, redisPasswordEnvBlock, "", 1)
			},
			wantProblem:            "reads no REDIS_PASSWORD",
			wantSidecarCredentials: 2,
		},
		{
			name: "literal credential in --requirepass",
			mutate: func(file, text string) string {
				if file != "redis-leader.yaml" {
					return text
				}
				return strings.Replace(text, leaderRequirepassBlock, "        - --requirepass\n        - supersecret\n", 1)
			},
			wantProblem:            "literal credential",
			wantSidecarCredentials: 2,
		},
		{
			name: "both containers read no credential: unauthenticated Redis",
			mutate: func(file, text string) string {
				text = strings.ReplaceAll(text, redisPasswordEnvBlock, "")
				text = strings.ReplaceAll(text, leaderRequirepassBlock, "")
				text = strings.ReplaceAll(text, replicaMasterauthBlock, "")
				return text
			},
			wantQuiet:              true,
			wantSidecarCredentials: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeManifestSet(t, dir, tt.mutate)

			problems, deployments, redisContainers, sidecarCredentials, err := redisAuthProblems(dir)
			if err != nil {
				t.Fatal(err)
			}
			if deployments != 2 || redisContainers != 2 {
				t.Fatalf("deployments = %d, redis containers = %d, want 2 and 2", deployments, redisContainers)
			}
			if sidecarCredentials != tt.wantSidecarCredentials {
				t.Fatalf("sidecar credentials = %d, want %d", sidecarCredentials, tt.wantSidecarCredentials)
			}
			if tt.wantQuiet {
				if len(problems) != 0 {
					t.Fatalf("problems = %v, want none for a pod that wires no credential", problems)
				}
				return
			}
			joined := strings.Join(problems, "\n")
			if len(problems) == 0 {
				t.Fatal("inconsistent pod accepted, want a problem reported")
			}
			if !strings.Contains(joined, tt.wantProblem) {
				t.Fatalf("problems = %v, want one naming %q", problems, tt.wantProblem)
			}
			if tt.wantProblemManifestPart != "" && !strings.Contains(joined, tt.wantProblemManifestPart) {
				t.Errorf("problems = %v, want one naming %q", problems, tt.wantProblemManifestPart)
			}

			// The unmutated set must pass, so the edit is what the check
			// caught rather than a reader that always complains.
			clean := t.TempDir()
			writeManifestSet(t, clean, nil)
			problems, _, _, sidecarCredentials, err = redisAuthProblems(clean)
			if err != nil {
				t.Fatal(err)
			}
			if sidecarCredentials != 2 {
				t.Fatalf("sidecar credentials = %d, want 2", sidecarCredentials)
			}
			if len(problems) != 0 {
				t.Fatalf("unmutated manifest set rejected: %v", problems)
			}
		})
	}
}
