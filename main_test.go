package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
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

// A label key that holds some other tool's value must survive: the tool only
// removes the label when it carries the value this instance itself manages.
func TestApplyLabel_SlaveRoleWithForeignValueKeepsLabelNoUpdate(t *testing.T) {
	defer configureTestFlags()()
	cs := newPodClientset(testPod("test-pod", map[string]string{testKey: "replica"}))

	if err := applyLabel(context.Background(), cs, "slave"); err != nil {
		t.Fatalf("applyLabel: %v", err)
	}

	pod, err := cs.CoreV1().Pods(testNamespace).Get(context.Background(), "test-pod", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if pod.Labels[testKey] != "replica" {
		t.Fatalf("label %s = %q, want %q untouched", testKey, pod.Labels[testKey], "replica")
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

// slaveReply mirrors the array layout Redis answers with for ROLE on a replica.
func slaveReply() []interface{} {
	return []interface{}{
		"slave",
		"10.244.0.11",
		int64(6379),
		"connected",
		int64(12345),
	}
}

func TestCheckAndLabel_SlaveReplyRemovesLabel(t *testing.T) {
	defer configureTestFlags()()
	cs := newPodClientset(testPod("test-pod", map[string]string{testKey: testValue}))

	if err := checkAndLabel(context.Background(), stubRedis{reply: slaveReply()}, cs); err != nil {
		t.Fatalf("checkAndLabel: %v", err)
	}

	pod, err := cs.CoreV1().Pods(testNamespace).Get(context.Background(), "test-pod", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if _, exists := pod.Labels[testKey]; exists {
		t.Fatalf("label %s still present, want removed", testKey)
	}
}

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
