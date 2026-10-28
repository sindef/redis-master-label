# redis-master-label

A small Kubernetes sidecar/utility that watches the Redis instance co-located with a pod, detects when it is the **master**, and applies a Kubernetes label to that pod (e.g., `redis-role=master`). This enables services or controllers to target the current Redis master via pod labels.

## What it does
- Connects to Redis and runs the `ROLE` command on a fixed interval.
- If the instance reports `master`, it patches the current pod with a configurable label key/value.
- If the instance is no longer `master`, it removes the label from the pod (the key is removed whenever it is present, whatever value it holds; the check is not scoped to `--label-value`).
- Provides a health check HTTP endpoint (`/healthz`) that monitors Redis connectivity.
- Runs inside the cluster using in-cluster Kubernetes credentials.

## How it works
1. Starts with flags/env for Redis connection, label key/value, pod name/namespace, check interval, and health port.
2. Builds a Kubernetes client from in-cluster config.
3. Starts an HTTP server for health checks on the configured port (default 8080).
4. On each interval:
   - Executes `ROLE` against Redis to check connectivity and update health status.
   - If the role is `master`, fetches the current pod and ensures the label key/value is set.
   - If the role is not `master` and the label key exists, removes the label key from the pod. The value currently under that key does not matter: only the value `--label-value` is compared when adding the label.
5. Repeats forever.

## Configuration
Flags (all have sensible defaults):
- `--redis-addr` (default `localhost:6379`)
- `--redis-password` (default empty; falls back to the `REDIS_PASSWORD` env var when not set)
- `--redis-tls` (default `false`; attaches a TLS config to the Redis connection)
- `--redis-tls-skip-verify` (default `false`; sets `InsecureSkipVerify` on that TLS config, so only meaningful together with `--redis-tls`. Passing it without `--redis-tls` is an invalid combination, rejected at startup: without TLS the connection is plaintext and the request to skip verification would silently do nothing)
- `--label-key` (default `redis-role`)
- `--label-value` (default `master`)
- `--pod-name` (defaults to `HOSTNAME` env)
- `--pod-namespace` (defaults to `POD_NAMESPACE` env or `default`)
- `--check-interval` (default `10s`)
- `--health-port` (default `8080`)
- `--version` (prints the build version and exits; stamped at Docker build time via `--build-arg VERSION=<tag>`, `dev` otherwise)

Environment defaults:
- `HOSTNAME` used when `--pod-name` not provided.
- `POD_NAMESPACE` used when `--pod-namespace` not provided.
- `REDIS_PASSWORD` used when `--redis-password` not provided. This is the preferred way to supply the Redis credential: an explicit `--redis-password` argument ends up in the container's argv, which Kubernetes records in the pod spec (visible to anyone with pod read access and echoed by `kubectl describe pod`) and in `/proc/<pid>/cmdline` inside the pod, readable by every container sharing the pod. `TestManifestSidecarsKeepPasswordOutOfArgs` fails the pull request when an example manifest passes the credential through args instead.

## Building locally
```bash
go build -o redis-master-label .
```

The Go toolchain version has a single source: the `go` directive at the top of
`go.mod`. CI installs exactly that toolchain (`go-version-file: go.mod`) and the
job prints `go version` next to the declared version, so the compiler running
`gofmt`, `go vet`, `go build` and `go test` is the one the module file names.
The container build is held to the same version: the build file's builder stage
must be a `golang` base image on the same MAJOR.MINOR line
(`FROM golang:1.27-alpine` for `go 1.27.0`), which
`TestDockerfileBuilderToolchainMatchesGoMod` and CI's "Check builder toolchain
matches go.mod" step enforce. Bump the `go` directive and that image tag in the
same change.

