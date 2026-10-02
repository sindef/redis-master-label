package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
// The same file holds the readers for the other two things CI cannot see:
// the sidecar containers' securityContext (main_test.go,
// TestManifestSidecarsRunUnprivileged) and the USER the build file's final
// stage selects (dockerfileFinalStageUser, TestDockerfileFinalStageRunsAsNonRootUser).

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

// containerEnvVar holds one env entry of a labeler container: the variable name
// and, when the value comes from the Downward API, the field it is projected
// from (metadata.name, metadata.namespace).
type containerEnvVar struct {
	Name      string `json:"name"`
	ValueFrom struct {
		FieldRef struct {
			FieldPath string `json:"fieldPath"`
		} `json:"fieldRef"`
	} `json:"valueFrom"`
}

// The Downward API fields the pod-identity env vars must be projected from: the
// name and namespace of the pod the labeler runs in.
const (
	podNameFieldPath      = "metadata.name"
	podNamespaceFieldPath = "metadata.namespace"
)

// podSpecManifest holds just the container command/args, env and
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
					Env             []containerEnvVar
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

// sidecarPodIdentityProblems returns one message per labeler container whose
// env wiring would not let resolvePodIdentity find the pod at startup, plus the
// number of labeler containers seen so a caller can tell "no problems" from
// "nothing was checked". The example sidecars pass neither --pod-name nor
// --pod-namespace, so the env-only path of resolvePodIdentity is the one that
// runs in production: HOSTNAME has to be projected from metadata.name and
// POD_NAMESPACE from metadata.namespace, the fields that name the pod the
// sidecar runs in. Without them the labeler exits at startup (empty pod name)
// or labels a pod in "default" whatever namespace it was deployed into.
func sidecarPodIdentityProblems(dir string) ([]string, int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, fmt.Errorf("read %s: %w", dir, err)
	}

	var problems []string
	sidecars := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := filepath.Ext(entry.Name())
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		doc, err := readPodSpecManifest(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, 0, fmt.Errorf("parse manifest: %w", err)
		}
		for _, c := range doc.Spec.Template.Spec.Containers {
			if !strings.Contains(c.Image, sidecarImage) {
				continue
			}
			sidecars++
			problems = append(problems, labelerPodIdentityProblems(entry.Name(), c.Name, c.Command, c.Args, c.Env)...)
		}
	}
	return problems, sidecars, nil
}

// labelerPodIdentityProblems reports the pod-identity wiring of one labeler
// container: it must not decide the pod through the flags (the manifests rely
// on the env wiring, and a flag would let the two disagree), and it must
// project both documented env variables from the field that names its own pod.
func labelerPodIdentityProblems(manifest, container string, command, args []string, env []containerEnvVar) []string {
	prefix := fmt.Sprintf("%s/%s", manifest, container)

	var problems []string
	for _, flagName := range containerFlagNames(sidecarImage, command, args) {
		if flagName != podNameFlag && flagName != podNamespaceFlag {
			continue
		}
		problems = append(problems, fmt.Sprintf(
			"%s: passes --%s, so the flag decides the pod identity while the %s/%s env wiring the manifests document can disagree with it",
			prefix, flagName, envPodName, envPodNamespace))
	}

	for _, want := range []struct{ name, fieldPath, consequence string }{
		{envPodName, podNameFieldPath, "an empty pod name is a fatal startup error"},
		{envPodNamespace, podNamespaceFieldPath, fmt.Sprintf("the namespace silently falls back to %q whatever namespace the pod runs in", defaultPodNamespace)},
	} {
		path, found, projected := envFieldPath(env, want.name)
		switch {
		case !found:
			problems = append(problems, fmt.Sprintf(
				"%s: declares no %s env var, so the documented fallback finds nothing (%s)",
				prefix, want.name, want.consequence))
		case !projected:
			problems = append(problems, fmt.Sprintf(
				"%s: %s carries a literal value rather than a Downward API fieldRef, so it does not name this pod (%s)",
				prefix, want.name, want.consequence))
		case path != want.fieldPath:
			problems = append(problems, fmt.Sprintf(
				"%s: %s is projected from %q, want %q (the field naming this pod)",
				prefix, want.name, path, want.fieldPath))
		}
	}
	return problems
}

// envFieldPath returns the Downward API field path an env var is projected
// from. found reports whether the variable is declared at all, and projected
// whether it comes from a fieldRef rather than a literal value: a literal value
// gives the binary some other pod's identity, so it is not the wiring that
// resolvePodIdentity needs.
func envFieldPath(env []containerEnvVar, name string) (path string, found, projected bool) {
	for _, e := range env {
		if e.Name != name {
			continue
		}
		return e.ValueFrom.FieldRef.FieldPath, true, e.ValueFrom.FieldRef.FieldPath != ""
	}
	return "", false, false
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
