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
// stage selects (dockerfileFinalStageUser, TestDockerfileFinalStageRunsAsNonRootUser,
// which reuses finalStageStart from dockerfile_check.go). The OCI labels of that
// same final stage - including the license the image advertises - are read in
// dockerfile_check.go.

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

// podSpecManifest holds just the container command/args and securityContext.
// Only fields present in the example manifests are decoded; manifests without
// a Pod spec (like Services) simply have no containers.
type podSpecManifest struct {
	Spec struct {
		Template struct {
			Spec struct {
				Containers []struct {
					Name            string
					Image           string
					Command         []string
					Args            []string
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
	lines, start, err := finalStageStart(path)
	if err != nil {
		return "", false, err
	}

	for _, line := range lines[start:] {
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
