package watcher

import (
	"context"
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

func newTestWatcher(s *store.Store, insp Inspector) *Watcher {
	return &Watcher{
		client:    k8sfake.NewClientset(),
		namespace: "default",
		store:     s,
		inspector: insp,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		sem:       make(chan struct{}, maxConcurrentInspections),
	}
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

// --- onAdd ---

func TestOnAdd_StoresImage(t *testing.T) {
	s := store.New()
	done := make(chan struct{})
	w := newTestWatcher(s, &fakeInspector{
		fn: func(_ context.Context, _ string, _ inspector.PodAuth) (string, []types.Platform, error) {
			defer close(done)
			return "sha256:deadbeef", []types.Platform{{OS: "linux", Arch: "amd64"}}, nil
		},
	})

	w.onAdd(context.Background(), makePod("default", "pod1", "nginx:latest"))

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for inspection goroutine")
	}

	imgs := s.Snapshot()
	if len(imgs) != 1 {
		t.Fatalf("expected 1 image in store, got %d", len(imgs))
	}
	if imgs[0].Digest != "sha256:deadbeef" {
		t.Errorf("unexpected digest: %s", imgs[0].Digest)
	}
	if len(imgs[0].Platforms) != 1 || imgs[0].Platforms[0].Arch != "amd64" {
		t.Errorf("unexpected platforms: %v", imgs[0].Platforms)
	}
}

func TestOnAdd_AlreadyKnownSkipsReinspection(t *testing.T) {
	s := store.New()
	calls := 0
	var mu sync.Mutex
	done := make(chan struct{}, 2)

	w := newTestWatcher(s, &fakeInspector{
		fn: func(_ context.Context, _ string, _ inspector.PodAuth) (string, []types.Platform, error) {
			mu.Lock()
			calls++
			mu.Unlock()
			done <- struct{}{}
			return "sha256:abc", []types.Platform{{OS: "linux", Arch: "amd64"}}, nil
		},
	})

	pod1 := makePod("default", "pod1", "nginx:latest")
	pod2 := makePod("default", "pod2", "nginx:latest")

	w.onAdd(context.Background(), pod1)
	// Wait for first inspection to complete before adding second pod.
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for first inspection")
	}

	w.onAdd(context.Background(), pod2)
	// Give the second onAdd time to potentially trigger an inspection.
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Errorf("expected 1 inspection call, got %d", got)
	}
}

func TestOnAdd_InspectionFailureDoesNotStore(t *testing.T) {
	s := store.New()
	done := make(chan struct{})
	w := newTestWatcher(s, &fakeInspector{
		fn: func(_ context.Context, _ string, _ inspector.PodAuth) (string, []types.Platform, error) {
			defer close(done)
			return "", nil, context.DeadlineExceeded
		},
	})

	w.onAdd(context.Background(), makePod("default", "pod1", "broken:image"))

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for inspection goroutine")
	}

	imgs := s.Snapshot()
	if len(imgs) != 0 {
		t.Errorf("expected 0 images in store after failure, got %d", len(imgs))
	}
}

func TestOnAdd_SemaphoreLimitsConcurrency(t *testing.T) {
	s := store.New()

	const numImages = 20
	images := make([]string, numImages)
	for i := range images {
		images[i] = "image-" + string(rune('a'+i)) + ":latest"
	}

	// Track peak concurrency.
	var mu sync.Mutex
	active, peak := 0, 0
	allDone := make(chan struct{})
	completed := 0

	w := newTestWatcher(s, &fakeInspector{
		fn: func(_ context.Context, imageRef string, _ inspector.PodAuth) (string, []types.Platform, error) {
			mu.Lock()
			active++
			if active > peak {
				peak = active
			}
			mu.Unlock()

			time.Sleep(20 * time.Millisecond)

			mu.Lock()
			active--
			completed++
			if completed == numImages {
				close(allDone)
			}
			mu.Unlock()

			return "sha256:ok", []types.Platform{{OS: "linux", Arch: "amd64"}}, nil
		},
	})

	pod := makePod("default", "big-pod", images...)
	w.onAdd(context.Background(), pod)

	select {
	case <-allDone:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for all inspections")
	}

	if peak > maxConcurrentInspections {
		t.Errorf("peak concurrency %d exceeded limit %d", peak, maxConcurrentInspections)
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
