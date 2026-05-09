package watcher

import (
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
)

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
