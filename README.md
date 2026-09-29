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
- `--redis-password` (default empty)
- `--redis-tls` (default `false`)
- `--redis-tls-skip-verify` (default `false`)
- `--label-key` (default `redis-role`)
- `--label-value` (default `master`)
- `--pod-name` (defaults to `HOSTNAME` env)
- `--pod-namespace` (defaults to `POD_NAMESPACE` env or `default`)
- `--check-interval` (default `10s`)
- `--health-port` (default `8080`)

Environment defaults:
- `HOSTNAME` used when `--pod-name` not provided.
- `POD_NAMESPACE` used when `--pod-namespace` not provided.

## Building locally
```bash
go build -o redis-master-label .
```

Note: running the binary outside a cluster is not supported. It builds its Kubernetes client exclusively from in-cluster config (`rest.InClusterConfig()`), so it exits with `failed to get in-cluster config` unless the environment provides the pod's service-account credentials (`KUBERNETES_SERVICE_HOST`, `KUBERNETES_SERVICE_PORT`, and the mounted service-account token). There is no kubeconfig fallback. Build locally to verify the code, then run the binary in-cluster via the manifests in `manifests/` (see "Kubernetes deployment" below).

## Container build,
```bash
# Build image using the provided Containerfile
podman build -t redis-master-label:latest .
# or
docker build -t redis-master-label:latest -f Containerfile .
```

## Kubernetes deployment
Manifests in `manifests/` provide an example service account, role, rolebinding, a Redis leader deployment and its Service, and a Redis replica deployment.

Apply them (edit image and args as needed):
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
- Mount/inject Redis connection info (address/password/TLS) as env/args.
- Ensure the pod has RBAC to `get`/`update` its own Pod object (see provided Role/RoleBinding).
- Run alongside your Redis container (as sidecar) or as a dedicated pod that points to the Redis service.

## Example usage in a pod
In your pod spec (sidecar pattern), set args or env:
```yaml
args:
  - "--redis-addr=$(REDIS_ADDR)"
  - "--redis-password=$(REDIS_PASSWORD)"
  - "--label-key=redis-role"
  - "--label-value=master"
env:
  - name: REDIS_ADDR
    value: "localhost:6379"
  - name: REDIS_PASSWORD
    valueFrom:
      secretKeyRef:
        name: redis-secret
        key: password
```

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