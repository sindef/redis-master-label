package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
// containers (main_test.go, TestManifestSidecarsDeclareHealthProbes), the
// credentials the sidecar must never carry in its args (main_test.go,
// TestManifestSidecarsKeepPasswordOutOfArgs), the USER the build file's final
// stage selects (dockerfileFinalStageUser,
// TestDockerfileFinalStageRunsAsNonRootUser) and the RBAC chain the example
// deployments run under (readRBACChain, TestManifestRBACWiring).

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

// Credential-in-args check: README "Configuration" states that an explicit
// --redis-password argument "ends up in the container's argv, which Kubernetes
// records in the pod spec (visible to anyone with pod read access and echoed by
// `kubectl describe pod`) and in /proc/<pid>/cmdline inside the pod", README
// "Example usage" says "Never put the password in args", and both example
// manifests repeat the rule in comments. Nothing enforced it:
// --redis-password is a registered flag, so TestManifestFlagsDefined - which
// only asserts that every sidecar argument names a flag that exists - accepted
// it, and CI's "Validate manifests" step runs kubeconform, which validates
// schemas and never reads container args. A contributor adding
// `- --redis-password=...` to either deployment therefore got a green pull
// request while the credential landed in the pod spec. The reader below makes
// the rule visible to main_test.go (TestManifestSidecarsKeepPasswordOutOfArgs),
// which rejects such an argument and names the REDIS_PASSWORD Secret env var
// (valueFrom.secretKeyRef), the documented alternative, in its message.

// credentialFlagNameFragments are the substrings that mark a flag name as
// carrying a credential: a password, a passwd, a secret, a token, a credential
// or an API/access key. The list reads the flag's name, so a hypothetical flag
// that only points at a credential without holding one (a --redis-password-file)
// is the single false positive it can have; --label-key, which the example
// manifests use, contains none of these fragments.
var credentialFlagNameFragments = []string{
	"password",
	"passwd",
	"secret",
	"token",
	"credential",
	"api-key",
	"apikey",
	"access-key",
	"secret-key",
}

// credentialValuePatterns match flag values shaped like a credential: an
// unbroken base64 or hex blob, which is what a generated password, token or key
// looks like. Both patterns require at least 32 characters, so they never fire
// on the values the example manifests use - an address, a duration, a port, a
// label key or a label value - because those either stay short or carry a
// separator (':', '/', '.') that the alphabets below exclude. The second
// pattern also has to mix upper case with digits before it counts, so a long
// all-lowercase DNS-style name is not read as a credential.
var credentialValuePatterns = []*regexp.Regexp{
	regexp.MustCompile(`^[A-Za-z0-9+/]{32,}={0,2}$`),
	regexp.MustCompile(`^[A-Za-z0-9_-]{32,}$`),
}

// credentialValueLooksSecret reports whether a flag value has the shape of a
// credential, so a secret pasted under an unrelated flag name is still caught.
// It is a shape rule, not an entropy estimate: a credential it misses is still
// caught whenever the flag's own name names one.
func credentialValueLooksSecret(value string) bool {
	if credentialValuePatterns[0].MatchString(value) {
		return true
	}
	if !credentialValuePatterns[1].MatchString(value) {
		return false
	}
	return strings.ContainsAny(value, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") &&
		strings.ContainsAny(value, "0123456789")
}

// flagValue returns the value of the argument at index i: the text after the
// first '=' or, when the argument names a flag on its own, the next argument,
// because the Go flag package accepts both forms.
func flagValue(arg string, args []string, i int) string {
	if _, value, found := strings.Cut(arg, "="); found {
		return value
	}
	if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
		return args[i+1]
	}
	return ""
}

// credentialArgProblemMessage explains one rejected argument: what an argument
// in the container's argv leaks and which documented alternative to use
// instead.
func credentialArgProblemMessage(manifest, container, arg string) string {
	return fmt.Sprintf("%s/%s: %q puts a credential in the container's argv; Kubernetes records args in the pod spec (visible to anyone with pod read access and echoed by `kubectl describe pod`) and exposes them in /proc/<pid>/cmdline to every container in the pod. Supply the Redis credential through the REDIS_PASSWORD environment variable from a Secret (valueFrom.secretKeyRef) instead, as README \"Configuration\" and \"Example usage\" document (\"Never put the password in args\")", manifest, container, arg)
}

