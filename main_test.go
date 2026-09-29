package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
)

// fakeRedis is a narrow fake of the only redis surface this tool uses: the
// Do-based ROLE query. It replays a canned reply or error and counts calls.
type fakeRedis struct {
	reply interface{}
	err   error

	mu    sync.Mutex
	calls int
}

func (f *fakeRedis) Do(ctx context.Context, args ...interface{}) (interface{}, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.reply, f.err
}

func (f *fakeRedis) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
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
		"10.244.0.12",
		"6379",
		"online",
		int64(12345),
		[]interface{}{"0:1", "0:2"},
	}
}

const (
	defaultLabelKey   = "redis-role"
	defaultLabelValue = "master"
	staleLabelValue   = "stale"
)

// wantLabel pointers point at these vars, since taking the address of a
// constant is not allowed in Go.
var (
	wantApplyLabel   = defaultLabelValue
	wantRemoveLabel  = ""
	wantStaleUntouch = staleLabelValue
)

func newTestPod(labels map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "redis-0",
			Namespace: "test-ns",
			Labels:    labels,
		},
	}
}

// newPodSource returns a fake clientset seeded with pod (nil allowed) plus a
// podGetterUpdater around its corev1 pods interface, so no live API server and
// no concrete *kubernetes.Clientset is needed.
func newPodSource(t *testing.T, pod *corev1.Pod) (*k8sfake.Clientset, *clientsetPods, corev1client.PodInterface) {
	t.Helper()
	t.Setenv("HOSTNAME", "redis-0")
	t.Setenv("POD_NAMESPACE", "test-ns")
	*podName = "redis-0"
	*podNamespace = "test-ns"
	*labelKey = defaultLabelKey
	*labelValue = defaultLabelValue
	objs := []runtime.Object{}
	if pod != nil {
		objs = append(objs, pod)
	}
	cs := k8sfake.NewSimpleClientset(objs...)
	pods := cs.CoreV1().Pods("test-ns")
	return cs, &clientsetPods{pods: cs.CoreV1()}, pods
}

func countUpdateActions(cs *k8sfake.Clientset) int {
	n := 0
	for _, action := range cs.Actions() {
		if action.GetVerb() == "update" && action.GetResource().Resource == "pods" {
			n++
		}
	}
	return n
}

