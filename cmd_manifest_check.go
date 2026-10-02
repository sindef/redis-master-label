package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/util/intstr"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// Manifest flag check: the sidecar flags used in the example manifests must
// still exist in the binary's flag set (flag.CommandLine). CI's kubeconform
// step only validates YAML against Kubernetes schemas and never inspects
// container args, so a mistyped or renamed flag would otherwise pass CI and
// only crash the sidecar at runtime, where the flag package exits with status
// 2 and the pod CrashLoops. The test lives in main_test.go
// (TestManifestFlagsDefined); this file holds the shared parsing helpers.
//
// The same file holds the readers for the other things CI cannot see:
// the sidecar containers' securityContext (main_test.go,
// TestManifestSidecarsRunUnprivileged), the health endpoint wiring of those
// containers (main_test.go, TestManifestSidecarsDeclareHealthProbes) and the
// USER the build file's final stage selects (dockerfileFinalStageUser,
// TestDockerfileFinalStageRunsAsNonRootUser).

// manifestDir is where the example manifests live relative to the repo root.
const manifestDir = "manifests"

// sidecarImage identifies the container running this binary's flag set. Only
// that container's args are checked: other containers (for example
// redis-server --replicaof) carry flags from different binaries.
const sidecarImage = "redis-master-label"

// containerSecurityContext holds the container securityContext fields the
// example manifests must set to run the labeler unprivileged. The booleans are
// pointers so an absent field is distinguishable from an explicit false: an
// omitted runAsNonRoot is not the same defence as runAsNonRoot: true.
type containerSecurityContext struct {
	RunAsNonRoot             *bool  `json:"runAsNonRoot"`
	RunAsUser                *int64 `json:"runAsUser"`
	RunAsGroup               *int64 `json:"runAsGroup"`
	AllowPrivilegeEscalation *bool  `json:"allowPrivilegeEscalation"`
	ReadOnlyRootFilesystem   *bool  `json:"readOnlyRootFilesystem"`
	Capabilities             struct {
		Drop []string `json:"drop"`
	} `json:"capabilities"`
}

// containerPort is one entry of a container's ports list. The name is optional
// in Kubernetes; probes may target either the number or the name.
type containerPort struct {
	Name          string `json:"name"`
	ContainerPort int64  `json:"containerPort"`
}

// httpGetAction is the httpGet half of a probe. Port is an IntOrString because
// a probe may name a containerPort entry ("health") or give the number.
type httpGetAction struct {
	Path string             `json:"path"`
	Port intstr.IntOrString `json:"port"`
}

// containerProbe is one probe of a container, as far as the endpoint it polls
// is concerned.
type containerProbe struct {
	HTTPGet *httpGetAction `json:"httpGet"`
}

// podSpecManifest holds just the container command/args, ports, probes and
// securityContext. Only fields present in the example manifests are decoded;
// manifests without a Pod spec (like Services) simply have no containers.
type podSpecManifest struct {
	Spec struct {
		Template struct {
			Spec struct {
				Containers []struct {
					Name            string
					Image           string
					Command         []string
					Args            []string
					Ports           []containerPort           `json:"ports"`
					LivenessProbe   *containerProbe           `json:"livenessProbe"`
					ReadinessProbe  *containerProbe           `json:"readinessProbe"`
					StartupProbe    *containerProbe           `json:"startupProbe"`
					SecurityContext *containerSecurityContext `json:"securityContext"`
				}
			}
		}
	}
}

// containerFlagNames returns the flag names used by one container's command
// and args.
func containerFlagNames(image string, command, args []string) []string {
	if !strings.Contains(image, sidecarImage) {
		return nil
	}
	var flags []string
	for _, arg := range append(append([]string{}, command...), args...) {
		if flagName, ok := flagNameFromArg(arg); ok {
			flags = append(flags, flagName)
		}
	}
	return flags
}

// flagNameFromArg extracts the flag name from a container argument such as
// "--redis-addr=localhost:6379". The binary is invoked through the Go flag
// package, which accepts single or double dash and either --flag=value or a
// following separate value argument, so all forms map to the same name.
func flagNameFromArg(arg string) (string, bool) {
	if !strings.HasPrefix(arg, "-") || arg == "-" || arg == "--" {
		return "", false
	}
	name := strings.TrimLeft(arg, "-")
	if name == "" {
		return "", false
	}
	if i := strings.IndexAny(name, "="); i >= 0 {
		name = name[:i]
	}
	return name, name != ""
}

