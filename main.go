package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// version is stamped at build time with -ldflags "-X main.version=<release
// tag>"; the release workflow passes the tag to the Docker build through
// --build-arg VERSION. Unstamped (local or CI test) builds report "dev".
var version = "dev"

var (
	redisAddr          = flag.String("redis-addr", "localhost:6379", "Redis server address")
	redisPassword      = flag.String("redis-password", "", "Redis password")
	redisTLS           = flag.Bool("redis-tls", false, "Enable TLS for Redis connection")
	redisTLSSkipVerify = flag.Bool("redis-tls-skip-verify", false, "Skip TLS certificate verification for Redis connection")
	labelKey           = flag.String("label-key", "redis-role", "Kubernetes label key to set")
	labelValue         = flag.String("label-value", "master", "Kubernetes label value for master")
	podName            = flag.String("pod-name", "", "Pod name to label (defaults to HOSTNAME env var)")
	podNamespace       = flag.String("pod-namespace", "", "Pod namespace (defaults to POD_NAMESPACE env var)")
	checkInterval      = flag.Duration("check-interval", 10*time.Second, "Interval to check Redis role")
	pollTimeout        = flag.Duration("poll-timeout", 5*time.Second, "Deadline for one poll iteration (ROLE plus label update); a deadline that expires reports the sidecar unhealthy")
	apiTimeout         = flag.Duration("api-timeout", 5*time.Second, "Timeout for a single Kubernetes API request (rest.Config.Timeout)")
	healthPort         = flag.String("health-port", "8080", "Port for health check HTTP server")
	showVersion        = flag.Bool("version", false, "Print the build version and exit")
)

var (
	healthStatus struct {
		mu                  sync.RWMutex
		consecutiveFailures int
		isHealthy           bool
	}
)

func main() {
	flag.Parse()

	if *showVersion {
		fmt.Printf("redis-master-label %s\n", version)
		return
	}

	if err := validateCheckInterval(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	if err := validateTimeouts(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	if *podName == "" {
		*podName = os.Getenv("HOSTNAME")
		if *podName == "" {
			fmt.Fprintf(os.Stderr, "pod-name must be set or HOSTNAME env var must be available\n")
			os.Exit(1)
		}
	}

	if *podNamespace == "" {
		*podNamespace = os.Getenv("POD_NAMESPACE")
		if *podNamespace == "" {
			*podNamespace = "default"
		}
	}

	config, err := rest.InClusterConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to get in-cluster config: %v\n", err)
		os.Exit(1)
	}

	// Bound every Kubernetes API request (Pods.Get and Pods.Update): a
	// half-open TCP connection to the API server satisfies the socket-level
	// timeouts and then never answers, and without rest.Config.Timeout such a
	// request hangs until the process dies.
	config, err = boundedConfig(config, *apiTimeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create Kubernetes client: %v\n", err)
		os.Exit(1)
	}

	redisOptions, err := redisOptionsFromFlags()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	rdb := redis.NewClient(redisOptions)
	defer rdb.Close()

	// Bind the health check HTTP server before entering the main loop so a
	// bind failure (port taken, invalid --health-port) aborts startup instead
	// of silently running without /healthz.
	errCh, err := startHealthServer(*healthPort)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to bind health check server on port %s: %v\n", *healthPort, err)
		os.Exit(1)
	}

	// Initialize health status as healthy
	healthStatus.mu.Lock()
	healthStatus.isHealthy = true
	healthStatus.consecutiveFailures = 0
	healthStatus.mu.Unlock()

	for {
		// Surface an unexpected health server death without blocking the loop.
		select {
		case err := <-errCh:
			if err != nil && err != http.ErrServerClosed {
				fmt.Fprintf(os.Stderr, "health check server error: %v\n", err)
				os.Exit(1)
			}
		default:
		}

		// Each iteration is bounded by --poll-timeout (and each API request by
		// --api-timeout), so a stalled Redis socket or API call cannot freeze
		// the loop and leave /healthz at its last 200: see runPollIteration.
		if err := runPollIteration(rdb, clientset); err != nil {
			fmt.Fprintf(os.Stderr, "error checking Redis role: %v\n", err)
		}
		time.Sleep(*checkInterval)
	}
}

