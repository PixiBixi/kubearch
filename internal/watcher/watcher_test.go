package watcher

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"github.com/PixiBixi/kubearch/internal/inspector"
	"github.com/PixiBixi/kubearch/internal/store"
	"github.com/PixiBixi/kubearch/internal/types"
)

// fakeInspector implements the Inspector interface for testing.
type fakeInspector struct {
	mu sync.Mutex
	fn func(ctx context.Context, imageRef string, auth inspector.PodAuth) (string, []types.Platform, error)
}

func (f *fakeInspector) Inspect(ctx context.Context, imageRef string, auth inspector.PodAuth) (string, []types.Platform, error) {
	f.mu.Lock()
	fn := f.fn
	f.mu.Unlock()
	return fn(ctx, imageRef, auth)
}

// fakeMetrics records calls instead of exporting them, so tests can assert on
// what the watcher reports without pulling in the collector package.
type fakeMetrics struct {
	mu          sync.Mutex
	inspections []string // recorded results, in call order
	retries     int
}

func (f *fakeMetrics) ObserveInspection(result string, _ time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inspections = append(f.inspections, result)
}

func (f *fakeMetrics) ObserveRetry() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retries++
}

func (f *fakeMetrics) snapshot() (inspections []string, retries int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.inspections), f.retries
}

func newTestWatcher(s *store.Store, insp Inspector) *Watcher {
	return newTestWatcherWithMetrics(s, insp, nil)
}

func newTestWatcherWithMetrics(s *store.Store, insp Inspector, metrics Metrics) *Watcher {
	w := New(k8sfake.NewClientset(), "default", s, insp, slog.New(slog.NewTextHandler(io.Discard, nil)), metrics)
	// Keep the backoff curve, compress it: retry behaviour is what we assert on,
	// not the wall-clock delay.
	w.queue = workqueue.NewTypedRateLimitingQueue(
		workqueue.NewTypedItemExponentialFailureRateLimiter[string](time.Millisecond, 20*time.Millisecond),
	)
	return w
}

// startWorkers runs the inspection workers for the duration of the test.
func startWorkers(t *testing.T, w *Watcher) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stop := w.startWorkers(ctx)
	t.Cleanup(func() {
		cancel()
		stop()
	})
}

// waitFor blocks until cond holds or the test times out.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func makePod(ns, name string, images ...string) *corev1.Pod {
	containers := make([]corev1.Container, len(images))
	for i, img := range images {
		containers[i] = corev1.Container{Image: img}
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       corev1.PodSpec{Containers: containers},
	}
}

// --- onPod / inspection loop ---

func TestOnPod_StoresImage(t *testing.T) {
	s := store.New()
	w := newTestWatcher(s, &fakeInspector{
		fn: func(_ context.Context, _ string, _ inspector.PodAuth) (string, []types.Platform, error) {
			return "sha256:deadbeef", []types.Platform{{OS: "linux", Arch: "amd64"}}, nil
		},
	})
	startWorkers(t, w)

	w.onPod(makePod("default", "pod1", "nginx:latest"))

	waitFor(t, "the image to land in the store", func() bool { return len(s.Snapshot()) == 1 })

	imgs := s.Snapshot()
	if imgs[0].Digest != "sha256:deadbeef" {
		t.Errorf("unexpected digest: %s", imgs[0].Digest)
	}
	if len(imgs[0].Platforms) != 1 || imgs[0].Platforms[0].Arch != "amd64" {
		t.Errorf("unexpected platforms: %v", imgs[0].Platforms)
	}
}

func TestOnPod_AlreadyKnownSkipsReinspection(t *testing.T) {
	s := store.New()
	var mu sync.Mutex
	calls := 0

	w := newTestWatcher(s, &fakeInspector{
		fn: func(_ context.Context, _ string, _ inspector.PodAuth) (string, []types.Platform, error) {
			mu.Lock()
			calls++
			mu.Unlock()
			return "sha256:abc", []types.Platform{{OS: "linux", Arch: "amd64"}}, nil
		},
	})
	startWorkers(t, w)

	w.onPod(makePod("default", "pod1", "nginx:latest"))
	waitFor(t, "the first inspection", func() bool { return len(s.Snapshot()) == 1 })

	w.onPod(makePod("default", "pod2", "nginx:latest"))
	time.Sleep(100 * time.Millisecond) // let a spurious inspection show up if any

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("expected 1 inspection call, got %d", calls)
	}
}

