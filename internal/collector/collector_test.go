package collector

import (
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/PixiBixi/kubearch/internal/store"
	"github.com/PixiBixi/kubearch/internal/types"
)

func populatedStore(t *testing.T) *store.Store {
	t.Helper()
	s := store.New()
	s.SetPodImages("ns/pod1", []string{"nginx:latest"})
	s.SetImage("nginx:latest", "sha256:aaa", []types.Platform{
		{OS: "linux", Arch: "amd64"},
		{OS: "linux", Arch: "arm64"},
	})
	s.SetPodImages("ns/pod2", []string{"redis:7"})
	s.SetImage("redis:7", "sha256:bbb", []types.Platform{
		{OS: "linux", Arch: "amd64"},
	})
	return s
}

func TestNew(t *testing.T) {
	s := store.New()
	c := New(s)
	if c == nil {
		t.Fatal("New() returned nil")
	}
}

func TestDescribe(t *testing.T) {
	c := New(store.New())
	ch := make(chan *prometheus.Desc, 10)
	c.Describe(ch)
	close(ch)

	var descs []*prometheus.Desc
	for d := range ch {
		descs = append(descs, d)
	}
	if len(descs) != 3 {
		t.Errorf("Describe sent %d descriptors, want 3", len(descs))
	}
}

func TestCollect_MetricCount(t *testing.T) {
	// nginx (2 platforms) + redis (1 platform):
	//   nginx: 2 kubearch_image_platform_supported + 1 count + 1 multi_arch = 4
	//   redis: 1 kubearch_image_platform_supported + 1 count + 1 multi_arch = 3
	// total = 7 metrics
	s := populatedStore(t)
	c := New(s)

	ch := make(chan prometheus.Metric, 20)
	c.Collect(ch)
	close(ch)

	var metrics []prometheus.Metric
	for m := range ch {
		metrics = append(metrics, m)
	}
	if len(metrics) != 7 {
		t.Errorf("Collect emitted %d metrics, want 7", len(metrics))
	}
}

func TestCollect_EmptyStore(t *testing.T) {
	c := New(store.New())
	ch := make(chan prometheus.Metric, 10)
	c.Collect(ch)
	close(ch)

	var metrics []prometheus.Metric
	for m := range ch {
		metrics = append(metrics, m)
	}
	if len(metrics) != 0 {
		t.Errorf("Collect on empty store emitted %d metrics, want 0", len(metrics))
	}
}

func TestCollect_MultiArchValue(t *testing.T) {
	s := store.New()
	s.SetPodImages("ns/pod1", []string{"multi:latest"})
	s.SetImage("multi:latest", "sha256:ccc", []types.Platform{
		{OS: "linux", Arch: "amd64"},
		{OS: "linux", Arch: "arm64"},
	})

	// Register via prometheus registry to validate the collector is well-formed.
	reg := prometheus.NewRegistry()
	c := New(s)
	if err := reg.Register(c); err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather failed: %v", err)
	}

	var multiArchValue float64 = -1
	for _, mf := range mfs {
		if mf.GetName() == "kubearch_image_multi_arch" {
			for _, m := range mf.GetMetric() {
				multiArchValue = m.GetGauge().GetValue()
			}
		}
	}
	if multiArchValue != 1.0 {
		t.Errorf("kubearch_image_multi_arch for multi-arch image = %v, want 1.0", multiArchValue)
	}
}

func TestCollect_SingleArchMultiArchValue(t *testing.T) {
	s := store.New()
	s.SetPodImages("ns/pod1", []string{"single:latest"})
	s.SetImage("single:latest", "sha256:ddd", []types.Platform{
		{OS: "linux", Arch: "amd64"},
	})

	reg := prometheus.NewRegistry()
	c := New(s)
	if err := reg.Register(c); err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather failed: %v", err)
	}

	var multiArchValue float64 = -1
	for _, mf := range mfs {
		if mf.GetName() == "kubearch_image_multi_arch" {
			for _, m := range mf.GetMetric() {
				multiArchValue = m.GetGauge().GetValue()
			}
		}
	}
	if multiArchValue != 0.0 {
		t.Errorf("kubearch_image_multi_arch for single-arch image = %v, want 0.0", multiArchValue)
	}
}

func TestCollect_ConcurrentCalls(t *testing.T) {
	s := populatedStore(t)
	c := New(s)

	const goroutines = 10
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			ch := make(chan prometheus.Metric, 20)
			c.Collect(ch)
			close(ch)
			for range ch {
			}
		}()
	}
	wg.Wait()
}

func TestCollect_PlatformCountValue(t *testing.T) {
	s := store.New()
	s.SetPodImages("ns/pod1", []string{"img:1"})
	s.SetImage("img:1", "sha256:eee", []types.Platform{
		{OS: "linux", Arch: "amd64"},
		{OS: "linux", Arch: "arm64"},
		{OS: "linux", Arch: "s390x"},
	})

	reg := prometheus.NewRegistry()
	c := New(s)
	if err := reg.Register(c); err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather failed: %v", err)
	}

	var countValue float64 = -1
	for _, mf := range mfs {
		if mf.GetName() == "kubearch_image_platform_count" {
			for _, m := range mf.GetMetric() {
				countValue = m.GetGauge().GetValue()
			}
		}
	}
	if countValue != 3.0 {
		t.Errorf("kubearch_image_platform_count = %v, want 3.0", countValue)
	}
}
