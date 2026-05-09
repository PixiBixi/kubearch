package store

import (
	"sync"
	"testing"

	"github.com/PixiBixi/kubearch/internal/types"
)

func TestNew(t *testing.T) {
	s := New()
	if s == nil {
		t.Fatal("New() returned nil")
	}
	if len(s.Snapshot()) != 0 {
		t.Error("new store should have empty snapshot")
	}
}

func TestTrackPodImage_NewImage(t *testing.T) {
	s := New()
	if !s.TrackPodImage("ns/pod1", "nginx:latest") {
		t.Error("first encounter of image should return true (needs inspection)")
	}
}

func TestTrackPodImage_KnownImage(t *testing.T) {
	s := New()
	s.TrackPodImage("ns/pod1", "nginx:latest")
	s.SetImage("nginx:latest", "sha256:abc", []types.Platform{{OS: "linux", Arch: "amd64"}})

	if s.TrackPodImage("ns/pod2", "nginx:latest") {
		t.Error("already-known image should return false")
	}
}

func TestTrackPodImage_PendingImage(t *testing.T) {
	s := New()
	s.TrackPodImage("ns/pod1", "nginx:latest") // marks as pending

	if s.TrackPodImage("ns/pod2", "nginx:latest") {
		t.Error("pending image should return false")
	}
}

func TestTrackPodImage_SamePodSameImage(t *testing.T) {
	s := New()
	if !s.TrackPodImage("ns/pod1", "nginx:latest") {
		t.Error("first call should return true")
	}
	// second call same pod/image: already pending, so false
	if s.TrackPodImage("ns/pod1", "nginx:latest") {
		t.Error("repeat call with same pod/image should return false (image is pending)")
	}
}

func TestSetImage_StoresResult(t *testing.T) {
	s := New()
	s.TrackPodImage("ns/pod1", "nginx:latest")
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
	s.TrackPodImage("ns/pod1", "nginx:latest")
	s.RemovePod("ns/pod1") // pod deleted before inspection completes

	s.SetImage("nginx:latest", "sha256:abc", []types.Platform{{OS: "linux", Arch: "amd64"}})

	if len(s.Snapshot()) != 0 {
		t.Error("image should be discarded when no pod references it")
	}
}

func TestSetImage_RemovesFromPending(t *testing.T) {
	s := New()
	s.TrackPodImage("ns/pod1", "nginx:latest")
	s.SetImage("nginx:latest", "sha256:abc", []types.Platform{{OS: "linux", Arch: "amd64"}})

	// After SetImage, a second pod tracking same image should return false (now known).
	if s.TrackPodImage("ns/pod2", "nginx:latest") {
		t.Error("image should be known after SetImage, not requiring re-inspection")
	}
}

func TestFailImage_AllowsReinspection(t *testing.T) {
	s := New()
	s.TrackPodImage("ns/pod1", "nginx:latest") // now pending

	s.FailImage("nginx:latest") // simulate failed inspection

	// Next pod event should be able to re-trigger inspection.
	if !s.TrackPodImage("ns/pod2", "nginx:latest") {
		t.Error("after FailImage, next pod should be able to trigger re-inspection")
	}
}

func TestRemovePod_CleansUpOrphanedImage(t *testing.T) {
	s := New()
	s.TrackPodImage("ns/pod1", "nginx:latest")
	s.SetImage("nginx:latest", "sha256:abc", []types.Platform{{OS: "linux", Arch: "amd64"}})

	s.RemovePod("ns/pod1")

	if len(s.Snapshot()) != 0 {
		t.Error("orphaned image should be removed when last pod is deleted")
	}
}

func TestRemovePod_KeepsSharedImage(t *testing.T) {
	s := New()
	s.TrackPodImage("ns/pod1", "nginx:latest")
	s.SetImage("nginx:latest", "sha256:abc", []types.Platform{{OS: "linux", Arch: "amd64"}})
	s.TrackPodImage("ns/pod2", "nginx:latest") // second pod uses same image

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
	s.TrackPodImage("ns/pod1", "nginx:latest")
	s.SetImage("nginx:latest", "sha256:abc", []types.Platform{{OS: "linux", Arch: "amd64"}})
	s.TrackPodImage("ns/pod2", "nginx:latest")
	s.TrackPodImage("ns/pod3", "nginx:latest")

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

func TestSnapshot_ReturnsCopy(t *testing.T) {
	s := New()
	s.TrackPodImage("ns/pod1", "nginx:latest")
	s.SetImage("nginx:latest", "sha256:abc", []types.Platform{{OS: "linux", Arch: "amd64"}})

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

			if i%3 == 0 {
				s.TrackPodImage(podRef, imageRef)
			} else if i%3 == 1 {
				s.SetImage(imageRef, "sha256:abc", []types.Platform{{OS: "linux", Arch: "amd64"}})
			} else {
				s.Snapshot()
			}
		}(i)
	}
	wg.Wait()
}
