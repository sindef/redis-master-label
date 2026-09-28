package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
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

func TestCheckAndLabel_MissingPodReturnsError(t *testing.T) {
	defer configureTestFlags()()
	cs := newPodClientset()

	err := applyLabel(context.Background(), cs, "master")
	if err == nil {
		t.Fatal("applyLabel with no pod, want error")
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
