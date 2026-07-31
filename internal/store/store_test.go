package store

import (
	"slices"
	"sync"
	"testing"

	"github.com/PixiBixi/kubearch/internal/types"
)

var linuxAmd64 = []types.Platform{{OS: "linux", Arch: "amd64"}}

// track registers a single image for a pod and reports whether it needs inspection.
func track(s *Store, podRef, imageRef string) bool {
	return len(s.SetPodImages(podRef, []string{imageRef})) == 1
}

func TestNew(t *testing.T) {
	s := New()
	if s == nil {
		t.Fatal("New() returned nil")
	}
	if len(s.Snapshot()) != 0 {
		t.Error("new store should have empty snapshot")
	}
}

func TestSetPodImages_NewImage(t *testing.T) {
	s := New()
	if !track(s, "ns/pod1", "nginx:latest") {
		t.Error("first encounter of image should require inspection")
	}
}

func TestSetPodImages_KnownImage(t *testing.T) {
	s := New()
	track(s, "ns/pod1", "nginx:latest")
	s.SetImage("nginx:latest", "sha256:abc", linuxAmd64)

	if track(s, "ns/pod2", "nginx:latest") {
		t.Error("already-known image should not require inspection")
	}
}

func TestSetPodImages_PendingImage(t *testing.T) {
	s := New()
	track(s, "ns/pod1", "nginx:latest") // marks as pending

	if track(s, "ns/pod2", "nginx:latest") {
		t.Error("pending image should not require a second inspection")
	}
}

func TestSetPodImages_SamePodSameImage(t *testing.T) {
	s := New()
	if !track(s, "ns/pod1", "nginx:latest") {
		t.Error("first call should require inspection")
	}
	if track(s, "ns/pod1", "nginx:latest") {
		t.Error("repeat call with same pod/image should not (image is pending)")
	}
}

func TestSetPodImages_DedupsWithinPod(t *testing.T) {
	s := New()
	got := s.SetPodImages("ns/pod1", []string{"nginx:latest", "nginx:latest", "redis:7"})
	if len(got) != 2 {
		t.Fatalf("expected 2 images to inspect, got %d (%v)", len(got), got)
	}
	if !slices.Contains(got, "nginx:latest") || !slices.Contains(got, "redis:7") {
		t.Errorf("unexpected images to inspect: %v", got)
	}
}

// A pod spec can be updated in place (kubectl set image, ephemeral containers),
// so the store must drop the images the pod no longer references.
func TestSetPodImages_ReplacesImageSet(t *testing.T) {
	s := New()
	track(s, "ns/pod1", "nginx:1.26")
	s.SetImage("nginx:1.26", "sha256:old", linuxAmd64)

	toInspect := s.SetPodImages("ns/pod1", []string{"nginx:1.27"})

	if !slices.Equal(toInspect, []string{"nginx:1.27"}) {
		t.Errorf("toInspect = %v, want [nginx:1.27]", toInspect)
	}
	snap := s.Snapshot()
	if len(snap) != 0 {
		t.Errorf("the replaced image should be gone, snapshot = %v", snap)
	}
}

func TestSetPodImages_ReplaceKeepsImageUsedElsewhere(t *testing.T) {
	s := New()
	track(s, "ns/pod1", "nginx:1.26")
	s.SetImage("nginx:1.26", "sha256:old", linuxAmd64)
	track(s, "ns/pod2", "nginx:1.26")

	s.SetPodImages("ns/pod1", []string{"nginx:1.27"})

	if len(s.Snapshot()) != 1 {
		t.Error("image still referenced by pod2 should survive pod1's update")
	}
}

func TestSetImage_StoresResult(t *testing.T) {
	s := New()
	track(s, "ns/pod1", "nginx:latest")
	platforms := []types.Platform{{OS: "linux", Arch: "amd64"}, {OS: "linux", Arch: "arm64"}}
	s.SetImage("nginx:latest", "sha256:abc", platforms)

	snap := s.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 image in snapshot, got %d", len(snap))
	}
	info := snap[0]
	if info.Ref != "nginx:latest" {
		t.Errorf("Ref = %q, want %q", info.Ref, "nginx:latest")
	}
	if info.Digest != "sha256:abc" {
		t.Errorf("Digest = %q, want %q", info.Digest, "sha256:abc")
	}
	if len(info.Platforms) != 2 {
		t.Errorf("Platforms len = %d, want 2", len(info.Platforms))
	}
}

func TestSetImage_DiscardsIfNoPodLeft(t *testing.T) {
	s := New()
	track(s, "ns/pod1", "nginx:latest")
	s.RemovePod("ns/pod1") // pod deleted before inspection completes

	s.SetImage("nginx:latest", "sha256:abc", linuxAmd64)

	if len(s.Snapshot()) != 0 {
		t.Error("image should be discarded when no pod references it")
	}
}

