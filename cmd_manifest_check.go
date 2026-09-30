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

// manifestDir is where the example manifests live relative to the repo root.
const manifestDir = "manifests"

// sidecarImage identifies the container running this binary's flag set. Only
// that container's args are checked: other containers (for example
// redis-server --replicaof) carry flags from different binaries.
const sidecarImage = "redis-master-label"

// podSpecManifest holds just the container command/args. Only fields present
// in the example manifests are decoded; manifests without a Pod spec (like
// Services) simply have no containers.
type podSpecManifest struct {
	Spec struct {
		Template struct {
			Spec struct {
				Containers []struct {
					Name    string
					Image   string
					Command []string
					Args    []string
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
