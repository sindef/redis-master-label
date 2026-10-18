package main

import (
	"context"
	"crypto/tls"
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
	checkTimeout       = flag.Duration("check-timeout", 5*time.Second, "Deadline for one poll iteration (Redis ROLE, pod get/update)")
	healthPort         = flag.String("health-port", "8080", "Port for health check HTTP server")
	showVersion        = flag.Bool("version", false, "Print the build version and exit")
)

var (
	healthStatus struct {
		mu                  sync.RWMutex
		consecutiveFailures int
		isHealthy           bool
		// iterationStalled records that the most recent poll iteration was cut
		// short by its --check-timeout deadline. While it is set, a successful
		// ROLE reply must not flip the endpoint back to 200: the iteration that
		// just started has not finished either, so the sidecar is still not
		// labelling anything.
		iterationStalled bool
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

	if err := validateCheckTimeout(); err != nil {
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

	// The clientset's requests carry the same deadline as one poll iteration:
	// rest.Config has no timeout by default, so a half-open TCP connection to
	// the API server would leave a pod get or update pending forever.
	clientset, err := kubernetes.NewForConfig(withRequestTimeout(config, *checkTimeout))
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

	// The loop's context is the process lifetime: it is never cancelled, so the
	// bound that keeps a stalled Redis or API call from freezing the loop is the
	// per-iteration deadline pollIteration applies.
	ctx := context.Background()

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

		// One bounded iteration: the ROLE query that feeds /healthz and the
		// label check (ROLE, pod get, pod update) share the --check-timeout
		// deadline, and a deadline that expires marks the sidecar unhealthy
		// instead of leaving /healthz on the last successful 200.
		if err := pollIteration(ctx, rdb, clientset); err != nil {
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

// validateCheckTimeout rejects a non-positive --check-timeout. Such a deadline
// expires before the iteration issues its first ROLE call, so every iteration
// would be reported as stalled: the sidecar would answer 503 and its liveness
// probe would restart it while Redis and the API server are perfectly healthy.
func validateCheckTimeout() error {
	if *checkTimeout <= 0 {
		return fmt.Errorf("--check-timeout must be positive, got %v", *checkTimeout)
	}
	return nil
}

// withRequestTimeout applies timeout as the client-go per-request timeout and
// returns config. rest.Config carries no timeout by default, so a half-open TCP
// connection to the API server - the connection is accepted and then goes
// silent - leaves a pod get or update pending with no error to report. The
// per-iteration context deadline bounds the same calls; this timeout bounds the
// HTTP request itself, so it also covers a call made from a code path that
// carries no deadline.
func withRequestTimeout(config *rest.Config, timeout time.Duration) *rest.Config {
	config.Timeout = timeout
	return config
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

// checkAndLabel reads the Redis ROLE reply and applies or removes the label.
func checkAndLabel(ctx context.Context, rdb roleQuerier, clientset kubernetes.Interface) error {
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

func updateHealthStatus(ctx context.Context, rdb roleQuerier) {
	_, err := rdb.Do(ctx, "ROLE").Result()
	if err != nil {
		healthStatus.mu.Lock()
		healthStatus.consecutiveFailures++
		if healthStatus.consecutiveFailures >= 3 {
			healthStatus.isHealthy = false
		}
		healthStatus.mu.Unlock()
		return
	}

	// Success - reset failure counter and mark as healthy. A stalled iteration
	// keeps the endpoint unhealthy until an iteration actually runs through:
	// while the deadline is the reason the label was not applied, a successful
	// ROLE reply is not evidence that the sidecar is doing its job.
	healthStatus.mu.Lock()
	healthStatus.consecutiveFailures = 0
	if !healthStatus.iterationStalled {
		healthStatus.isHealthy = true
	}
	healthStatus.mu.Unlock()
}

// pollIteration runs one poll iteration under its own --check-timeout deadline.
// The deadline covers the ROLE query that feeds /healthz and the whole label
// check (ROLE again, pod get, pod update), because nothing else bounds them:
// client-go sets no request timeout at all, and the Redis client's socket-level
// timeouts (a 3s read timeout with up to 3 retries by default) are defeated by a
// peer that keeps the connection open and never answers, whether a half-open TCP
// connection or a stalled proxy. Such an iteration would never return: /healthz
// would keep answering 200 from the last successful check, the kubelet's
// liveness probe would never restart the sidecar, and readiness would keep the
// pod in Service endpoints with a stale redis-role label.
//
// An iteration cut short by the deadline is reported unhealthy immediately
// rather than through updateHealthStatus's three-consecutive-failures threshold:
// a stalled iteration is not one failed check among successful ones, it is the
// loop making no progress at all, and a stuck Redis or API call must turn
// /healthz into 503 as soon as the deadline expires.
func pollIteration(parent context.Context, rdb roleQuerier, clientset kubernetes.Interface) error {
	ctx, cancel := context.WithTimeout(parent, *checkTimeout)
	defer cancel()

	// Redis connectivity first: this keeps the consecutive-failure counter and
	// the recovery behaviour (a successful ROLE clears the count).
	updateHealthStatus(ctx, rdb)

	err := checkAndLabel(ctx, rdb, clientset)

	// An expired context tells the two outcomes apart: err without an expired
	// context is Redis or the API server answering with a failure, while err
	// with an expired context is the iteration being cut short mid-call.
	if err != nil && ctx.Err() != nil {
		recordStalledIteration()
		return fmt.Errorf("poll iteration did not finish within --check-timeout=%v (%v): %w", *checkTimeout, ctx.Err(), err)
	}

	recordCompletedIteration(err == nil)
	return err
}

// recordStalledIteration reports the sidecar unhealthy at once and latches the
// stalled state, so the next iteration's successful ROLE reply cannot clear the
// verdict while the call that applies the label still does not return.
func recordStalledIteration() {
	healthStatus.mu.Lock()
	defer healthStatus.mu.Unlock()
	healthStatus.iterationStalled = true
	healthStatus.isHealthy = false
}

// recordCompletedIteration clears the stalled latch after an iteration returned
// within its deadline. A fully successful iteration also reports healthy again
// immediately, so recovery does not have to wait for another --check-interval
// round (and a restart from the liveness probe) once Redis and the API server
// answer again.
func recordCompletedIteration(succeeded bool) {
	healthStatus.mu.Lock()
	defer healthStatus.mu.Unlock()
	healthStatus.iterationStalled = false
	if succeeded {
		healthStatus.isHealthy = true
	}
}
