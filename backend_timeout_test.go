package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
	"k8s.io/client-go/rest"
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
