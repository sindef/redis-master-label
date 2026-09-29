package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

func isHealthyNow() bool {
	healthStatus.mu.RLock()
	defer healthStatus.mu.RUnlock()
	return healthStatus.isHealthy
}

func assertHealthy(t *testing.T, want bool) {
	t.Helper()
	if got := isHealthyNow(); got != want {
		t.Fatalf("healthStatus.isHealthy = %v, want %v", got, want)
	}
}

func countActions(cs *k8sfake.Clientset, verb string) int {
	updates := 0
	for _, action := range cs.Actions() {
		if action.GetVerb() == verb {
			updates++
		}
	}
	return updates
}

func newPodClientset(pods ...runtime.Object) *k8sfake.Clientset {
	return k8sfake.NewSimpleClientset(pods...)
}

func testPod(name string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, Labels: labels},
	}
}

// stubRedis implements checkAndLabel/updateHealthStatus's redisDoer without a
// live server.
type stubRedis struct {
	err error
	val []interface{}
}

func (s stubRedis) Do(ctx context.Context, args ...interface{}) *redis.Cmd {
	return redis.NewCmdResult(s.val, s.err)
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
	if updates := countActions(cs, "update"); updates != 0 {
		t.Fatalf("update calls = %d, want 0", updates)
	}
}

func TestCheckAndLabel_SlaveRoleWithoutLabelIsNoOp(t *testing.T) {
	defer configureTestFlags()()
	cs := newPodClientset(testPod("test-pod", nil))

	if err := applyLabel(context.Background(), cs, "slave"); err != nil {
		t.Fatalf("applyLabel: %v", err)
	}

	if updates := countActions(cs, "update"); updates != 0 {
		t.Fatalf("update calls = %d, want 0", updates)
	}
}

func TestCheckAndLabel_SlaveRoleOtherValueIsNoOp(t *testing.T) {
	defer configureTestFlags()()
	cs := newPodClientset(testPod("test-pod", map[string]string{testKey: "other"}))

	if err := applyLabel(context.Background(), cs, "slave"); err != nil {
		t.Fatalf("applyLabel: %v", err)
	}

	if updates := countActions(cs, "update"); updates != 0 {
		t.Fatalf("update calls = %d, want 0", updates)
	}
}

func TestCheckAndLabel_MissingPodReturnsError(t *testing.T) {
	defer configureTestFlags()()
	cs := newPodClientset()

	err := applyLabel(context.Background(), cs, "master")
	if err == nil {
		t.Fatal("applyLabel with no pod, want error")
	}
	if want := "failed to get pod:"; !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %q, want it to contain %q", err.Error(), want)
	}
}

func TestCheckAndLabel_MasterRoleStaleValueUpdates(t *testing.T) {
	defer configureTestFlags()()
	cs := newPodClientset(testPod("test-pod", map[string]string{testKey: "stale"}))

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
	if updates := countActions(cs, "update"); updates != 1 {
		t.Fatalf("update calls = %d, want 1", updates)
	}
}

func TestCheckAndLabel_UpdateError(t *testing.T) {
	defer configureTestFlags()()
	cs := newPodClientset(testPod("test-pod", map[string]string{"app": "redis"}))
	updateErr := errors.New("update failed")
	cs.PrependReactor("update", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, updateErr
	})

	err := checkAndLabel(context.Background(), stubRedis{val: []interface{}{"master", 0, nil}}, cs)
	if err == nil {
		t.Fatal("checkAndLabel on update failure, want error")
	}
	if !strings.Contains(err.Error(), "failed to update pod label") {
		t.Fatalf("error = %q, want it to contain %q", err.Error(), "updated pod label")
	}
}

func TestCheckAndLabel_RoleCommandError(t *testing.T) {
	defer configureTestFlags()()
	cs := newPodClientset(testPod("test-pod", nil))

	err := checkAndLabel(context.Background(), stubRedis{err: errors.New("connection refused")}, cs)
	if err == nil {
		t.Fatal("checkAndLabel with ROLE failure, want error")
	}
	if want := "failed to execute ROLE command"; !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %q, want it to contain %q", err.Error(), want)
	}
}

func TestCheckAndLabel_MalformedRoleReply(t *testing.T) {
	defer configureTestFlags()()
	cs := newPodClientset(testPod("test-pod", nil))
	defer setHealthStatus(0, true)

	err := checkAndLabel(context.Background(), stubRedis{val: nil}, cs)
	if err == nil || err.Error() != "unexpected ROLE response format" {
		t.Fatalf("error = %v, want %q", err, "unexpected ROLE response format")
	}
	if updates := countActions(cs, "update"); updates != 0 {
		t.Fatalf("update calls = %d, want 0", updates)
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

func TestUpdateHealthStatusTracksConsecutiveFailures(t *testing.T) {
	setHealthStatus(0, true)
	defer setHealthStatus(0, true)
	ctx := context.Background()

	failing := stubRedis{err: errors.New("connection refused")}
	updateHealthStatus(ctx, failing)
	assertHealthy(t, true)
	updateHealthStatus(ctx, failing)
	assertHealthy(t, true)
	updateHealthStatus(ctx, failing)
	assertHealthy(t, false)

	updateHealthStatus(ctx, stubRedis{val: []interface{}{"master", 0, nil}})
	assertHealthy(t, true)
}
