package main

import (
	"context"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

// The reviewed defect: main built one ctx := context.Background() and passed it
// to updateHealthStatus and checkAndLabel, and the clientset came from a
// rest.Config without a timeout, so a Redis socket or API server that stopped
// answering left the poll iteration blocked for ever: no ROLE ran, no label was
// applied or removed, and /healthz kept answering the stale 200 of the last
// successful check, which keeps the liveness probe from restarting the sidecar.
//
// The tests below drive exactly those two functions with a stub that blocks
// until the context deadline. Every existing fixture (stubRedis, the fake
// clientset) answers immediately, so it could never observe the freeze: against
// the unbounded code the calls below never return and the watchdog fails the
// test, while the bounded code ends them at --poll-timeout and reports the
// sidecar unhealthy.

// unboundedCallWatchdog is how long a test waits for a poll call before
// declaring it unbounded. Against the reviewed code the call does not return at
// all: the context it runs under has no deadline to end it.
const unboundedCallWatchdog = 3 * time.Second

// stallDeadline pins --poll-timeout to a value that keeps a bounded call quick,
// set through the flag the operator would use, and returns a function restoring
// the previous value. The flag does not exist on the unbounded code, where the
// calls are expected to run into the watchdog instead.
func stallDeadline(t *testing.T) func() {
	t.Helper()

	f := flag.CommandLine.Lookup("poll-timeout")
	if f == nil {
		return func() {}
	}
	prev := f.Value.String()
	if err := f.Value.Set("100ms"); err != nil {
		t.Fatalf("set --poll-timeout=100ms: %v", err)
	}
	return func() { _ = f.Value.Set(prev) }
}

// healthzStatusCode serves /healthz against the package-level health status and
// returns the status code the kubelet probe would see.
func healthzStatusCode() int {
	rec := httptest.NewRecorder()
	healthHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	return rec.Code
}

// blockingRedis never answers ROLE: Do waits for the caller's context to be
// done and then reports its error. It stands in for a half-open TCP connection
// that passes the socket-level timeouts and then stops making progress.
type blockingRedis struct{}

func (blockingRedis) Do(ctx context.Context, args ...interface{}) *redis.Cmd {
	<-ctx.Done()
	return redis.NewCmdResult(nil, ctx.Err())
}

// blockingPods answers no Pods.Get: it waits for the request context to be done
// and reports its error, standing in for a half-open connection to the API
// server.
type blockingPods struct {
	typedcorev1.PodInterface
}

func (blockingPods) Get(ctx context.Context, name string, opts metav1.GetOptions) (*corev1.Pod, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// blockingCoreV1 serves only Pods(); the other methods come from the embedded
// interface, which the code under test does not call.
type blockingCoreV1 struct {
	typedcorev1.CoreV1Interface
	pods typedcorev1.PodInterface
}

func (c blockingCoreV1) Pods(namespace string) typedcorev1.PodInterface { return c.pods }

// blockingClientset is the kubernetes.Interface implementation whose Pods.Get
// blocks until the request context is done.
type blockingClientset struct {
	kubernetes.Interface
	core typedcorev1.CoreV1Interface
}

func (c blockingClientset) CoreV1() typedcorev1.CoreV1Interface { return c.core }

func newBlockingPodsClientset() kubernetes.Interface {
	return blockingClientset{core: blockingCoreV1{pods: blockingPods{}}}
}

// A ROLE that never answers must end the health check at --poll-timeout and
// fail the probe at once. The call would run for ever on an unbounded context,
// so the assertion is on returning inside the watchdog and on /healthz turning
// 503 instead of keeping the stale 200.
func TestUpdateHealthStatus_StalledRedisEndsAtDeadlineAndFailsProbe(t *testing.T) {
	defer configureTestFlags()()
	defer stallDeadline(t)()

	setHealthStatus(0, true)
	defer setHealthStatus(0, true)

	done := make(chan struct{})
	go func() {
		updateHealthStatus(context.Background(), blockingRedis{})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(unboundedCallWatchdog):
		t.Fatalf("updateHealthStatus did not return %v after a ROLE that never answers: the poll iteration is unbounded, so /healthz would keep answering the stale 200", unboundedCallWatchdog)
	}

	if healthy, failures := healthSnapshot(); healthy {
		t.Errorf("isHealthy = true after the ROLE deadline expired, want false (consecutiveFailures = %d)", failures)
	}
	if status := healthzStatusCode(); status != http.StatusServiceUnavailable {
		t.Fatalf("/healthz status = %d, want %d: a wedged sidecar must not keep answering the stale 200", status, http.StatusServiceUnavailable)
	}
}

// The same failure mode on the ROLE half of the label reconciliation: a Redis
// socket that never answers must end checkAndLabel at the deadline instead of
// blocking the loop before any label is applied or removed.
func TestCheckAndLabel_StalledRedisEndsAtDeadline(t *testing.T) {
	defer configureTestFlags()()
	defer stallDeadline(t)()

	cs := newPodClientset(testPod("test-pod", nil))
	errCh := make(chan error, 1)
	go func() { errCh <- checkAndLabel(context.Background(), blockingRedis{}, cs) }()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("checkAndLabel with a ROLE that never answers returned no error")
		}
		if !strings.Contains(err.Error(), "failed to execute ROLE command") {
			t.Fatalf("error = %v, want the wrapped ROLE failure", err)
		}
	case <-time.After(unboundedCallWatchdog):
		t.Fatalf("checkAndLabel did not return %v after a ROLE that never answers: the label reconciliation is unbounded and blocks the poll loop", unboundedCallWatchdog)
	}
}

