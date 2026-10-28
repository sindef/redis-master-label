package main

// Pod-level endpoint check for the Service the replicas replicate from.
//
// manifests/redis-leader.yaml runs the single Redis master together with the
// redis-master-label sidecar in one pod, and
// manifests/redis-leader-service.yaml selects that pod with
// `selector: app: redis-leader`. The sidecar's readiness therefore decides
// whether the Redis master is reachable: Kubernetes marks a pod Ready only when
// every container is Ready, so a labeler readinessProbe on /healthz couples
// Redis availability to the labeler's own Redis client. After three consecutive
// ROLE failures (main.go updateHealthStatus sets isHealthy=false) the probe
// answers 503, the kubelet marks the whole pod NotReady, the API server removes
// the only endpoint of the redis-leader Service, and the three replicas in
// manifests/deployment-example.yaml - which replicate from
// redis-leader.default.svc.cluster.local:6379 - stall even though redis-server
// is up and answering.
//
// The reviewed manifests declared a readinessProbe on that sidecar, so a
// transient labeler-side Redis hiccup produced the outage shape README's
// "Kubernetes deployment" section otherwise attributes only to the Recreate
// rollout. Nothing asserted the coupling: TestManifestSidecarsDeclareHealthProbes
// accepted either probe, so dropping the readinessProbe stayed green and adding
// it back stayed green too.
//
// The choice this repository makes is: a labeler in a pod that backs a Service
// keeps only the livenessProbe, which restarts a wedged labeler without changing
// pod readiness. serviceBackedLabelers reads every Service selector and every
// Deployment pod template under manifests/, matches the two by label, and
// labelerEndpointProblem reports a readinessProbe on a labeler whose pod backs a
// Service, or a missing livenessProbe. TestManifestServiceBackedLabelerKeeps-
// ReadinessOutOfPodEndpoint asserts both against the shipped manifests and its
// regression fixtures reproduce the reviewed coupling.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// serviceKind is the Service document kind these checks select, so a manifest
// without a pod template is not misread as a Deployment.
const serviceKind = "Service"

// manifestSelectorDocument is the Service subset these checks read: its name and
// pod selector. A Service without a selector has no pod endpoints, so it cannot
// gate anything.
type manifestSelectorDocument struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Selector map[string]string `json:"selector"`
	} `json:"spec"`
}

// labelerProbeContainer is one container of a Deployment pod template, reduced
// to the identity and the probes that decide whether its readiness can gate the
// pod's Service endpoint.
type labelerProbeContainer struct {
	Name           string          `json:"name"`
	Image          string          `json:"image"`
	LivenessProbe  *containerProbe `json:"livenessProbe"`
	ReadinessProbe *containerProbe `json:"readinessProbe"`
}

// labelerPodTemplate is one Deployment under manifests/ with its pod labels and
// every container, decoded so a Service selector can be matched against the
// labels the pod actually carries.
type labelerPodTemplate struct {
	Manifest   string
	Deployment string
	Labels     map[string]string
	Containers []labelerProbeContainer
}

