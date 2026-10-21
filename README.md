# kubearch

Prometheus exporter that reports the CPU architectures of the container images running in your Kubernetes cluster, without pulling a single layer.

[![CI](https://github.com/PixiBixi/kubearch/actions/workflows/ci.yml/badge.svg)](https://github.com/PixiBixi/kubearch/actions/workflows/ci.yml)
[![Lint](https://github.com/PixiBixi/kubearch/actions/workflows/lint.yml/badge.svg)](https://github.com/PixiBixi/kubearch/actions/workflows/lint.yml)
[![Release](https://img.shields.io/github/v/release/PixiBixi/kubearch)](https://github.com/PixiBixi/kubearch/releases/latest)
[![Go version](https://img.shields.io/github/go-mod/go-version/PixiBixi/kubearch)](go.mod)
[![License](https://img.shields.io/github/license/PixiBixi/kubearch)](LICENSE)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/PixiBixi/kubearch/badge)](https://scorecard.dev/viewer/?uri=github.com/PixiBixi/kubearch)

## Why kubearch

- Track multi-arch readiness across every namespace.
- Find the images that block an arm64 migration.
- Audit mixed-architecture clusters.
- Event-driven: a shared informer watches pods, there is no polling.
- Each image is inspected once, through OCI manifest requests only, never layers.
- Works with public registries (Docker Hub, ghcr.io, gcr.io, ECR, ACR, ...) and private ones, using the pods' own pull secrets.

## Quick start

```bash
helm upgrade --install kubearch oci://ghcr.io/pixibixi/kubearch/charts/kubearch \
  --namespace monitoring \
  --create-namespace
```

Metrics are served on port `9101` at `/metrics`. `/healthz` is a liveness check (200 once the process is up). `/readyz` returns 200 once the pod informer cache has synced, 503 before.

## Installation

### Helm

```bash
# Cluster-wide, with a Prometheus Operator ServiceMonitor
helm upgrade --install kubearch oci://ghcr.io/pixibixi/kubearch/charts/kubearch \
  --namespace monitoring \
  --create-namespace \
  --set serviceMonitor.enabled=true \
  --set serviceMonitor.labels.release=kube-prometheus-stack

# Restricted to a single namespace (namespace-scoped Role instead of ClusterRole)
helm upgrade --install kubearch oci://ghcr.io/pixibixi/kubearch/charts/kubearch \
  --namespace monitoring \
  --create-namespace \
  --set watchNamespace=production
```

| Value | Default | Description |
| --- | --- | --- |
| `image.repository` | `ghcr.io/PixiBixi/kubearch` | Container image repository |
| `image.tag` | `""` (chart appVersion) | Image tag |
| `image.pullPolicy` | `IfNotPresent` | Image pull policy |
| `watchNamespace` | `""` | Namespace to watch. Empty watches all namespaces (ClusterRole). |
| `extraArgs` | `[]` | Extra CLI flags for the container, e.g. `["-digest-label=false"]` |
| `serviceAccount.create` | `true` | Create a dedicated ServiceAccount |
| `serviceAccount.annotations` | `{}` | ServiceAccount annotations (e.g. Workload Identity) |
| `rbac.create` | `true` | Create the Role or ClusterRole and its binding |
| `rbac.pullSecrets` | `true` | Grant `get` on `secrets` and `serviceaccounts` to resolve pull secrets. `false` means anonymous registry access only. |
| `serviceMonitor.enabled` | `false` | Create a Prometheus Operator ServiceMonitor |
| `serviceMonitor.interval` | `60s` | Scrape interval |
| `serviceMonitor.labels` | `{}` | Extra ServiceMonitor labels, to match your Prometheus selector |
| `resources.requests.cpu` | `10m` | CPU request |
| `resources.requests.memory` | `64Mi` | Memory request |
| `resources.limits.memory` | `256Mi` | Memory limit |
| `nodeSelector` | `{}` | Node selector |
| `tolerations` | `[]` | Tolerations |
| `affinity` | `{}` | Affinity rules |

### Container image

Multi-arch images (`linux/amd64`, `linux/arm64`) are published to `ghcr.io/PixiBixi/kubearch`.

### Binaries

Archives for `linux` and `darwin` on `amd64` and `arm64` are attached to each [GitHub release](https://github.com/PixiBixi/kubearch/releases).

### RBAC

| API group | Resources | Verbs | Used for |
| --- | --- | --- | --- |
| `""` | `pods` | `get`, `list`, `watch` | Discovering the images in use |
| `""` | `secrets`, `serviceaccounts` | `get` | Resolving `imagePullSecrets` and ServiceAccount pull secrets |

The `secrets` and `serviceaccounts` rule is dropped when `rbac.pullSecrets=false`. The chart grants these through a `ClusterRole`, or a namespace-scoped `Role` when `watchNamespace` is set.

## Configuration

| Flag | Default | Description |
| --- | --- | --- |
| `-listen-address` | `:9101` | Address to expose Prometheus metrics on |
| `-namespace` | `""` | Kubernetes namespace to watch (empty = all namespaces) |
| `-kubeconfig` | `""` | Path to kubeconfig file (empty = auto-detect) |
| `-context` | `""` | Kubernetes context to use (empty = current context) |
| `-digest-label` | `true` | Include the `digest` label on image metrics. Disable to avoid a new time series per image rebuild of the same tag. |
| `-reinspect-interval` | `0` | Re-inspect known images this often to follow mutable tags (0 = inspect each image once) |
| `-version` | - | Print version information and exit |

### Running outside a cluster

kubearch tries the in-cluster config first and falls back to your kubeconfig, which is handy for local development and one-shot audits.

```bash
# Current kubectl context
./kubearch

# Specific context, one namespace
./kubearch --context=prod-cluster --namespace=kube-system

# Explicit kubeconfig
./kubearch --kubeconfig=/path/to/config --context=staging
```

### Private registries

Credentials are resolved from the pod's `imagePullSecrets`, then its ServiceAccount pull secrets, then anonymously. No extra configuration is needed when the pod spec already has valid pull secrets.

## Metrics

Image metrics:

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `kubearch_image_platform_supported` | Gauge | `image`, `digest`, `os`, `arch`, `variant` | Always `1`. One series per supported platform. `variant` is empty when the platform has none (e.g. `v7` on `linux/arm`). |
| `kubearch_image_platform_count` | Gauge | `image`, `digest` | Number of platforms the image supports. |
| `kubearch_image_multi_arch` | Gauge | `image`, `digest` | `1` if the image supports more than one platform, `0` otherwise. |

Exporter self-monitoring metrics:

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `kubearch_build_info` | Gauge | `version`, `commit`, `go_version` | Always `1`. Build metadata of the running binary. |
| `kubearch_inspections_total` | Counter | `result` | Inspection attempts, `result` is `success` or `failure`. |
| `kubearch_inspection_retries_total` | Counter | - | Failed inspections requeued for another attempt. |
| `kubearch_inspection_duration_seconds` | Histogram | - | Latency of registry calls. |
| `kubearch_store_images` | Gauge | - | Images with a known result. |
| `kubearch_store_pending_inspections` | Gauge | - | Inspections in flight. |
| `kubearch_store_pods` | Gauge | - | Pods currently tracked. |
| `kubearch_queue_depth` | Gauge | - | Images waiting in the work queue. |

Notes:

- `kubearch_inspections_total` appears only after the first inspection attempt.
- The `digest` label creates a new series whenever an image is rebuilt under the same tag. Pass `-digest-label=false` to drop it. It changes nothing if your pods already pin images by digest (`repo:tag@sha256:...`), because the digest is then part of the `image` label.

Example output:

```text
kubearch_image_platform_supported{arch="amd64",digest="sha256:abc...",image="nginx:1.27",os="linux",variant=""} 1
kubearch_image_platform_supported{arch="arm64",digest="sha256:abc...",image="nginx:1.27",os="linux",variant=""} 1
kubearch_image_platform_count{digest="sha256:abc...",image="nginx:1.27"} 2
kubearch_image_multi_arch{digest="sha256:abc...",image="nginx:1.27"} 1
```

## Example queries

```promql
# Single-arch images
kubearch_image_multi_arch == 0

# Images without linux/arm64 support (arm64 migration blockers)
group by (image, digest) (kubearch_image_platform_count)
  unless on (image, digest)
  (kubearch_image_platform_supported{os="linux", arch="arm64"})

# Platforms per image, highest first
sort_desc(kubearch_image_platform_count)

# Platforms of the images running in one namespace (needs kube-state-metrics)
kubearch_image_platform_supported * on (image) group_left()
  kube_pod_container_info{namespace="production"}
```

Watching the exporter itself:

```promql
# Inspection failure ratio
sum(rate(kubearch_inspections_total{result="failure"}[15m]))
  / sum(rate(kubearch_inspections_total[15m]))

# Pods tracked but no image ever resolved
kubearch_store_pods > 0 and kubearch_store_images == 0

# Backlog not draining: registry unreachable or rate-limiting
kubearch_queue_depth > 0 and rate(kubearch_inspection_retries_total[10m]) > 0

# p99 registry latency
histogram_quantile(0.99, sum by (le) (rate(kubearch_inspection_duration_seconds_bucket[15m])))
```

## Development

| Command | Description |
| --- | --- |
| `make build` | Build for the current platform |
| `make build-all` | Build `linux` and `darwin` on `amd64` and `arm64` |
| `make test` | `go test -race ./...` |
| `make lint` | `golangci-lint run ./...` |
| `make docker` | Build a local Docker image |
| `make snapshot` | GoReleaser dry-run |

Architecture and design decisions are in [AGENTS.md](AGENTS.md), measurements in [PERFORMANCE.md](PERFORMANCE.md). Releases are automatic from [Conventional Commits](https://www.conventionalcommits.org/) on `main`: `feat:` cuts a minor, `fix:` a patch.

## License

MIT. See [LICENSE](LICENSE).
