package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
)

// Resource requests/limits check: neither example Deployment declared a
// resources block, so every pod ran at the BestEffort QoS class - no request
// for the scheduler to score or reserve, no limit to stop one container from
// taking the node. The manifests are what README "Kubernetes deployment" tells
// users to apply, so they taught that shape. Nothing caught it: CI's
// "Validate manifests" step runs kubeconform, whose schema accepts an absent
// resources block, and the other readers in this package extract container
// args, probes, securityContext and the RBAC chain, never resources.
//
// The readers below expose the declared requests and limits of every container
// of manifests/*.yaml to the tests in manifest_resources_test.go
// (TestManifestContainersDeclareResourceRequestsAndLimits and its fixtures),
// which require requests and limits for
// cpu and memory on every example container, a limit that is not below its
// request, and a pod QoS class better than BestEffort. The declared pair is
// documented in README "Resource sizing".
//
// The requested quantities are namespace-qualified strings in the API
// ("resources.requests.cpu"), so they are decoded as text and parsed with the
// API's own quantity parser: a typo like "memory: 128 MB" is then reported as
// a problem by name instead of being silently dropped by the decoder.

// containerResourceRequirements is one container's resources block: the
// requests and limits maps of namespace-qualified quantities.
type containerResourceRequirements struct {
	Requests map[string]string `json:"requests"`
	Limits   map[string]string `json:"limits"`
}

// containerResourceSpec is one container of a pod spec with the resources it
// declares, together with the manifest file it came from so a failure names
// the right container instead of the test.
type containerResourceSpec struct {
	Manifest  string
	Name      string
	Image     string
	Resources containerResourceRequirements
}

// requiredResourceNames are the resources every example container must declare
// twice, as a request and as a limit. cpu and memory decide the pod's QoS
// class: a container without them for either is what the finding reported.
var requiredResourceNames = []string{"cpu", "memory"}

// QoS class names, as the API computes them for a pod.
const (
	qosClassGuaranteed = "Guaranteed"
	qosClassBurstable  = "Burstable"
	qosClassBestEffort = "BestEffort"
	qosClassUnknown    = "Unknown"
)

// containerResourceSpecs returns every container of every Pod spec under dir
// with its resources block. Unlike sidecarContainers (which selects the
// containers running this binary) it returns the co-located redis containers
// too: the QoS class is a property of the whole pod, and the finding is about
// every container in the example Deployments.
func containerResourceSpecs(dir string) ([]containerResourceSpec, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var containers []containerResourceSpec
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := filepath.Ext(entry.Name())
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		doc, err := readPodSpecManifest(path)
		if err != nil {
			return nil, fmt.Errorf("parse manifest: %w", err)
		}
		for _, c := range doc.Spec.Template.Spec.Containers {
			containers = append(containers, containerResourceSpec{
				Manifest:  entry.Name(),
				Name:      c.Name,
				Image:     c.Image,
				Resources: c.Resources,
			})
		}
	}
	return containers, nil
}

// parseResourceQuantity parses one declared quantity with the API's parser, so
// the check accepts exactly what the API accepts ("100m", "128Mi", "0.1") and
// rejects what it rejects ("128 MB", "one", "").
func parseResourceQuantity(value string) (resource.Quantity, bool) {
	if strings.TrimSpace(value) == "" {
		return resource.Quantity{}, false
	}
	quantity, err := resource.ParseQuantity(value)
	if err != nil {
		return resource.Quantity{}, false
	}
	return quantity, true
}

