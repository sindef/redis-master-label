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

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create Kubernetes client: %v\n", err)
		os.Exit(1)
	}

	redisOptions := &redis.Options{
		Addr:     *redisAddr,
		Password: resolveRedisPassword(),
	}

	if *redisTLS {
		redisOptions.TLSConfig = &tls.Config{
			InsecureSkipVerify: *redisTLSSkipVerify,
		}
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

		// Update health status based on Redis connectivity
		updateHealthStatus(ctx, rdb)

		if err := checkAndLabel(ctx, rdb, clientset); err != nil {
			fmt.Fprintf(os.Stderr, "error checking Redis role: %v\n", err)
		}
		time.Sleep(*checkInterval)
	}
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

	// Success - reset failure counter and mark as healthy
	healthStatus.mu.Lock()
	healthStatus.consecutiveFailures = 0
	healthStatus.isHealthy = true
	healthStatus.mu.Unlock()
}
