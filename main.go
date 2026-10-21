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
	config = backendRestConfig(config)

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

		// One deadline per iteration, derived from --check-interval, bounds
		// every backend call the iteration makes: the Redis ROLE and the pod
		// get/update. Without it a stalled backend (half-open connection,
		// black-holed packets, unresponsive API server) blocks the loop, so
		// updateHealthStatus never counts a failure and /healthz keeps
		// answering 200 while no label is applied or removed - exactly the
		// state the liveness/readiness probes exist to detect.
		checkCtx, cancel := context.WithTimeout(ctx, backendTimeout())

		// Update health status based on Redis connectivity
		updateHealthStatus(checkCtx, rdb)

		checkErr := checkAndLabel(checkCtx, rdb, clientset)
		cancel()
		if checkErr != nil {
			fmt.Fprintf(os.Stderr, "error checking Redis role: %v\n", checkErr)
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
//
// The connection also carries finite timeouts derived from --check-interval
// (backendTimeout). Leaving DialTimeout, ReadTimeout and WriteTimeout at the
// library defaults ties the worst-case cost of one check to a number this
// program does not control: a stalled server then costs several plug
// attempts per check, so a check can outlast the interval and the failures
// the health endpoint counts arrive far slower than the interval the
// probes are tuned to.
func redisOptionsFromFlags() (*redis.Options, error) {
	if err := validateRedisTLSFlags(); err != nil {
		return nil, err
	}

	timeout := backendTimeout()
	options := &redis.Options{
		Addr:         *redisAddr,
		Password:     resolveRedisPassword(),
		DialTimeout:  timeout,
		ReadTimeout:  timeout,
		WriteTimeout: timeout,
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

// minBackendTimeout floors the per-check deadline, so a tiny --check-interval
// still yields a positive timeout instead of an already-expired context.
const minBackendTimeout = time.Millisecond

// backendTimeout is the deadline for the backend calls of one poll iteration:
// the Redis ROLE and the pod get/update. It is half of --check-interval,
// floored at minBackendTimeout, so it is always finite and always leaves half
// the interval for the loop to react, and it never depends on a library
// default. A non-positive --check-interval is rejected at startup; the floor
// keeps this helper safe for callers that build options before that check.
func backendTimeout() time.Duration {
	timeout := *checkInterval / 2
	if timeout < minBackendTimeout {
		return minBackendTimeout
	}
	return timeout
}

// backendRestConfig bounds the Kubernetes REST client: rest.Config.Timeout is
// the http.Client timeout every request through the clientset inherits. The
// in-cluster config sets none, so a stalled API server response blocks the
// call (and with it the poll loop) until the TCP connection dies on its own,
// which for a half-open connection is never.
func backendRestConfig(config *rest.Config) *rest.Config {
	if config == nil {
		return nil
	}
	config.Timeout = backendTimeout()
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
	// The ROLE check carries its own deadline so a stalled Redis fails the
	// check and counts toward consecutiveFailures instead of blocking here
	// forever. A parent deadline (the per-iteration one main sets) still
	// wins when it is sooner.
	checkCtx, cancel := context.WithTimeout(ctx, backendTimeout())
	_, err := rdb.Do(checkCtx, "ROLE").Result()
	cancel()
	if err != nil {
		healthStatus.mu.Lock()
		healthStatus.consecutiveFailures++
		if healthStatus.consecutiveFailures >= 3 {
			healthStatus.isHealthy = false
		}
		healthStatus.mu.Unlock()
		return
	}

	// Success - reset failure counter and mark as healthy
	healthStatus.mu.Lock()
	healthStatus.consecutiveFailures = 0
	healthStatus.isHealthy = true
	healthStatus.mu.Unlock()
}
