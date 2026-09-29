package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
)

// redisDoer answers context-bound commands (ROLE) without requiring a live
// *redis.Client, so the role decision logic stays testable with a narrow fake.
type redisDoer interface {
	Do(ctx context.Context, args ...interface{}) (interface{}, error)
}

// podGetterUpdater reads one pod and writes labels back. A narrow interface
// instead of the concrete *kubernetes.Clientset keeps the label decision logic
// testable without a live Kubernetes API server.
type podGetterUpdater interface {
	GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error)
	UpdatePod(ctx context.Context, pod *corev1.Pod) (*corev1.Pod, error)
}

// clientsetPods adapts *kubernetes.Clientset's typed CoreV1 client to
// podGetterUpdater, so main wiring stays on concrete types.
type clientsetPods struct {
	pods corev1client.CoreV1Interface
}

func (c clientsetPods) GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	return c.pods.Pods(namespace).Get(ctx, name, metav1.GetOptions{})
}

func (c clientsetPods) UpdatePod(ctx context.Context, pod *corev1.Pod) (*corev1.Pod, error) {
	return c.pods.Pods(pod.Namespace).Update(ctx, pod, metav1.UpdateOptions{})
}

// redisClientDoer adapts *redis.Client to redisDoer, since redis.Cmdable has
// no Do method: the concrete client returns *redis.Cmd, while fake
// implementations only need to return the plain reply.
type redisClientDoer struct {
	client *redis.Client
}

func (r redisClientDoer) Do(ctx context.Context, args ...interface{}) (interface{}, error) {
	val, err := r.client.Do(ctx, args...).Result()
	return val, err
}

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
		Password: *redisPassword,
	}

	if *redisTLS {
		redisOptions.TLSConfig = &tls.Config{
			InsecureSkipVerify: *redisTLSSkipVerify,
		}
	}

	rdb := redis.NewClient(redisOptions)
	defer rdb.Close()

	ctx := context.Background()

	pods := clientsetPods{pods: clientset.CoreV1()}
	doer := redisClientDoer{client: rdb}

	// Start health check HTTP server
	go startHealthServer(*healthPort)

	// Initialize health status as healthy
	healthStatus.mu.Lock()
	healthStatus.isHealthy = true
	healthStatus.consecutiveFailures = 0
	healthStatus.mu.Unlock()

	for {
		// Update health status based on Redis connectivity
		updateHealthStatus(ctx, doer)

		if err := checkAndLabel(ctx, doer, pods); err != nil {
			fmt.Fprintf(os.Stderr, "error checking Redis role: %v\n", err)
		}
		time.Sleep(*checkInterval)
	}
}

// roleFromResponse parses the reply of the Redis ROLE command into its role
// string ("master" for a primary, "slave" or "sentinel" otherwise). Keeping the
// parsing isolated makes the master/replica decision testable on its own.
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

func checkAndLabel(ctx context.Context, rdb redisDoer, pods podGetterUpdater) error {
	role, err := rdb.Do(ctx, "ROLE")
	if err != nil {
		return fmt.Errorf("failed to execute ROLE command: %w", err)
	}

	roleStr, err := roleFromResponse(role)
	if err != nil {
		return err
	}

	pod, err := pods.GetPod(ctx, *podNamespace, *podName)
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
			_, err = pods.UpdatePod(ctx, pod)
			if err != nil {
				return fmt.Errorf("failed to update pod label: %w", err)
			}
			fmt.Printf("Labeled pod %s/%s with %s=%s\n", *podNamespace, *podName, *labelKey, *labelValue)
		}
	} else {
		// Remove the label if it exists and the pod is no longer master
		if exists && currentValue == *labelValue {
			delete(pod.Labels, *labelKey)
			_, err = pods.UpdatePod(ctx, pod)
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

func startHealthServer(port string) {
	server := &http.Server{
		Addr:    ":" + port,
		Handler: healthHandler(),
	}

	fmt.Printf("Starting health check server on port %s\n", port)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "health check server error: %v\n", err)
	}
}

func updateHealthStatus(ctx context.Context, rdb redisDoer) {
	_, err := rdb.Do(ctx, "ROLE")
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
