package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/go-redis/redis/v8"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

var (
	redisAddr     = flag.String("redis-addr", "localhost:6379", "Redis server address")
	redisPassword = flag.String("redis-password", "", "Redis password")
	redisTLS      = flag.Bool("redis-tls", false, "Enable TLS for Redis connection")
	labelKey      = flag.String("label-key", "redis-role", "Kubernetes label key to set")
	labelValue    = flag.String("label-value", "master", "Kubernetes label value for master")
	podName       = flag.String("pod-name", "", "Pod name to label (defaults to HOSTNAME env var)")
	podNamespace  = flag.String("pod-namespace", "", "Pod namespace (defaults to POD_NAMESPACE env var)")
	checkInterval = flag.Duration("check-interval", 10*time.Second, "Interval to check Redis role")
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
			InsecureSkipVerify: false,
		}
	}

	rdb := redis.NewClient(redisOptions)
	defer rdb.Close()

	ctx := context.Background()

	for {
		if err := checkAndLabel(ctx, rdb, clientset); err != nil {
			fmt.Fprintf(os.Stderr, "error checking Redis role: %v\n", err)
		}
		time.Sleep(*checkInterval)
	}
}

func checkAndLabel(ctx context.Context, rdb *redis.Client, clientset *kubernetes.Clientset) error {
	role, err := rdb.Do(ctx, "ROLE").Result()
	if err != nil {
		return fmt.Errorf("failed to execute ROLE command: %w", err)
	}

	roleArray, ok := role.([]interface{})
	if !ok || len(roleArray) == 0 {
		return fmt.Errorf("unexpected ROLE response format")
	}

	roleStr, ok := roleArray[0].(string)
	if !ok {
		return fmt.Errorf("unexpected ROLE response format")
	}

	if roleStr == "master" {
		pod, err := clientset.CoreV1().Pods(*podNamespace).Get(ctx, *podName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to get pod: %w", err)
		}

		if pod.Labels == nil {
			pod.Labels = make(map[string]string)
		}

		currentValue, exists := pod.Labels[*labelKey]
		if !exists || currentValue != *labelValue {
			pod.Labels[*labelKey] = *labelValue
			_, err = clientset.CoreV1().Pods(*podNamespace).Update(ctx, pod, metav1.UpdateOptions{})
			if err != nil {
				return fmt.Errorf("failed to update pod label: %w", err)
			}
			fmt.Printf("Labeled pod %s/%s with %s=%s\n", *podNamespace, *podName, *labelKey, *labelValue)
		}
	}

	return nil
}