The module path in `go.mod` is the repository the project is published from, not
a folder label: `module github.com/redis-master-label/redis-master-label` is what
`go get` and the module proxy resolve and what module-derived SBOM/purl metadata
records, and it must be the repository the image's
`org.opencontainers.image.source` label, `releaseImage` in `main_test.go` and the
example manifests name. A declaration that stops one segment short
(`github.com/redis-master-label`) names a different repository, so module lookups
and module-derived metadata never point at the released project; nothing else in
the suite reads that line.
`TestGoModModulePathMatchesPublishedRepository` fails the pull request when the
module path and those surfaces disagree.

CI reports the module-graph gate as a job of its own: `update-go_modules-graph`
rebuilds the graph from `go.mod`/`go.sum` (`go mod graph`, then a non-empty
check) and fails when the graph cannot be produced, for example when `go.sum` is
missing an entry. It stays a separate job because GitHub keys a required status
check on the name of the job that reported it: folding those commands into
another job keeps running them while the `update-go_modules-graph` context stops
reporting, and every pull request then waits on a check that can no longer
arrive. `TestWorkflowsReportModuleGraphCheck` fails when the job is dropped,
renamed, commented out, or left without its graph commands.

The graph file is generated output, not source. The job writes it to
`$RUNNER_TEMP/go_modules_graph.txt` (outside the checkout), so `git status` never
shows it and `git add -A` cannot commit a dependency graph as if it were code;
`.gitignore` also lists `go_modules_graph.txt`, so replaying the older in-tree
`go mod graph > go_modules_graph.txt` locally stays uncommittable.
`TestModuleGraphOutputStaysOutOfWorkingTree` fails when a workflow redirects the
graph into the working tree without that `.gitignore` entry, or when the entry is
dropped.

Note: running the binary outside a cluster is not supported. It builds its Kubernetes client exclusively from in-cluster config (`rest.InClusterConfig()`), so it exits with `failed to get in-cluster config` unless the environment provides the pod's service-account credentials (`KUBERNETES_SERVICE_HOST`, `KUBERNETES_SERVICE_PORT`, and the mounted service-account token). There is no kubeconfig fallback. Build locally to verify the code, then run the binary in-cluster via the manifests in `manifests/` (see "Kubernetes deployment" below).

## Container build
```bash
# Build image using the provided Dockerfile
podman build --build-arg VERSION=v0.1.0 -t redis-master-label:v0.1.0 .
# or
docker build --build-arg VERSION=v0.1.0 -t redis-master-label:v0.1.0 .
```

`--build-arg VERSION` stamps the binary's `--version` output and the image's
`org.opencontainers.image.version` label. The build file defaults `VERSION` to
`dev` (with a shell fallback for an empty value), so a plain `docker build .`
still succeeds and reports `redis-master-label dev`; a `vX.Y.Z` tag passed
through `--build-arg` is reported exactly.

The builder stage's `FROM golang:<tag>` line is the toolchain that compiles the
published binary, so it must stay on the `go.mod` minor: CI's "Check builder
toolchain matches go.mod" step fails the pull request when the tag and the `go`
directive name different versions, and a Dependabot base-image bump therefore
has to move `go.mod` (or be closed) rather than land alone.

The five OCI labels (title, description, source, licenses and version) are
declared in the shipping stage of the build file, so they land on the image that
is pulled rather than being discarded with the builder stage. Verify them with
`docker inspect --format '{{json .Config.Labels}}' redis-master-label:v0.1.0`;
CI's "Verify image OCI labels" step fails the pull request when one of them is
missing, empty, or when the version label disagrees with `--build-arg VERSION`.

