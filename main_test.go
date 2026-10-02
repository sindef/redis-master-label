package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const (
	testNamespace = "test-namespace"
	testKey       = "redis-role"
	testValue     = "master"
)

// configureTestFlags pins the package-level flag variables for a test and
// returns a function that restores the original values.
func configureTestFlags() func() {
	origName, origNS := *podName, *podNamespace
	origKey, origValue := *labelKey, *labelValue

	*podName = "test-pod"
	*podNamespace = testNamespace
	*labelKey = testKey
	*labelValue = testValue

	return func() {
		*podName, *podNamespace = origName, origNS
		*labelKey, *labelValue = origKey, origValue
	}
}

// setHealthStatus mutates the package-level health status under the lock.
func setHealthStatus(failures int, healthy bool) {
	healthStatus.mu.Lock()
	defer healthStatus.mu.Unlock()
	healthStatus.consecutiveFailures = failures
	healthStatus.isHealthy = healthy
}

// healthSnapshot reads the package-level health status under the lock.
func healthSnapshot() (bool, int) {
	healthStatus.mu.RLock()
	defer healthStatus.mu.RUnlock()
	return healthStatus.isHealthy, healthStatus.consecutiveFailures
}

func newPodClientset(pods ...runtime.Object) *k8sfake.Clientset {
	return k8sfake.NewSimpleClientset(pods...)
}

func testPod(name string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, Labels: labels},
	}
}

