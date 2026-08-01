# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

**kubearch** is a Kubernetes Prometheus exporter that reports CPU architectures supported by container images running in a cluster — without pulling image layers. It uses a shared informer (event-driven, no polling) and inspects each image only once via OCI manifest HEAD requests.

## Commands

```bash
# Build
make build          # current platform
make build-all      # all target platforms (linux/darwin × amd64/arm64)

# Test
make test           # go test -race ./...
go test -run TestFoo ./internal/store/   # single test / single package

# Lint
make lint           # golangci-lint run ./... (config: .golangci.yml)

# Docker
make docker         # local image (multi-stage Dockerfile)
make snapshot       # GoReleaser dry-run (requires goreleaser CLI)

# Release: automatic on push to main (conventional commits drive the bump).
# feat: -> minor, fix: -> patch. Manual tag still works as an escape hatch:
git tag -a vX.Y.Z -m "..."
git push origin vX.Y.Z
```

Pre-commit hooks run `go fmt`, `go vet`, `go mod tidy`, `go build`, and `staticcheck` automatically. Install once with `pre-commit install`.

## Architecture

Data flows through four packages wired together in `main.go`:

```text
watcher → workqueue → workers → store ← collector → Prometheus /metrics
                         ↑                  ↑
                  inspector (registry)   SelfMetrics
```

- **`internal/store`** — thread-safe `imageRef → ImageInfo` map with pod reference counting, kept in two directions: `podImages` (podRef → images) and `imagePods` (image → podRefs). The reverse index is what makes every operation independent of cluster size. Key invariant: an image entry exists while at least one pod references it, and is dropped when the last one goes. A `pending` set prevents duplicate concurrent inspections. `Stats()` feeds the self-monitoring gauges.

- **`internal/inspector`** — fetches OCI manifests via `go-containerregistry`. Resolves auth through `k8schain` (imagePullSecrets + ServiceAccount pull secrets + anonymous fallback), caching a `remote.Puller` per `(namespace, service account, pull secrets)` for 5 min with `singleflight` on misses and eviction on 401/403. Handles both multi-arch (OCI image index / Docker manifest list) and single-arch images. Only reads manifests — never pulls layers.

- **`internal/watcher`** — Kubernetes shared informer on pods feeding a `client-go` rate-limited workqueue drained by 10 workers. `AddFunc`/`UpdateFunc` call `onPod`, which reconciles the pod's image set; `sameImages` short-circuits the frequent status-only updates. Each attempt runs under a 30 s deadline, failures retry with exponential backoff up to 5 attempts. `DeleteFunc` calls `store.RemovePod` and handles `DeletedFinalStateUnknown`.

- **`internal/collector`** — two `prometheus.Collector` implementations. `Collector` emits the image families (`kubearch_image_platform_supported`, `_platform_count`, `_multi_arch`) from `store.Snapshot()`, with the `digest` label optional via `WithDigestLabel`. `SelfMetrics` emits the exporter's own health: build info, inspection counters/histogram, and scrape-time gauges for store size and queue depth.

## Key Design Decisions

- **No polling**: relies entirely on informer watch events. The store is a pure in-memory cache; no persistence.
- **Deduplication**: `store.SetPodImages` is the single entry point for deciding whether to inspect. It takes the pod's full image set (replace semantics) and returns only the refs that are neither known nor pending.
- **Pod specs are not immutable**: `kubectl set image` rewrites `spec.containers[*].image` and ephemeral containers appear on running pods, which is why `UpdateFunc` exists.
- **Orphan cleanup**: `store.SetImage` checks that at least one pod still references the image before storing (handles race between inspection and pod deletion).
- **Bounded everything**: goroutines (worker pool), inspection latency (deadline), retries (max attempts), credential staleness (TTL). See `PERFORMANCE.md` for the measurements behind these choices.
- **Go version**: requires Go 1.26. The code uses `maps.Values` (1.23), `iter.Seq`/`iter.Pull` (1.23), `WaitGroup.Go` (1.25), and per-iteration loop variable scoping (1.22).

## CI

All workflows live in `.github/workflows/`, actions are pinned by SHA:

- **CI** (`ci.yml`): `go mod verify`, build, `go test -race`
- **Lint** (`lint.yml`): golangci-lint v2.12.2 against `.golangci.yml`
- **github-actions** (`github-actions.yml`): zizmor audit of the workflows, SARIF to code scanning
- **govulncheck** (`govulncheck.yml`): reachable vulnerabilities in dependencies
- **Go format** / **markdownlint**: reviewdog inline suggestions on PRs
- **Release** (`release.yml`): see below

## Release

Automatic on push to `main` — [`svu`](https://github.com/caarlos0/svu) computes the next `vX.Y.Z` from the conventional commits since the last tag, the workflow creates the tag through the API, then GoReleaser and the Helm chart push run in the same job. Manual `v*` tag push still works as an escape hatch.

Only `feat:` (minor) and `fix:` (patch) cut a release; `--v0` keeps a breaking change from jumping to `v1.0.0` while the project is pre-1.0. **`perf:` does not release** — svu follows the Conventional Commits spec, where only `fix` and `feat` are normative. Use `fix:` if a change must ship on its own.

Renovate drives dependency releases: gomod minor → `feat(deps)` (minor), patch/digest → `fix(deps)` (patch), github-actions and dockerfile → `chore(deps)` (no release). Minor/patch/digest PRs automerge once CI is green.

GoReleaser (`.goreleaser.yml`) builds `linux/amd64` and `linux/arm64` binaries and, via ko, pushes multi-arch images to `ghcr.io/PixiBixi/kubearch`; the Helm chart goes to `ghcr.io/pixibixi/kubearch/charts`. Version/commit/date are injected via ldflags into `main.version`, `main.commit`, `main.date`.