// readPodSpecManifest parses a YAML manifest from manifests/ into a
// podSpecManifest. YAMLToJSON handles the indentation-sensitive YAML syntax
// and json.Unmarshal fills the struct.
func readPodSpecManifest(path string) (podSpecManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return podSpecManifest{}, err
	}
	jsonData, err := utilyaml.ToJSON(data)
	if err != nil {
		return podSpecManifest{}, fmt.Errorf("%s: YAML to JSON: %w", path, err)
	}
	var doc podSpecManifest
	if err := json.Unmarshal(jsonData, &doc); err != nil {
		return podSpecManifest{}, fmt.Errorf("%s: JSON decode: %w", path, err)
	}
	return doc, nil
}

// sidecarImages returns every image reference of a sidecar container across
// manifests/*.yaml, walking the same manifest set (and tolerating the same
// non-Pod-spec documents) as sidecarFlagUsages. Used by the release-image
// regression tests in main_test.go.
func sidecarImages(dir string) (images []string, sidecars int, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, fmt.Errorf("read %s: %w", dir, err)
	}
	var readErr error
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
			readErr = fmt.Errorf("parse manifest: %w", err)
			continue
		}
		for _, c := range doc.Spec.Template.Spec.Containers {
			if strings.Contains(c.Image, sidecarImage) {
				sidecars++
				images = append(images, c.Image)
			}
		}
	}
	return images, sidecars, readErr
}

// sidecarContainer is one labeler container found in a manifest, together with
// the manifest file it came from so a failure names the right deployment.
type sidecarContainer struct {
	Manifest        string
	Name            string
	SecurityContext *containerSecurityContext
}

// sidecarContainers returns every labeler container across manifests/*.yaml
// with its securityContext, so a test can prove each example deployment runs
// the sidecar unprivileged. Used by the securityContext regression tests in
// main_test.go.
func sidecarContainers(dir string) ([]sidecarContainer, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var containers []sidecarContainer
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
			if strings.Contains(c.Image, sidecarImage) {
				containers = append(containers, sidecarContainer{
					Manifest:        entry.Name(),
					Name:            c.Name,
					SecurityContext: c.SecurityContext,
				})
			}
		}
	}
	return containers, nil
}

// dockerfileFinalStageUser reports the USER value the build file's final stage
// selects (the shipping image) and whether that stage creates a dedicated
// account first. An empty user means the stage never selects one, so the
// container runs as whatever the base image defaults to (uid 0 for alpine).
// The final stage is found by taking the last FROM line, so a USER in an
// earlier builder stage does not count.
func dockerfileFinalStageUser(path string) (user string, createsUser bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false, err
	}

	lines := strings.Split(string(data), "\n")
	finalStage := finalStageStart(lines)
	if finalStage < 0 {
		return "", false, fmt.Errorf("%s: no FROM stage found", path)
	}

	for _, line := range lines[finalStage+1:] {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if createsAccount(trimmed) {
			createsUser = true
		}
		if strings.HasPrefix(strings.ToUpper(trimmed), "USER ") {
			fields := strings.Fields(trimmed)
			if len(fields) > 1 {
				user = fields[1]
			}
		}
	}
	return user, createsUser, nil
}

// createsAccount reports whether a build line creates a user or group, whether
// it stands alone (USER/ADDUSER instructions) or is one command in a shell
// RUN line joined with && (as in `RUN apk add ... && adduser ...`).
func createsAccount(line string) bool {
	lower := strings.ToLower(line)
	for _, cmd := range []string{"adduser", "useradd", "addgroup", "groupadd"} {
		if strings.Contains(lower, cmd+" ") {
			return true
		}
	}
	return false
}

// healthPortDefault and healthPath mirror the binary's health endpoint:
// main.go binds --health-port (default 8080) and serves /healthz there,
// answering 503 after three consecutive Redis failures. A probe in an example
// manifest has to poll exactly that endpoint, otherwise the probe answers
// independently of the labeler's own health state.
const (
	healthPortDefault = "8080"
	healthPath        = "/healthz"
)

// sidecarHealth is the health wiring of one labeler container: the port the
// manifest tells the binary to listen on (--health-port, default 8080), the
// containerPort entries it declares and its probes.
type sidecarHealth struct {
	Manifest       string
	Name           string
	HealthPort     string
	Ports          []containerPort
	LivenessProbe  *containerProbe
	ReadinessProbe *containerProbe
	StartupProbe   *containerProbe
}

// healthPortFromArgs returns the port the container tells the binary to listen
// on. No --health-port argument means the flag default, which is the port the
// manifest then has to probe. Both the `--health-port=8080` and the
// `--health-port 8080` forms are read, because the flag package accepts both.
func healthPortFromArgs(command, args []string) string {
	all := append(append([]string{}, command...), args...)
	port := healthPortDefault
	for i, arg := range all {
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		name := strings.TrimLeft(arg, "-")
		if name == "health-port" && i+1 < len(all) {
			port = all[i+1]
			continue
		}
		if value, ok := strings.CutPrefix(name, "health-port="); ok {
			port = value
		}
	}
	return port
}

