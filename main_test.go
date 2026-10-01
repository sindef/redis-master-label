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