// containerResourceProblems returns why one container's resources block does
// not declare the pair the example manifests are held to, or nil when it does.
// Every message names the manifest, the container, the field and what goes
// wrong without it: a pod without a request is scheduled as BestEffort and
// evicted first, and a container without a limit can consume its node.
func containerResourceProblems(c containerResourceSpec) []string {
	requests, limits := c.Resources.Requests, c.Resources.Limits
	if len(requests) == 0 && len(limits) == 0 {
		return []string{fmt.Sprintf("%s/%s: declares no resources block, so the pod runs at the BestEffort QoS class: with no request the scheduler scores and evicts this pod first under node pressure, and with no limit one container (a Redis replica or the labeler's --check-interval=10s poll loop) can consume the whole node. Declare resources.requests and resources.limits for cpu and memory on every container (README \"Resource sizing\")", c.Manifest, c.Name)}
	}

	var problems []string
	for _, name := range requiredResourceNames {
		request, hasRequest := requests[name]
		limit, hasLimit := limits[name]

		switch {
		case !hasRequest && !hasLimit:
			problems = append(problems, fmt.Sprintf("%s/%s: resources.%s is declared neither as a request nor as a limit: the container has no share the scheduler can reserve or cap (README \"Resource sizing\")", c.Manifest, c.Name, name))
			continue
		case !hasRequest:
			problems = append(problems, fmt.Sprintf("%s/%s: resources.requests.%s is missing (only resources.limits.%s = %q is declared): Kubernetes defaults the request to the limit, so the example would show a container scheduled as if it needs its whole limit. Declare the request explicitly (README \"Resource sizing\")", c.Manifest, c.Name, name, name, limit))
		case !hasLimit:
			problems = append(problems, fmt.Sprintf("%s/%s: resources.limits.%s is missing (only resources.requests.%s = %q is declared): nothing caps what this container may use, so it can consume the node (README \"Resource sizing\")", c.Manifest, c.Name, name, name, request))
		}

		if hasRequest {
			if _, ok := parseResourceQuantity(request); !ok {
				problems = append(problems, fmt.Sprintf("%s/%s: resources.requests.%s = %q is not a Kubernetes quantity (write it as a number, optionally with a suffix such as 100m or 128Mi)", c.Manifest, c.Name, name, request))
			}
		}
		if hasLimit {
			if _, ok := parseResourceQuantity(limit); !ok {
				problems = append(problems, fmt.Sprintf("%s/%s: resources.limits.%s = %q is not a Kubernetes quantity (write it as a number, optionally with a suffix such as 100m or 128Mi)", c.Manifest, c.Name, name, limit))
			}
		}
	}

	// The comparison covers every declared resource, not only cpu and memory:
	// the API rejects a limit below its request whatever the resource is, so an
	// extra resource (ephemeral-storage, for example) is held to the same rule.
	for name, request := range requests {
		limit, hasLimit := limits[name]
		if !hasLimit {
			continue
		}
		requestQuantity, requestOK := parseResourceQuantity(request)
		limitQuantity, limitOK := parseResourceQuantity(limit)
		if requestOK && limitOK && limitQuantity.Cmp(requestQuantity) < 0 {
			problems = append(problems, fmt.Sprintf("%s/%s: resources.limits.%s = %q is below resources.requests.%s = %q, which the API rejects: the request is the amount the scheduler reserves, and the limit has to be at least as large (README \"Resource sizing\")", c.Manifest, c.Name, name, limit, name, request))
		}
	}

	return problems
}

// podQoSClass returns the QoS class the API assigns to a pod whose containers
// carry the given resources: Guaranteed when every container declares cpu and
// memory limits and no request differs from its limit (an absent request
// defaults to the limit, as the API does), BestEffort when no container
// declares any request or limit at all, and Burstable otherwise. An empty
// container list is Unknown rather than BestEffort, so a reader that found no
// container cannot pass as a pod that declared nothing.
func podQoSClass(containers []containerResourceSpec) string {
	if len(containers) == 0 {
		return qosClassUnknown
	}

	anyRequestOrLimit := false
	guaranteed := true
	for _, c := range containers {
		requests, limits := c.Resources.Requests, c.Resources.Limits
		if len(requests) == 0 && len(limits) == 0 {
			guaranteed = false
			continue
		}
		anyRequestOrLimit = true

		for _, name := range requiredResourceNames {
			if _, hasLimit := limits[name]; !hasLimit {
				guaranteed = false
			}
		}
		for name, limit := range limits {
			if request, hasRequest := requests[name]; hasRequest && !resourceQuantitiesEqual(request, limit) {
				guaranteed = false
			}
		}
		for name := range requests {
			if _, hasLimit := limits[name]; !hasLimit {
				guaranteed = false
			}
		}
	}

	if !anyRequestOrLimit {
		return qosClassBestEffort
	}
	if guaranteed {
		return qosClassGuaranteed
	}
	return qosClassBurstable
}

// resourceQuantitiesEqual compares two declared quantities by value, so the
// same amount written differently ("100m" and "0.1") still counts as equal.
// Quantities that do not parse fall back to text equality; the manifests are
// read from the API's point of view here, and an unparseable quantity is
// reported as a problem by containerResourceProblems rather than by this
// comparison.
func resourceQuantitiesEqual(a, b string) bool {
	left, leftOK := parseResourceQuantity(a)
	right, rightOK := parseResourceQuantity(b)
	if !leftOK || !rightOK {
		return a == b
	}
	return left.Cmp(right) == 0
}