// A pod spec can be updated in place; the new image must be inspected and the
// replaced one dropped.
func TestOnPod_UpdateSwapsImage(t *testing.T) {
	s := store.New()
	w := newTestWatcher(s, &fakeInspector{
		fn: func(_ context.Context, imageRef string, _ inspector.PodAuth) (string, []types.Platform, error) {
			return "sha256:" + imageRef, []types.Platform{{OS: "linux", Arch: "amd64"}}, nil
		},
	})
	startWorkers(t, w)

	w.onPod(makePod("default", "pod1", "nginx:1.26"))
	waitFor(t, "the first image", func() bool { return len(s.Snapshot()) == 1 })

	w.onPod(makePod("default", "pod1", "nginx:1.27"))
	waitFor(t, "the swapped image", func() bool {
		snap := s.Snapshot()
		return len(snap) == 1 && snap[0].Ref == "nginx:1.27"
	})
}

func TestInspect_RetriesUntilSuccess(t *testing.T) {
	s := store.New()
	var mu sync.Mutex
	calls := 0

	w := newTestWatcher(s, &fakeInspector{
		fn: func(_ context.Context, _ string, _ inspector.PodAuth) (string, []types.Platform, error) {
			mu.Lock()
			calls++
			n := calls
			mu.Unlock()
			if n < 3 {
				return "", nil, errors.New("registry unavailable")
			}
			return "sha256:abc", []types.Platform{{OS: "linux", Arch: "amd64"}}, nil
		},
	})
	startWorkers(t, w)

	w.onPod(makePod("default", "pod1", "flaky:image"))

	waitFor(t, "the retried inspection to succeed", func() bool { return len(s.Snapshot()) == 1 })

	mu.Lock()
	defer mu.Unlock()
	if calls != 3 {
		t.Errorf("expected 3 attempts, got %d", calls)
	}
}

func TestInspect_ReportsSuccessMetric(t *testing.T) {
	s := store.New()
	metrics := &fakeMetrics{}
	w := newTestWatcherWithMetrics(s, &fakeInspector{
		fn: func(_ context.Context, _ string, _ inspector.PodAuth) (string, []types.Platform, error) {
			return "sha256:abc", []types.Platform{{OS: "linux", Arch: "amd64"}}, nil
		},
	}, metrics)
	startWorkers(t, w)

	w.onPod(makePod("default", "pod1", "nginx:latest"))
	waitFor(t, "the inspection to succeed", func() bool { return len(s.Snapshot()) == 1 })

	inspections, retries := metrics.snapshot()
	if !slices.Equal(inspections, []string{"success"}) {
		t.Errorf("recorded inspections = %v, want [success]", inspections)
	}
	if retries != 0 {
		t.Errorf("recorded retries = %d, want 0", retries)
	}
}

func TestInspect_ReportsFailureAndRetryMetrics(t *testing.T) {
	s := store.New()
	metrics := &fakeMetrics{}
	w := newTestWatcherWithMetrics(s, &fakeInspector{
		fn: func(_ context.Context, _ string, _ inspector.PodAuth) (string, []types.Platform, error) {
			return "", nil, errors.New("registry unavailable")
		},
	}, metrics)
	startWorkers(t, w)

	w.onPod(makePod("default", "pod1", "broken:image"))

	waitFor(t, "retries to be exhausted", func() bool {
		inspections, _ := metrics.snapshot()
		return len(inspections) == maxAttempts
	})

	inspections, retries := metrics.snapshot()
	for _, result := range inspections {
		if result != "failure" {
			t.Errorf("recorded inspections = %v, want all \"failure\"", inspections)
			break
		}
	}
	// Every attempt but the last (which gives up instead) is followed by a retry.
	if want := maxAttempts - 1; retries != want {
		t.Errorf("recorded retries = %d, want %d", retries, want)
	}
}

func TestInspect_GivesUpAfterMaxAttempts(t *testing.T) {
	s := store.New()
	var mu sync.Mutex
	calls := 0

	w := newTestWatcher(s, &fakeInspector{
		fn: func(_ context.Context, _ string, _ inspector.PodAuth) (string, []types.Platform, error) {
			mu.Lock()
			calls++
			mu.Unlock()
			return "", nil, errors.New("always down")
		},
	})
	startWorkers(t, w)

	w.onPod(makePod("default", "pod1", "broken:image"))

	waitFor(t, "the retries to be exhausted", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return calls == maxAttempts
	})
	time.Sleep(100 * time.Millisecond) // no further attempt must follow

	mu.Lock()
	defer mu.Unlock()
	if calls != maxAttempts {
		t.Errorf("expected exactly %d attempts, got %d", maxAttempts, calls)
	}
	if n := len(s.Snapshot()); n != 0 {
		t.Errorf("expected 0 images in store after failure, got %d", n)
	}
	// Pending must be cleared, so a later pod event can retry the image.
	if _, pending, _ := s.Stats(); pending != 0 {
		t.Errorf("expected 0 pending images after giving up, got %d", pending)
	}
}