// validateRedisTLSFlags rejects a nonsensical TLS flag combination:
// --redis-tls-skip-verify without --redis-tls. Without TLS the connection is
// plaintext, so the requested certificate-skip never applies and would leave
// the operator believing a setting is in force that does nothing.
func validateRedisTLSFlags() error {
	if *redisTLSSkipVerify && !*redisTLS {
		return fmt.Errorf("--redis-tls-skip-verify requires --redis-tls: without --redis-tls the connection is plaintext and the flag has no effect")
	}
	return nil
}

// redisOptionsFromFlags builds the redis.Options for the client from the
// --redis-* flags (and the REDIS_PASSWORD environment fallback). It is the
// single place a TLS config is attached: a TLS config with
// InsecureSkipVerify matching --redis-tls-skip-verify only when --redis-tls
// is set, never otherwise (README "Configuration"):
//
//	--redis-tls=false: TLSConfig stays nil even with
//	                   --redis-tls-skip-verify set, because the connection is
//	                   plaintext.
//	--redis-tls=true:  TLSConfig carries InsecureSkipVerify from
//	                   --redis-tls-skip-verify.
//
// Startup config invalid in combination (--redis-tls-skip-verify without
// --redis-tls) is rejected instead of applied silently.
func redisOptionsFromFlags() (*redis.Options, error) {
	if err := validateRedisTLSFlags(); err != nil {
		return nil, err
	}

	options := &redis.Options{
		Addr:     *redisAddr,
		Password: resolveRedisPassword(),
	}

	if *redisTLS {
		options.TLSConfig = &tls.Config{
			InsecureSkipVerify: *redisTLSSkipVerify,
		}
	}

	return options, nil
}

// validateCheckInterval rejects a non-positive --check-interval. time.Sleep
// returns immediately for such values, so the poll loop would busy-spin,
// hammering the Redis server and the Kubernetes API server.
func validateCheckInterval() error {
	if *checkInterval <= 0 {
		return fmt.Errorf("--check-interval must be positive, got %v", *checkInterval)
	}
	return nil
}

// validateTimeouts rejects non-positive --poll-timeout and --api-timeout. A
// deadline of zero or less makes context.WithTimeout expire at once, so every
// iteration would fail and /healthz would answer 503 permanently, while a
// rest.Config.Timeout of zero means no request timeout at all: the stalled call
// the deadline exists to bound would hang for ever again.
func validateTimeouts() error {
	if *pollTimeout <= 0 {
		return fmt.Errorf("--poll-timeout must be positive, got %v", *pollTimeout)
	}
	if *apiTimeout <= 0 {
		return fmt.Errorf("--api-timeout must be positive, got %v", *apiTimeout)
	}
	return nil
}

// boundedConfig applies rest.Config.Timeout to cfg and returns it, so every
// request the derived client makes fails at the deadline instead of hanging on
// a connection that never answers. A non-positive timeout is refused rather
// than applied: client-go reads a zero Timeout as "no timeout", which is the
// configuration this bounds.
func boundedConfig(cfg *rest.Config, timeout time.Duration) (*rest.Config, error) {
	if timeout <= 0 {
		return nil, fmt.Errorf("--api-timeout must be positive, got %v (client-go reads zero as no request timeout)", timeout)
	}
	cfg.Timeout = timeout
	return cfg, nil
}

// resolveRedisPassword returns the credential for the Redis connection. The
// explicit --redis-password flag wins; when it is empty, the REDIS_PASSWORD
// environment variable is used. Environment is the preferred way to supply
// the credential in Kubernetes (valueFrom.secretKeyRef on the env var):
// a password passed on the command line lands in the pod spec and in
// /proc/<pid>/cmdline, both readable by anyone who can inspect the pod.
func resolveRedisPassword() string {
	if *redisPassword != "" {
		return *redisPassword
	}
	return os.Getenv("REDIS_PASSWORD")
}

