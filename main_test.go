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

// The repository must grant the license its image advertises. A heading alone
// is not a grant, so the whole MIT text is required: the title, the permission
// grant, the warranty disclaimer and a copyright line.
func TestLicenseFileGrantsMIT(t *testing.T) {
	data, err := os.ReadFile(licenseFile)
	if err != nil {
		t.Fatalf("read %s: %v", licenseFile, err)
	}
	text := string(data)
	for _, want := range []string{"MIT License", mitLicensePermission, "WITHOUT WARRANTY OF ANY KIND", "Copyright (c)"} {
		if !strings.Contains(text, want) {
			t.Errorf("%s is missing %q: it must be the full MIT license text, not a pointer to one", licenseFile, want)
		}
	}

	identifier, err := licenseIdentifier(licenseFile)
	if err != nil {
		t.Fatal(err)
	}
	if identifier != "MIT" {
		t.Errorf("licenseIdentifier(%s) = %q, want \"MIT\"", licenseFile, identifier)
	}
}

// The image's OCI metadata is where a user learns the license of the image they
// pulled, so the label the shipping stage sets must name the license the
// repository grants. The label must be in the final stage as well: a label in
// the builder stage is discarded with it, which is how an image advertises a
// license that no file in the repository backed.
func TestImageAdvertisesRepositoryLicense(t *testing.T) {
	granted, err := licenseIdentifier(licenseFile)
	if err != nil {
		t.Fatal(err)
	}
	labels, err := dockerfileFinalStageLabels(dockerfilePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := imageLicenseProblem(labels, granted); err != nil {
		t.Fatal(err)
	}
}

// The README is where a user looks for licensing, and it must link the file the
// repository actually ships rather than only naming a license.
func TestReadmeDocumentsLicense(t *testing.T) {
	data, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	text := string(data)
	for _, want := range []string{"## License", "[LICENSE](LICENSE)", imageLicenseLabel} {
		if !strings.Contains(text, want) {
			t.Errorf("README.md is missing %q", want)
		}
	}
}

// The reviewed failure modes of the license label: a label in a discarded
// builder stage must not count, a quoted value continued over several lines
// must be read whole, a value claiming a license the repository does not grant
// must be reported, and a LICENSE file that is not the MIT grant must be
// rejected instead of being reported as MIT.
func TestDockerfileFinalStageLabels_RegressionFixtures(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	builderOnly := write("builder-only.Dockerfile", "FROM golang:1.27-alpine AS builder\nLABEL org.opencontainers.image.licenses=\"MIT\"\nRUN go build .\n\nFROM alpine:3.24\nRUN apk --no-cache add ca-certificates\n")
	labels, err := dockerfileFinalStageLabels(builderOnly)
	if err != nil {
		t.Fatal(err)
	}
	if len(labels) != 0 {
		t.Errorf("labels read from the builder stage = %v, want none: that stage is discarded", labels)
	}
	if err := imageLicenseProblem(labels, "MIT"); err == nil {
		t.Error("imageLicenseProblem accepted an image with no license label, want the missing-label failure")
	}

	continued := write("continued.Dockerfile", "FROM alpine:3.24\nARG VERSION=dev\nLABEL org.opencontainers.image.title=\"redis-master-label\" \\\n      org.opencontainers.image.licenses=\"MIT\" \\\n      org.opencontainers.image.version=\"${VERSION}\"\nLABEL legacy-license MIT\n")
	labels, err = dockerfileFinalStageLabels(continued)
	if err != nil {
		t.Fatal(err)
	}
	if got := labels[imageLicenseLabel]; got != "MIT" {
		t.Errorf("continuation fixture: %s = %q, want \"MIT\"", imageLicenseLabel, got)
	}
	if got := labels["org.opencontainers.image.title"]; got != "redis-master-label" {
		t.Errorf("continuation fixture: org.opencontainers.image.title = %q, want \"redis-master-label\"", got)
	}
	if got := labels["legacy-license"]; got != "MIT" {
		t.Errorf("legacy `LABEL name value` form = %q, want \"MIT\"", got)
	}
	if err := imageLicenseProblem(labels, "MIT"); err != nil {
		t.Errorf("imageLicenseProblem rejected an image advertising the granted license: %v", err)
	}

	mismatch := write("mismatch.Dockerfile", "FROM alpine:3.24\nLABEL org.opencontainers.image.licenses=\"Apache-2.0\"\n")
	labels, err = dockerfileFinalStageLabels(mismatch)
	if err != nil {
		t.Fatal(err)
	}
	if err := imageLicenseProblem(labels, "MIT"); err == nil {
		t.Error("imageLicenseProblem accepted an image claiming Apache-2.0 while the repository grants MIT")
	}

	notMIT := write("LICENSE", "Apache License\nVersion 2.0, January 2004\n")
	if identifier, err := licenseIdentifier(notMIT); err == nil {
		t.Errorf("licenseIdentifier(%s) = %q, want an error: the file grants no MIT", notMIT, identifier)
	}
	if _, err := licenseIdentifier(filepath.Join(dir, "missing-LICENSE")); err == nil {
		t.Error("licenseIdentifier with a missing LICENSE file returned no error")
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
