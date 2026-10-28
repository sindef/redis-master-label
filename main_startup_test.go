package main

import (
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The reviewed test_gap: main() wires the whole startup contract (--version,
// --check-interval, pod-name/pod-namespace defaulting, the in-cluster client,
// the Redis options, the health-server bind and the poll loop) and no test
// executed it, so deleting a call site or turning the TLS rejection into a
// warning left `go test ./...` green while the README's "rejected at startup"
// promise changed silently.
//
// The tests below drive the real program: most of them re-execute the test
// binary with an env switch that makes it call main() instead of running the
// test suite, so the child's exit code and stderr/stdout are the program's.
// One test calls main() in-process (the --version path returns instead of
// exiting) so the package's own coverage report counts main().
const (
	// reExecMainEnv marks a child process that must run main() instead of the
	// test suite. It is never set for the parent test run.
	reExecMainEnv = "REDIS_MASTER_LABEL_TEST_RUN_MAIN"
	// reExecMainArgsEnv carries the child's argv as JSON, so arguments survive
	// spaces and re-quoting.
	reExecMainArgsEnv = "REDIS_MASTER_LABEL_TEST_RUN_MAIN_ARGS"
)

// TestMainProcessHelper is the child half of the re-executed tests: when the
// env switch is set it replaces os.Args with the program arguments and calls
// main(), so the real startup path decides the child's exit code. Without the
// switch it skips, so a normal `go test ./...` run is unaffected.
func TestMainProcessHelper(t *testing.T) {
	if os.Getenv(reExecMainEnv) != "1" {
		t.Skip("helper process only: re-executed by the main() startup tests")
	}

	rawArgs := os.Getenv(reExecMainArgsEnv)
	var argv []string
	if err := json.Unmarshal([]byte(rawArgs), &argv); err != nil {
		t.Fatalf("decode %s = %q: %v", reExecMainArgsEnv, rawArgs, err)
	}
	if len(argv) == 0 {
		t.Fatalf("%s carries no argv", reExecMainArgsEnv)
	}

	os.Args = argv
	// --version returns; every other path below exits the process itself.
	main()
}

// runMainAsProgram re-executes the test binary as the program under test with
// argv[0] fixed and the named environment variables removed, then reports its
// stdout, stderr and exit code. Removing HOSTNAME/POD_NAMESPACE and the
// in-cluster service variables keeps startup deterministic, including on a
// runner that itself runs inside a pod.
func runMainAsProgram(t *testing.T, argv []string, dropped ...string) (stdout, stderr string, exitCode int) {
	t.Helper()

	encoded, err := json.Marshal(argv)
	if err != nil {
		t.Fatalf("encode argv: %v", err)
	}

	skip := map[string]bool{reExecMainEnv: true, reExecMainArgsEnv: true}
	for _, name := range dropped {
		skip[name] = true
	}

	env := make([]string, 0, len(os.Environ())+2)
	for _, entry := range os.Environ() {
		name := entry
		if i := strings.IndexByte(entry, '='); i >= 0 {
			name = entry[:i]
		}
		if skip[name] {
			continue
		}
		env = append(env, entry)
	}
	env = append(env, reExecMainEnv+"=1", reExecMainArgsEnv+"="+string(encoded))

	cmd := exec.Command(os.Args[0], "-test.run=^TestMainProcessHelper$")
	cmd.Env = env

	var out, errOut strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errOut

	err = cmd.Run()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("re-executing the program with %v: %v", argv, err)
		}
		code = exitErr.ExitCode()
	}
	return out.String(), errOut.String(), code
}

// --version must exit 0 and print the stamped version (README "Configuration":
// "prints the build version and exits"). This call is in-process because
// main() returns on this path without calling os.Exit, so it also makes the
// package coverage report count main() instead of leaving it at 0.0%.
func TestMainVersionFlagReturnsInsteadOfStartingTheProgram(t *testing.T) {
	origArgs, origShowVersion := os.Args, *showVersion
	defer func() {
		os.Args, *showVersion = origArgs, origShowVersion
	}()

	os.Args = []string{"redis-master-label", "--version"}
	main()

	if !*showVersion {
		t.Fatal("main() did not parse --version: flag.Parse() is no longer reached")
	}
}

