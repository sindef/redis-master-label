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
- `--redis-tls` (default `false`)
- `--redis-tls-skip-verify` (default `false`)
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
- `REDIS_PASSWORD` used when `--redis-password` not provided. This is the preferred way to supply the Redis credential: an explicit `--redis-password` argument ends up in the container's argv, which Kubernetes records in the pod spec (visible to anyone with pod read access and echoed by `kubectl describe pod`) and in `/proc/<pid>/cmdline` inside the pod, readable by every container sharing the pod.

## Building locally
```bash
go build -o redis-master-label .
```

The Go toolchain version has a single source: the `go` directive at the top of
`go.mod`. CI installs exactly that toolchain (`go-version-file: go.mod`) and the
job prints `go version` next to the declared version, so the compiler running
`gofmt`, `go vet`, `go build` and `go test` is the one the module file names.

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

## Releases
Releases are cut by pushing a `vX.Y.Z` tag (`.github/workflows/release.yml`):
the tag-push workflow runs `go vet` and the full test suite, then builds the
image with `--build-arg VERSION=<tag>` and pushes exactly
`ghcr.io/redis-master-label/redis-master-label:<tag>` to GHCR (lowercase repo
name, `GITHUB_TOKEN` with `packages: write`). The image carries provenance and
SBOM attestations, and the workflow's step summary reports the pushed digest.
Nothing floating (`:latest`) is ever published.

### Release checklist

The pinned image reference must be identical in three places: the example
manifests that deploy it and `main_test.go`, whose `releaseImage` constant pins
what `TestManifestsPinVersionedReleaseImage` accepts. The release workflow's
Test step runs `go test -count=1 ./...` before it builds or publishes anything,
so push a stale pin and no image is released — and the test's error text says
the manifest image "is not the pinned release image", which points at the YAML
even when the stale string lives in `main_test.go`.

1. Bump the new `ghcr.io/redis-master-label/redis-master-label:vX.Y.Z`
   reference in every one of these files:
   - `main_test.go` — `const releaseImage = "..."`
   - `manifests/deployment-example.yaml`
   - `manifests/redis-leader.yaml`
2. Commit the bumps, verify the gate locally, then tag that commit:
   ```bash
   go test -count=1 ./...
   git tag v0.2.0 && git push origin v0.2.0
   ```
   The tag push starts the release workflow (vet + tests, then build and push
   `ghcr.io/<owner>/redis-master-label:<tag>`).
3. Apply the updated manifests; the pinned tag rolls out the new image.

Rollback: redeploy the previous release's full image reference
(`kubectl set image ...` or re-applying the manifest with the older tag) —
every published tag stays available in GHCR.


## Kubernetes deployment
Manifests in `manifests/` provide an example service account, role, rolebinding, a Redis leader deployment and its Service, and a Redis replica deployment.

Apply them (edit image and args as needed). The labeler sidecar reads its Redis password from a `redis-secret` Secret, so create it first if your Redis requires authentication (see "Example usage in a pod"):
```bash
kubectl apply -f manifests/serviceaccount.yaml
kubectl apply -f manifests/role.yaml
kubectl apply -f manifests/rolebinding.yaml
kubectl apply -f manifests/redis-leader.yaml
kubectl apply -f manifests/redis-leader-service.yaml
kubectl apply -f manifests/deployment-example.yaml
```

`manifests/redis-leader.yaml` and `manifests/redis-leader-service.yaml` run a single Redis master (one replica, no `--replicaof`) and expose it in the `default` namespace as a Service named `redis-leader`, so the DNS name used by the replicas resolves inside the example itself. That leader pod also runs the labeler sidecar, so you can see `redis-role=master` get applied and stay on the master; the replica deployment points its Redis containers at `redis-leader.default.svc.cluster.local:6379`, so all three replicas replicate from it, and their labeler sidecars remove the `redis-role` label if a pod is ever demoted or promoted back to a replica.

If you prefer to run Redis replication against your own Redis leader, apply `serviceaccount.yaml`, `role.yaml`, `rolebinding.yaml`, and `deployment-example.yaml`, change `--replicaof` in `deployment-example.yaml` to point at your leader's Service DNS name, and provide that Service (or a headless Service with a stable DNS name for a Redis master, e.g. a StatefulSet headless Service) yourself — the example deployment expects a `redis-leader` Service (or your own equivalent) to exist.

Key points for the deployment:
- Inject Redis connection info via args (address, TLS) and the credential via the `REDIS_PASSWORD` env var from a Secret (see "Example usage in a pod").
- Ensure the pod has RBAC to `get`/`update` its own Pod object (see provided Role/RoleBinding).
- Run alongside your Redis container (as sidecar) or as a dedicated pod that points to the Redis service.
- Run the labeler unprivileged (see "Running unprivileged" below).

### Running unprivileged
The labeler only reads Redis and calls the Kubernetes API with its mounted
credentials, so it needs no privileges and writes nothing to disk. Both
example deployments therefore declare this `securityContext` on the
`redis-master-label` container:
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
That is the shape the restricted Pod Security Standard requires, so the
manifests are accepted by a namespace enforcing it. The uid matches the image:
the final Dockerfile stage creates the `labeler` account (uid/gid 10001) and
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

If your Redis has no password, create the Secret with an empty value (`--from-literal=password=''`) or remove the `REDIS_PASSWORD` env entry from the manifests; the sidecar then connects unauthenticated.

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

## Operational notes
- Labels are applied when the instance is `master` and removed when it's no longer master.
- Update frequency controlled by `--check-interval`.
- If using TLS, set `--redis-tls` and `--redis-tls-skip-verify` if required.
- The health check endpoint can be used by Kubernetes liveness/readiness probes.

## License
Released under the [MIT License](LICENSE). The container image advertises the
same license through its `org.opencontainers.image.licenses` label (check it
with `docker inspect`), so the published image metadata and the `LICENSE` file
that grants the license agree.
