package store

import (
	"strconv"
	"testing"
)

// clusterSizes mirrors realistic pod counts: a small cluster, a busy one, and
// the scale where an O(pods) scan per event starts to hurt.
var clusterSizes = []int{100, 1_000, 10_000}

// seedCluster fills the store with podCount pods, each running imagesPerPod
// images shared across the cluster (as a Deployment's replicas would).
func seedCluster(b *testing.B, podCount, imagesPerPod int) (*Store, []string) {
	b.Helper()
	s := New()
	images := make([]string, imagesPerPod)
	for i := range images {
		images[i] = "registry.example.com/app-" + strconv.Itoa(i) + ":v1"
	}
	for p := range podCount {
		podRef := "ns/pod-" + strconv.Itoa(p)
		s.SetPodImages(podRef, images)
	}
	for _, img := range images {
		s.SetImage(img, "sha256:abc", linuxAmd64)
	}
	return s, images
}

// BenchmarkRemovePod is the pod-churn hot path: it holds the same write lock
// that Prometheus scrapes contend on.
func BenchmarkRemovePod(b *testing.B) {
	for _, pods := range clusterSizes {
		b.Run("pods="+strconv.Itoa(pods), func(b *testing.B) {
			s, images := seedCluster(b, pods, 3)
			podRef := "ns/churn"

			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				b.StopTimer()
				s.SetPodImages(podRef, images)
				b.StartTimer()

				s.RemovePod(podRef)
			}
		})
	}
}

// BenchmarkSetImage runs once per completed inspection, and must decide whether
// any pod still references the image.
func BenchmarkSetImage(b *testing.B) {
	for _, pods := range clusterSizes {
		b.Run("pods="+strconv.Itoa(pods), func(b *testing.B) {
			s, images := seedCluster(b, pods, 3)

			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				s.SetImage(images[i%len(images)], "sha256:abc", linuxAmd64)
			}
		})
	}
}

// BenchmarkSetPodImages is the steady-state pod-add path.
func BenchmarkSetPodImages(b *testing.B) {
	for _, pods := range clusterSizes {
		b.Run("pods="+strconv.Itoa(pods), func(b *testing.B) {
			s, images := seedCluster(b, pods, 3)

			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				s.SetPodImages("ns/new-"+strconv.Itoa(i), images)
			}
		})
	}
}

// BenchmarkSnapshot is the scrape path.
func BenchmarkSnapshot(b *testing.B) {
	for _, images := range []int{100, 1_000, 10_000} {
		b.Run("images="+strconv.Itoa(images), func(b *testing.B) {
			s, _ := seedCluster(b, 1, images)

			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				_ = s.Snapshot()
			}
		})
	}
}