func TestRoleFromResponse(t *testing.T) {
	tests := []struct {
		name    string
		reply   interface{}
		want    string
		wantErr string
	}{
		{name: "master reply", reply: masterReply(), want: "master"},
		{name: "slave reply", reply: slaveReply(), want: "slave"},
		{name: "nil reply", reply: nil, wantErr: "unexpected ROLE response format"},
		{name: "empty role array", reply: []interface{}{}, wantErr: "unexpected ROLE response format"},
		{name: "non-array reply", reply: "master", wantErr: "unexpected ROLE response format"},
		{name: "non-string first element", reply: []interface{}{int64(12345)}, wantErr: "unexpected ROLE response format"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := roleFromResponse(tt.reply)
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Errorf("roleFromResponse() error = %q, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("roleFromResponse() unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("roleFromResponse() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCheckAndLabel(t *testing.T) {
	tests := []struct {
		name        string
		role        interface{}
		redisErr    error
		initLabels  map[string]string
		wantLabel   *string // empty string asserts label absence after removal
		wantUpdates int
	}{
		{
			name:        "master applies label to unlabeled pod",
			role:        masterReply(),
			initLabels:  nil,
			wantLabel:   &wantApplyLabel,
			wantUpdates: 1,
		},
		{
			name:        "master keeps existing matching label without update",
			role:        masterReply(),
			initLabels:  map[string]string{defaultLabelKey: defaultLabelValue},
			wantUpdates: 0,
		},
		{
			name:        "master overwrites stale label value",
			role:        masterReply(),
			initLabels:  map[string]string{defaultLabelKey: staleLabelValue},
			wantLabel:   &wantApplyLabel,
			wantUpdates: 1,
		},
		{
			name:        "replica removes existing master label",
			role:        slaveReply(),
			initLabels:  map[string]string{defaultLabelKey: defaultLabelValue},
			wantLabel:   &wantRemoveLabel,
			wantUpdates: 1,
		},
		{
			name:        "replica leaves unlabeled pod untouched",
			role:        slaveReply(),
			initLabels:  nil,
			wantUpdates: 0,
		},
		{
			name:        "replica leaves wrong-valued label untouched",
			role:        slaveReply(),
			initLabels:  map[string]string{defaultLabelKey: staleLabelValue},
			wantLabel:   &wantStaleUntouch,
			wantUpdates: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rdb := &fakeRedis{reply: tt.role, err: tt.redisErr}
			pod := newTestPod(tt.initLabels)
			cs, source, pods := newPodSource(t, pod)

			err := checkAndLabel(context.Background(), rdb, source)
			if err != nil {
				t.Fatalf("checkAndLabel() error = %v, want nil", err)
			}
			if got := rdb.callCount(); got != 1 {
				t.Errorf("ROLE calls = %d, want 1", got)
			}
			if got := countUpdateActions(cs); got != tt.wantUpdates {
				t.Fatalf("update actions = %d, want %d", got, tt.wantUpdates)
			}
			switch {
			case tt.wantUpdates == 1 && *tt.wantLabel == wantRemoveLabel:
				checkLabelAbsent(t, pods)
			case tt.wantUpdates == 1:
				checkLabelValue(t, pods, *tt.wantLabel)
			}
		})
	}
}

func checkLabelValue(t *testing.T, pods corev1client.PodInterface, want string) {
	t.Helper()
	got, err := pods.Get(context.Background(), "redis-0", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting pod: %v", err)
	}
	if got.Labels[defaultLabelKey] != want {
		t.Errorf("label value = %q, want %q", got.Labels[defaultLabelKey], want)
	}
}

func checkLabelAbsent(t *testing.T, pods corev1client.PodInterface) {
	t.Helper()
	got, err := pods.Get(context.Background(), "redis-0", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting pod: %v", err)
	}
	if _, exists := got.Labels[defaultLabelKey]; exists {
		t.Errorf("label %q still present, want removed", defaultLabelKey)
	}
}

func TestCheckAndLabelMissingPod(t *testing.T) {
	rdb := &fakeRedis{reply: masterReply()}
	_, source, _ := newPodSource(t, nil)

	err := checkAndLabel(context.Background(), rdb, source)
	if err == nil {
		t.Fatal("checkAndLabel() error = nil, want pod not found error")
	}
	if !strings.HasPrefix(err.Error(), "failed to get pod") {
		t.Errorf("error = %v, want wrapped pod-get failure", err)
	}
}

func TestCheckAndLabelRedisError(t *testing.T) {
	rdb := &fakeRedis{err: errors.New("connection refused")}
	_, source, _ := newPodSource(t, newTestPod(nil))

	err := checkAndLabel(context.Background(), rdb, source)
	if err == nil {
		t.Fatal("checkAndLabel() error = nil, want ROLE command error")
	}
	if !strings.HasPrefix(err.Error(), "failed to execute ROLE command") {
		t.Errorf("error = %v, want ROLE command wrapped error", err)
	}
}

func TestCheckAndLabelMalformedReply(t *testing.T) {
	for name, reply := range map[string]interface{}{
		"nil reply":                nil,
		"empty role array":         []interface{}{},
		"non-array reply":          "master",
		"non-string first element": []interface{}{int64(12345)},
	} {
		t.Run(name, func(t *testing.T) {
			rdb := &fakeRedis{reply: reply}
			cs, source, _ := newPodSource(t, newTestPod(nil))
			_ = cs

			err := checkAndLabel(context.Background(), rdb, source)
			if err == nil || err.Error() != "unexpected ROLE response format" {
				t.Errorf("error = %q, want %q", err, "unexpected ROLE response format")
			}
		})
	}
}

// resetHealth forces the shared health state into the healthy baseline so
// threshold tests run independently of other tests.
func resetHealth(t *testing.T) {
	t.Helper()
	setHealth(0, true)
	t.Cleanup(func() { setHealth(0, true) })
}

func setHealth(failures int, healthy bool) {
	healthStatus.mu.Lock()
	healthStatus.consecutiveFailures = failures
	healthStatus.isHealthy = healthy
	healthStatus.mu.Unlock()
}

func healthSnapshot() (bool, int) {
	healthStatus.mu.RLock()
	defer healthStatus.mu.RUnlock()
	return healthStatus.isHealthy, healthStatus.consecutiveFailures
}

func TestUpdateHealthStatusThreshold(t *testing.T) {
	resetHealth(t)
	rdbFail := &fakeRedis{err: errors.New("connection refused")}

	for i := 1; i <= 5; i++ {
		updateHealthStatus(context.Background(), rdbFail)
		healthy, failures := healthSnapshot()
		wantHealthy := i < 3
		if healthy != wantHealthy {
			t.Errorf("after %d consecutive failures, isHealthy = %v, want %v", i, healthy, wantHealthy)
		}
		if failures != i {
			t.Errorf("after %d consecutive failures, consecutiveFailures = %d, want %d", i, failures, i)
		}
	}
}

func TestUpdateHealthStatusResetAfterFailure(t *testing.T) {
	resetHealth(t)
	rdbFail := &fakeRedis{err: errors.New("connection refused")}
	rdbOk := &fakeRedis{reply: masterReply()}

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

func TestUpdateHealthStatusRecoveryAfterUnhealthy(t *testing.T) {
	resetHealth(t)
	rdbFail := &fakeRedis{err: errors.New("connection refused")}
	rdbOk := &fakeRedis{reply: masterReply()}

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

func TestHealthEndpoint(t *testing.T) {
	setHealth(0, true)
	srv := httptest.NewServer(healthHandler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	body := readAll(t, resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthy status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if !strings.Contains(body, "OK") {
		t.Errorf("healthy body = %q, want OK", body)
	}

	setHealth(0, false)
	resp, err = http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	body = readAll(t, resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("unhealthy status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
	if !strings.Contains(body, "Unhealthy") {
		t.Errorf("unhealthy body = %q, want Unhealthy", body)
	}
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	buf := make([]byte, 64)
	n, _ := resp.Body.Read(buf)
	return string(buf[:n])
}