## Releases
Releases are cut by pushing a `vX.Y.Z` tag (`.github/workflows/release.yml`):
before anything is built, the tag-push workflow checks that the tagged commit is
reachable from `main` and that the tag is the version the repository deploys —
it reads `releaseImage` from `main_test.go` and requires that reference to end in
the tag and to appear in `manifests/`. A tag that disagrees with the pin, or a
tag on a commit `main` never held, fails the run instead of publishing an image
no manifest references. The workflow then runs `go vet` and the full test suite,
builds the image once with `--build-arg VERSION=<tag>` (`docker/build-push-action`
with `load: true`, so the build lands in the runner's Docker daemon), verifies
that built image before anything is pushed, and publishes exactly that image with
`docker push` to `ghcr.io/redis-master-label/redis-master-label:<tag>` (lowercase
repo name, `GITHUB_TOKEN` with `packages: write`). The push does not rebuild the
image: what leaves the runner is the artifact the run just verified, so a
nondeterministic base-image or module download cannot swap in a different one.
The workflow's step summary reports the pushed digest plus the
`docker buildx imagetools inspect` output for it.
Nothing floating (`:latest`) is ever published.

The tag is also not movable: after logging in and before `docker push`, the job
asks the registry for the manifest `<image>:<tag>` already points at
(`docker buildx imagetools inspect`) and fails when one exists. Re-running the
workflow, or force-pushing the same tag after a build-file change, therefore
cannot replace the digest the manifests pin — bump the version and tag that
commit instead. Only the registry's "not found" answer lets the release
continue; any other lookup error fails closed, so an auth or network failure
cannot bypass the guard.

`TestReleaseWorkflowBuildsImageOnce` fails the pull request when the release
workflow builds the image a second time instead of pushing the build it
verified, and when the tag push stops reporting the pushed digest.
`TestReleaseWorkflowRefusesToOverwritePublishedTag` fails it when the
already-published-tag guard is dropped, moved after the push, or no longer fails
on an existing tag.

Before anything is pushed, the release job runs the image it just built and
inspects its metadata, making the same assertions CI makes on a pull request: the
binary must report `redis-master-label <tag>`, the five
`org.opencontainers.image.*` labels must be present and non-empty on the shipping
image, and the `version` label must equal the tag. Those checks are image-level
and a Go test cannot make them (the build-file readers inspect the file, not the
image), so they run in the publishing path too: a build file that stops passing
`VERSION` to the linker or leaves a `LABEL` in the discarded builder stage fails
the tag push instead of reaching GHCR. `TestReleaseWorkflowVerifiesImageBeforePush`
fails the pull request when the workflow drops either check or moves it after the
push.

### Release checklist

The pinned image reference must be identical in three places: the example
manifests that deploy it and `main_test.go`, whose `releaseImage` constant pins
what `TestManifestsPinVersionedReleaseImage` accepts. Two gates run before the
release workflow builds or publishes anything: `go test -count=1 ./...` (so a
stale pin releases no image, with an error text saying the manifest image "is
not the pinned release image", which points at the YAML even when the stale
string lives in `main_test.go`), and a tag-versus-pin step that stops a tag
whose version differs from `releaseImage` or that no manifest references. A tag
pushed from a commit that is not on `main` is refused too, so only merged work
can be released. `workflow_check.go` fails the pull request when either gate is
dropped from the workflow or moved after the build.

1. Bump the new `ghcr.io/redis-master-label/redis-master-label:vX.Y.Z`
   reference in every one of these files:
   - `main_test.go` — `const releaseImage = "..."`
   - `manifests/deployment-example.yaml`
   - `manifests/redis-leader.yaml`
2. Merge the bumps to `main`, verify the gate locally, then tag that commit:
   ```bash
   go test -count=1 ./...
   git tag v0.2.0 && git push origin v0.2.0
   ```
   The tag push starts the release workflow (tag checks, vet + tests, then build
   and push `ghcr.io/<owner>/redis-master-label:<tag>`). Tagging any commit that
   is not on `main`, or tagging a version the pin does not name, fails the run.
3. Apply the updated manifests; the pinned tag rolls out the new image.

Rollback: redeploy the previous release's full image reference
(`kubectl set image ...` or re-applying the manifest with the older tag) —
every published tag stays available in GHCR.


## Kubernetes deployment
Manifests in `manifests/` provide an example service account, role, rolebinding, a Redis leader deployment and its Service, and a Redis replica deployment.

Apply them (edit image and args as needed). Both the labeler sidecar and the co-located `redis` container read their password from the same `redis-secret` Secret and the same `password` key, so create it first (see "Example usage in a pod"). Without it both pods stay `CreateContainerConfigError`, because the `secretKeyRef` is not optional:
```bash
kubectl apply -f manifests/serviceaccount.yaml
kubectl apply -f manifests/role.yaml
kubectl apply -f manifests/rolebinding.yaml
kubectl apply -f manifests/redis-leader.yaml
kubectl apply -f manifests/redis-leader-service.yaml
kubectl apply -f manifests/deployment-example.yaml
```

Both example `redis` containers start `redis-server` with `--requirepass $(REDIS_PASSWORD)`, and the replica also passes `--masterauth $(REDIS_PASSWORD)`. The `$(REDIS_PASSWORD)` reference is expanded by the kubelet from the container's own env entry, which reads Secret `redis-secret` key `password`, so the example Redis and the sidecar that AUTHs to it agree. The credential never lands in the pod spec; like any `--requirepass` argument it is visible in the `redis` container's own argv inside the pod (and to nothing outside it). Creating the Secret with a real value therefore gives a labeler that answers `200` on `/healthz` and applies `redis-role=master`. `TestManifestRedisAuthMatchesSidecarCredential` fails the pull request when the redis container's credential and the sidecar's diverge.

`manifests/redis-leader.yaml` and `manifests/redis-leader-service.yaml` run a single Redis master (one replica, no `--replicaof`) and expose it in the `default` namespace as a Service named `redis-leader`, so the DNS name used by the replicas resolves inside the example itself. That leader pod also runs the labeler sidecar, so you can see `redis-role=master` get applied and stay on the master; the replica deployment points its Redis containers at `redis-leader.default.svc.cluster.local:6379`, so all three replicas replicate from it, and their labeler sidecars remove the `redis-role` label if a pod is ever demoted or promoted back to a replica.

That leader Deployment is pinned to `strategy: {type: Recreate}`, because master
election here is label-based rather than a leader-election loop: whichever pod's
`redis-server` reports `role:master` gets `redis-role=master` from its sidecar,
and the `redis-leader` Service — the DNS name every replica's `--replicaof`
points at — selects every pod carrying `app: redis-leader`. With the default
strategy (`RollingUpdate`) a rollout, `kubectl rollout restart redis-leader`
included, creates the replacement pod before terminating the old one, and a
fresh `redis-server` starts with no `--replicaof`, so it becomes a master too:
two pods would be labelled `app: redis-leader` and `redis-role=master` at the
same time, the Service would route the replicas across two divergent datasets,
and there would be two writers. `Recreate` terminates the old pod before the
replacement starts, so at most one `redis-leader` pod exists at any moment;
expect a short write outage while the new pod starts, and a brief replication
interruption for the `redis-with-labeler` replicas until they reconnect through
the Service. `TestLeaderDeploymentUsesRecreateStrategy` fails the pull request
when a Deployment whose `redis-server` starts without `--replicaof` keeps the
default or an explicit `RollingUpdate` strategy, declares more than one replica,
or combines `Recreate` with `rollingUpdate` parameters, which the API server
rejects; kubeconform only validates the schema, where `strategy` is optional.

If you prefer to run Redis replication against your own Redis leader, apply `serviceaccount.yaml`, `role.yaml`, `rolebinding.yaml`, and `deployment-example.yaml`, change `--replicaof` in `deployment-example.yaml` to point at your leader's Service DNS name, and provide that Service (or a headless Service with a stable DNS name for a Redis master, e.g. a StatefulSet headless Service) yourself — the example deployment expects a `redis-leader` Service (or your own equivalent) to exist.

Key points for the deployment:
- Inject Redis connection info via args (address, TLS) and the credential via the `REDIS_PASSWORD` env var from a Secret (see "Example usage in a pod").
- Ensure the pod has RBAC to `get`/`update` its own Pod object (see provided Role/RoleBinding).
- Run alongside your Redis container (as sidecar) or as a dedicated pod that points to the Redis service.
- Expose the health port the labeler is told to listen on and poll `/healthz`
  from a liveness probe (see "Health Check Endpoint"); both example deployments
  do. Add a readiness probe only when the pod backs no Service: the leader pod
  is the only endpoint of the `redis-leader` Service the replicas replicate
  from, so its labeler keeps the liveness probe alone.
- Size every container with `resources.requests` and `resources.limits` (see
  "Resource sizing" below); both example deployments do, so their pods are
  `Burstable` instead of `BestEffort`.
- Keep a single-master Redis Deployment on `strategy: {type: Recreate}` so a
  rollout never runs two masters at once (see above); the example leader
  Deployment does.
- Run every container unprivileged (see "Running unprivileged" below). Both
  example deployments declare the full restricted profile on the labeler
  sidecar and on the co-located `redis` container, plus the writable `/data`
  volume `redis-server` needs, so they apply unchanged into a namespace that
  enforces the restricted Pod Security Standard.

### Resource sizing
Both example deployments declare a `resources` block on every container — the
`redis` master or replica and the `redis-master-label` sidecar:

```yaml
resources:
  requests:
    cpu: 100m
    memory: 128Mi
  limits:
    cpu: 500m
    memory: 512Mi
```

The sidecar uses smaller values (`10m`/`32Mi` requested, `100m`/`64Mi` limited)
because it only polls Redis every `--check-interval` and patches one pod. The
requests are what the scheduler packs and scores pods by, so a container without
one makes its pod `BestEffort` and the first eviction candidate under node
pressure; the limits cap what a single replica's Redis or the labeler's poll
loop can take from the node. Requests below limits keep the pods `Burstable`,
which lets an idle example pod use spare capacity without reserving it. The
example numbers are deliberately small demo sizes: size your own from
`kubectl top pod` and the dataset Redis holds, and note that a `redis-server`
with no `maxmemory` set can grow up to its memory limit. A limit below its
request is rejected by the API server, so raise the limit instead.
`TestManifestContainersDeclareResources` fails the pull request when any
container in either example deployment loses a cpu or memory request or limit,
or declares a limit below its request; kubeconform only checks the schema, where
`resources` is optional, which is how the blocks went missing.

### Running unprivileged
The labeler only reads Redis and calls the Kubernetes API with its mounted
credentials, so it needs no privileges and writes nothing to disk. The
restricted Pod Security Standard is enforced against every container in the
pod, not just the one on the highest-numbered line, so both example deployments
declare the same `securityContext` on the `redis-master-label` sidecar and on
the co-located `redis` container:
```yaml
securityContext:
  runAsNonRoot: true
  runAsUser: 10001
  runAsGroup: 10001
  allowPrivilegeEscalation: false
  readOnlyRootFilesystem: true
  capabilities:
    drop:
    - ALL
  seccompProfile:
    type: RuntimeDefault
```
A pod-level block cannot satisfy the per-container fields, so each container
carries its own copy and both example manifests apply unchanged into a namespace
that enforces the restricted Pod Security Standard.

`readOnlyRootFilesystem: true` imposes one requirement on Redis itself:
`redis-server` writes its RDB snapshot under `/data`, so each `redis` container
mounts a writable `emptyDir` there:
```yaml
volumeMounts:
- name: redis-data
  mountPath: /data
# pod spec
securityContext:
  fsGroup: 10001
volumes:
- name: redis-data
  emptyDir: {}
```
The `fsGroup` is not decoration: the kubelet creates an `emptyDir` owned by
`root:root` with mode `0755` unless the pod declares one, so a container forced
to `runAsUser: 10001` cannot write `/data` even though the mount exists. That is
the bug the example used to ship — `redis:7-alpine` starts Redis under its own
`redis` user (uid 999), not 10001, so the declared uid owned nothing in the
emptyDir, every background RDB save failed with `EACCES`, and the leader kept
answering reads while silently losing persistence. `fsGroup: 10001` makes the
kubelet give the volume to that group and add it to the container's
supplementary groups, which is what makes the mount writable for the declared
user. Keep the value equal to the `redis` container's `runAsUser` (or its
`runAsGroup`); `TestRedisSnapshotStorageProblem_RegressionFixtures` covers a
missing and a mismatched `fsGroup`.