// containerCredentialArgProblems returns one message per command or argument of
// one container that carries a credential, or nil when none does. Both argument
// forms the flag package accepts are read: --flag=value and --flag value.
func containerCredentialArgProblems(manifest, container string, args []string) []string {
	var problems []string
	for i, arg := range args {
		name, ok := flagNameFromArg(arg)
		if !ok {
			continue
		}
		if credentialFlagName(name) || credentialValueLooksSecret(flagValue(arg, args, i)) {
			problems = append(problems, credentialArgProblemMessage(manifest, container, arg))
		}
	}
	return problems
}

// credentialFlagName reports whether a flag name is one whose value is a
// credential.
func credentialFlagName(name string) bool {
	lower := strings.ToLower(name)
	for _, fragment := range credentialFlagNameFragments {
		if strings.Contains(lower, fragment) {
			return true
		}
	}
	return false
}

// sidecarCredentialArgProblems walks manifests/*.yaml and returns one message
// per labeler argument that carries a credential, plus a count of sidecar
// containers seen so the caller can detect a manifest set where the sidecar
// silently disappeared. It walks the same manifest set as sidecarFlagUsages and
// reads the same container: the other containers run different binaries with
// flag sets of their own, so their arguments (redis-server's --replicaof, for
// example) are none of this check's business.
func sidecarCredentialArgProblems(dir string) (problems []string, sidecars int, err error) {
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
			return nil, sidecars, fmt.Errorf("parse manifest: %w", err)
		}
		for _, c := range doc.Spec.Template.Spec.Containers {
			if !strings.Contains(c.Image, sidecarImage) {
				continue
			}
			sidecars++
			args := append(append([]string{}, c.Command...), c.Args...)
			problems = append(problems, containerCredentialArgProblems(entry.Name(), c.Name, args)...)
		}
	}
	return problems, sidecars, nil
}

// RBAC wiring check: the example manifests hand the labeler a ServiceAccount, a
// Role granting the pod verbs it needs and a RoleBinding tying the two
// together, and both Deployments run under that ServiceAccount. CI's kubeconform
// step validates each document against the Kubernetes schemas and never
// resolves a roleRef, a subject or a serviceAccountName across documents, and
// the readers above only extract container fields, so a Role renamed without its
// roleRef, a RoleBinding pointed at another subject or a Role that lost a verb
// all pass CI and then fail every check interval at runtime with forbidden/get
// errors (README "RBAC requirements"). The readers here make the chain visible
// to main_test.go (TestManifestRBACWiring) instead.
//
// The pod verbs the Role has to grant are not guessed from the manifest: they
// are the verbs main.go's applyLabel issues on the pod it labels.

// The manifest kinds the RBAC chain is made of.
const (
	serviceAccountKind = "ServiceAccount"
	roleKind           = "Role"
	roleBindingKind    = "RoleBinding"
	deploymentKind     = "Deployment"
)

// podsVerbsApplyLabelIssues are the pod verbs applyLabel issues on the pod it
// labels, in the core API group: a Get of its own pod and the Update that
// writes or removes the label. A Role missing either one lets the labeler start
// and report Ready and then fail every check interval with a forbidden error.
var podsVerbsApplyLabelIssues = []string{"get", "update"}