// A registry that accepts the connection and never answers must not pin a
// worker: every inspection runs under a deadline.
func TestInspect_AppliesTimeout(t *testing.T) {
	s := store.New()
	deadlines := make(chan time.Duration, 1)

	w := newTestWatcher(s, &fakeInspector{
		fn: func(ctx context.Context, _ string, _ inspector.PodAuth) (string, []types.Platform, error) {
			deadline, ok := ctx.Deadline()
			if !ok {
				deadlines <- 0
				return "", nil, errors.New("no deadline")
			}
			deadlines <- time.Until(deadline)
			return "sha256:abc", []types.Platform{{OS: "linux", Arch: "amd64"}}, nil
		},
	})
	startWorkers(t, w)

	w.onPod(makePod("default", "pod1", "slow:image"))

	select {
	case d := <-deadlines:
		if d <= 0 || d > inspectTimeout {
			t.Errorf("inspection deadline = %v, want in (0, %v]", d, inspectTimeout)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the inspection")
	}
}

func TestOnDelete_RemovesPodFromStore(t *testing.T) {
	s := store.New()
	w := newTestWatcher(s, &fakeInspector{
		fn: func(_ context.Context, _ string, _ inspector.PodAuth) (string, []types.Platform, error) {
			return "sha256:abc", []types.Platform{{OS: "linux", Arch: "amd64"}}, nil
		},
	})

	pod := makePod("default", "pod1", "nginx:latest")

	// Seed the store directly so we can verify removal.
	s.SetPodImages("default/pod1", []string{"nginx:latest"})
	s.SetImage("nginx:latest", "sha256:abc", []types.Platform{{OS: "linux", Arch: "amd64"}})

	if n := len(s.Snapshot()); n != 1 {
		t.Fatalf("expected 1 image before deletion, got %d", n)
	}

	w.onDelete(pod)

	if n := len(s.Snapshot()); n != 0 {
		t.Errorf("expected 0 images after pod deletion, got %d", n)
	}
}

func TestWorkers_BoundConcurrency(t *testing.T) {
	s := store.New()

	const numImages = 20
	images := make([]string, numImages)
	for i := range images {
		images[i] = "image-" + string(rune('a'+i)) + ":latest"
	}

	var mu sync.Mutex
	active, peak := 0, 0

	w := newTestWatcher(s, &fakeInspector{
		fn: func(_ context.Context, _ string, _ inspector.PodAuth) (string, []types.Platform, error) {
			mu.Lock()
			active++
			if active > peak {
				peak = active
			}
			mu.Unlock()

			time.Sleep(20 * time.Millisecond)

			mu.Lock()
			active--
			mu.Unlock()

			return "sha256:ok", []types.Platform{{OS: "linux", Arch: "amd64"}}, nil
		},
	})
	startWorkers(t, w)

	w.onPod(makePod("default", "big-pod", images...))

	waitFor(t, "all inspections", func() bool { return len(s.Snapshot()) == numImages })

	mu.Lock()
	defer mu.Unlock()
	if peak > w.workers {
		t.Errorf("peak concurrency %d exceeded the %d workers", peak, w.workers)
	}
}

// --- sameImages ---

func TestSameImages_StatusOnlyChange(t *testing.T) {
	a := makePod("default", "pod1", "nginx:1.26")
	b := makePod("default", "pod1", "nginx:1.26")
	b.Status.Phase = corev1.PodRunning

	if !sameImages(a, b) {
		t.Error("a status-only update must not look like an image change")
	}
}

func TestSameImages_ImageSwapped(t *testing.T) {
	a := makePod("default", "pod1", "nginx:1.26")
	b := makePod("default", "pod1", "nginx:1.27")

	if sameImages(a, b) {
		t.Error("a rewritten image must be detected")
	}
}

func TestSameImages_EphemeralContainerAdded(t *testing.T) {
	a := makePod("default", "pod1", "nginx:1.26")
	b := makePod("default", "pod1", "nginx:1.26")
	b.Spec.EphemeralContainers = []corev1.EphemeralContainer{
		{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Image: "busybox:1"}},
	}

	if sameImages(a, b) {
		t.Error("an ephemeral container added to a running pod must be detected")
	}
}

// --- toPod ---

func TestToPod_DirectPod(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "ns"}}
	got, ok := toPod(pod)
	if !ok {
		t.Fatal("expected ok=true for *corev1.Pod")
	}
	if got.Name != "p1" {
		t.Errorf("Name = %q, want %q", got.Name, "p1")
	}
}

