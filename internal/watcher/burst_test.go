package watcher

import (
	"context"
	"runtime"
	"strconv"
	"testing"

	"github.com/PixiBixi/kubearch/internal/inspector"
	"github.com/PixiBixi/kubearch/internal/store"
	"github.com/PixiBixi/kubearch/internal/types"
)

// burstSize approximates a mid-size cluster's initial informer sync: every pod
// is delivered at once, before anything has been inspected.
const burstSize = 2000

// TestPodBurst_BoundsGoroutines pins down the property the workqueue exists
// for: a cluster-wide burst must land in the queue, not in goroutine stacks.
// The pre-refactor code spawned one goroutine per image here.
func TestPodBurst_BoundsGoroutines(t *testing.T) {
	s := store.New()
	release := make(chan struct{})
	defer close(release) // let the blocked workers finish before cleanup waits on them

	w := newTestWatcher(s, &fakeInspector{
		fn: func(ctx context.Context, _ string, _ inspector.PodAuth) (string, []types.Platform, error) {
			select {
			case <-release:
			case <-ctx.Done():
			}
			return "sha256:abc", []types.Platform{{OS: "linux", Arch: "amd64"}}, nil
		},
	})

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	baseGoroutines := runtime.NumGoroutine()

	startWorkers(t, w)
	for i := range burstSize {
		n := strconv.Itoa(i)
		w.onPod(makePod("default", "pod-"+n, "registry.example.com/app-"+n+":v1"))
	}

	extraGoroutines := runtime.NumGoroutine() - baseGoroutines
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	heapKB := (float64(after.HeapAlloc) - float64(before.HeapAlloc)) / 1024
	stackKB := (float64(after.StackInuse) - float64(before.StackInuse)) / 1024

	t.Logf("burst of %d images: +%d goroutines, +%.0f KB heap, +%.0f KB goroutine stacks",
		burstSize, extraGoroutines, heapKB, stackKB)

	// Workers, plus a little slack for the runtime's own bookkeeping.
	if limit := w.workers + 5; extraGoroutines > limit {
		t.Errorf("burst spawned %d goroutines, want <= %d", extraGoroutines, limit)
	}
}
