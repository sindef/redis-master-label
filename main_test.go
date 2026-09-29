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
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const (
	testNamespace = "test-ns"
	testPodName   = "test-pod"
	testLabelKey  = "redis-role"
	testLabelVal  = "master"
)

func newTestPod(labels map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testPodName,
			Namespace: testNamespace,
			Labels:    labels,
		},
	}
}

func labelOf(clientset kubernetes.Interface) (string, bool) {
	pod, err := clientset.CoreV1().Pods(testNamespace).Get(context.Background(), testPodName, metav1.GetOptions{})
	if err != nil {
		return "", false
	}
	v, ok := pod.Labels[testLabelKey]
	return v, ok
}

func TestRoleFromResponse(t *testing.T) {
	cases := []struct {
		name    string
		in      interface{}
		want    string
		wantErr bool
	}{
		{"master triplet", []interface{}{"master", 0, nil}, "master", false},
		{"slave", []interface{}{"slave"}, "slave", false},
		{"sentinel", []interface{}{"sentinel"}, "sentinel", false},
		{"not an array", "master", "", true},
		{"empty array", []interface{}{}, "", true},
		{"non-string first element", []interface{}{42}, "", true},
		{"nil response", nil, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := roleFromResponse(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("roleFromResponse() error = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil && got != tc.want {
				t.Fatalf("roleFromResponse() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCheckAndLabelMasterNotLabeled(t *testing.T) {
	pod := newTestPod(map[string]string{"app": "redis"})
	clientset := fake.NewSimpleClientset(pod)

	err := checkAndLabel(context.Background(), clientset, testNamespace, testPodName, testLabelKey, testLabelVal, "master")
	if err != nil {
		t.Fatalf("checkAndLabel() error = %v, want nil", err)
	}

	if got, ok := labelOf(clientset); !ok || got != testLabelVal {
		t.Fatalf("pod label = %q (exists=%v), want %q", got, ok, testLabelVal)
	}
}

func TestCheckAndLabelMasterAlreadyLabeledIsNoOp(t *testing.T) {
	pod := newTestPod(map[string]string{testLabelKey: testLabelVal})
	clientset := fake.NewSimpleClientset(pod)

	err := checkAndLabel(context.Background(), clientset, testNamespace, testPodName, testLabelKey, testLabelVal, "master")
	if err != nil {
		t.Fatalf("checkAndLabel() error = %v, want nil", err)
	}

	if actions := clientset.Actions(); len(actions) != 1 {
		t.Fatalf("expected only the initial Get, got %d actions: %v", len(actions), actions)
	}
}

func TestCheckAndLabelNoLongerMasterRemovesLabel(t *testing.T) {
	pod := newTestPod(map[string]string{testLabelKey: testLabelVal})
	clientset := fake.NewSimpleClientset(pod)

	err := checkAndLabel(context.Background(), clientset, testNamespace, testPodName, testLabelKey, testLabelVal, "slave")
	if err != nil {
		t.Fatalf("checkAndLabel() error = %v, want nil", err)
	}

	if _, exists := labelOf(clientset); exists {
		t.Fatal("pod should no longer carry the label, but it does")
	}
}

func TestCheckAndLabelNoLongerMasterWithoutLabelIsNoOp(t *testing.T) {
	pod := newTestPod(map[string]string{testLabelKey: "other-value"})
	clientset := fake.NewSimpleClientset(pod)

	err := checkAndLabel(context.Background(), clientset, testNamespace, testPodName, testLabelKey, testLabelVal, "slave")
	if err != nil {
		t.Fatalf("checkAndLabel() error = %v, want nil", err)
	}

	if actions := clientset.Actions(); len(actions) != 1 {
		t.Fatalf("expected only the initial Get, got %d actions: %v", len(actions), actions)
	}
}

func TestCheckAndLabelPodGetError(t *testing.T) {
	clientset := fake.NewSimpleClientset()

	err := checkAndLabel(context.Background(), clientset, testNamespace, testPodName, testLabelKey, testLabelVal, "master")
	if err == nil {
		t.Fatal("checkAndLabel() error = nil, want missing-pod error")
	}
	if want := "failed to get pod:"; !strings.Contains(err.Error(), want) {
		t.Fatalf("checkAndLabel() error = %q, want it to contain %q", err.Error(), want)
	}
}

func TestCheckAndLabelUpdateError(t *testing.T) {
	pod := newTestPod(map[string]string{"app": "redis"})
	clientset := fake.NewSimpleClientset(pod)
	updateErr := errors.New("update failed")
	clientset.PrependReactor("update", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, updateErr
	})

	err := checkAndLabel(context.Background(), clientset, testNamespace, testPodName, testLabelKey, testLabelVal, "master")
	if err == nil {
		t.Fatal("checkAndLabel() error = nil, want update failure")
	}
	if !strings.Contains(err.Error(), updateErr.Error()) {
		t.Fatalf("checkAndLabel() error = %q, want it to contain %q", err.Error(), updateErr.Error())
	}
}

func TestHealthHandlerHealthy(t *testing.T) {
	setHealthState(true)
	srv := httptest.NewServer(healthHandler())
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestHealthHandlerUnhealthy(t *testing.T) {
	setHealthState(false)
	srv := httptest.NewServer(healthHandler())
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("GET /healthz status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
}

func setHealthState(healthy bool) {
	healthStatus.mu.Lock()
	defer healthStatus.mu.Unlock()
	healthStatus.consecutiveFailures = 0
	healthStatus.isHealthy = healthy
}

// stubRedis implements updateHealthStatus's roleQuerier without a live server.
type stubRedis struct {
	err error
	val []interface{}
}

func (s stubRedis) Do(ctx context.Context, args ...interface{}) *redis.Cmd {
	return redis.NewCmdResult(s.val, s.err)
}

func TestUpdateHealthStatusTracksConsecutiveFailures(t *testing.T) {
	setHealthState(true)
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

func assertHealthy(t *testing.T, want bool) {
	t.Helper()
	healthStatus.mu.RLock()
	defer healthStatus.mu.RUnlock()
	if healthStatus.isHealthy != want {
		t.Fatalf("healthStatus.isHealthy = %v, want %v", healthStatus.isHealthy, want)
	}
}
