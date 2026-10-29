package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
)

// stalledRoleQuerier never answers ROLE: it only returns when its context is
// done, modelling a half-open Redis connection that ignores no deadline of its
// own.
type stalledRoleQuerier struct{}

func (stalledRoleQuerier) Do(ctx context.Context, args ...interface{}) *redis.Cmd {
	cmd := redis.NewCmd(ctx, args...)
	<-ctx.Done()
	cmd.SetErr(ctx.Err())
	return cmd
}

// A stalled backend must mark the labeler unhealthy on the first deadline, not
// after three failures: the deadline itself proves the backend did not answer,
// and /healthz must stop answering a stale 200.
func TestUpdateHealthStatus_DeadlineTurnsHealthz503(t *testing.T) {
	setHealthStatus(0, true)
	defer setHealthStatus(0, true)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	updateHealthStatus(ctx, stalledRoleQuerier{})

	healthy, failures := healthSnapshot()
	if healthy {
		t.Error("isHealthy = true after a ROLE that only ended at the deadline, want false")
	}
	if failures == 0 {
		t.Error("consecutiveFailures = 0 after a stalled ROLE, want the failure recorded")
	}

	recorder := httptest.NewRecorder()
	healthHandler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Errorf("/healthz status = %d after a stalled ROLE, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
}

// A client that ignores cancellation must not block the health path: the
// iteration deadline bounds the ROLE command.
func TestQueryRoleRacesDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := queryRole(ctx, stalledRoleQuerier{}); err == nil {
		t.Fatal("queryRole returned no error for a ROLE that never answered")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("queryRole blocked for %v, want it bounded by the 20ms deadline", elapsed)
	}
}

// One poll iteration must be bounded by backendTimeout() even when ROLE never
// answers, and the loop must record the failure before returning so /healthz
// stops answering a stale 200.
func TestRunPollIteration_StalledRedisTurnsHealthz503(t *testing.T) {
	defer configureTestFlags()()
	orig := *checkInterval
	defer func() { *checkInterval = orig }()
	*checkInterval = 200 * time.Millisecond

	setHealthStatus(0, true)
	defer setHealthStatus(0, true)

	clientset := newPodClientset(testPod("test-pod", nil))

	start := time.Now()
	err := runPollIteration(context.Background(), stalledRoleQuerier{}, clientset)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("runPollIteration returned no error for a ROLE that never answered")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("runPollIteration blocked for %v, want it bounded by the 100ms iteration deadline", elapsed)
	}

	healthy, failures := healthSnapshot()
	if healthy || failures == 0 {
		t.Fatalf("isHealthy = %v, consecutiveFailures = %d after a stalled ROLE, want unhealthy with the failure recorded", healthy, failures)
	}

	recorder := httptest.NewRecorder()
	healthHandler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Errorf("/healthz status = %d after a stalled ROLE, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
}

// A stalled Kubernetes API server must not freeze the iteration either: the
// rest.Config timeout set by boundRestConfig bounds Pods.Get, and the label-phase
// timeout is recorded directly.
func TestRunPollIteration_StalledAPIServerIsBounded(t *testing.T) {
	defer configureTestFlags()()
	orig := *checkInterval
	defer func() { *checkInterval = orig }()
	*checkInterval = 400 * time.Millisecond

	setHealthStatus(0, true)
	defer setHealthStatus(0, true)

	stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer stalled.Close()

	clientset, err := kubernetes.NewForConfig(boundRestConfig(&rest.Config{Host: stalled.URL}))
	if err != nil {
		t.Fatalf("build clientset for the stalled API server: %v", err)
	}

	start := time.Now()
	err = runPollIteration(context.Background(), stubRedis{reply: masterReply()}, clientset)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("runPollIteration returned no error for a Pods.Get that never answered")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("runPollIteration blocked for %v, want it bounded by the 200ms client timeout", elapsed)
	}

	healthy, failures := healthSnapshot()
	if healthy || failures == 0 {
		t.Fatalf("isHealthy = %v, consecutiveFailures = %d after a stalled API server, want unhealthy with the failure recorded", healthy, failures)
	}
}

