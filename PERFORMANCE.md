# Performance

Measured changes to kubearch's hot paths, with the method used to obtain each
number. Everything here is reproducible from the repository:

```bash
go test -run='^$' -bench=. -benchtime=1000x -count=6 ./internal/store/
go test -run TestPodBurst -v ./internal/watcher/
go test -run TestInspect_ReusesCredentials -v ./internal/inspector/
```

Before/after figures were produced by running the *same* workload against the
commit preceding each change, in a separate git worktree, and comparing with
[`benchstat`](https://pkg.go.dev/golang.org/x/perf/cmd/benchstat) (6 runs).

Environment: Apple M4, darwin/arm64, Go 1.26. Absolute numbers are
machine-specific; the ratios and, more importantly, the *shape* of the curves
are not.

## Summary

| Change | Hot path | Result |
| --- | --- | --- |
| [`perf(store)`](#reverse-index-in-the-store) | pod delete, inspection result | O(pods) → O(1) |
| [`fix(watcher)`](#work-queue-instead-of-goroutine-per-image) | informer initial sync | 2000 goroutines → 10 |
| [`perf(inspector)`](#credential-and-registry-client-caching) | every inspection | N API-server round-trips → 1 per credential set |

## Reverse index in the store

`SetImage` and `RemovePod` answered "is this image still referenced?" by walking
the whole `podRef → images` map, under the write lock that `Snapshot` (and
therefore every Prometheus scrape) contends on. An `imageRef → podRefs` index
makes the answer a single map lookup.

The percentages matter less than the slope. Before, the cost tracked cluster
size; after, it is flat.

```text
                        before        after       delta
SetImage/pods=100      101.4 ns      58.2 ns     -42.6%
SetImage/pods=1000     119.9 ns      57.6 ns     -52.0%
SetImage/pods=10000    325.2 ns      59.5 ns     -81.7%

RemovePod/pods=100     697.7 ns     627.0 ns     -10.1%
RemovePod/pods=1000    918.7 ns     684.3 ns     -25.5%
RemovePod/pods=10000  1475.5 ns     751.8 ns     -49.1%
```

`RemovePod` does not fall further because the benchmark re-registers the pod
between iterations: the `StopTimer`/`StartTimer` pair puts a ~600 ns floor under
the measurement. The operation itself is well below that.

### The cost side

`SetPodImages` (the pod-add path) got slower, and that is inherent: it now
maintains a second index.

```text
SetPodImages/pods=100   374 ns → 570 ns   (+52%, +82% bytes)
SetPodImages/pods=1000  385 ns → 588 ns   (+53%)
```

The trade is ~200 ns paid once per pod add, against ~266 ns saved per completed
inspection and ~724 ns per pod delete at 10k pods, plus the removal of the
growth curve. Pod adds and deletes come in equal numbers on a churning cluster,
so the balance is positive, and the index is a prerequisite for the replace
semantics that fixed the missed pod-update events.

Memory: roughly 50 bytes per pod↔image edge, so ~1.5 MB for 10k pods running
3 images each.

`Snapshot` (the scrape path) is unchanged: one allocation, same volume.

## Work queue instead of goroutine-per-image

The semaphore bounded concurrent registry fetches, not goroutines. The
informer's initial sync delivers every pod at once, so a cluster start spawned
one goroutine per image, all immediately blocked on the semaphore.

`TestPodBurst_BoundsGoroutines` delivers 2000 pods with nothing draining the
queue, and measures the footprint:

```text
                       before      after
goroutines             +2000        +10
goroutine stacks     +4160 KB     +64 KB
heap                 +4682 KB   +2662 KB
─────────────────────────────────────────
total                  ~8.6 MB    ~2.7 MB
```

The backlog now lives in the queue rather than in scheduler state. The test is a
regression guard, not just a probe: it fails if the goroutine count ever exceeds
the worker count again (verified by running it against the previous commit,
where it reports `burst spawned 2000 goroutines, want <= 15`).

The same change also bounded inspection latency (30 s deadline per attempt,
previously unbounded) and added exponential-backoff retries, but those are
correctness properties rather than throughput ones.

## Credential and registry client caching

`authnk8s.New` is not a local call: it issues one ServiceAccount `GET` plus one
`GET` per `imagePullSecret`, live against the API server. It was called once per
image.

`TestInspect_ReusesCredentialsAcrossImages` inspects three images sharing one
credential set and counts the calls recorded by the fake clientset:

```text
before: 3 ServiceAccount GETs for 3 images
after:  1
```

Extrapolated to a 2000-image cluster start, that is 2000 reads on Secrets and
ServiceAccounts collapsing to one per distinct `(namespace, service account,
pull secrets)` tuple, typically one per workload. `singleflight` keeps the
concurrent misses of a cold start from each triggering their own build.

The cache holds a `remote.Puller`, not just a keychain, so go-containerregistry
also reuses the registry auth tokens it has already negotiated; the previous
`remote.Get` renegotiated one per image, which is what trips registry rate
limits on large clusters.

Staleness is bounded by a 5 minute TTL, plus eviction on a 401/403 so a rotated
pull secret is picked up on the next retry rather than at the end of the TTL.

## What was deliberately not optimised

- **`Snapshot` copies the whole image set on every scrape.** Returning pointers
  would save ~48 bytes per image, but the copy is what lets the collector emit
  metrics without holding the store's read lock across channel sends: the lock
  would otherwise be held for the duration of the scrape.
- **`uniqueImages` allocates a dedup map per pod event.** Pods have a handful of
  containers; a slice-based dedup would be marginally faster but the allocation
  is dwarfed by the informer's own per-event work.