// healthProbeProblem returns why a labeler container is not wired to its own
// health endpoint, or nil when it is. A container that serves /healthz but
// declares no probe keeps reporting Running when its Redis became unreachable
// (or when its health listener died), so the label on the pod goes stale and
// nothing restarts or de-registers it. Declaring the port alone changes
// nothing: only a probe makes the kubelet poll the endpoint, so both the
// containerPort entry and a probe targeting it are required.
func healthProbeProblem(h sidecarHealth) error {
	port, err := strconv.Atoi(h.HealthPort)
	if err != nil {
		return fmt.Errorf("--health-port=%q is not a port number, so no probe can target the health endpoint", h.HealthPort)
	}

	if h.LivenessProbe == nil && h.ReadinessProbe == nil {
		return fmt.Errorf("serves %s on port %s but declares no livenessProbe or readinessProbe: the kubelet never polls the endpoint, so a sidecar whose Redis became unreachable keeps reporting Running with a stale label", healthPath, h.HealthPort)
	}

	portName := ""
	declared := false
	for _, p := range h.Ports {
		if p.ContainerPort == int64(port) {
			declared = true
			portName = p.Name
		}
	}
	if !declared {
		return fmt.Errorf("no containerPort %d entry: declare the port the health endpoint listens on so the pod spec documents it", port)
	}

	for _, ref := range []struct {
		name  string
		probe *containerProbe
	}{
		{"livenessProbe", h.LivenessProbe},
		{"readinessProbe", h.ReadinessProbe},
		{"startupProbe", h.StartupProbe},
	} {
		if ref.probe == nil {
			continue
		}
		if ref.probe.HTTPGet == nil {
			return fmt.Errorf("%s is not an httpGet probe: only an HTTP GET proves the %s listener answers", ref.name, healthPath)
		}
		if ref.probe.HTTPGet.Path != healthPath {
			return fmt.Errorf("%s polls path %q, want %q: the binary serves its health endpoint only there", ref.name, ref.probe.HTTPGet.Path, healthPath)
		}
		if !probePortMatches(ref.probe.HTTPGet.Port, port, portName) {
			return fmt.Errorf("%s polls port %q, want %d or the declared port name %q", ref.name, ref.probe.HTTPGet.Port.String(), port, portName)
		}
	}

	return nil
}

// probePortMatches reports whether a probe's port target selects the health
// port: the number itself (as an int or a string), or the name of the
// containerPort entry declaring that number.
func probePortMatches(target intstr.IntOrString, port int, portName string) bool {
	switch target.Type {
	case intstr.Int:
		return int(target.IntValue()) == port
	case intstr.String:
		if n, err := strconv.Atoi(target.StrVal); err == nil {
			return n == port
		}
		return portName != "" && target.StrVal == portName
	}
	return false
}

// sidecarHealthWiring returns the health wiring of every labeler container
// across manifests/*.yaml, walking the same manifest set (and tolerating the
// same non-Pod-spec documents) as sidecarContainers. Used by the health probe
// regression test in main_test.go.
func sidecarHealthWiring(dir string) ([]sidecarHealth, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var wiring []sidecarHealth
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
			if strings.Contains(c.Image, sidecarImage) {
				wiring = append(wiring, sidecarHealth{
					Manifest:       entry.Name(),
					Name:           c.Name,
					HealthPort:     healthPortFromArgs(c.Command, c.Args),
					Ports:          c.Ports,
					LivenessProbe:  c.LivenessProbe,
					ReadinessProbe: c.ReadinessProbe,
					StartupProbe:   c.StartupProbe,
				})
			}
		}
	}
	return wiring, nil
}

// sidecarFlagUsages walks manifests/*.yaml and returns one entry per --flag
// used by a sidecar container, formatted "manifest/container:flag", plus a
// count of sidecar containers seen so the caller can detect a manifest set
// where the sidecar silently disappeared.
func sidecarFlagUsages(dir string) (usages []string, sidecars int, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, fmt.Errorf("read %s: %w", dir, err)
	}
	var readErr error
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
			readErr = fmt.Errorf("parse manifest: %w", err)
			continue
		}
		for _, c := range doc.Spec.Template.Spec.Containers {
			flags := containerFlagNames(c.Image, c.Command, c.Args)
			if strings.Contains(c.Image, sidecarImage) {
				sidecars++
			} else if len(flags) == 0 {
				continue
			}
			for _, name := range flags {
				if flag.CommandLine.Lookup(name) == nil {
					usages = append(usages, fmt.Sprintf("%s/%s:%s", entry.Name(), c.Name, name))
				}
			}
		}
	}
	return usages, sidecars, readErr
}
