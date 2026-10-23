package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	resourceapi "k8s.io/apimachinery/pkg/api/resource"
)

// Container resource check: neither example Deployment declared a resources
// block, so every container ran unsized. A container with no cpu or memory
// request puts the whole pod in the BestEffort QoS class: the scheduler has no
// size to pack it by and the kubelet evicts it first under node pressure. A
// container with no limit can consume the whole node - one replica's
// redis-server, or the labeler's poll loop (--check-interval, 10s in the
// examples) talking to a stalled Redis, is enough. The two Deployments are what
// README "Kubernetes deployment" tells users to apply, so they taught that
// shape. Nothing caught it: kubeconform validates the schema only, where
// resources are optional, and the Go manifest readers in cmd_manifest_check.go
// decoded command/args, ports, probes and securityContext but no resources.
//
// The reader and check below make the sizing visible to
// TestManifestContainersDeclareResources in resources_manifest_test.go, which
// runs against the shipped manifests and against fixtures of them. They cover
// every container of every Deployment, not only the labeler sidecar: the
// co-located redis container is the one whose memory the kubelet can exhaust
// first.

// containerResources is a container's resources block, reduced to the fields
// the example deployments declare. The quantities stay strings so an invalid
// value reaches the check as written instead of being silently dropped by the
// JSON decoder.
type containerResources struct {
	Requests resourceRequirements `json:"requests"`
	Limits   resourceRequirements `json:"limits"`
}

// resourceRequirements holds the two quantities every example container must
// declare. Only cpu and memory are read: they are what the scheduler packs,
// what the kubelet evicts and OOM-kills on, and what decides the QoS class.
type resourceRequirements struct {
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
}

// value returns the declared quantity of one supported resource name.
func (r resourceRequirements) value(name string) string {
	switch name {
	case "cpu":
		return r.CPU
	case "memory":
		return r.Memory
	}
	return ""
}

// requiredResourceNames are the resources every example container must request
// and limit. Both are required for both halves: a request without a limit lets
// the container grow to the node's size, and a limit without a request leaves
// the pod BestEffort.
var requiredResourceNames = []string{"cpu", "memory"}

// resourcesContainer is one container of one example Deployment with the
// resources block it declares.
type resourcesContainer struct {
	Manifest  string
	Name      string
	Resources *containerResources
}

// deploymentContainers returns every container of every Deployment document
// across manifests/*.yaml, plus the number of Deployment documents seen so a
// caller can tell "every container is sized" from "no Deployment was read".
// Unlike the sidecar readers it does not filter by image: sizing is a property
// of every container in the pod, so the redis containers count too.
func deploymentContainers(dir string) (containers []resourcesContainer, deployments int, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, fmt.Errorf("read %s: %w", dir, err)
	}
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
			return nil, 0, fmt.Errorf("parse manifest: %w", err)
		}
		if doc.Kind != deploymentKind {
			continue
		}
		deployments++
		for _, c := range doc.Spec.Template.Spec.Containers {
			containers = append(containers, resourcesContainer{
				Manifest:  entry.Name(),
				Name:      c.Name,
				Resources: c.Resources,
			})
		}
	}
	return containers, deployments, nil
}

// positiveQuantity parses one declared quantity and rejects the empty and the
// non-positive value. Kubernetes rejects a zero request at admission for the
// same reason the check does: a container that asks for nothing is scheduled
// on any node, however full, and a zero limit permits no CPU time at all.
func positiveQuantity(text string) (resourceapi.Quantity, error) {
	if strings.TrimSpace(text) == "" {
		return resourceapi.Quantity{}, fmt.Errorf("is missing")
	}
	quantity, err := resourceapi.ParseQuantity(text)
	if err != nil {
		return resourceapi.Quantity{}, fmt.Errorf("= %q is not a valid Kubernetes quantity: %w", text, err)
	}
	if quantity.Sign() <= 0 {
		return resourceapi.Quantity{}, fmt.Errorf("= %q must be greater than zero", text)
	}
	return quantity, nil
}

// containerResourceProblem returns why a container is not sized for the node,
// or nil when it requests and limits every required resource. The messages say
// which half is missing and what the omission costs, so the fix is editing the
// named manifest rather than reading the test.
func containerResourceProblem(manifest, container string, resources *containerResources) error {
	where := fmt.Sprintf("%s/%s", manifest, container)
	if resources == nil {
		return fmt.Errorf("%s: declares no resources block, so the pod runs in the BestEffort QoS class: the scheduler has no size to pack it by and evicts it first under node pressure, and one container can consume the whole node. Declare resources.requests and resources.limits for cpu and memory", where)
	}

	for _, name := range requiredResourceNames {
		request, err := positiveQuantity(resources.Requests.value(name))
		if err != nil {
			return fmt.Errorf("%s: resources.requests.%s %v: without a request the scheduler cannot size the container, so the pod lands in the BestEffort or Burstable class and is evicted before pods that declare one", where, name, err)
		}
		limit, err := positiveQuantity(resources.Limits.value(name))
		if err != nil {
			return fmt.Errorf("%s: resources.limits.%s %v: without a limit one replica's Redis or the labeler's poll loop can consume the whole node", where, name, err)
		}
		if limit.Cmp(request) < 0 {
			return fmt.Errorf("%s: resources.limits.%s = %s is below resources.requests.%s = %s: Kubernetes rejects the pod (a limit below its request can never be satisfied), so declare a limit at or above the request", where, name, limit.String(), name, request.String())
		}
	}

	return nil
}

// deploymentResourceProblems returns one message per container of every
// Deployment under dir whose resources block does not size cpu and memory,
// plus the number of Deployment documents and containers read, so a caller can
// tell "every container is sized" from "no container was found".
func deploymentResourceProblems(dir string) (problems []string, deployments, containers int, err error) {
	found, deployments, err := deploymentContainers(dir)
	if err != nil {
		return nil, 0, 0, err
	}
	for _, c := range found {
		containers++
		if problem := containerResourceProblem(c.Manifest, c.Name, c.Resources); problem != nil {
			problems = append(problems, problem.Error())
		}
	}
	return problems, deployments, containers, nil
}
