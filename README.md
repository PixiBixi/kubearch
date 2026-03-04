# kubearch

![Go version](https://img.shields.io/badge/go-1.26-blue)
![License](https://img.shields.io/github/license/PixiBixi/kubearch)
![Docker](https://img.shields.io/badge/ghcr.io-PixiBixi%2Fkubearch-blue)

**kubearch** is a Kubernetes Prometheus exporter that reports the CPU architectures supported by every container image running in your cluster — without pulling image layers.

It reads each pod's image references, fetches the OCI manifest list from the registry, and exposes the supported platforms as Prometheus metrics. Useful for tracking multi-arch readiness, identifying images blocking arm64 migrations, or auditing mixed-architecture clusters.

## How it works

```
K8s API (pod watch)
      │
      ▼  new image detected
Registry API (OCI manifest list, HEAD only)
      │
      ▼
Prometheus /metrics
```

- Watches pod `Add`/`Delete` events via a shared informer — **no polling**
- Inspects each image only **once** (in-memory store, invalidated when the last pod using it is deleted)
- Authenticates via `imagePullSecrets` and service account pull secrets, with anonymous fallback
- Supports public registries (Docker Hub, ghcr.io, gcr.io, ECR, ACR, ...) and private ones

## Metrics

| Metric | Type | Labels | Description |
|---|---|---|---|
| `kubearch_image_platform_supported` | Gauge | `image`, `digest`, `os`, `arch` | Always `1`. One time series per supported platform. |
| `kubearch_image_platform_count` | Gauge | `image`, `digest` | Total number of platforms the image supports. |
| `kubearch_image_multi_arch` | Gauge | `image`, `digest` | `1` if the image supports more than one platform, `0` otherwise. |

### Example output

```
kubearch_image_platform_supported{arch="amd64",digest="sha256:abc…",image="nginx:1.27",os="linux"} 1
kubearch_image_platform_supported{arch="arm64",digest="sha256:abc…",image="nginx:1.27",os="linux"} 1
kubearch_image_platform_supported{arch="arm",digest="sha256:abc…",image="nginx:1.27",os="linux"}   1

kubearch_image_platform_count{digest="sha256:abc…",image="nginx:1.27"} 3
kubearch_image_multi_arch{digest="sha256:abc…",image="nginx:1.27"}     1
```

### Useful PromQL queries

```promql
# Images that support only one platform (single-arch)
kubearch_image_multi_arch == 0

# Images without linux/arm64 support (arm64 migration blockers)
group by (image, digest) (kubearch_image_platform_count)
  unless on (image, digest)
  (kubearch_image_platform_supported{os="linux", arch="arm64"})

# Number of platforms per image, sorted
sort_desc(kubearch_image_platform_count)

# All platforms supported by images in a specific namespace
# (requires joining with kube_pod_container_info from kube-state-metrics)
kubearch_image_platform_supported * on (image) group_left()
  kube_pod_container_info{namespace="production"}
```

## Installation

### Helm (recommended)

```bash
helm upgrade --install kubearch oci://ghcr.io/PixiBixi/kubearch/charts/kubearch \
  --namespace monitoring \
  --create-namespace
```

With Prometheus Operator (kube-prometheus-stack):

```bash
helm upgrade --install kubearch oci://ghcr.io/PixiBixi/kubearch/charts/kubearch \
  --namespace monitoring \
  --create-namespace \
  --set serviceMonitor.enabled=true \
  --set serviceMonitor.labels.release=kube-prometheus-stack
```

Restricted to a single namespace:

```bash
helm upgrade --install kubearch oci://ghcr.io/PixiBixi/kubearch/charts/kubearch \
  --namespace monitoring \
  --create-namespace \
  --set watchNamespace=production
```

> When `watchNamespace` is set, a namespace-scoped `Role` is created instead of a `ClusterRole`.

## Helm values

| Parameter | Default | Description |
|---|---|---|
| `image.repository` | `ghcr.io/PixiBixi/kubearch` | Container image repository |
| `image.tag` | `""` (chart appVersion) | Image tag |
| `image.pullPolicy` | `IfNotPresent` | Image pull policy |
| `watchNamespace` | `""` | Namespace to watch. Empty = all namespaces (ClusterRole). Set = namespace-scoped Role. |
| `serviceAccount.create` | `true` | Create a dedicated ServiceAccount |
| `serviceAccount.annotations` | `{}` | Annotations for the ServiceAccount (e.g. Workload Identity) |
| `rbac.create` | `true` | Create the required Role/ClusterRole and binding |
| `serviceMonitor.enabled` | `false` | Create a Prometheus Operator ServiceMonitor |
| `serviceMonitor.interval` | `60s` | Scrape interval (data changes on pod events, 60s is enough) |
| `serviceMonitor.labels` | `{}` | Extra labels for the ServiceMonitor (to match your Prometheus selector) |
| `resources.requests.cpu` | `10m` | CPU request |
| `resources.requests.memory` | `64Mi` | Memory request |
| `resources.limits.memory` | `256Mi` | Memory limit |
| `nodeSelector` | `{}` | Node selector |
| `tolerations` | `[]` | Tolerations |
| `affinity` | `{}` | Affinity rules |

## CLI flags

| Flag | Default | Description |
|---|---|---|
| `-listen-address` | `:9101` | Address to expose Prometheus metrics on |
| `-namespace` | `""` | Kubernetes namespace to watch (empty = all) |
| `-kubeconfig` | `""` | Path to kubeconfig file (empty = auto-detect) |
| `-context` | `""` | Kubernetes context to use (empty = current context) |
| `-version` | — | Print version and exit |

## Standalone mode

kubearch can run outside a cluster against any context in your kubeconfig — useful for local development or one-shot audits.

**Auto-detection**: kubearch tries in-cluster config first. If it fails (i.e. not running inside a pod), it falls back to `~/.kube/config`.

```bash
# Use current kubectl context
./kubearch

# Target a specific context
./kubearch --context=prod-cluster

# Restrict to one namespace in a specific context
./kubearch --context=prod-cluster --namespace=kube-system

# Explicit kubeconfig path
./kubearch --kubeconfig=/path/to/config --context=staging
```

The startup log tells you which mode is active:

```
level=INFO msg="config: in-cluster"
# or
level=INFO msg="config: kubeconfig" context=current
```

## Private registries

kubearch resolves credentials automatically from:

1. The pod's `imagePullSecrets`
2. The pod's ServiceAccount pull secrets
3. Anonymous (public images)

No additional configuration is needed as long as the pod spec already has valid pull secrets.

## Development

```bash
# Build for current platform
make build

# Run locally against current kubectl context
./kubearch --namespace=default

# Lint
make lint

# Docker image (local)
make docker

# GoReleaser dry-run
make snapshot
```

### Project structure

```
.
├── main.go                         # entry point, flags, wiring
├── internal/
│   ├── store/store.go              # thread-safe image → platforms store
│   ├── inspector/inspector.go      # OCI manifest inspection (go-containerregistry)
│   ├── watcher/watcher.go          # Kubernetes pod informer
│   └── collector/collector.go      # prometheus.Collector implementation
├── charts/kubearch/                # Helm chart
├── deploy/                         # raw Kubernetes manifests
├── Dockerfile                      # multi-stage build (local dev)
└── Dockerfile.release              # slim image used by GoReleaser
```

## License

MIT — see [LICENSE](LICENSE).