Swap the `emptyDir` for a PersistentVolumeClaim if the data must survive a pod
restart. Declare your own `runAsUser`/`runAsGroup` **and a matching `fsGroup`**
if the image you use starts Redis under a different uid, or the same permission
failure comes back under a different number.

`TestExampleManifestsEveryContainerMeetsRestrictedStandard` reads every
Deployment under `manifests/` and asserts the restricted fields on **every**
container of every pod, and
`TestExampleManifestsMountWritableRedisData` asserts a `/data` mount that the
declared uid can actually write to (a non-read-only mount backed by a declared
volume, with a pod-level `fsGroup` equal to that uid or its group). Neither
filters by the sidecar image (the earlier `TestManifestSidecarsRunUnprivileged`
did, which is how the `redis` container could stay unhardened while the suite
stayed green), and kubeconform only checks the schema, where `securityContext`
and `fsGroup` are optional.

The uid matches the image: the final Dockerfile stage creates the
`labeler` account (uid/gid 10001) and
selects it with `USER 10001:10001`, so the shipping image no longer runs as
root even when a manifest is applied without the block above. Copy the block
into your own pod spec when you run the sidecar elsewhere — the cluster needs
`runAsNonRoot` there to enforce the image's non-root user instead of accepting
uid 0.

## Example usage in a pod
In your pod spec (sidecar pattern), pass connection settings as args and the credential as an env var backed by a Secret:
```yaml
args:
  - "--redis-addr=$(REDIS_ADDR)"
  - "--label-key=redis-role"
  - "--label-value=master"
env:
  - name: REDIS_ADDR
    value: "localhost:6379"
  # Never put the password in args (see Configuration above): inject it as
  # REDIS_PASSWORD through valueFrom.secretKeyRef. The binary reads it from
  # that env var when --redis-password is not set.
  - name: REDIS_PASSWORD
    valueFrom:
      secretKeyRef:
        name: redis-secret
        key: password
```