func TestCheckAndLabel_MasterRoleSetsLabel(t *testing.T) {
	defer configureTestFlags()()
	cs := newPodClientset(testPod("test-pod", nil))

	if err := applyLabel(context.Background(), cs, "master"); err != nil {
		t.Fatalf("applyLabel: %v", err)
	}

	pod, err := cs.CoreV1().Pods(testNamespace).Get(context.Background(), "test-pod", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if pod.Labels[testKey] != testValue {
		t.Fatalf("label %s = %q, want %q", testKey, pod.Labels[testKey], testValue)
	}
}

func TestCheckAndLabel_SlaveRoleRemovesLabel(t *testing.T) {
	defer configureTestFlags()()
	cs := newPodClientset(testPod("test-pod", map[string]string{testKey: testValue}))

	if err := applyLabel(context.Background(), cs, "slave"); err != nil {
		t.Fatalf("applyLabel: %v", err)
	}

	pod, err := cs.CoreV1().Pods(testNamespace).Get(context.Background(), "test-pod", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if _, exists := pod.Labels[testKey]; exists {
		t.Fatalf("label %s still present, want removed", testKey)
	}
}

func TestCheckAndLabel_MasterRoleAlreadySetIsNoOp(t *testing.T) {
	defer configureTestFlags()()
	cs := newPodClientset(testPod("test-pod", map[string]string{testKey: testValue}))

	if err := applyLabel(context.Background(), cs, "master"); err != nil {
		t.Fatalf("applyLabel: %v", err)
	}

	// If the label was not touched, no Update call ran; the fake client would
	// bump resourceVersion accordingly.
	updates := 0
	for _, action := range cs.Actions() {
		if action.GetVerb() == "update" {
			updates++
		}
	}
	if updates != 0 {
		t.Fatalf("update calls = %d, want 0", updates)
	}
}

func TestCheckAndLabel_SlaveRoleWithoutLabelIsNoOp(t *testing.T) {
	defer configureTestFlags()()
	cs := newPodClientset(testPod("test-pod", nil))

	if err := applyLabel(context.Background(), cs, "slave"); err != nil {
		t.Fatalf("applyLabel: %v", err)
	}

	updates := 0
	for _, action := range cs.Actions() {
		if action.GetVerb() == "update" {
			updates++
		}
	}
	if updates != 0 {
		t.Fatalf("update calls = %d, want 0", updates)
	}
}

// A pod that is no longer master must lose the label key even when the value
// under that key differs from --label-value, for example after a second writer
// or a manual edit set it, or after --label-value changed while the pod was a
// replica. Leaving the key in place would keep a Service selecting on that key
// pointed at a non-master pod.
func TestCheckAndLabel_NonMasterRemovesLabelWithForeignValue(t *testing.T) {
	defer configureTestFlags()()
	cs := newPodClientset(testPod("test-pod", map[string]string{testKey: "replica"}))

	if err := applyLabel(context.Background(), cs, "slave"); err != nil {
		t.Fatalf("applyLabel: %v", err)
	}

	pod, err := cs.CoreV1().Pods(testNamespace).Get(context.Background(), "test-pod", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if value, exists := pod.Labels[testKey]; exists {
		t.Fatalf("label %s = %q still present, want the key removed", testKey, value)
	}

	updates := 0
	for _, action := range cs.Actions() {
		if action.GetVerb() == "update" {
			updates++
		}
	}
	if updates != 1 {
		t.Fatalf("update calls = %d, want 1", updates)
	}
}

// The same applies to any non-master role, not only "slave".
func TestCheckAndLabel_NonMasterRolesRemoveForeignValueLabel(t *testing.T) {
	defer configureTestFlags()()

	for _, role := range []string{"slave", "sentinel"} {
		t.Run(role, func(t *testing.T) {
			cs := newPodClientset(testPod("test-pod", map[string]string{testKey: "previous-value", "keep": "me"}))

			if err := applyLabel(context.Background(), cs, role); err != nil {
				t.Fatalf("applyLabel: %v", err)
			}

			pod, err := cs.CoreV1().Pods(testNamespace).Get(context.Background(), "test-pod", metav1.GetOptions{})
			if err != nil {
				t.Fatalf("get pod: %v", err)
			}
			if value, exists := pod.Labels[testKey]; exists {
				t.Fatalf("label %s = %q still present, want the key removed", testKey, value)
			}
			if pod.Labels["keep"] != "me" {
				t.Fatalf("unrelated label keep = %q, want %q", pod.Labels["keep"], "me")
			}
		})
	}
}

func TestCheckAndLabel_MissingPodReturnsError(t *testing.T) {
	defer configureTestFlags()()
	cs := newPodClientset()

	err := applyLabel(context.Background(), cs, "master")
	if err == nil {
		t.Fatal("applyLabel with no pod, want error")
	}
}

func TestCheckAndLabel_RedisErrorReturnsWrappedError(t *testing.T) {
	defer configureTestFlags()()
	cs := newPodClientset(testPod("test-pod", nil))

	err := checkAndLabel(context.Background(), stubRedis{err: errors.New("connection refused")}, cs)
	if err == nil {
		t.Fatal("checkAndLabel with Redis error, want error")
	}
	if !strings.HasPrefix(err.Error(), "failed to execute ROLE command") {
		t.Fatalf("error = %v, want wrapped ROLE command failure", err)
	}
}

func TestCheckAndLabel_MalformedReplyReturnsError(t *testing.T) {
	defer configureTestFlags()()

	for name, reply := range map[string]interface{}{
		"nil reply":                nil,
		"empty role array":         []interface{}{},
		"non-array reply":          "master",
		"non-string first element": []interface{}{int64(12345)},
	} {
		t.Run(name, func(t *testing.T) {
			cs := newPodClientset(testPod("test-pod", nil))

			err := checkAndLabel(context.Background(), stubRedis{reply: reply}, cs)
			if err == nil || err.Error() != "unexpected ROLE response format" {
				t.Fatalf("error = %v, want %q", err, "unexpected ROLE response format")
			}
		})
	}
}

// stubRedis replays a canned ROLE reply or error without a live connection.
type stubRedis struct {
	reply interface{}
	err   error
}

func (s stubRedis) Do(ctx context.Context, args ...interface{}) *redis.Cmd {
	return redis.NewCmdResult(s.reply, s.err)
}

// masterReply mirrors the array layout Redis answers with for ROLE on a primary.
func masterReply() []interface{} {
	return []interface{}{
		"master",
		int64(12345),
		[]interface{}{[]interface{}{"10.244.0.11", "6379", "12345"}},
	}
}

// slaveReply mirrors the array layout Redis answers with for ROLE on a replica.
func slaveReply() []interface{} {
	return []interface{}{
		"slave",
		"10.244.0.10",
		int64(6379),
		"connected",
		int64(12345),
	}
}

// Every --flag a sidecar container uses in manifests/*.yaml must be defined in
// the binary's flag set (flag.CommandLine). kubeconform only checks Kubernetes
// schemas and never inspects container args, so without this check a mistyped
// or renamed flag passes CI and only crashes the sidecar at runtime, where the
// flag package exits with status 2 and the pod CrashLoops.
func TestManifestFlagsDefined(t *testing.T) {
	unknown, sidecars, err := sidecarFlagUsages(manifestDir)
	if err != nil {
		t.Fatal(err)
	}

	if sidecars == 0 {
		t.Fatal("no sidecar container found in manifests: the flag check ran against nothing")
	}

	if len(unknown) > 0 {
		for _, usage := range unknown {
			t.Errorf("unknown flag in manifest: %s", usage)
		}
	}
}

// The reported failure mode: a mistyped flag (for example --redis-adr after a
// --redis-addr rename) in a manifest must fail this test, not pass CI and
// crash the sidecar at runtime with flag package status 2.
func TestManifestFlagsDefined_UnknownFlagFails(t *testing.T) {
	dir := t.TempDir()
	manifest := "apiVersion: apps/v1\nkind: Deployment\nspec:\n  template:\n    spec:\n      containers:\n      - name: redis\n        image: redis:7-alpine\n        command:\n        - redis-server\n      - name: redis-master-label\n        image: redis-master-label:latest\n        command:\n        - /app/redis-master-label\n        - --redis-addr=localhost:6379\n        - --redis-adr=localhost:6379\n"
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "deployment-typo.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	unknown, sidecars, err := sidecarFlagUsages(dir)
	if err != nil {
		t.Fatal(err)
	}
	if sidecars != 1 {
		t.Fatalf("sidecars = %d, want 1", sidecars)
	}
	if len(unknown) != 1 || unknown[0] != "deployment-typo.yaml/redis-master-label:redis-adr" {
		t.Fatalf("unknown = %v, want the --redis-adr usage reported", unknown)
	}

	// A well-typed same manifest yields no unknown flags.
	if err := os.Remove(filepath.Join(dir, "deployment-typo.yaml")); err != nil {
		t.Fatal(err)
	}
	good := strings.ReplaceAll(manifest, "--redis-adr", "--redis-password")
	if err := os.WriteFile(filepath.Join(dir, "deployment-good.yaml"), []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}
	unknown, sidecars, err = sidecarFlagUsages(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(unknown) != 0 {
		t.Fatalf("unknown = %v, want none after fixing the typo", unknown)
	}
	if sidecars != 1 {
		t.Fatalf("sidecars = %d, want 1", sidecars)
	}
}

// End-to-end through checkAndLabel: a full replica ROLE reply must still strip
// a foreign value under the label key.
func TestCheckAndLabel_SlaveReplyRemovesForeignValueLabel(t *testing.T) {
	defer configureTestFlags()()
	cs := newPodClientset(testPod("test-pod", map[string]string{testKey: "replica"}))

	if err := checkAndLabel(context.Background(), stubRedis{reply: slaveReply()}, cs); err != nil {
		t.Fatalf("checkAndLabel: %v", err)
	}

	pod, err := cs.CoreV1().Pods(testNamespace).Get(context.Background(), "test-pod", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if value, exists := pod.Labels[testKey]; exists {
		t.Fatalf("label %s = %q still present, want the key removed", testKey, value)
	}
}

func TestRoleFromResponse(t *testing.T) {
	tests := []struct {
		name    string
		input   interface{}
		want    string
		wantErr bool
	}{
		{"master", []interface{}{"master"}, "master", false},
		{"slave", []interface{}{"slave"}, "slave", false},
		{"sentinel", []interface{}{"sentinel"}, "sentinel", false},
		{"nil", nil, "", true},
		{"non-array", "master", "", true},
		{"empty array", []interface{}{}, "", true},
		{"non-string element", []interface{}{1}, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := roleFromResponse(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("roleFromResponse(%v) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("roleFromResponse(%v) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestRoleFromResponse_KeepsOriginalErrorText(t *testing.T) {
	_, err := roleFromResponse(nil)
	if err == nil || err.Error() != "unexpected ROLE response format" {
		t.Fatalf("error = %v, want %q", err, "unexpected ROLE response format")
	}
}

// resolveRedisPassword must prefer the explicit --redis-password flag and fall
// back to the REDIS_PASSWORD environment variable when the flag is empty, so
// the credential can be injected through a Secret env var instead of the
// command line (argv lands in the pod spec and /proc/<pid>/cmdline).
func TestResolveRedisPassword(t *testing.T) {
	orig := *redisPassword
	defer func() { *redisPassword = orig }()

	t.Setenv("REDIS_PASSWORD", "")
	if got := resolveRedisPassword(); got != "" {
		t.Fatalf("resolveRedisPassword with empty flag and empty env = %q, want \"\"", got)
	}

	t.Setenv("REDIS_PASSWORD", "env-secret")
	if got := resolveRedisPassword(); got != "env-secret" {
		t.Fatalf("resolveRedisPassword with empty flag = %q, want the env value", got)
	}

	*redisPassword = "flag-secret"
	if got := resolveRedisPassword(); got != "flag-secret" {
		t.Fatalf("resolveRedisPassword with flag set = %q, want the flag value (flag must override env)", got)
	}
}

// The reviewed test_gap: --redis-tls and --redis-tls-skip-verify had no
// coverage. redisOptionsFromFlags is the sole place the Redis connection
// options are built, so a table over the four flag combinations pins the
// documented behaviour (README "Configuration" and "Operational notes"): no
// TLS config at all while --redis-tls is off, also when --redis-tls-skip-verify
// is set, and a TLS config whose InsecureSkipVerify follows
// --redis-tls-skip-verify only when TLS is on.
func TestRedisOptionsFromFlags_TLS(t *testing.T) {
	origAddr, origPassword := *redisAddr, *redisPassword
	origTLS, origSkipVerify := *redisTLS, *redisTLSSkipVerify
	defer func() {
		*redisAddr, *redisPassword = origAddr, origPassword
		*redisTLS, *redisTLSSkipVerify = origTLS, origSkipVerify
	}()

	*redisAddr = "redis.example.com:6380"
	*redisPassword = "from-flag"

	tests := []struct {
		name          string
		enableTLS     bool
		skipVerify    bool
		wantErr       bool
		wantTLSConfig bool
		wantInsecure  bool
	}{
		{
			name:          "tls off, skip-verify off: no TLS config",
			enableTLS:     false,
			skipVerify:    false,
			wantTLSConfig: false,
		},
		{
			// Rejected as the invalid combination: the connection stays
			// plaintext and no options are built at all.
			name:       "tls off with skip-verify: rejected, no options",
			enableTLS:  false,
			skipVerify: true,
			wantErr:    true,
		},
		{
			name:          "tls on, verify enabled",
			enableTLS:     true,
			skipVerify:    false,
			wantTLSConfig: true,
			wantInsecure:  false,
		},
		{
			name:          "tls on, skip verify",
			enableTLS:     true,
			skipVerify:    true,
			wantTLSConfig: true,
			wantInsecure:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			*redisTLS = tt.enableTLS
			*redisTLSSkipVerify = tt.skipVerify

			opts, err := redisOptionsFromFlags()

			if tt.wantErr {
				if err == nil {
					t.Fatal("redisOptionsFromFlags accepted --redis-tls-skip-verify without --redis-tls, want an error")
				}
				if opts != nil {
					t.Errorf("options = %+v, want nil for the rejected combination", opts)
				}
				return
			}
			if err != nil {
				t.Fatalf("redisOptionsFromFlags: %v", err)
			}

			if tt.wantTLSConfig {
				if opts.TLSConfig == nil {
					t.Fatal("TLSConfig not set, want a TLS config for the Redis connection")
				}
				if opts.TLSConfig.InsecureSkipVerify != tt.wantInsecure {
					t.Fatalf("TLSConfig.InsecureSkipVerify = %v, want %v",
						opts.TLSConfig.InsecureSkipVerify, tt.wantInsecure)
				}
			} else if opts.TLSConfig != nil {
				t.Fatalf("TLSConfig = %+v, want nil: TLS must stay off unless --redis-tls is set", opts.TLSConfig)
			}

			if opts.Addr != *redisAddr {
				t.Errorf("Addr = %q, want %q", opts.Addr, *redisAddr)
			}
			if opts.Password != *redisPassword {
				t.Errorf("Password = %q, want %q", opts.Password, *redisPassword)
			}
		})
	}
}

// Both TLS flags must be registered with their documented `false` default.
// A default flipped to true would silently run TLS against plaintext servers
// in every pod whose manifest args name no TLS flag at all.
func TestRedisTLSFlagsRegistered(t *testing.T) {
	for _, flagName := range []string{"redis-tls", "redis-tls-skip-verify"} {
		f := flag.CommandLine.Lookup(flagName)
		if f == nil {
			t.Fatalf("--%s is not registered in flag.CommandLine", flagName)
		}
		if f.DefValue != "false" {
			t.Errorf("--%s default = %q, want \"false\"", flagName, f.DefValue)
		}
	}
}

// --redis-tls-skip-verify only means something with --redis-tls: without TLS
// the connection is plaintext and the skip-verify request silently does
// nothing, while the operator believes it is applied. The decision taken here
// is to reject such a startup with an error instead of a warning.
func TestValidateRedisTLSFlags(t *testing.T) {
	origTLS, origSkipVerify := *redisTLS, *redisTLSSkipVerify
	defer func() {
		*redisTLS, *redisTLSSkipVerify = origTLS, origSkipVerify
	}()

	tests := []struct {
		name       string
		enableTLS  bool
		skipVerify bool
		wantErr    bool
	}{
		{"both off (defaults)", false, false, false},
		{"tls on, skip-verify off", true, false, false},
		{"tls on, skip-verify on", true, true, false},
		{"skip-verify without tls", false, true, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			*redisTLS = tt.enableTLS
			*redisTLSSkipVerify = tt.skipVerify

			err := validateRedisTLSFlags()

			if tt.wantErr != (err != nil) {
				t.Fatalf("validateRedisTLSFlags() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "--redis-tls") {
				t.Errorf("error %q does not name --redis-tls", err)
			}
		})
	}
}

// The released image: both example sidecars must run exactly this reference.
const releaseImage = "ghcr.io/redis-master-label/redis-master-label:v0.1.0"

// releaseImageCheck validates one sidecar image reference against the pin.
func releaseImageCheck(image string) error {
	if image == releaseImage {
		return nil
	}
	if strings.HasSuffix(image, ":latest") {
		return fmt.Errorf("floating :latest tag: %q (ImagePullBackOff / stale-pod failure mode), want the pinned %q", image, releaseImage)
	}
	return fmt.Errorf("image %q is not the pinned release image %q", image, releaseImage)
}

// Both example sidecars must pin exactly the registry-qualified versioned
// release image. A floating tag or an unqualifiable image name is exactly what
// shipped the ImagePullBackOff failure mode the manifests suffered before.
func TestManifestsPinVersionedReleaseImage(t *testing.T) {
	images, sidecars, err := sidecarImages(manifestDir)
	if err != nil {
		t.Fatal(err)
	}
	if sidecars == 0 {
		t.Fatal("no sidecar container found in manifests: the image check ran against nothing")
	}
	if len(images) != 2 {
		t.Fatalf("sidecar image across manifests = %d entries, want the two example deployments", len(images))
	}
	for _, image := range images {
		if err := releaseImageCheck(image); err != nil {
			t.Errorf("%s", err)
		}
	}
}

// The pinned tag must be a strict semver vX.Y.Z so the release workflow's tag
// pattern, the manifest pin and the OCI version label stay in one scheme.
func TestReleaseImagePinnedTagIsSemver(t *testing.T) {
	tag := releaseImage[strings.LastIndex(releaseImage, ":")+1:]
	if ok, err := regexp.MatchString(`^v[0-9]+\.[0-9]+\.[0-9]+$`, tag); err != nil || !ok {
		t.Fatalf("pinned release tag %q is not vMAJOR.MINOR.PATCH", tag)
	}
}

// The :latest regression fixture: the checker must reject the exact failure
// mode the manifests had, not merely accept the current pin.
func TestReleaseImageCheck_RejectsLatestFixture(t *testing.T) {
	for _, image := range []string{"redis-master-label:latest", "ghcr.io/redis-master-label/redis-master-label:latest", "redis:7-alpine"} {
		if err := releaseImageCheck(image); err == nil {
			t.Fatalf("releaseImageCheck(%q) accepted a non-pinned image", image)
		}
	}
}

// The build file's final stage is the shipping image. It must create a
// dedicated account and select it with USER: without USER the container runs
// as uid 0 whatever the manifests declare, and runAsNonRoot then makes the
// kubelet refuse to start the pod.
const dockerfilePath = "Dockerfile"

// boolPtr is shorthand for the *bool securityContext fields.
func boolPtr(v bool) *bool { return &v }

// restrictedSecurityContextProblem returns why a sidecar container is not
// restricted enough to run the labeler, or nil when it is. The declared shape
// matters as much as the declared values: an omitted runAsNonRoot is not the
// same defence as runAsNonRoot: true.
func restrictedSecurityContextProblem(sc *containerSecurityContext) error {
	if sc == nil {
		return errors.New("no securityContext: the container runs with the image defaults (uid 0 when the image sets no USER)")
	}
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		return errors.New("runAsNonRoot must be true")
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		return errors.New("allowPrivilegeEscalation must be false")
	}
	if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		return errors.New("readOnlyRootFilesystem must be true")
	}
	if !dropsAllCapabilities(sc.Capabilities.Drop) {
		return errors.New(`capabilities.drop must include "ALL"`)
	}
	return nil
}

func dropsAllCapabilities(drop []string) bool {
	for _, capability := range drop {
		if strings.EqualFold(capability, "ALL") {
			return true
		}
	}
	return false
}

// Both example deployments must run the labeler unprivileged: runAsNonRoot
// keeps uid 0 out, readOnlyRootFilesystem blocks writes to the container
// filesystem, allowPrivilegeEscalation: false blocks setuid-style escalation
// and dropping ALL capabilities removes the remaining kernel powers. The
// binary only reads Redis and calls the Kubernetes API with its mounted
// credentials, so nothing here costs it any function.
func TestManifestSidecarsRunUnprivileged(t *testing.T) {
	containers, err := sidecarContainers(manifestDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(containers) == 0 {
		t.Fatal("no sidecar container found in manifests: the securityContext check ran against nothing")
	}
	if len(containers) != 2 {
		t.Fatalf("sidecar containers = %d, want the two example deployments", len(containers))
	}

	for _, c := range containers {
		if err := restrictedSecurityContextProblem(c.SecurityContext); err != nil {
			t.Errorf("%s/%s: %v", c.Manifest, c.Name, err)
			continue
		}
		if c.SecurityContext.RunAsUser != nil && *c.SecurityContext.RunAsUser == 0 {
			t.Errorf("%s/%s: runAsUser = 0, want the image's non-root uid", c.Manifest, c.Name)
		}
	}
}

// The pre-fix failure mode at container level: no securityContext, or a
// partial one, must be reported instead of passing as "restricted".
func TestRestrictedSecurityContextProblem_RegressionFixtures(t *testing.T) {
	if err := restrictedSecurityContextProblem(nil); err == nil {
		t.Error("nil securityContext accepted, want the missing-context failure")
	}

	restricted := &containerSecurityContext{
		RunAsNonRoot:             boolPtr(true),
		AllowPrivilegeEscalation: boolPtr(false),
		ReadOnlyRootFilesystem:   boolPtr(true),
	}
	restricted.Capabilities.Drop = []string{"ALL"}
	if err := restrictedSecurityContextProblem(restricted); err != nil {
		t.Fatalf("restricted securityContext rejected: %v", err)
	}

	for name, mutate := range map[string]func(*containerSecurityContext){
		"runAsNonRoot missing":     func(sc *containerSecurityContext) { sc.RunAsNonRoot = nil },
		"runAsNonRoot false":       func(sc *containerSecurityContext) { sc.RunAsNonRoot = boolPtr(false) },
		"privilege escalation":     func(sc *containerSecurityContext) { sc.AllowPrivilegeEscalation = boolPtr(true) },
		"privilege escalation nil": func(sc *containerSecurityContext) { sc.AllowPrivilegeEscalation = nil },
		"writable root filesystem": func(sc *containerSecurityContext) { sc.ReadOnlyRootFilesystem = boolPtr(false) },
		"capabilities kept":        func(sc *containerSecurityContext) { sc.Capabilities.Drop = []string{"NET_BIND_SERVICE"} },
		"capabilities empty":       func(sc *containerSecurityContext) { sc.Capabilities.Drop = nil },
	} {
		t.Run(name, func(t *testing.T) {
			sc := *restricted
			mutate(&sc)
			if err := restrictedSecurityContextProblem(&sc); err == nil {
				t.Error("accepted a securityContext that does not meet the restricted shape")
			}
		})
	}
}

// The same failure mode one level up: a manifest whose labeler omits
// securityContext must be reported by the manifest reader, and adding the
// block must clear it. This is the check that would have failed before the fix.
func TestManifestSidecarsRunUnprivileged_MissingContextFails(t *testing.T) {
	dir := t.TempDir()
	withoutContext := "apiVersion: apps/v1\nkind: Deployment\nspec:\n  template:\n    spec:\n      containers:\n      - name: redis\n        image: redis:7-alpine\n      - name: redis-master-label\n        image: ghcr.io/redis-master-label/redis-master-label:v0.1.0\n        command:\n        - /app/redis-master-label\n"
	if err := os.WriteFile(filepath.Join(dir, "deployment-nosec.yaml"), []byte(withoutContext), 0o644); err != nil {
		t.Fatal(err)
	}

	containers, err := sidecarContainers(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(containers) != 1 {
		t.Fatalf("sidecar containers = %d, want 1", len(containers))
	}
	if err := restrictedSecurityContextProblem(containers[0].SecurityContext); err == nil {
		t.Error("labeler without securityContext accepted, want the missing-context failure")
	}

	withContext := withoutContext + "        securityContext:\n          runAsNonRoot: true\n          allowPrivilegeEscalation: false\n          readOnlyRootFilesystem: true\n          capabilities:\n            drop:\n            - ALL\n"
	if err := os.WriteFile(filepath.Join(dir, "deployment-withsec.yaml"), []byte(withContext), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "deployment-nosec.yaml")); err != nil {
		t.Fatal(err)
	}

	containers, err = sidecarContainers(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(containers) != 1 {
		t.Fatalf("sidecar containers = %d, want 1", len(containers))
	}
	if err := restrictedSecurityContextProblem(containers[0].SecurityContext); err != nil {
		t.Errorf("securityContext in the manifest was not read back: %v", err)
	}
}

// The shipping image is the final build stage of the Dockerfile. It must
// create its own account and select it with USER; otherwise the container runs
// as the base image's default uid (0 on alpine) and every manifest defence is
// either useless or rejected by the kubelet.
func TestDockerfileFinalStageRunsAsNonRootUser(t *testing.T) {
	user, createsUser, err := dockerfileFinalStageUser(dockerfilePath)
	if err != nil {
		t.Fatal(err)
	}
	if user == "" {
		t.Fatal("final build stage has no USER: the shipping image runs as the base image's default user (uid 0 for alpine)")
	}
	if !createsUser {
		t.Errorf("final build stage selects USER %s but never creates that account (no adduser/addgroup)", user)
	}

	uid := strings.SplitN(user, ":", 2)[0]
	if strings.EqualFold(uid, "root") {
		t.Errorf("final build stage selects USER %q, want an unprivileged user", user)
	}
	n, err := strconv.ParseInt(uid, 10, 64)
	if err != nil {
		t.Fatalf("final build stage USER %q has no numeric uid: %v", user, err)
	}
	if n == 0 {
		t.Errorf("final build stage USER %q is uid 0, want an unprivileged uid", user)
	}
}

// The image and the manifests must agree on who runs the labeler. runAsNonRoot
// is satisfied by any non-root uid, so the manifests pin the uid the final
// image stage creates; a mismatch means the manifests and the image drifted
// apart.
func TestManifestRunAsUserMatchesImageUser(t *testing.T) {
	user, _, err := dockerfileFinalStageUser(dockerfilePath)
	if err != nil {
		t.Fatal(err)
	}
	uid, err := strconv.ParseInt(strings.SplitN(user, ":", 2)[0], 10, 64)
	if err != nil {
		t.Fatalf("final build stage USER %q has no numeric uid: %v", user, err)
	}

	containers, err := sidecarContainers(manifestDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(containers) == 0 {
		t.Fatal("no sidecar container found in manifests: the runAsUser check ran against nothing")
	}
	for _, c := range containers {
		if c.SecurityContext == nil || c.SecurityContext.RunAsUser == nil {
			t.Errorf("%s/%s: runAsUser is not pinned, want the image uid %d", c.Manifest, c.Name, uid)
			continue
		}
		if *c.SecurityContext.RunAsUser != uid {
			t.Errorf("%s/%s: runAsUser = %d, want the image uid %d", c.Manifest, c.Name, *c.SecurityContext.RunAsUser, uid)
		}
	}
}

// The pre-fix failure mode of the build file: a final stage with no USER (or
// with root/0) must be rejected, and a USER in an earlier builder stage must
// not count as the shipping image's user.
func TestDockerfileFinalStageUser_RegressionFixtures(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	tests := []struct {
		name        string
		content     string
		wantUser    string
		wantCreates bool
	}{
		{
			name:        "no USER in the final stage",
			content:     "FROM golang:1.27-alpine AS builder\nUSER builder\nRUN go build .\n\nFROM alpine:3.24\nRUN apk --no-cache add ca-certificates\nENTRYPOINT [\"/app/redis-master-label\"]\n",
			wantUser:    "",
			wantCreates: false,
		},
		{
			name:        "USER root",
			content:     "FROM alpine:3.24\nRUN adduser -S labeler\nUSER root\n",
			wantUser:    "root",
			wantCreates: true,
		},
		{
			name:        "USER 0:0",
			content:     "FROM alpine:3.24\nRUN adduser -S labeler\nUSER 0:0\n",
			wantUser:    "0:0",
			wantCreates: true,
		},
		{
			name:        "USER without creating the account",
			content:     "FROM alpine:3.24\nUSER 10001:10001\n",
			wantUser:    "10001:10001",
			wantCreates: false,
		},
		{
			name:        "unprivileged user created and selected",
			content:     "FROM alpine:3.24\nRUN apk --no-cache add ca-certificates && addgroup -S -g 10001 labeler && adduser -S -u 10001 -G labeler -H labeler\nUSER 10001:10001\n",
			wantUser:    "10001:10001",
			wantCreates: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := write(strings.ReplaceAll(tt.name, " ", "-")+".Dockerfile", tt.content)
			user, createsUser, err := dockerfileFinalStageUser(path)
			if err != nil {
				t.Fatal(err)
			}
			if user != tt.wantUser {
				t.Errorf("user = %q, want %q", user, tt.wantUser)
			}
			if createsUser != tt.wantCreates {
				t.Errorf("createsUser = %v, want %v", createsUser, tt.wantCreates)
			}
		})
	}

	if _, _, err := dockerfileFinalStageUser(filepath.Join(dir, "missing.Dockerfile")); err == nil {
		t.Error("missing build file accepted, want a read error")
	}
	if _, _, err := dockerfileFinalStageUser(write("no-from.Dockerfile", "RUN true\n")); err == nil {
		t.Error("build file without FROM accepted, want an error")
	}
}

// licenseID is the SPDX identifier the project grants in LICENSE and the value
// the shipping image's org.opencontainers.image.licenses label must carry.
const licenseID = "MIT"

// The reviewed release_hygiene defect: the image advertised
// org.opencontainers.image.licenses="MIT" while the repository shipped no
// LICENSE file, so the label claimed a license nobody had granted. The file at
// the repo root is the grant, and it must hold the MIT text.
func TestLicenseFileGrantsMITLicense(t *testing.T) {
	data, err := os.ReadFile(licenseFile)
	if err != nil {
		t.Fatalf("read %s: %v (the image advertises %q, so the repository must grant it)", licenseFile, err, licenseID)
	}

	text := string(data)
	for _, want := range []string{
		"MIT License",
		"Copyright (c)",
		"Permission is hereby granted, free of charge",
		"WITHOUT WARRANTY OF ANY KIND",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("%s is missing %q: it is not the MIT license text", licenseFile, want)
		}
	}
}

// The label and the grant must agree, and the label must be in the shipping
// (final) build stage: a LABEL in the discarded builder stage reaches no image,
// so the claim would be invisible to anyone pulling it.
func TestImageLicenseLabelMatchesGrantedLicense(t *testing.T) {
	buildFile, err := locateBuildFile()
	if err != nil {
		t.Fatal(err)
	}
	labels, err := dockerfileFinalStageLabels(buildFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := imageLicenseProblem(labels, licenseID); err != nil {
		t.Errorf("%s: %v", buildFile, err)
	}
}

// The failure mode at build-file level: a license label that lives only in the
// builder stage must not be read back as the shipping image's metadata, a label
// in the final stage must be, and a label naming a license the repository does
// not grant must be rejected.
func TestDockerfileFinalStageLabels_RejectsBuilderStageLabels(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Dockerfile")

	builderStageOnly := "FROM golang:1.27-alpine AS builder\nLABEL org.opencontainers.image.licenses=\"MIT\"\nRUN go build .\n\nFROM alpine:3.24\nUSER 10001:10001\n"
	writeBuildFile(t, path, builderStageOnly)
	labels, err := dockerfileFinalStageLabels(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := imageLicenseProblem(labels, licenseID); err == nil {
		t.Error("license label in the discarded builder stage accepted as the shipping image's license")
	}

	finalStage := "FROM golang:1.27-alpine AS builder\nRUN go build .\n\nFROM alpine:3.24\nLABEL org.opencontainers.image.licenses=\"MIT\" \\\n      org.opencontainers.image.source=https://example.com/repo\nUSER 10001:10001\n"
	writeBuildFile(t, path, finalStage)
	labels, err = dockerfileFinalStageLabels(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := imageLicenseProblem(labels, licenseID); err != nil {
		t.Fatalf("license label in the final stage not read back: %v", err)
	}
	if got := labels["org.opencontainers.image.source"]; got != "https://example.com/repo" {
		t.Errorf("label on a continuation line = %q, want the unquoted URL", got)
	}
	if err := imageLicenseProblem(labels, "Apache-2.0"); err == nil {
		t.Error("label disagreeing with the granted license was accepted")
	}
}

// The published image must carry the full OCI metadata, and it must carry it in
// the final stage: a LABEL block left in the builder stage reads exactly the
// same in the build file yet is discarded with that stage, so the image users
// pull has no title, source, license or version. Build args do not cross stage
// boundaries either, so the final stage must declare ARG VERSION itself or its
// version label expands to nothing.
func TestDockerfileFinalStageCarriesOCILabels(t *testing.T) {
	buildFile, err := locateBuildFile()
	if err != nil {
		t.Fatal(err)
	}
	labels, err := dockerfileFinalStageLabels(buildFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := ociLabelsProblem(labels); err != nil {
		t.Errorf("%s: %v", buildFile, err)
	}
	if err := ociVersionLabelProblem(labels); err != nil {
		t.Errorf("%s: %v", buildFile, err)
	}

	declared, err := dockerfileFinalStageDeclaresArg(buildFile, "VERSION")
	if err != nil {
		t.Fatal(err)
	}
	if !declared {
		t.Errorf("%s: the final stage declares no ARG VERSION, so its ${VERSION} resolves to nothing", buildFile)
	}
}

// The failure modes at build-file level: labels in a discarded builder stage, a
// final stage missing part of the metadata, a hard-coded version label, and an
// ARG VERSION that was left behind in the builder stage.
func TestDockerfileFinalStageCarriesOCILabels_RegressionFixtures(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Dockerfile")

	builderStageLabels := "FROM golang:1.27-alpine AS builder\n" +
		"LABEL org.opencontainers.image.title=\"redis-master-label\" \\\n" +
		"      org.opencontainers.image.version=\"${VERSION}\"\n" +
		"RUN go build .\n\n" +
		"FROM alpine:3.24\nARG VERSION=dev\nUSER 10001:10001\n"
	writeBuildFile(t, path, builderStageLabels)
	labels, err := dockerfileFinalStageLabels(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := ociLabelsProblem(labels); err == nil {
		t.Error("labels in the discarded builder stage passed as the shipping image's metadata")
	}

	finalStageMissing := "FROM golang:1.27-alpine AS builder\nRUN go build .\n\n" +
		"FROM alpine:3.24\nARG VERSION=dev\n" +
		"LABEL org.opencontainers.image.title=\"redis-master-label\" \\\n" +
		"      org.opencontainers.image.version=\"${VERSION}\"\n"
	writeBuildFile(t, path, finalStageMissing)
	labels, err = dockerfileFinalStageLabels(path)
	if err != nil {
		t.Fatal(err)
	}
	err = ociLabelsProblem(labels)
	if err == nil {
		t.Fatal("a final stage without description, source and licenses was accepted")
	}
	for _, missing := range []string{"org.opencontainers.image.description", "org.opencontainers.image.source", "org.opencontainers.image.licenses"} {
		if !strings.Contains(err.Error(), missing) {
			t.Errorf("error %q does not name the missing %s label", err, missing)
		}
	}
	if err := ociVersionLabelProblem(labels); err != nil {
		t.Errorf("version label of the final stage rejected: %v", err)
	}

	complete := "FROM golang:1.27-alpine AS builder\nARG VERSION=dev\nRUN go build .\n\n" +
		"FROM alpine:3.24\nARG VERSION=dev\n" +
		"LABEL org.opencontainers.image.title=\"redis-master-label\" \\\n" +
		"      org.opencontainers.image.description=\"labels the pod hosting the Redis master\" \\\n" +
		"      org.opencontainers.image.source=https://example.com/repo \\\n" +
		"      org.opencontainers.image.licenses=\"MIT\" \\\n" +
		"      org.opencontainers.image.version=\"${VERSION}\"\n" +
		"USER 10001:10001\n"
	writeBuildFile(t, path, complete)
	labels, err = dockerfileFinalStageLabels(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := ociLabelsProblem(labels); err != nil {
		t.Fatalf("complete final-stage label block rejected: %v", err)
	}
	if err := ociVersionLabelProblem(labels); err != nil {
		t.Fatalf("version label stamped from ${VERSION} rejected: %v", err)
	}
	declared, err := dockerfileFinalStageDeclaresArg(path, "VERSION")
	if err != nil {
		t.Fatal(err)
	}
	if !declared {
		t.Error("ARG VERSION declared in the final stage was not seen")
	}

	literalVersion := strings.Replace(complete, `org.opencontainers.image.version="${VERSION}"`, `org.opencontainers.image.version="v0.1.0"`, 1)
	writeBuildFile(t, path, literalVersion)
	labels, err = dockerfileFinalStageLabels(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := ociVersionLabelProblem(labels); err == nil {
		t.Error("a hard-coded version label was accepted; it goes stale at the next release")
	}

	argInBuilderOnly := strings.Replace(complete, "FROM alpine:3.24\nARG VERSION=dev\n", "FROM alpine:3.24\n", 1)
	writeBuildFile(t, path, argInBuilderOnly)
	declared, err = dockerfileFinalStageDeclaresArg(path, "VERSION")
	if err != nil {
		t.Fatal(err)
	}
	if declared {
		t.Error("ARG VERSION declared only in the builder stage counted for the final stage: its version label would expand to nothing")
	}

	if _, err := dockerfileFinalStageDeclaresArg(filepath.Join(dir, "missing.Dockerfile"), "VERSION"); err == nil {
		t.Error("missing build file accepted, want a read error")
	}
	noFrom := filepath.Join(dir, "no-from.Dockerfile")
	writeBuildFile(t, noFrom, "RUN true\n")
	if _, err := dockerfileFinalStageDeclaresArg(noFrom, "VERSION"); err == nil {
		t.Error("build file without FROM accepted, want an error")
	}
}

// The README must state the license too, so the grant is discoverable without
// digging into image metadata.
func TestREADMEStatesLicense(t *testing.T) {
	data, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	readme := string(data)
	for _, want := range []string{"## License", licenseID, licenseFile} {
		if !strings.Contains(readme, want) {
			t.Errorf("README.md does not mention %q; it must state the project license", want)
		}
	}
}

// The reviewed ci_gap: the CI workflow pinned `go-version: "1.23"` on the same
// setup-go step as `go-version-file: go.mod` while go.mod declared go 1.26.0.
// setup-go honours one input only (with both set it ignores the file), so the
// job's toolchain was decided by a line go.mod disagreed with and the version
// that compiled the code could not be read from the files. Every workflow must
// therefore declare exactly one source, and it must be go.mod.
func TestWorkflowsDeclareSingleGoToolchainSource(t *testing.T) {
	problems, setupGoSteps, err := workflowGoToolchainProblems(ciWorkflowDir)
	if err != nil {
		t.Fatal(err)
	}
	if setupGoSteps == 0 {
		t.Fatal("no setup-go step found in the workflows: the toolchain check ran against nothing")
	}
	for _, problem := range problems {
		t.Errorf("%s", problem)
	}
}

// The single source must be a real one: go-version-file resolves the module
// file's `go` directive, so that directive has to name a version.
func TestGoModDeclaresToolchainVersion(t *testing.T) {
	declared, err := goModGoDirective(goToolchainFile)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := regexp.MatchString(`^[0-9]+\.[0-9]+(\.[0-9]+)?$`, declared); err != nil || !ok {
		t.Fatalf("%s declares go %q, want a MAJOR.MINOR[.PATCH] version", goToolchainFile, declared)
	}
}

// The failure modes at workflow level: the pre-fix state (both inputs on one
// step) and every other shape that hides which toolchain the job installs must
// be reported, while the single go-version-file source must pass.
func TestWorkflowGoToolchainProblems_RegressionFixtures(t *testing.T) {
	tests := []struct {
		name       string
		workflow   string
		wantQuiet  bool
		wantSubstr string
	}{
		{
			name:       "reviewed pre-fix state: go-version beside go-version-file",
			workflow:   "jobs:\n  go:\n    steps:\n      - uses: actions/setup-go@v5\n        with:\n          go-version: \"1.23\"\n          go-version-file: go.mod\n",
			wantSubstr: "go-version: 1.23",
		},
		{
			name:       "go-version alone",
			workflow:   "jobs:\n  go:\n    steps:\n      - uses: actions/setup-go@v5\n        with:\n          go-version: \"1.23\"\n",
			wantSubstr: "go-version: 1.23",
		},
		{
			name:       "go-version-file pointing elsewhere",
			workflow:   "jobs:\n  go:\n    steps:\n      - uses: actions/setup-go@v5\n        with:\n          go-version-file: .go-version\n",
			wantSubstr: "go-version-file: .go-version",
		},
		{
			name:       "setup-go without any version input",
			workflow:   "jobs:\n  go:\n    steps:\n      - uses: actions/setup-go@v5\n        with:\n          cache: true\n",
			wantSubstr: "declares no go-version-file",
		},
		{
			name:      "go-version-file: go.mod alone",
			workflow:  "jobs:\n  go:\n    steps:\n      - uses: actions/setup-go@v5\n        with:\n          go-version-file: go.mod\n",
			wantQuiet: true,
		},
		{
			name:      "the extra pin only in a comment",
			workflow:  "jobs:\n  go:\n    steps:\n      - uses: actions/setup-go@v5\n        with:\n          # go-version: \"1.23\"\n          go-version-file: go.mod\n",
			wantQuiet: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "ci.yml"), []byte(tt.workflow), 0o644); err != nil {
				t.Fatal(err)
			}

			problems, setupGoSteps, err := workflowGoToolchainProblems(dir)
			if err != nil {
				t.Fatal(err)
			}
			if setupGoSteps != 1 {
				t.Fatalf("setup-go steps = %d, want 1", setupGoSteps)
			}
			if tt.wantQuiet {
				if len(problems) != 0 {
					t.Fatalf("problems = %v, want none", problems)
				}
				return
			}
			if len(problems) == 0 {
				t.Fatal("workflow declaring a second toolchain source accepted, want a problem reported")
			}
			if !strings.Contains(strings.Join(problems, "\n"), tt.wantSubstr) {
				t.Fatalf("problems = %v, want one naming %q", problems, tt.wantSubstr)
			}
		})
	}
}

// The reviewed infra_drift: the build file's builder stage read
// `FROM golang:1.27-alpine` while go.mod declared go 1.26.0. CI's gofmt, vet,
// build and test run on the version setup-go installs from go.mod, so the
// release image was compiled and published by a compiler no step had audited,
// and nothing compared the two files: CI's build-file step inspects the COPY and
// `go build` lines only, Dependabot bumps the golang tag on its own schedule,
// and a `go` directive bump touches the build file not at all. The builder base
// image must therefore carry the same Go MAJOR.MINOR as go.mod's directive.
func TestDockerfileBuilderToolchainMatchesGoMod(t *testing.T) {
	declared, err := goModGoDirective(goToolchainFile)
	if err != nil {
		t.Fatal(err)
	}

	buildFile, err := locateBuildFile()
	if err != nil {
		t.Fatal(err)
	}

	tag, err := dockerfileBuilderGoTag(buildFile)
	if err != nil {
		t.Fatal(err)
	}

	if err := builderToolchainProblem(tag, declared); err != nil {
		t.Fatalf("%s: %v", buildFile, err)
	}
}

// The drift and the shapes that must not pass as agreement: the reviewed
// `golang:1.27-alpine` beside `go 1.26.0` is reported in either direction, a tag
// on the declared line passes whether it floats or pins a patch, and a tag that
// names no Go version cannot count as matching.
func TestBuilderToolchainProblem_RegressionFixtures(t *testing.T) {
	tests := []struct {
		name        string
		imageTag    string
		goDirective string
		wantQuiet   bool
		wantSubstr  string
	}{
		{
			name:        "reviewed drift: 1.27 image beside go 1.26.0",
			imageTag:    "1.27-alpine",
			goDirective: "1.26.0",
			wantSubstr:  "golang:1.27-alpine",
		},
		{
			name:        "go directive ahead of the image",
			imageTag:    "1.26-alpine",
			goDirective: "1.27.0",
			wantSubstr:  "golang:1.26-alpine",
		},
		{
			name:        "floating tag on the declared minor",
			imageTag:    "1.26-alpine",
			goDirective: "1.26.0",
			wantQuiet:   true,
		},
		{
			name:        "pinned patch tag on the declared minor",
			imageTag:    "1.26.0-alpine3.24",
			goDirective: "1.26.0",
			wantQuiet:   true,
		},
		{
			name:        "directive patch newer than the image",
			imageTag:    "1.26-alpine",
			goDirective: "1.26.5",
			wantQuiet:   true,
		},
		{
			name:        "image tag naming no Go version",
			imageTag:    "latest",
			goDirective: "1.26.0",
			wantSubstr:  "golang:latest",
		},
		{
			name:        "go directive naming no version",
			imageTag:    "1.26-alpine",
			goDirective: "toolchain",
			wantSubstr:  goToolchainFile,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := builderToolchainProblem(tt.imageTag, tt.goDirective)
			if tt.wantQuiet {
				if err != nil {
					t.Fatalf("builderToolchainProblem(%q, %q) = %v, want nil", tt.imageTag, tt.goDirective, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("builderToolchainProblem(%q, %q) = nil, want the drift reported", tt.imageTag, tt.goDirective)
			}
			if !strings.Contains(err.Error(), tt.wantSubstr) {
				t.Fatalf("error = %q, want it to name %q", err, tt.wantSubstr)
			}
		})
	}
}

// The base-image reader must see what the build executes: the first golang stage
// is the one that compiles the binary, a golang stage named only in a comment is
// not part of the build, and a file without a golang stage or without any
// content is an error rather than a silent pass.
func TestDockerfileBuilderGoTag_RegressionFixtures(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Dockerfile")

	declared := "FROM golang:1.26-alpine AS builder\nRUN go build .\n\nFROM alpine:3.24\n"
	writeBuildFile(t, path, declared)
	tag, err := dockerfileBuilderGoTag(path)
	if err != nil {
		t.Fatal(err)
	}
	if tag != "1.26-alpine" {
		t.Fatalf("tag = %q, want %q", tag, "1.26-alpine")
	}

	flagged := "FROM --platform=linux/amd64 golang:1.26.0-alpine AS builder\nRUN go build .\n"
	writeBuildFile(t, path, flagged)
	if tag, err = dockerfileBuilderGoTag(path); err != nil || tag != "1.26.0-alpine" {
		t.Fatalf("tag = %q, err = %v, want %q for a flagged FROM line", tag, err, "1.26.0-alpine")
	}

	// The reviewed shape: a builder stage first, so the first golang stage is the
	// compiler even when a later stage names another golang tag.
	twoStages := "FROM golang:1.26-alpine AS builder\nRUN go build .\n\nFROM golang:1.27-alpine AS other\n"
	writeBuildFile(t, path, twoStages)
	if tag, err = dockerfileBuilderGoTag(path); err != nil || tag != "1.26-alpine" {
		t.Fatalf("tag = %q, err = %v, want %q from the first golang stage", tag, err, "1.26-alpine")
	}

	commentedOut := "# FROM golang:1.27-alpine AS builder\nFROM alpine:3.24\n"
	writeBuildFile(t, path, commentedOut)
	if tag, err = dockerfileBuilderGoTag(path); err == nil {
		t.Fatalf("tag = %q, err = nil, want an error: the only golang stage is commented out", tag)
	}

	writeBuildFile(t, path, "FROM alpine:3.24\n")
	if tag, err = dockerfileBuilderGoTag(path); err == nil {
		t.Fatalf("tag = %q, err = nil, want an error for a build file with no golang stage", tag)
	}

	if _, err := dockerfileBuilderGoTag(filepath.Join(dir, "missing.Dockerfile")); err == nil {
		t.Error("missing build file accepted, want a read error")
	}
}

func TestVersionDefaultsToDev(t *testing.T) {
	if version != "dev" {
		t.Fatalf("version = %q, want the unstamped default \"dev\"", version)
	}
}

func TestVersionFlagRegistered(t *testing.T) {
	f := flag.CommandLine.Lookup("version")
	if f == nil {
		t.Fatal("--version flag is not registered in flag.CommandLine")
	}
	if f.DefValue != "false" {
		t.Fatalf("--version default = %q, want \"false\"", f.DefValue)
	}
}

// The reviewed defect: the build file expanded an empty VERSION into
// `-ldflags "-X main.version="`, which overrides main.go's `var version = "dev"`,
// so `docker build .` (the command the README documents) produced an image whose
// `--version` printed an empty version. `go test` compiles without the linker
// stamp, so this reads the build file instead.
func TestBuildFileStampsDevWhenVersionUnset(t *testing.T) {
	buildFile, err := locateBuildFile()
	if err != nil {
		t.Fatal(err)
	}

	got, err := buildFileUnstampedVersion(buildFile)
	if err != nil {
		t.Fatal(err)
	}
	if got != version {
		t.Fatalf("%s stamps version %q for a build with no --build-arg VERSION, want %q (the unstamped default in main.go)", buildFile, got, version)
	}
}

// The failure mode and its two accepted fixes must be distinguished: an empty
// ARG VERSION expanded unconditionally stamps an empty version, while a non-empty
// default or a shell fallback keeps "dev" - and a tag supplied through
// --build-arg VERSION still reaches the linker in every accepted variant.
func TestBuildFileUnstampedVersion_RejectsEmptyVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Dockerfile")

	broken := "FROM golang:1.27-alpine\nARG VERSION=\"\"\nRUN CGO_ENABLED=0 GOOS=linux go build -ldflags \"-X main.version=${VERSION}\" -o redis-master-label .\n"
	writeBuildFile(t, path, broken)
	got, err := buildFileUnstampedVersion(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("unstamped version = %q, want \"\" for the reviewed broken build file", got)
	}

	fixedDefault := strings.Replace(broken, `ARG VERSION=""`, "ARG VERSION=dev", 1)
	writeBuildFile(t, path, fixedDefault)
	if got, err = buildFileUnstampedVersion(path); err != nil || got != "dev" {
		t.Fatalf("ARG VERSION=dev: unstamped version = %q, err = %v, want \"dev\"", got, err)
	}

	fixedFallback := "FROM golang:1.27-alpine\nARG VERSION=\"\"\nRUN VERSION=\"${VERSION:-dev}\" && CGO_ENABLED=0 GOOS=linux go build -ldflags \"-X main.version=${VERSION}\" -o redis-master-label .\n"
	writeBuildFile(t, path, fixedFallback)
	if got, err = buildFileUnstampedVersion(path); err != nil || got != "dev" {
		t.Fatalf("shell fallback: unstamped version = %q, err = %v, want \"dev\"", got, err)
	}

	// A build step that stops passing ${VERSION} to the linker breaks
	// --build-arg VERSION=<tag>, so it must be rejected as unsupported.
	noStamp := "FROM golang:1.27-alpine\nARG VERSION=dev\nRUN CGO_ENABLED=0 GOOS=linux go build -o redis-master-label .\n"
	writeBuildFile(t, path, noStamp)
	if _, err = buildFileUnstampedVersion(path); err == nil {
		t.Fatal("build file without a version-stamping go build step was accepted")
	}
}

func writeBuildFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestHealthHandler_HealthyReturns200(t *testing.T) {
	setHealthStatus(0, true)
	defer setHealthStatus(0, false)

	server := httptest.NewServer(healthHandler())
	defer server.Close()

	resp, err := http.Get(server.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestHealthHandler_UnhealthyReturns503(t *testing.T) {
	setHealthStatus(3, false)
	defer setHealthStatus(0, true)

	server := httptest.NewServer(healthHandler())
	defer server.Close()

	resp, err := http.Get(server.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
}

func TestApplyLabel_UpdateErrorReturnsWrappedError(t *testing.T) {
	defer configureTestFlags()()
	cs := newPodClientset(testPod("test-pod", nil))
	cs.PrependReactor("update", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("update rejected")
	})

	err := applyLabel(context.Background(), cs, "master")
	if err == nil {
		t.Fatal("applyLabel with update error, want error")
	}
	if !strings.HasPrefix(err.Error(), "failed to update pod label") {
		t.Fatalf("error = %v, want wrapped pod update failure", err)
	}
}

func TestApplyLabel_MasterRoleWithWrongValueRewritesLabel(t *testing.T) {
	defer configureTestFlags()()
	cs := newPodClientset(testPod("test-pod", map[string]string{testKey: "replica"}))

	if err := applyLabel(context.Background(), cs, "master"); err != nil {
		t.Fatalf("applyLabel: %v", err)
	}

	pod, err := cs.CoreV1().Pods(testNamespace).Get(context.Background(), "test-pod", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if pod.Labels[testKey] != testValue {
		t.Fatalf("label %s = %q, want %q", testKey, pod.Labels[testKey], testValue)
	}
}

// TestApplyLabel_SlaveRoleWithForeignValueKeepsLabelNoUpdate was removed:
// since the label-removal fix the key is deleted for every non-master role
// whatever value it holds, so this obsolete test contradicted
// TestCheckAndLabel_NonMasterRemovesLabelWithForeignValue.

func TestCheckAndLabel_MasterReplyAppliesLabel(t *testing.T) {
	defer configureTestFlags()()
	cs := newPodClientset(testPod("test-pod", nil))

	if err := checkAndLabel(context.Background(), stubRedis{reply: masterReply()}, cs); err != nil {
		t.Fatalf("checkAndLabel: %v", err)
	}

	pod, err := cs.CoreV1().Pods(testNamespace).Get(context.Background(), "test-pod", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if pod.Labels[testKey] != testValue {
		t.Fatalf("label %s = %q, want %q", testKey, pod.Labels[testKey], testValue)
	}
}

// TestCheckAndLabel_SlaveReplyRemovesLabel was removed: obsolete duplicate of
// TestCheckAndLabel_SlaveReplyRemovesForeignValueLabel above (its canned
// replica reply repeated the same stubRedis/masterReply fixtures).

func TestStartHealthServer_BindFailureReturnsError(t *testing.T) {
	_, err := startHealthServer("999999999")
	if err == nil {
		t.Fatal("startHealthServer with invalid port, want bind error")
	}
	if !strings.HasPrefix(err.Error(), "failed to bind health check server on port 999999999") {
		t.Fatalf("error = %v, want bind failure message", err)
	}
}

func TestStartHealthServer_BindConflictReturnsError(t *testing.T) {
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer listener.Close()
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)

	_, err = startHealthServer(port)
	if err == nil {
		t.Fatalf("startHealthServer on occupied port %s, want bind error", port)
	}
	if !strings.HasPrefix(err.Error(), "failed to bind health check server on port "+port) {
		t.Fatalf("error = %v, want bind failure message", err)
	}
}

func TestStartHealthServer_ServesHealthzAfterBind(t *testing.T) {
	setHealthStatus(0, true)
	defer setHealthStatus(0, false)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer listener.Close()
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)

	errCh := serveHealth(listener)

	resp, err := http.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	select {
	case err := <-errCh:
		t.Fatalf("health server reported unexpected error: %v", err)
	default:
	}
}

func TestStartHealthServer_ErrorChannelReportsServeDeath(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}

	errCh := serveHealth(listener)
	listener.Close()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("error channel entry is nil, want server failure")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("health server Serve did not report its death on the error channel")
	}
}

func TestUpdateHealthStatus_FailureThreshold(t *testing.T) {
	rdbFail := stubRedis{err: errors.New("connection refused")}
	setHealthStatus(0, true)
	defer setHealthStatus(0, true)

	for i := 1; i <= 5; i++ {
		updateHealthStatus(context.Background(), rdbFail)

		healthy, failures := healthSnapshot()
		if want := i < 3; healthy != want {
			t.Errorf("after %d consecutive failures, isHealthy = %v, want %v", i, healthy, want)
		}
		if failures != i {
			t.Errorf("after %d consecutive failures, consecutiveFailures = %d, want %d", i, failures, i)
		}
	}
}

func TestUpdateHealthStatus_ResetAfterFailure(t *testing.T) {
	rdbFail := stubRedis{err: errors.New("connection refused")}
	rdbOk := stubRedis{reply: masterReply()}
	setHealthStatus(0, true)
	defer setHealthStatus(0, true)

	for i := 0; i < 2; i++ {
		updateHealthStatus(context.Background(), rdbFail)
	}
	if healthy, failures := healthSnapshot(); healthy != true || failures != 2 {
		t.Fatalf("health after 2 failures: isHealthy = %v, failures = %d, want true / 2", healthy, failures)
	}

	updateHealthStatus(context.Background(), rdbOk)
	healthy, failures := healthSnapshot()
	if !healthy {
		t.Error("isHealthy after a successful ROLE = false, want true")
	}
	if failures != 0 {
		t.Errorf("consecutiveFailures after a successful ROLE = %d, want 0", failures)
	}
}

func TestUpdateHealthStatus_RecoveryAfterUnhealthy(t *testing.T) {
	rdbFail := stubRedis{err: errors.New("connection refused")}
	rdbOk := stubRedis{reply: masterReply()}
	setHealthStatus(0, true)
	defer setHealthStatus(0, true)

	for i := 0; i < 3; i++ {
		updateHealthStatus(context.Background(), rdbFail)
	}
	if healthy, _ := healthSnapshot(); healthy != false {
		t.Fatal("isHealthy = true after 3 failures, want false")
	}

	updateHealthStatus(context.Background(), rdbOk)
	if healthy, _ := healthSnapshot(); healthy != true {
		t.Error("isHealthy stays false after one successful ROLE, want true")
	}
}

// Non-positive --check-interval values must be rejected at startup:
// time.Sleep returns immediately for them, so the poll loop busy-spins,
// hammering the Redis server and the Kubernetes API server.
func TestValidateCheckInterval(t *testing.T) {
	orig := *checkInterval
	defer func() { *checkInterval = orig }()

	for _, interval := range []time.Duration{0, -5 * time.Second, -time.Nanosecond} {
		*checkInterval = interval
		if err := validateCheckInterval(); err == nil {
			t.Errorf("--check-interval=%v accepted, want an error", interval)
		} else if !strings.Contains(err.Error(), "--check-interval") {
			t.Errorf("error %q does not name --check-interval", err)
		}
	}

	*checkInterval = 10 * time.Second
	if err := validateCheckInterval(); err != nil {
		t.Errorf("positive --check-interval rejected: %v", err)
	}
}

// Version sources are counted per setup-go step, not per file. The reviewed
// defect shape: two setup-go steps where only the first declares
// `go-version-file: go.mod` - the whole-file count of `go-version-file:` lines
// saw one line and stayed quiet, while the second step (only `cache: true`)
// silently installed the runner's default toolchain. The unpinned step must be
// reported, at the line of its own `uses:`.
func TestWorkflowGoToolchainProblems_SecondSetupGoStepWithoutSource(t *testing.T) {
	dir := t.TempDir()
	workflow := "jobs:\n" +
		"  build:\n" +
		"    steps:\n" +
		"      - uses: actions/setup-go@v5\n" +
		"        with:\n" +
		"          go-version-file: go.mod\n" +
		"  second:\n" +
		"    steps:\n" +
		"      - uses: actions/setup-go@v5\n" +
		"        with:\n" +
		"          cache: true\n"
	if err := os.WriteFile(filepath.Join(dir, "ci.yml"), []byte(workflow), 0o644); err != nil {
		t.Fatal(err)
	}

	problems, setupGoSteps, err := workflowGoToolchainProblems(dir)
	if err != nil {
		t.Fatal(err)
	}
	if setupGoSteps != 2 {
		t.Fatalf("setup-go steps = %d, want 2", setupGoSteps)
	}
	if len(problems) != 1 {
		t.Fatalf("problems = %v, want exactly one, for the unpinned step", problems)
	}
	if !strings.Contains(problems[0], "declares no go-version-file") {
		t.Fatalf("problem %q does not name the missing version source", problems[0])
	}
	// Line 9 is the second step's `uses: actions/setup-go@v5` line.
	if !strings.Contains(problems[0], "ci.yml:9") {
		t.Fatalf("problem %q does not point at the unpinned step's uses line", problems[0])
	}
}

// The counting must not be satisfied by anything but the setup-go step itself:
// a `go-version-file:` line in a step that runs another action tells setup-go
// nothing, so the setup-go step stays unpinned and must be reported.
func TestWorkflowGoToolchainProblems_StraySourceInAnotherStep(t *testing.T) {
	dir := t.TempDir()
	workflow := "jobs:\n" +
		"  build:\n" +
		"    steps:\n" +
		"      - uses: actions/setup-go@v5\n" +
		"        with:\n" +
		"          cache: true\n" +
		"      - uses: actions/cache@v4\n" +
		"        with:\n" +
		"          go-version-file: go.mod\n"
	if err := os.WriteFile(filepath.Join(dir, "ci.yml"), []byte(workflow), 0o644); err != nil {
		t.Fatal(err)
	}

	problems, setupGoSteps, err := workflowGoToolchainProblems(dir)
	if err != nil {
		t.Fatal(err)
	}
	if setupGoSteps != 1 {
		t.Fatalf("setup-go steps = %d, want 1", setupGoSteps)
	}
	if len(problems) != 1 {
		t.Fatalf("problems = %v, want exactly one, for the unpinned setup-go step", problems)
	}
	if !strings.Contains(problems[0], "declares no go-version-file") {
		t.Fatalf("problem %q does not name the missing version source", problems[0])
	}
}

// A pinned setup-go step next to an unpinned one must not drag the pinned step
// into the report, and the file-level count of problems equals the number of
// unpinned steps, not one.
func TestWorkflowGoToolchainProblems_PinnedAndUnpinnedSteps(t *testing.T) {
	dir := t.TempDir()
	workflow := "jobs:\n" +
		"  one:\n" +
		"    steps:\n" +
		"      - uses: actions/setup-go@v5\n" +
		"        with:\n" +
		"          go-version-file: go.mod\n" +
		"  two:\n" +
		"    steps:\n" +
		"      - uses: actions/setup-go@v5\n" +
		"        with:\n" +
		"          go-version: \"1.23\"\n"
	if err := os.WriteFile(filepath.Join(dir, "ci.yml"), []byte(workflow), 0o644); err != nil {
		t.Fatal(err)
	}

	problems, setupGoSteps, err := workflowGoToolchainProblems(dir)
	if err != nil {
		t.Fatal(err)
	}
	if setupGoSteps != 2 {
		t.Fatalf("setup-go steps = %d, want 2", setupGoSteps)
	}
	joined := strings.Join(problems, "\n")
	if len(problems) != 1 || !strings.Contains(joined, "go-version: 1.23") {
		t.Fatalf("problems = %v, want exactly one, naming the explicit go-version pin", problems)
	}
}

// ci_check_comments.py is the offline Python guard CI runs right after
// gofmt/migrations. Its setup-go scan is indent-aware: a step's own `with:`
// block ends at the first content line indented no deeper than the step's
// `uses:` line, so a following step's `run:` text (for example a shell line
// mentioning go-version) is never read as that step's second version source,
// while a real `go-version:` input on the setup-go step itself still fails.
// The workflow fixtures replaying those shapes live next to the checker
// (harness.py, which CI runs right after it); a Go test can only rerun them
// where a local python exists.
func TestCiCheckCommentsSetupGoScanIsStepAware(t *testing.T) {
	var python string
	for _, candidate := range []string{"python3", "python"} {
		if found, err := exec.LookPath(candidate); err == nil {
			python = found
			break
		}
	}
	if python == "" {
		t.Skip("no local python; native CI runs harness.py directly")
	}

	out, err := exec.Command(python, "harness.py").CombinedOutput()
	if err != nil {
		t.Fatalf("python harness.py: %v\n%s", err, out)
	}
}

// A workflow that pushes images with provenance/SBOM attestations needs the
// permissions docker/build-push-action documents for that push: id-token: write
// (keyless signing via the workflow's OIDC identity) and attestations: write
// (upload through the GitHub attestations API). Without them the release job
// fails or silently publishes unattested images, so the README's claim about
// image attestations would be unprovable. The real workflows must pass, and a
// workflow shaped like the pre-fix release.yml must be rejected.
func TestWorkflowsDeclareAttestationPermissions(t *testing.T) {
	problems, err := attestationPermissionsProblems(ciWorkflowDir)
	if err != nil {
		t.Fatalf("attestationPermissionsProblems: %v", err)
	}
	if len(problems) != 0 {
		t.Fatalf("attestation permissions problems:\n%s", strings.Join(problems, "\n"))
	}
}

func TestWorkflowsDeclareAttestationPermissions_MissingWriteFails(t *testing.T) {
	dir := t.TempDir()
	preFix := `name: Release
on:
  push:
    tags:
      - 'v*.*.*'

permissions:
  contents: read
  packages: write

jobs:
  build-and-publish:
    runs-on: ubuntu-latest
    steps:
      - name: Publish image
        uses: docker/build-push-action@v6
        with:
          push: true
          provenance: mode=max
          sbom: true
`
	path := filepath.Join(dir, "release.yml")
	if err := os.WriteFile(path, []byte(preFix), 0o644); err != nil {
		t.Fatal(err)
	}
	problems, err := attestationPermissionsProblems(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) == 0 {
		t.Fatal("workflow with attestations but no id-token/attestations write accepted")
	}
}

// GitHub keys a required status check on the name of the job that reported it,
// not on the steps that did the work, and branch protection requires the
// `update-go_modules-graph` check here. The module-graph gate must therefore
// stay a job of its own: folding its commands into another job keeps running
// them while the required context stops reporting, and every pull request then
// waits on a check that can no longer arrive (the reviewed conflict: the job was
// gone from .github/workflows/ci.yml while the check was still required).
func TestWorkflowsReportModuleGraphCheck(t *testing.T) {
	problems, jobs, err := moduleGraphJobProblems(ciWorkflowDir)
	if err != nil {
		t.Fatal(err)
	}
	if jobs != 1 {
		t.Fatalf("%s: %d workflows declare the %s job, want exactly 1: the required status check must report once per commit", ciWorkflowDir, jobs, moduleGraphJobName)
	}
	for _, problem := range problems {
		t.Errorf("%s", problem)
	}
}

// The failure modes at workflow level: the missing job (the reviewed shape and
// the current one, where the same commands live inside another job), a job that
// no longer rebuilds the graph, a job without the non-empty assertion, a
// commented-out job and a renamed job must all be reported, while the required
// job with its two commands must pass.
func TestModuleGraphJobProblems_RegressionFixtures(t *testing.T) {
	graphSteps := "      - name: Update go_modules graph\n        run: |\n          go mod graph > go_modules_graph.txt\n          test -s go_modules_graph.txt\n"

	tests := []struct {
		name          string
		workflow      string
		wantJobs      int
		wantProblem   bool
		wantSubstring string
	}{
		{
			name:          "reviewed pre-repair shape: no module-graph job",
			workflow:      "jobs:\n  go:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@v4\n",
			wantProblem:   true,
			wantSubstring: moduleGraphJobName,
		},
		{
			name:          "graph commands folded into another job",
			workflow:      "jobs:\n  go:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@v4\n" + graphSteps,
			wantProblem:   true,
			wantSubstring: moduleGraphJobName,
		},
		{
			name:          "job declared without rebuilding the graph",
			workflow:      "jobs:\n  " + moduleGraphJobName + ":\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@v4\n",
			wantJobs:      1,
			wantProblem:   true,
			wantSubstring: "does not run `go mod graph`",
		},
		{
			name:          "job declared without the non-empty assertion",
			workflow:      "jobs:\n  " + moduleGraphJobName + ":\n    runs-on: ubuntu-latest\n    steps:\n      - name: Update go_modules graph\n        run: go mod graph > go_modules_graph.txt\n",
			wantJobs:      1,
			wantProblem:   true,
			wantSubstring: "test -s",
		},
		{
			name:          "job declared only in a comment",
			workflow:      "jobs:\n#  " + moduleGraphJobName + ":\n  go:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@v4\n",
			wantProblem:   true,
			wantSubstring: moduleGraphJobName,
		},
		{
			name:        "required job with both commands",
			workflow:    "jobs:\n  " + moduleGraphJobName + ":\n    name: " + moduleGraphJobName + "\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@v4\n" + graphSteps,
			wantJobs:    1,
			wantProblem: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "ci.yml"), []byte(tt.workflow), 0o644); err != nil {
				t.Fatal(err)
			}

			problems, jobs, err := moduleGraphJobProblems(dir)
			if err != nil {
				t.Fatal(err)
			}
			if jobs != tt.wantJobs {
				t.Errorf("jobs declaring %s = %d, want %d", moduleGraphJobName, jobs, tt.wantJobs)
			}
			if !tt.wantProblem {
				if len(problems) != 0 {
					t.Fatalf("problems = %v, want none", problems)
				}
				return
			}
			if len(problems) == 0 {
				t.Fatal("workflow that would stop reporting the required status check accepted, want a problem")
			}
			if !strings.Contains(strings.Join(problems, "\n"), tt.wantSubstring) {
				t.Fatalf("problems = %v, want one naming %q", problems, tt.wantSubstring)
			}
		})
	}
}