func TestToPod_DeletedFinalStateUnknown(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p2", Namespace: "ns"}}
	tombstone := cache.DeletedFinalStateUnknown{Key: "ns/p2", Obj: pod}
	got, ok := toPod(tombstone)
	if !ok {
		t.Fatal("expected ok=true for DeletedFinalStateUnknown wrapping *corev1.Pod")
	}
	if got.Name != "p2" {
		t.Errorf("Name = %q, want %q", got.Name, "p2")
	}
}

func TestToPod_UnknownType(t *testing.T) {
	_, ok := toPod("not a pod")
	if ok {
		t.Error("expected ok=false for non-pod type")
	}
}

func TestToPod_TombstoneWithNonPodObj(t *testing.T) {
	tombstone := cache.DeletedFinalStateUnknown{Key: "ns/x", Obj: "not a pod"}
	_, ok := toPod(tombstone)
	if ok {
		t.Error("expected ok=false for tombstone wrapping non-pod")
	}
}

// --- uniqueImages ---

func TestUniqueImages_Deduplication(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{Image: "nginx:1"}},
			Containers:     []corev1.Container{{Image: "nginx:1"}, {Image: "redis:7"}},
		},
	}
	var images []string
	for img := range uniqueImages(pod) {
		images = append(images, img)
	}
	if len(images) != 2 {
		t.Errorf("expected 2 unique images, got %d: %v", len(images), images)
	}
}

func TestUniqueImages_SkipsEmpty(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Image: ""}, {Image: "nginx:1"}},
		},
	}
	var images []string
	for img := range uniqueImages(pod) {
		images = append(images, img)
	}
	if slices.Contains(images, "") {
		t.Error("uniqueImages should skip empty image refs")
	}
	if len(images) != 1 {
		t.Errorf("expected 1 image, got %d: %v", len(images), images)
	}
}

func TestUniqueImages_AllContainerTypes(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			InitContainers:      []corev1.Container{{Image: "init:1"}},
			Containers:          []corev1.Container{{Image: "app:1"}},
			EphemeralContainers: []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Image: "debug:1"}}},
		},
	}
	var images []string
	for img := range uniqueImages(pod) {
		images = append(images, img)
	}
	if len(images) != 3 {
		t.Errorf("expected 3 images (one per container type), got %d: %v", len(images), images)
	}
}

func TestUniqueImages_EmptyPod(t *testing.T) {
	pod := &corev1.Pod{}
	var images []string
	for img := range uniqueImages(pod) {
		images = append(images, img)
	}
	if len(images) != 0 {
		t.Errorf("expected 0 images for empty pod, got %d", len(images))
	}
}

// --- containerImages ---

func TestContainerImages_Order(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{Image: "init:1"}, {Image: "init:2"}},
			Containers:     []corev1.Container{{Image: "app:1"}},
			EphemeralContainers: []corev1.EphemeralContainer{
				{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Image: "debug:1"}},
			},
		},
	}
	var got []string
	for img := range containerImages(pod) {
		got = append(got, img)
	}
	want := []string{"init:1", "init:2", "app:1", "debug:1"}
	if !slices.Equal(got, want) {
		t.Errorf("containerImages order: got %v, want %v", got, want)
	}
}

// --- pullSecretNames ---

func TestPullSecretNames(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			ImagePullSecrets: []corev1.LocalObjectReference{
				{Name: "secret-a"},
				{Name: "secret-b"},
			},
		},
	}
	got := pullSecretNames(pod)
	want := []string{"secret-a", "secret-b"}
	if !slices.Equal(got, want) {
		t.Errorf("pullSecretNames = %v, want %v", got, want)
	}
}

func TestPullSecretNames_Empty(t *testing.T) {
	pod := &corev1.Pod{}
	got := pullSecretNames(pod)
	if len(got) != 0 {
		t.Errorf("expected empty slice for pod with no pull secrets, got %v", got)
	}
}

// --- shortDigest ---

func TestShortDigest_Long(t *testing.T) {
	digest := "sha256:abcdefghijklmnopqrstuvwxyz0123456789"
	got := shortDigest(digest)
	if len(got) != 19 {
		t.Errorf("shortDigest of long digest: len=%d, want 19", len(got))
	}
	if got != digest[:19] {
		t.Errorf("shortDigest = %q, want %q", got, digest[:19])
	}
}

func TestShortDigest_Short(t *testing.T) {
	digest := "sha256:abc"
	got := shortDigest(digest)
	if got != digest {
		t.Errorf("shortDigest of short digest = %q, want %q", got, digest)
	}
}

func TestShortDigest_Exactly19(t *testing.T) {
	digest := "sha256:abcdefghijk" // exactly 19 chars
	got := shortDigest(digest)
	if got != digest {
		t.Errorf("shortDigest of 19-char digest = %q, want %q", got, digest)
	}
}