Create the Secret referenced by the examples before applying them:
```bash
kubectl create secret generic redis-secret --from-literal=password=<your-redis-password>
```

The example manifests expect an authenticated Redis: the `redis` containers and the labeler sidecars both read this Secret, and the servers start with `--requirepass` (plus `--masterauth` on the replica). To run the examples unauthenticated instead, remove the `REDIS_PASSWORD` env entry from **both** containers of each Deployment and drop `--requirepass`/`--masterauth` from the `redis` command; the sidecar then connects without AUTH. Doing only half of that restores the original failure: Redis answers `ERR Client sent AUTH, but no password is set`, `ROLE` never succeeds, `/healthz` answers `503` and the liveness probe restarts the sidecar. For your own passwordless Redis outside the examples, create the Secret with an empty value (`--from-literal=password=''`) so the sidecar also connects unauthenticated.

## RBAC requirements
- Needs `get` and `update` on the Pod resource in its namespace.
- Applying the provided manifests into any namespace other than `default` fails the labeler with `get` errors on every check interval, because each manifest pins `namespace: default` and the kubelet binds the pod at deploy time, so the labeler never labels anything.

### Namespacing caveat

Everything in `manifests/` is fixed to the `default` namespace:

- `manifests/serviceaccount.yaml` — ServiceAccount lives in `default`.
- `manifests/role.yaml` — Role is scoped to the `default` namespace.
- `manifests/rolebinding.yaml` — RoleBinding references the role and the service account in `default`.
- `manifests/redis-leader.yaml` and `manifests/redis-leader-service.yaml` — expose `redis-leader.default.svc.cluster.local`, referenced by name from the replica deployment.
- `manifests/deployment-example.yaml` — deployed into `default` and points at the `redis-leader` Service.

