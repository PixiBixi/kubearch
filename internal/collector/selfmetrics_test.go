package collector

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/PixiBixi/kubearch/internal/store"
	"github.com/PixiBixi/kubearch/internal/types"
)

func TestSelfMetrics_BuildInfo(t *testing.T) {
	sm := NewSelfMetrics("1.2.3", "abc1234", store.New(), func() int { return 0 })

	expected := fmt.Sprintf(`
		# HELP kubearch_build_info Build metadata for the running kubearch binary (value is always 1).
		# TYPE kubearch_build_info gauge
		kubearch_build_info{commit="abc1234",go_version=%q,version="1.2.3"} 1
	`, runtime.Version())
	if err := testutil.CollectAndCompare(sm, strings.NewReader(expected), "kubearch_build_info"); err != nil {
		t.Error(err)
	}
}

func TestSelfMetrics_StoreGauges(t *testing.T) {
	s := store.New()
	s.SetPodImages("ns/pod1", []string{"nginx:latest"})
	s.SetImage("nginx:latest", "sha256:aaa", []types.Platform{{OS: "linux", Arch: "amd64"}})
	s.SetPodImages("ns/pod2", []string{"pending:image"}) // stays pending: never resolved via SetImage/FailImage

	sm := NewSelfMetrics("dev", "none", s, func() int { return 0 })

	images, pending, pods := s.Stats()
	expected := fmt.Sprintf(`
		# HELP kubearch_store_images Number of images with a known inspection result.
		# TYPE kubearch_store_images gauge
		kubearch_store_images %d
		# HELP kubearch_store_pending_inspections Number of inspections currently in flight.
		# TYPE kubearch_store_pending_inspections gauge
		kubearch_store_pending_inspections %d
		# HELP kubearch_store_pods Number of pods currently tracked by the store.
		# TYPE kubearch_store_pods gauge
		kubearch_store_pods %d
	`, images, pending, pods)
	err := testutil.CollectAndCompare(sm, strings.NewReader(expected),
		"kubearch_store_images", "kubearch_store_pending_inspections", "kubearch_store_pods")
	if err != nil {
		t.Error(err)
	}
}

func TestSelfMetrics_QueueDepth_ReadAtScrapeTime(t *testing.T) {
	depth := 0
	sm := NewSelfMetrics("dev", "none", store.New(), func() int { return depth })

	assertQueueDepth := func(want int) {
		t.Helper()
		expected := fmt.Sprintf(`
			# HELP kubearch_queue_depth Number of images waiting in the inspection work queue.
			# TYPE kubearch_queue_depth gauge
			kubearch_queue_depth %d
		`, want)
		if err := testutil.CollectAndCompare(sm, strings.NewReader(expected), "kubearch_queue_depth"); err != nil {
			t.Error(err)
		}
	}

	assertQueueDepth(0)

	depth = 7 // the provider, not a value cached at construction, is what backs the gauge
	assertQueueDepth(7)
}

func TestSelfMetrics_ObserveInspection_IncrementsByResult(t *testing.T) {
	sm := NewSelfMetrics("dev", "none", store.New(), func() int { return 0 })

	sm.ObserveInspection("success", 10*time.Millisecond)
	sm.ObserveInspection("success", 20*time.Millisecond)
	sm.ObserveInspection("failure", 5*time.Millisecond)
	sm.ObserveRetry()

	if got := testutil.ToFloat64(sm.inspections.WithLabelValues("success")); got != 2 {
		t.Errorf("inspections_total{result=success} = %v, want 2", got)
	}
	if got := testutil.ToFloat64(sm.inspections.WithLabelValues("failure")); got != 1 {
		t.Errorf("inspections_total{result=failure} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(sm.retries); got != 1 {
		t.Errorf("inspection_retries_total = %v, want 1", got)
	}
	// Requeues must not inflate the attempt count: 3 attempts were observed.
	attempts := testutil.ToFloat64(sm.inspections.WithLabelValues("success")) +
		testutil.ToFloat64(sm.inspections.WithLabelValues("failure"))
	if attempts != 3 {
		t.Errorf("sum of inspections_total = %v, want 3 (retries belong to their own counter)", attempts)
	}
}

func TestSelfMetrics_Describe(t *testing.T) {
	sm := NewSelfMetrics("dev", "none", store.New(), func() int { return 0 })
	if n := testutil.CollectAndCount(sm); n != 7 {
		// build_info (1) + inspections_total (0: a CounterVec emits nothing
		// until a label combination is observed) + inspection_retries_total (1)
		// + inspection_duration_seconds (1) + the 4 scrape-time gauges.
		t.Errorf("CollectAndCount = %d, want 7", n)
	}
}
