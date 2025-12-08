# redis-master-label

A small Kubernetes sidecar/utility that watches the Redis instance co-located with a pod, detects when it is the **master**, and applies a Kubernetes label to that pod (e.g., `redis-role=master`). This enables services or controllers to target the current Redis master via pod labels.

## What it does
- Connects to Redis and runs the `ROLE` command on a fixed interval.
- If the instance reports `master`, it patches the current pod with a configurable label key/value.
- Runs inside the cluster using in-cluster Kubernetes credentials.

## How it works
1. Starts with flags/env for Redis connection, label key/value, pod name/namespace, and check interval.
2. Builds a Kubernetes client from in-cluster config.
3. On each interval:
   - Executes `ROLE` against Redis.
   - If the role is `master`, fetches the current pod and ensures the label key/value is set.
4. Repeats forever.

## Configuration
Flags (all have sensible defaults):
- `--redis-addr` (default `localhost:6379`)
- `--redis-password` (default empty)
- `--redis-tls` (default `false`)
- `--label-key` (default `redis-role`)
- `--label-value` (default `master`)
- `--pod-name` (defaults to `HOSTNAME` env)
- `--pod-namespace` (defaults to `POD_NAMESPACE` env or `default`)
- `--check-interval` (default `10s`)

Environment defaults:
- `HOSTNAME` used when `--pod-name` not provided.
- `POD_NAMESPACE` used when `--pod-namespace` not provided.

## Building and running locally
```bash
go build -o redis-master-label .
./redis-master-label --redis-addr localhost:6379
```

## Container build
```bash
# Build image using the provided Containerfile
podman build -t redis-master-label:latest .
# or
docker build -t redis-master-label:latest -f Containerfile .
```

## Kubernetes deployment
Manifests in `manifests/` provide an example service account, role, rolebinding, and deployment.

Apply them (edit image and args as needed):
```bash
kubectl apply -f manifests/serviceaccount.yaml
kubectl apply -f manifests/role.yaml
kubectl apply -f manifests/rolebinding.yaml
kubectl apply -f manifests/deployment-example.yaml
```

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
- The provided Role/RoleBinding in `manifests/` scope this to the pod’s namespace.

## Operational notes
- Labels only applied when the instance is `master`; no action taken for replicas.
- Update frequency controlled by `--check-interval`.
- If using TLS, set `--redis-tls` and `--redis-tls-skip-verify` if required. 