// And on the API side: the ROLE check answers, but Pods.Get never does. Without
// the deadline the iteration stops there and the pod keeps its stale redis-role
// label while /healthz stays 200.
func TestCheckAndLabel_StalledPodsGetEndsAtDeadline(t *testing.T) {
	defer configureTestFlags()()
	defer stallDeadline(t)()

	errCh := make(chan error, 1)
	go func() {
		errCh <- checkAndLabel(context.Background(), stubRedis{reply: masterReply()}, newBlockingPodsClientset())
	}()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("checkAndLabel with a Pods.Get that never answers returned no error")
		}
		if !strings.Contains(err.Error(), "failed to get pod") {
			t.Fatalf("error = %v, want the wrapped Pods.Get failure", err)
		}
	case <-time.After(unboundedCallWatchdog):
		t.Fatalf("checkAndLabel did not return %v after a Pods.Get that never answers: the label update is unbounded and blocks the poll loop", unboundedCallWatchdog)
	}
}

// The deadline the fix relies on must be operator-visible and configurable:
// both bounds are registered flags with a positive default. A zero default
// would silently restore the unbounded call the finding reported, since a
// context deadline of zero expires at once and a rest.Config.Timeout of zero
// means no request timeout at all.
func TestPollDeadlineFlagsRegisteredWithPositiveDefault(t *testing.T) {
	for _, name := range []string{"poll-timeout", "api-timeout"} {
		f := flag.CommandLine.Lookup(name)
		if f == nil {
			t.Errorf("--%s is not registered: the poll iteration and its API requests have no bound an operator can set", name)
			continue
		}
		d, err := time.ParseDuration(f.DefValue)
		if err != nil {
			t.Errorf("--%s default = %q, want a Go duration: %v", name, f.DefValue, err)
			continue
		}
		if d <= 0 {
			t.Errorf("--%s default = %q, want a positive deadline", name, f.DefValue)
		}
	}
}

// The bounds only help if main applies them, and main's loop is the location
// the finding reports: the iteration must run under a context with a deadline,
// and the REST config the clientset is derived from must carry a per-request
// timeout, because a client-go request without rest.Config.Timeout has nothing
// of its own to end a half-open connection. Neither is observable from a test
// that cannot run main()'s loop, so the wiring is checked where it is written.
func TestMainAppliesPollDeadlineAndAPITimeout(t *testing.T) {
	data, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	text := string(data)

	if !strings.Contains(text, "context.WithTimeout") {
		t.Error("main.go never derives a context with a deadline: a stalled Redis socket or API call would block the poll loop for ever")
	}
	clientset := strings.Index(text, "kubernetes.NewForConfig(")
	if clientset < 0 {
		t.Fatal("main.go no longer builds a clientset from the REST config")
	}
	timeout := strings.Index(text, "boundedConfig(config, *apiTimeout)")
	if timeout < 0 {
		t.Error("main.go does not apply the request timeout to the REST config the clientset is built from: a half-open connection to the API server has nothing to end its request")
	} else if timeout > clientset {
		t.Errorf("main.go builds the clientset at offset %d before bounding the REST config at offset %d: the API requests carry no client-side bound", clientset, timeout)
	}
}