// manifestLabelerDocument is the Deployment subset serviceBackedLabelers
// decodes.
type manifestLabelerDocument struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Template struct {
			Metadata struct {
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
			Spec struct {
				Containers []labelerProbeContainer `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
}

// serviceBackedLabeler is one labeler container whose pod labels match at least
// one Service selector, together with the probes it declares and the names of
// every matching Service, so a failure names the endpoint that a 503 would
// remove.
type serviceBackedLabeler struct {
	Manifest   string
	Deployment string
	Container  string
	Services   []string
	Liveness   *containerProbe
	Readiness  *containerProbe
}

// key identifies the container the way sidecarHealthWiring reports it, so the
// shipped probe test can ask whether a given wiring backs a Service.
func (l serviceBackedLabeler) key() string {
	return l.Manifest + "/" + l.Container
}

// selectorMatchesLabels returns whether a Service selector selects a pod with
// the given labels. Kubernetes requires every selector key to match; an empty
// selector selects no pods, which is why it is rejected here instead of matching
// everything.
func selectorMatchesLabels(selector, labels map[string]string) bool {
	if len(selector) == 0 {
		return false
	}
	for key, want := range selector {
		if labels[key] != want {
			return false
		}
	}
	return true
}

// serviceBackedLabelers reads every Service selector and every Deployment under
// dir and returns one entry per labeler container whose pod template labels
// match a Service selector. It walks the same manifest set (and tolerates the
// same non-Pod-spec documents) as the other manifest checks. A labeler of a
// Deployment no Service selects is not returned: its readiness gates no
// endpoint the repository defines. An unreadable directory is an error, so a
// renamed or emptied manifests/ cannot pass by checking nothing.
func serviceBackedLabelers(dir string) ([]serviceBackedLabeler, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}

	var (
		services    []manifestSelectorDocument
		deployments []labelerPodTemplate
	)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := filepath.Ext(entry.Name())
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		jsonData, err := utilyaml.ToJSON(data)
		if err != nil {
			return nil, fmt.Errorf("%s: YAML to JSON: %w", path, err)
		}

		// The kind is read first because a Deployment's own spec.selector is a
		// LabelSelector object, not the map of pod labels a Service carries.
		var header struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(jsonData, &header); err != nil {
			return nil, fmt.Errorf("%s: JSON decode: %w", path, err)
		}

		var service manifestSelectorDocument
		if header.Kind == serviceKind {
			if err := json.Unmarshal(jsonData, &service); err != nil {
				return nil, fmt.Errorf("%s: JSON decode: %w", path, err)
			}
			services = append(services, service)
			continue
		}

		var deployment manifestLabelerDocument
		if err := json.Unmarshal(jsonData, &deployment); err != nil {
			return nil, fmt.Errorf("%s: JSON decode: %w", path, err)
		}
		if deployment.Kind != deploymentKind {
			continue
		}
		deployments = append(deployments, labelerPodTemplate{
			Manifest:   entry.Name(),
			Deployment: deployment.Metadata.Name,
			Labels:     deployment.Spec.Template.Metadata.Labels,
			Containers: deployment.Spec.Template.Spec.Containers,
		})
	}

	var labelers []serviceBackedLabeler
	for _, deployment := range deployments {
		var matched []string
		for _, service := range services {
			if selectorMatchesLabels(service.Spec.Selector, deployment.Labels) {
				matched = append(matched, service.Metadata.Name)
			}
		}
		if len(matched) == 0 {
			continue
		}
		sort.Strings(matched)
		for _, container := range deployment.Containers {
			if !strings.Contains(container.Image, sidecarImage) {
				continue
			}
			labelers = append(labelers, serviceBackedLabeler{
				Manifest:   deployment.Manifest,
				Deployment: deployment.Deployment,
				Container:  container.Name,
				Services:   matched,
				Liveness:   container.LivenessProbe,
				Readiness:  container.ReadinessProbe,
			})
		}
	}
	return labelers, nil
}

// labelerEndpointProblem returns why a labeler container's probes would gate or
// fail to recover the Service its pod backs, or nil when the wiring is the one
// this repository chose. A readinessProbe is the reviewed defect: it makes the
// master's Service membership depend on the labeler, so a 503 from /healthz (three
// consecutive ROLE failures, or a dead health listener) removes the only endpoint
// the replicas replicate from while Redis is healthy. The livenessProbe has to
// stay: it restarts a wedged labeler and does not touch readiness.
func labelerEndpointProblem(l serviceBackedLabeler) error {
	where := fmt.Sprintf("%s/%s/%s", l.Manifest, l.Deployment, l.Container)
	services := strings.Join(l.Services, ", ")
	if l.Readiness != nil {
		return fmt.Errorf("%s: the labeler declares a readinessProbe, but its pod labels match the selector of Service %s, so the pod is an endpoint of that Service and the labeler's own health decides whether it stays one. Kubernetes marks a pod Ready only when every container is Ready, and the labeler answers 503 on %s after three consecutive ROLE failures (main.go updateHealthStatus), so a labeler-side Redis hiccup marks the whole pod NotReady, removes every endpoint of %s and stalls each replica that replicates from it, even though redis-server is up: drop the readinessProbe and keep the livenessProbe, which restarts a wedged labeler without gating pod readiness", where, services, healthPath, services)
	}
	if l.Liveness == nil {
		return fmt.Errorf("%s: the labeler declares no livenessProbe, so nothing restarts it when it wedges even though its pod is an endpoint of Service %s: keep the livenessProbe polling %s", where, services, healthPath)
	}
	return nil
}

// labelerEndpointProblems returns one message per service-backed labeler whose
// probes are wrong, plus the number of labelers checked, so a test fails when the
// scan reads nothing instead of passing on an empty list.
func labelerEndpointProblems(dir string) ([]string, int, error) {
	labelers, err := serviceBackedLabelers(dir)
	if err != nil {
		return nil, 0, err
	}
	var problems []string
	for _, labeler := range labelers {
		if err := labelerEndpointProblem(labeler); err != nil {
			problems = append(problems, err.Error())
		}
	}
	return problems, len(labelers), nil
}