// roleQuerier answers context-bound Redis commands (ROLE) without requiring a
// live connection, so the ROLE handling stays testable with a narrow stub.
type roleQuerier interface {
	Do(ctx context.Context, args ...interface{}) *redis.Cmd
}

// pollDeadline bounds one poll call with --poll-timeout. A parent that already
// carries a deadline keeps it, so the calls of one iteration can share a single
// deadline; a caller that hands in a context without one (context.Background(),
// as the reviewed code did) gets the deadline applied here instead of leaving
// the call unbounded. A Redis socket or API call that stops answering then
// fails at the deadline instead of blocking the poll loop for ever.
func pollDeadline(parent context.Context) (context.Context, context.CancelFunc) {
	if _, ok := parent.Deadline(); ok {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, *pollTimeout)
}

// checkAndLabel reads the Redis ROLE reply and applies or removes the label.
// Both the ROLE command and the Pods.Get/Pods.Update calls run under the poll
// deadline: without it, a half-open connection to Redis or to the API server
// leaves this call blocked indefinitely, so no label is applied or removed and
// the health endpoint keeps its stale 200.
func checkAndLabel(ctx context.Context, rdb roleQuerier, clientset kubernetes.Interface) error {
	ctx, cancel := pollDeadline(ctx)
	defer cancel()

	role, err := rdb.Do(ctx, "ROLE").Result()
	if err != nil {
		return fmt.Errorf("failed to execute ROLE command: %w", err)
	}

	roleStr, err := roleFromResponse(role)
	if err != nil {
		return err
	}

	return applyLabel(ctx, clientset, roleStr)
}

func roleFromResponse(role interface{}) (string, error) {
	roleArray, ok := role.([]interface{})
	if !ok || len(roleArray) == 0 {
		return "", fmt.Errorf("unexpected ROLE response format")
	}

	roleStr, ok := roleArray[0].(string)
	if !ok {
		return "", fmt.Errorf("unexpected ROLE response format")
	}
	return roleStr, nil
}

func applyLabel(ctx context.Context, clientset kubernetes.Interface, roleStr string) error {
	pod, err := clientset.CoreV1().Pods(*podNamespace).Get(ctx, *podName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get pod: %w", err)
	}

	if pod.Labels == nil {
		pod.Labels = make(map[string]string)
	}

	currentValue, exists := pod.Labels[*labelKey]

	if roleStr == "master" {
		// Label the pod if it's master
		if !exists || currentValue != *labelValue {
			pod.Labels[*labelKey] = *labelValue
			_, err = clientset.CoreV1().Pods(*podNamespace).Update(ctx, pod, metav1.UpdateOptions{})
			if err != nil {
				return fmt.Errorf("failed to update pod label: %w", err)
			}
			fmt.Printf("Labeled pod %s/%s with %s=%s\n", *podNamespace, *podName, *labelKey, *labelValue)
		}
	} else {
		// Remove the label whenever the pod is no longer master and the key is
		// present, whatever value it currently holds. A value-scoped check would
		// leave a foreign or stale value under the key, so a Service selecting
		// on that key would keep routing to a pod that is no longer master.
		if exists {
			delete(pod.Labels, *labelKey)
			_, err = clientset.CoreV1().Pods(*podNamespace).Update(ctx, pod, metav1.UpdateOptions{})
			if err != nil {
				return fmt.Errorf("failed to remove pod label: %w", err)
			}
			fmt.Printf("Removed label %s from pod %s/%s (no longer master)\n", *labelKey, *podNamespace, *podName)
		}
	}

	return nil
}

func healthHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		healthStatus.mu.RLock()
		healthy := healthStatus.isHealthy
		healthStatus.mu.RUnlock()

		if healthy {
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, "OK\n")
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, "Unhealthy\n")
		}
	})
	return mux
}