// The same contract from outside: the process must exit 0 and print exactly the
// program name and version, so a --version path that falls through into the
// startup validation is caught by an exit code or a missing line.
func TestMainVersionFlagExitsZeroAndPrintsVersion(t *testing.T) {
	stdout, stderr, code := runMainAsProgram(t, []string{"redis-master-label", "--version"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 for --version (stderr: %q)", code, stderr)
	}

	want := "redis-master-label " + version
	if !strings.Contains(stdout, want) {
		t.Fatalf("stdout = %q, want it to print %q", stdout, want)
	}
}

// A non-positive --check-interval makes time.Sleep return immediately, so the
// poll loop busy-spins. main() must reject it at startup (README
// "Configuration"), not accept it and spin.
func TestMainRejectsNonPositiveCheckIntervalAtStartup(t *testing.T) {
	for _, interval := range []string{"0", "-5s", "-1ns"} {
		t.Run(interval, func(t *testing.T) {
			argv := []string{
				"redis-master-label",
				"--check-interval=" + interval,
				"--pod-name=test-pod",
			}
			_, stderr, code := runMainAsProgram(t, argv)
			if code == 0 {
				t.Fatalf("--check-interval=%s exited 0, want a startup failure", interval)
			}
			if !strings.Contains(stderr, "--check-interval must be positive") {
				t.Fatalf("stderr = %q, want the --check-interval validation message", stderr)
			}
		})
	}
}

// Without --pod-name and without HOSTNAME the program cannot know which pod to
// label, so main() must exit non-zero with the documented message instead of
// polling with an empty name.
func TestMainRequiresPodNameAtStartup(t *testing.T) {
	argv := []string{"redis-master-label", "--check-interval=10s"}

	_, stderr, code := runMainAsProgram(t, argv, "HOSTNAME")
	if code == 0 {
		t.Fatal("missing --pod-name and HOSTNAME exited 0, want a startup failure")
	}
	if !strings.Contains(stderr, "pod-name must be set or HOSTNAME env var must be available") {
		t.Fatalf("stderr = %q, want the pod-name validation message", stderr)
	}
}

// An explicit --pod-name must get past that check (and past the
// pod-namespace defaulting) and reach the in-cluster client, whose failure is
// the next documented startup exit. Without the service-account token that
// client cannot be built in a test environment (rest.InClusterConfig reads the
// fixed /var/run/secrets/kubernetes.io/serviceaccount/token path), so this is
// as far as the startup path is observable from outside.
func TestMainAcceptsExplicitPodNameAndStopsAtInClusterConfig(t *testing.T) {
	argv := []string{
		"redis-master-label",
		"--check-interval=10s",
		"--pod-name=test-pod",
	}

	_, stderr, code := runMainAsProgram(t, argv, "HOSTNAME", "POD_NAMESPACE", "KUBERNETES_SERVICE_HOST", "KUBERNETES_SERVICE_PORT")
	if code == 0 {
		t.Fatal("startup outside a cluster exited 0, want the in-cluster config failure")
	}
	if strings.Contains(stderr, "pod-name must be set") {
		t.Fatalf("stderr = %q: an explicit --pod-name was rejected", stderr)
	}
	if !strings.Contains(stderr, "failed to get in-cluster config") {
		t.Fatalf("stderr = %q, want the in-cluster config failure that follows the pod checks", stderr)
	}
}

// main() cannot be executed past rest.InClusterConfig() in a test: the client
// is built from the hard-coded service-account token path, so the startup
// validation after it (the documented --redis-tls-skip-verify without
// --redis-tls rejection), the health-server bind exit and the poll loop are
// unreachable from the process under test. The unit tests of
// validateRedisTLSFlags, redisOptionsFromFlags, startHealthServer,
// updateHealthStatus and checkAndLabel cover the helpers' behaviour; this test
// pins that main() still wires them, in order, and still exits on each invalid
// startup. Without it, deleting `if err := validateCheckInterval();`,
// reverting the TLS rejection to a warning or dropping the loop's
// updateHealthStatus/checkAndLabel/sleep calls would keep `go test ./...` green.
func TestMainWiresStartupValidationAndPollLoop(t *testing.T) {
	body := mainFunctionSource(t)

	ordered := []struct {
		name string
		text string
	}{
		{"flags parsed", "flag.Parse()"},
		{"--version branch", "if *showVersion {"},
		{"--version printed", `fmt.Printf("redis-master-label %s\n", version)`},
		{"--version returns without starting the program", "return"},
		{"--check-interval validated at startup", "if err := validateCheckInterval(); err != nil {"},
		{"HOSTNAME defaulting", `*podName = os.Getenv("HOSTNAME")`},
		{"missing pod-name rejected", "pod-name must be set or HOSTNAME env var must be available"},
		{"POD_NAMESPACE defaulting", `*podNamespace = os.Getenv("POD_NAMESPACE")`},
		{"namespace falls back to default", `*podNamespace = "default"`},
		{"in-cluster config built", "rest.InClusterConfig()"},
		{"Kubernetes client request timeout bound", "boundRestConfig(config)"},
		{"Kubernetes client built", "kubernetes.NewForConfig(config)"},
		{"Redis options built from flags", "redisOptionsFromFlags()"},
		{"Redis client built", "redis.NewClient(redisOptions)"},
		{"health server bound before the loop", "startHealthServer(*healthPort)"},
		{"poll loop entered", "for {"},
		{"health server death surfaced in the loop", "case err := <-errCh:"},
		{"iteration deadline before the health and label calls", "context.WithTimeout(ctx, backendTimeout())"},
		{"health updated each iteration", "updateHealthStatus(iterCtx, rdb)"},
		{"role checked and label applied each iteration", "checkAndLabel(iterCtx, rdb, clientset)"},
		{"iteration sleeps for the check interval", "time.Sleep(*checkInterval)"},
	}

	previous := -1
	for _, step := range ordered {
		index := strings.Index(body, step.text)
		if index < 0 {
			t.Errorf("main() no longer contains %s: %q is missing", step.name, step.text)
			continue
		}
		if index < previous {
			t.Errorf("main() no longer runs %s in order: %q appears before an earlier startup step", step.name, step.text)
		}
		previous = index
	}

	// Each startup validation must exit, not warn and continue. The window runs
	// from the check to the next startup step, so a branch that only logs is
	// caught while the rest of main() keeps running.
	exits := []struct {
		name  string
		from  string
		until string
	}{
		{"--check-interval rejection", "if err := validateCheckInterval(); err != nil {", `*podName = os.Getenv("HOSTNAME")`},
		{"missing pod-name rejection", "pod-name must be set or HOSTNAME env var must be available", `*podNamespace = os.Getenv("POD_NAMESPACE")`},
		{"in-cluster config failure", "rest.InClusterConfig()", "kubernetes.NewForConfig(config)"},
		{"invalid Redis TLS flags", "redisOptionsFromFlags()", "redis.NewClient(redisOptions)"},
		{"health server bind failure", "startHealthServer(*healthPort)", "for {"},
		{"health server death", "case err := <-errCh:", "default:"},
	}
	for _, check := range exits {
		from := strings.Index(body, check.from)
		until := strings.Index(body, check.until)
		if from < 0 || until < 0 || until <= from {
			t.Errorf("cannot locate the %s branch in main()", check.name)
			continue
		}
		if !strings.Contains(body[from:until], "os.Exit(1)") {
			t.Errorf("main() no longer exits on %s: the branch between %q and %q has no os.Exit(1)", check.name, check.from, check.until)
		}
	}
}

// mainFunctionSource returns the source text of main() only, so the wiring
// assertions above cannot be satisfied by a helper, a comment elsewhere in
// main.go or dead code outside main().
func mainFunctionSource(t *testing.T) string {
	t.Helper()

	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", src, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || fn.Name.Name != "main" {
			continue
		}
		start := fset.Position(fn.Pos()).Offset
		end := fset.Position(fn.End()).Offset
		return string(src[start:end])
	}

	t.Fatal("main.go declares no main() function")
	return ""
}
