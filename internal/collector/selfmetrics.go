package collector

import (
	"runtime"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/PixiBixi/kubearch/internal/store"
)

var (
	descStoreImages = prometheus.NewDesc(
		"kubearch_store_images",
		"Number of images with a known inspection result.",
		nil, nil,
	)
	descStorePending = prometheus.NewDesc(
		"kubearch_store_pending_inspections",
		"Number of inspections currently in flight.",
		nil, nil,
	)
	descStorePods = prometheus.NewDesc(
		"kubearch_store_pods",
		"Number of pods currently tracked by the store.",
		nil, nil,
	)
	descQueueDepth = prometheus.NewDesc(
		"kubearch_queue_depth",
		"Number of images waiting in the inspection work queue.",
		nil, nil,
	)
)

// SelfMetrics instruments the exporter itself, as opposed to Collector which
// reports on the cluster's images. Without it, a total inspection outage
// (bad auth, unreachable registries) looks identical to an empty cluster:
// the image metrics just vanish either way.
type SelfMetrics struct {
	store      *store.Store
	queueDepth func() int

	buildInfo   prometheus.Gauge
	inspections *prometheus.CounterVec
	retries     prometheus.Counter
	duration    prometheus.Histogram
}

// NewSelfMetrics builds the exporter's self-monitoring metrics.
//
// queueDepth is a callback rather than a *watcher.Watcher reference: collector
// already imports store, and watcher imports both store and inspector, so a
// direct collector->watcher import would cycle back on itself.
func NewSelfMetrics(version, commit string, s *store.Store, queueDepth func() int) *SelfMetrics {
	m := &SelfMetrics{
		store:      s,
		queueDepth: queueDepth,
		buildInfo: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "kubearch_build_info",
			Help: "Build metadata for the running kubearch binary (value is always 1).",
			ConstLabels: prometheus.Labels{
				"version":    version,
				"commit":     commit,
				"go_version": runtime.Version(),
			},
		}),
		inspections: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kubearch_inspections_total",
			Help: "Total number of image inspection attempts, by result (success or failure).",
		}, []string{"result"}),
		// Requeues live in their own counter rather than as a third "result"
		// value: a retried attempt already counted as a failure, so folding it
		// into the same family would make sum by (result) exceed the number of
		// attempts.
		retries: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kubearch_inspection_retries_total",
			Help: "Total number of failed inspections requeued for another attempt.",
		}),
		duration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "kubearch_inspection_duration_seconds",
			Help:    "Duration of image inspection calls against the registry.",
			Buckets: prometheus.DefBuckets,
		}),
	}
	m.buildInfo.Set(1)
	return m
}

// ObserveInspection records the outcome and wall-clock duration of one
// inspection attempt. result is "success" or "failure".
func (m *SelfMetrics) ObserveInspection(result string, d time.Duration) {
	m.inspections.WithLabelValues(result).Inc()
	m.duration.Observe(d.Seconds())
}

// ObserveRetry records that a failed inspection is being requeued, so
// transient flakiness can be told apart from attempts that exhaust retries.
func (m *SelfMetrics) ObserveRetry() {
	m.retries.Inc()
}

// Describe implements prometheus.Collector.
func (m *SelfMetrics) Describe(ch chan<- *prometheus.Desc) {
	m.buildInfo.Describe(ch)
	m.inspections.Describe(ch)
	m.retries.Describe(ch)
	m.duration.Describe(ch)
	ch <- descStoreImages
	ch <- descStorePending
	ch <- descStorePods
	ch <- descQueueDepth
}

// Collect implements prometheus.Collector. The store/queue gauges are
// snapshotted at scrape time rather than tracked incrementally, since they
// represent current state, not cumulative counts.
func (m *SelfMetrics) Collect(ch chan<- prometheus.Metric) {
	m.buildInfo.Collect(ch)
	m.inspections.Collect(ch)
	m.retries.Collect(ch)
	m.duration.Collect(ch)

	images, pending, pods := m.store.Stats()
	ch <- prometheus.MustNewConstMetric(descStoreImages, prometheus.GaugeValue, float64(images))
	ch <- prometheus.MustNewConstMetric(descStorePending, prometheus.GaugeValue, float64(pending))
	ch <- prometheus.MustNewConstMetric(descStorePods, prometheus.GaugeValue, float64(pods))
	ch <- prometheus.MustNewConstMetric(descQueueDepth, prometheus.GaugeValue, float64(m.queueDepth()))
}