// startHealthServer owns the bind: it returns the net.Listener (owned by the
// caller so Serve errors stay visible) plus a buffered channel reporting a
// fatal Serve error. A bind failure returns an error before main's loop runs.
func startHealthServer(port string) (<-chan error, error) {
	listener, err := net.Listen("tcp", ":"+port)
	if err != nil {
		return nil, fmt.Errorf("failed to bind health check server on port %s: %v", port, err)
	}

	fmt.Printf("Starting health check server on port %s\n", port)
	return serveHealth(listener), nil
}

// serveHealth runs the health server on an already-bound listener and reports
// a fatal Serve error (http.ErrServerClosed excluded) on a buffered channel.
func serveHealth(listener net.Listener) <-chan error {
	errCh := make(chan error, 1)
	server := &http.Server{
		Handler: healthHandler(),
	}

	go func() {
		if serveErr := server.Serve(listener); serveErr != nil && serveErr != http.ErrServerClosed {
			errCh <- serveErr
		}
	}()

	return errCh
}

// healthFailureThreshold is the number of consecutive failed poll iterations
// after which /healthz reports unhealthy.
const healthFailureThreshold = 3

// runPollIteration performs one poll under a single --poll-timeout deadline and
// records the health result of that call. The deadline is what ends a Redis
// socket or API call that stops answering: without it the loop blocks for ever,
// no ROLE runs, no label is applied or removed, and /healthz keeps answering
// 200 from the last successful check, so a liveness probe cannot restart the
// sidecar and readiness keeps the pod in Service endpoints with a stale
// redis-role label.
func runPollIteration(rdb roleQuerier, clientset kubernetes.Interface) error {
	ctx, cancel := pollDeadline(context.Background())
	defer cancel()

	updateHealthStatus(ctx, rdb)
	labelErr := checkAndLabel(ctx, rdb, clientset)

	// A deadline that expired means the iteration did not finish. Waiting for
	// healthFailureThreshold more iterations would keep answering the stale 200
	// of the last successful check exactly while the sidecar is wedged, so the
	// probe is failed at once; the deadline already is the grace period.
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		markUnhealthyOverdue()
	}

	return labelErr
}

// recordHealthFailure counts one failed poll and fails the probe once
// healthFailureThreshold consecutive failures are reached.
func recordHealthFailure() {
	healthStatus.mu.Lock()
	healthStatus.consecutiveFailures++
	if healthStatus.consecutiveFailures >= healthFailureThreshold {
		healthStatus.isHealthy = false
	}
	healthStatus.mu.Unlock()
}

// recordHealthSuccess resets the failure counter and reports healthy.
func recordHealthSuccess() {
	healthStatus.mu.Lock()
	healthStatus.consecutiveFailures = 0
	healthStatus.isHealthy = true
	healthStatus.mu.Unlock()
}

// markUnhealthyOverdue fails the probe immediately when a poll call outran its
// deadline: the deadline is the grace period, and reporting unhealthy at once
// is what turns a stalled Redis or API call into a 503 instead of leaving the
// last 200 in place.
func markUnhealthyOverdue() {
	healthStatus.mu.Lock()
	healthStatus.consecutiveFailures = healthFailureThreshold
	healthStatus.isHealthy = false
	healthStatus.mu.Unlock()
}

// updateHealthStatus runs the ROLE health check under the poll deadline and
// records the outcome. A ROLE that never answers must not leave the failure
// counter untouched: the expired deadline fails the probe at once, because
// otherwise the stale 200 keeps the kubelet from restarting the sidecar.
func updateHealthStatus(ctx context.Context, rdb roleQuerier) {
	ctx, cancel := pollDeadline(ctx)
	defer cancel()

	if _, err := rdb.Do(ctx, "ROLE").Result(); err != nil {
		recordHealthFailure()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			markUnhealthyOverdue()
		}
		return
	}

	// Success - reset failure counter and mark as healthy
	recordHealthSuccess()
}