func TestSetImage_RemovesFromPending(t *testing.T) {
	s := New()
	track(s, "ns/pod1", "nginx:latest")
	s.SetImage("nginx:latest", "sha256:abc", linuxAmd64)

	if track(s, "ns/pod2", "nginx:latest") {
		t.Error("image should be known after SetImage, not requiring re-inspection")
	}
}

func TestFailImage_AllowsReinspection(t *testing.T) {
	s := New()
	track(s, "ns/pod1", "nginx:latest") // now pending

	s.FailImage("nginx:latest") // simulate failed inspection

	if !track(s, "ns/pod2", "nginx:latest") {
		t.Error("after FailImage, next pod should be able to trigger re-inspection")
	}
}

func TestRemovePod_CleansUpOrphanedImage(t *testing.T) {
	s := New()
	track(s, "ns/pod1", "nginx:latest")
	s.SetImage("nginx:latest", "sha256:abc", linuxAmd64)

	s.RemovePod("ns/pod1")

	if len(s.Snapshot()) != 0 {
		t.Error("orphaned image should be removed when last pod is deleted")
	}
}

func TestRemovePod_KeepsSharedImage(t *testing.T) {
	s := New()
	track(s, "ns/pod1", "nginx:latest")
	s.SetImage("nginx:latest", "sha256:abc", linuxAmd64)
	track(s, "ns/pod2", "nginx:latest") // second pod uses same image

	s.RemovePod("ns/pod1")

	if len(s.Snapshot()) != 1 {
		t.Error("image still referenced by pod2 should not be removed")
	}
}

func TestRemovePod_UnknownPod(t *testing.T) {
	s := New()
	// Should not panic on unknown pod.
	s.RemovePod("ns/nonexistent")
}

func TestRemovePod_MultiplePodsSameImage(t *testing.T) {
	s := New()
	track(s, "ns/pod1", "nginx:latest")
	s.SetImage("nginx:latest", "sha256:abc", linuxAmd64)
	track(s, "ns/pod2", "nginx:latest")
	track(s, "ns/pod3", "nginx:latest")

	s.RemovePod("ns/pod1")
	if len(s.Snapshot()) != 1 {
		t.Error("image should remain after removing one of three pods")
	}

	s.RemovePod("ns/pod2")
	if len(s.Snapshot()) != 1 {
		t.Error("image should remain after removing second pod (pod3 still references it)")
	}

	s.RemovePod("ns/pod3")
	if len(s.Snapshot()) != 0 {
		t.Error("image should be removed when last pod is deleted")
	}
}

// A pod deleted and recreated with the same name must not leave a stale edge
// behind, otherwise the image would look referenced forever.
func TestRemovePod_ThenReadd(t *testing.T) {
	s := New()
	track(s, "ns/pod1", "nginx:latest")
	s.SetImage("nginx:latest", "sha256:abc", linuxAmd64)
	s.RemovePod("ns/pod1")

	if !track(s, "ns/pod1", "nginx:latest") {
		t.Fatal("recreated pod should re-trigger inspection of the forgotten image")
	}
	s.SetImage("nginx:latest", "sha256:abc", linuxAmd64)
	s.RemovePod("ns/pod1")

	if len(s.Snapshot()) != 0 {
		t.Error("image should be orphaned again after the recreated pod is deleted")
	}
}

func TestStats(t *testing.T) {
	s := New()
	track(s, "ns/pod1", "nginx:latest")
	s.SetImage("nginx:latest", "sha256:abc", linuxAmd64)
	track(s, "ns/pod2", "redis:7") // stays pending

	images, pending, pods := s.Stats()
	if images != 1 || pending != 1 || pods != 2 {
		t.Errorf("Stats() = (%d, %d, %d), want (1, 1, 2)", images, pending, pods)
	}
}

func TestSnapshot_ReturnsCopy(t *testing.T) {
	s := New()
	track(s, "ns/pod1", "nginx:latest")
	s.SetImage("nginx:latest", "sha256:abc", linuxAmd64)

	snap1 := s.Snapshot()
	snap1[0].Ref = "mutated"

	snap2 := s.Snapshot()
	if snap2[0].Ref == "mutated" {
		t.Error("Snapshot should return a copy, not a reference to internal state")
	}
}

func TestConcurrentAccess(t *testing.T) {
	s := New()
	const goroutines = 20

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := range goroutines {
		go func(i int) {
			defer wg.Done()
			podRef := "ns/pod"
			imageRef := "nginx:latest"

			switch i % 4 {
			case 0:
				s.SetPodImages(podRef, []string{imageRef})
			case 1:
				s.SetImage(imageRef, "sha256:abc", linuxAmd64)
			case 2:
				s.RemovePod(podRef)
			default:
				s.Snapshot()
			}
		}(i)
	}
	wg.Wait()
}