// rbacManifest is the manifest subset the RBAC chain is read from: document
// metadata, a Role's rules, a RoleBinding's subjects and roleRef, and the
// Deployment pod spec's serviceAccountName. Fields absent from a document (the
// example Service has none of them) read back as the zero value.
type rbacManifest struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Rules    []rbacRule    `json:"rules"`
	Subjects []rbacSubject `json:"subjects"`
	RoleRef  rbacRoleRef   `json:"roleRef"`
	Spec     struct {
		Template struct {
			Spec struct {
				ServiceAccountName string `json:"serviceAccountName"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
}

// rbacRule is one policy rule of a Role.
type rbacRule struct {
	APIGroups []string `json:"apiGroups"`
	Resources []string `json:"resources"`
	Verbs     []string `json:"verbs"`
}

// rbacSubject is one subject of a RoleBinding.
type rbacSubject struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// rbacRoleRef is the role a RoleBinding refers to.
type rbacRoleRef struct {
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	APIGroup string `json:"apiGroup"`
}

// rbacObject is one RBAC document with the metadata a reference to it has to
// match, plus the manifest file it came from so a failure names that file.
type rbacObject struct {
	Manifest  string
	Kind      string
	Name      string
	Namespace string
}

// rbacRole is a Role with the rules that decide which pod verbs its holder may
// use; rbacRoleBinding is a RoleBinding with the subject it grants them to.
type rbacRole struct {
	rbacObject
	Rules []rbacRule
}

type rbacRoleBinding struct {
	rbacObject
	Subjects []rbacSubject
	RoleRef  rbacRoleRef
}

// rbacDeployment is one Deployment with the ServiceAccount its pod spec runs
// as.
type rbacDeployment struct {
	rbacObject
	ServiceAccountName string
}

// rbacChain is the RBAC chain the manifests under a directory describe. Each
// list holds what was found, so a missing document is reported as an empty list
// instead of being taken for the documents that exist.
type rbacChain struct {
	ServiceAccounts []rbacObject
	Roles           []rbacRole
	RoleBindings    []rbacRoleBinding
	Deployments     []rbacDeployment
}

// readRBACManifest parses one manifest into the RBAC subset above. YAMLToJSON
// handles the YAML syntax and json.Unmarshal fills the struct; a misspelled or
// misplaced field reads back as the zero value and is reported by the checks
// rather than silently passing.
func readRBACManifest(path string) (rbacManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return rbacManifest{}, err
	}
	jsonData, err := utilyaml.ToJSON(data)
	if err != nil {
		return rbacManifest{}, fmt.Errorf("%s: YAML to JSON: %w", path, err)
	}
	var doc rbacManifest
	if err := json.Unmarshal(jsonData, &doc); err != nil {
		return rbacManifest{}, fmt.Errorf("%s: JSON decode: %w", path, err)
	}
	return doc, nil
}

// readRBACChain reads every manifest under dir and collects the RBAC documents
// it holds. Documents of other kinds (the example Service) contribute nothing,
// so the whole example manifest set can be read in one pass.
func readRBACChain(dir string) (rbacChain, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return rbacChain{}, fmt.Errorf("read %s: %w", dir, err)
	}
	var chain rbacChain
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := filepath.Ext(entry.Name())
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		doc, err := readRBACManifest(path)
		if err != nil {
			return rbacChain{}, err
		}
		object := rbacObject{
			Manifest:  entry.Name(),
			Kind:      doc.Kind,
			Name:      doc.Metadata.Name,
			Namespace: doc.Metadata.Namespace,
		}
		switch doc.Kind {
		case serviceAccountKind:
			chain.ServiceAccounts = append(chain.ServiceAccounts, object)
		case roleKind:
			chain.Roles = append(chain.Roles, rbacRole{rbacObject: object, Rules: doc.Rules})
		case roleBindingKind:
			chain.RoleBindings = append(chain.RoleBindings, rbacRoleBinding{
				rbacObject: object,
				Subjects:   doc.Subjects,
				RoleRef:    doc.RoleRef,
			})
		case deploymentKind:
			chain.Deployments = append(chain.Deployments, rbacDeployment{
				rbacObject:         object,
				ServiceAccountName: doc.Spec.Template.Spec.ServiceAccountName,
			})
		}
	}
	return chain, nil
}

// rbacChainProblems returns one message per way a manifest set's RBAC chain
// fails to line up, or nil when every link holds: one ServiceAccount, one Role
// and one RoleBinding; the binding's roleRef naming that Role in its own
// namespace; the binding's ServiceAccount subject naming that account; every
// Deployment running as that subject in that namespace; and the Role granting
// the pod verbs applyLabel issues. The messages name the manifest files, so a
// failure points at the YAML rather than at the test.
func rbacChainProblems(chain rbacChain) []string {
	var problems []string
	if len(chain.ServiceAccounts) != 1 {
		problems = append(problems, fmt.Sprintf("%d ServiceAccount documents across the manifests, want exactly one: the account the Deployments run as and the RoleBinding's subject names", len(chain.ServiceAccounts)))
	}
	if len(chain.Roles) != 1 {
		problems = append(problems, fmt.Sprintf("%d Role documents across the manifests, want exactly one: the role the RoleBinding's roleRef has to name", len(chain.Roles)))
	}
	if len(chain.RoleBindings) != 1 {
		problems = append(problems, fmt.Sprintf("%d RoleBinding documents across the manifests, want exactly one: the binding that ties the subject to the role", len(chain.RoleBindings)))
	}
	if len(problems) > 0 {
		return problems
	}

	serviceAccount := chain.ServiceAccounts[0]
	role := chain.Roles[0]
	binding := chain.RoleBindings[0]

	if err := podsVerbsProblem(role); err != nil {
		problems = append(problems, err.Error())
	}

	if binding.RoleRef.Kind != roleKind {
		problems = append(problems, fmt.Sprintf("%s: roleRef.kind = %q, want %q: the binding has to name the namespaced Role %s declares", binding.Manifest, binding.RoleRef.Kind, roleKind, role.Manifest))
	}
	if binding.RoleRef.Name != role.Name {
		problems = append(problems, fmt.Sprintf("%s: roleRef.name = %q, but %s declares Role %q: the binding grants no permissions, so every pod get/update the labeler issues is forbidden", binding.Manifest, binding.RoleRef.Name, role.Manifest, role.Name))
	}
	if role.Namespace != binding.Namespace {
		problems = append(problems, fmt.Sprintf("%s: Role %q is in namespace %q, but %s is in %q: a RoleBinding only names a Role in its own namespace", role.Manifest, role.Name, role.Namespace, binding.Manifest, binding.Namespace))
	}

	subjects := make([]rbacSubject, 0, len(binding.Subjects))
	for _, subject := range binding.Subjects {
		if subject.Kind == serviceAccountKind {
			subjects = append(subjects, subject)
		}
	}
	if len(subjects) != 1 {
		problems = append(problems, fmt.Sprintf("%s: %d ServiceAccount subjects, want exactly one naming the account the Deployments run as", binding.Manifest, len(subjects)))
		return problems
	}
	subject := subjects[0]

	if serviceAccount.Name != subject.Name {
		problems = append(problems, fmt.Sprintf("%s: ServiceAccount %q is not the RoleBinding subject %q (%s): the subject names an account that does not exist, so the binding grants it nothing", serviceAccount.Manifest, serviceAccount.Name, subject.Name, binding.Manifest))
	}
	if serviceAccount.Namespace != subject.Namespace {
		problems = append(problems, fmt.Sprintf("%s: ServiceAccount %q is in namespace %q, but the RoleBinding subject is in %q (%s)", serviceAccount.Manifest, serviceAccount.Name, serviceAccount.Namespace, subject.Namespace, binding.Manifest))
	}

	for _, deployment := range chain.Deployments {
		if deployment.ServiceAccountName == "" {
			problems = append(problems, fmt.Sprintf("%s: deployment %q declares no serviceAccountName, so its pods run as the namespace's default ServiceAccount instead of the subject %q (%s)", deployment.Manifest, deployment.Name, subject.Name, binding.Manifest))
			continue
		}
		if deployment.ServiceAccountName != subject.Name {
			problems = append(problems, fmt.Sprintf("%s: deployment %q declares serviceAccountName %q, but the RoleBinding subject is %q (%s): the pods' credentials are bound to no Role", deployment.Manifest, deployment.Name, deployment.ServiceAccountName, subject.Name, binding.Manifest))
		}
		if deployment.Namespace != subject.Namespace {
			problems = append(problems, fmt.Sprintf("%s: deployment %q is in namespace %q, but the RoleBinding subject is in %q (%s): the subject's credentials exist only in its own namespace", deployment.Manifest, deployment.Name, deployment.Namespace, subject.Namespace, binding.Manifest))
		}
	}

	return problems
}

// podsVerbsProblem returns why the Role does not grant every pod verb
// applyLabel issues, or nil when it does. Only rules covering the core API
// group's pods resource count: a rule naming another resource, or naming pods
// in another API group, grants nothing here.
func podsVerbsProblem(role rbacRole) error {
	granted := make(map[string]bool)
	for _, rule := range role.Rules {
		if !ruleTargetsPods(rule) {
			continue
		}
		for _, verb := range rule.Verbs {
			granted[strings.ToLower(verb)] = true
		}
	}

	var missing []string
	for _, verb := range podsVerbsApplyLabelIssues {
		if !granted[verb] {
			missing = append(missing, verb)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s: Role %q grants no %s verb on pods, but applyLabel issues %s on the pod it labels every check interval: grant them in the Role",
			role.Manifest, role.Name, strings.Join(missing, "/"), strings.Join(podsVerbsApplyLabelIssues, "+"))
	}
	return nil
}

// ruleTargetsPods reports whether a Role rule covers the core API group's pods
// resource, which is what applyLabel's Get and Update target. A wildcard
// resource or apiGroup counts: Kubernetes grants what the rule names.
func ruleTargetsPods(rule rbacRule) bool {
	return containsValue(rule.Resources, "pods") && containsValue(rule.APIGroups, "")
}

// containsValue reports whether values holds want or the "*" wildcard.
func containsValue(values []string, want string) bool {
	for _, value := range values {
		if value == want || value == "*" {
			return true
		}
	}
	return false
}