// timeoutNetError is the client-timeout shape a rest request returns while the
// iteration context is still live: a net.Error whose Timeout() is true.
type timeoutNetError struct{}

func (timeoutNetError) Error() string   { return "i/o timeout" }
func (timeoutNetError) Timeout() bool   { return true }
func (timeoutNetError) Temporary() bool { return true }

var _ net.Error = timeoutNetError{}

// A label-phase client timeout must mark health unhealthy on its own. Probing
// ROLE again afterwards would reset /healthz to 200 a moment after the dead
// API call, which is the ordering that made the older stalled-API test flaky.
func TestRunPollIteration_LabelPhaseTimeoutMarksUnhealthy(t *testing.T) {
	defer configureTestFlags()()
	orig := *checkInterval
	defer func() { *checkInterval = orig }()
	// Large enough that the iteration context is still live when the reactor
	// fails: the error has to be classified, not just the deadline.
	*checkInterval = 10 * time.Second

	setHealthStatus(0, true)
	defer setHealthStatus(0, true)

	clientset := newPodClientset(testPod("test-pod", nil))
	clientset.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, &url.Error{Op: "Get", URL: "https://api.example.test/pods/test-pod", Err: timeoutNetError{}}
	})

	start := time.Now()
	err := runPollIteration(context.Background(), stubRedis{reply: masterReply()}, clientset)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("runPollIteration returned no error for a Pods.Get client timeout")
	}
	if !isTimeoutError(err) {
		t.Fatalf("isTimeoutError(%v) = false, want the client timeout classified", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("runPollIteration took %v, want the client timeout returned immediately instead of waiting for the 5s deadline", elapsed)
	}

	healthy, failures := healthSnapshot()
	if healthy {
		t.Error("isHealthy = true after a label-phase client timeout, want false: a later ROLE probe must not reset a stalled backend to healthy")
	}
	if failures == 0 {
		t.Error("consecutiveFailures = 0 after a label-phase client timeout, want the failure recorded")
	}

	recorder := httptest.NewRecorder()
	healthHandler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Errorf("/healthz status = %d after a label-phase timeout, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
}

func TestBackendTimeoutBoundedByCheckInterval(t *testing.T) {
	orig := *checkInterval
	defer func() { *checkInterval = orig }()

	*checkInterval = 10 * time.Second
	if got, want := backendTimeout(), 5*time.Second; got != want {
		t.Errorf("backendTimeout() = %v for a 10s interval, want %v", got, want)
	}

	*checkInterval = 10 * time.Millisecond
	if got, want := backendTimeout(), 100*time.Millisecond; got != want {
		t.Errorf("backendTimeout() = %v for a 10ms interval, want the %v floor", got, want)
	}
}

func TestBoundRestConfigSetsRequestTimeout(t *testing.T) {
	orig := *checkInterval
	defer func() { *checkInterval = orig }()

	*checkInterval = 2 * time.Second
	config := boundRestConfig(&rest.Config{})
	if got, want := config.Timeout, time.Second; got != want {
		t.Errorf("boundRestConfig Timeout = %v, want %v", got, want)
	}
}

func TestRedisOptionsBoundSocketTimeouts(t *testing.T) {
	defer configureTestFlags()()
	orig := *checkInterval
	defer func() { *checkInterval = orig }()

	*checkInterval = time.Second
	options, err := redisOptionsFromFlags()
	if err != nil {
		t.Fatalf("redisOptionsFromFlags: %v", err)
	}

	want := 500 * time.Millisecond
	if options.DialTimeout != want {
		t.Errorf("DialTimeout = %v, want %v", options.DialTimeout, want)
	}
	if options.ReadTimeout != want {
		t.Errorf("ReadTimeout = %v, want %v", options.ReadTimeout, want)
	}
	if options.WriteTimeout != want {
		t.Errorf("WriteTimeout = %v, want %v", options.WriteTimeout, want)
	}
}