At runtime the binary reads its namespace from the pod itself (the `POD_NAMESPACE` env var, injected from the Downward API in `manifests/deployment-example.yaml`), not from anything in the manifests. If you deploy into a different namespace, edit the `metadata.namespace` (and `subjects[].namespace` in the RoleBinding) of each manifest before applying, or template them per namespace — otherwise the pod gets a service account with no permissions in its own namespace.

## Health Check Endpoint

The application provides a health check endpoint at `/healthz` that:
- Returns `200 OK` when healthy
- Returns `503 Service Unavailable` when unhealthy
- Monitors Redis connectivity by executing the `ROLE` command
- Tracks consecutive failures; after 3 consecutive failures, the endpoint reports unhealthy
- Runs on port 8080 by default (configurable via `--health-port`)

Example:
```bash
curl http://localhost:8080/healthz
```

Both example deployments wire that endpoint into the kubelet. The
`redis-master-label` container in `manifests/deployment-example.yaml` and
`manifests/redis-leader.yaml` declares `containerPort: 8080` (named `health`)
and a `livenessProbe` that polls it:

```yaml
ports:
- name: health
  containerPort: 8080
livenessProbe:
  httpGet:
    path: /healthz
    port: health
  periodSeconds: 10
  failureThreshold: 3
```

