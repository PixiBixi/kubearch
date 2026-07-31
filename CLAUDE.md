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

```
watcher → store ← collector → Prometheus /metrics
            ↑
         inspector (registry fetch)
```

- **`internal/store`** — thread-safe in-memory map `imageRef → ImageInfo` with pod reference counting. Key invariant: an image entry is added on first pod Add event and removed when the last pod using it is deleted. A `pending` set prevents duplicate concurrent inspections.

- **`internal/inspector`** — fetches OCI manifests via `go-containerregistry`. Resolves auth through `k8schain` (imagePullSecrets + ServiceAccount pull secrets + anonymous fallback). Handles both multi-arch (OCI image index / Docker manifest list) and single-arch images. Only reads manifests — never pulls layers.

- **`internal/watcher`** — Kubernetes shared informer on pods. `AddFunc` triggers image inspection goroutines (bounded to 10 concurrent via semaphore channel). `UpdateFunc` is intentionally omitted (container spec is immutable). `DeleteFunc` calls `store.RemovePod`. Handles `DeletedFinalStateUnknown` for missed delete events.

- **`internal/collector`** — implements `prometheus.Collector`. On each scrape, calls `store.Snapshot()` and emits three metric families: `kubearch_image_platform_supported`, `kubearch_image_platform_count`, `kubearch_image_multi_arch`.

## Key Design Decisions

- **No polling**: relies entirely on informer watch events. The store is a pure in-memory cache; no persistence.
- **Deduplication**: `store.TrackPodImage` is the single entry point for deciding whether to inspect. It returns `true` only if the image is neither known nor pending.
- **Orphan cleanup**: `store.SetImage` checks that at least one pod still references the image before storing (handles race between inspection and pod deletion).
- **Go version**: requires Go 1.26. The code uses `maps.Values` (1.23), `iter.Seq` (1.23), and per-iteration loop variable scoping (1.22).

## CI

All workflows live in `.github/workflows/`, actions are pinned by SHA:

- **CI** (`ci.yml`): `go mod verify`, build, `go test -race`
- **Lint** (`lint.yml`): golangci-lint v2.12.2 against `.golangci.yml`
- **github-actions** (`github-actions.yml`): zizmor audit of the workflows, SARIF to code scanning
- **govulncheck** (`govulncheck.yml`): reachable vulnerabilities in dependencies
- **Go format** / **markdownlint**: reviewdog inline suggestions on PRs
- **Release** (`release.yml`): see below

## Release

Automatic on push to `main` — `mathieudutour/github-tag-action` computes the next `vX.Y.Z` from conventional commits (`default_bump: false`, so a chore-only push releases nothing) and tags, then GoReleaser and the Helm chart push run in the same job. Manual `v*` tag push still works.

Renovate drives dependency releases: gomod minor → `feat(deps)` (minor), patch/digest → `fix(deps)` (patch), github-actions and dockerfile → `chore(deps)` (no release). Minor/patch/digest PRs automerge once CI is green.

GoReleaser (`.goreleaser.yml`) builds `linux/amd64` and `linux/arm64` binaries and, via ko, pushes multi-arch images to `ghcr.io/PixiBixi/kubearch`; the Helm chart goes to `ghcr.io/pixibixi/kubearch/charts`. Version/commit/date are injected via ldflags into `main.version`, `main.commit`, `main.date`.