The liveness probe restarts the labeler after three failing polls, so a sidecar
whose Redis became unreachable — or whose health listener died — is restarted
instead of sitting `Running` with a stale `redis-role` label.

The `redis-with-labeler` replicas in `manifests/deployment-example.yaml` also
declare a `readinessProbe` on the same path and port. The leader's labeler in
`manifests/redis-leader.yaml` deliberately declares only the liveness probe.
Pod readiness is the AND of every container's readiness, and
`manifests/redis-leader-service.yaml` selects that leader pod, which is the only
endpoint of the Service every replica replicates from (`--replicaof
redis-leader.default.svc.cluster.local 6379` in `deployment-example.yaml`). A
readiness probe there answers 503 after three consecutive `ROLE` failures, so a
transient labeler-side Redis hiccup would mark the whole pod `NotReady`, remove
that sole endpoint and stall all three replicas even though `redis-server` is
healthy. Keep the liveness probe beside a Service endpoint and no readiness
probe; a labeler readiness probe is only safe in a pod no Service selects.

Declaring the port alone changes nothing: only a probe makes the kubelet poll
the endpoint, so `TestManifestSidecarsDeclareHealthProbes` fails the build when
a labeler container serves `/healthz` on `--health-port` without a liveness
probe targeting that path and port (or without the matching `containerPort`
entry).
`TestManifestServiceBackedLabelerKeepsReadinessOutOfPodEndpoint` reads the
Service selectors and the Deployment pod labels and fails when a labeler in a
Service-backed pod declares a readiness probe (the reviewed defect) or loses its
liveness probe, so the leader's Redis availability cannot be made to depend on
the labeler again. kubeconform only validates the manifest schema and never
asks whether anything polls the port or which container's readiness gates an
endpoint. If you change `--health-port`, move the `containerPort` entry and the
probe ports with it.

## Operational notes
- Labels are applied when the instance is `master` and removed when it's no longer master.
- Update frequency controlled by `--check-interval`.
- If using TLS, set `--redis-tls` and, if required, `--redis-tls-skip-verify`. `--redis-tls-skip-verify` without `--redis-tls` is rejected at startup instead of being silently ignored.
- The health check endpoint is what the example deployments' probes poll, so the kubelet restarts the sidecar when it turns unhealthy (see "Health Check Endpoint").

## License
Released under the [MIT License](LICENSE). The container image advertises the
same license through its `org.opencontainers.image.licenses` label (check it
with `docker inspect`), so the published image metadata and the `LICENSE` file
that grants the license agree.
