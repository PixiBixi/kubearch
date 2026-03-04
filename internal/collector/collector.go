package collector

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/PixiBixi/kubearch/internal/store"
)

var (
	descPlatformSupported = prometheus.NewDesc(
		"kubearch_image_platform_supported",
		"Whether the image supports the given platform (value is always 1 when the entry exists).",
		[]string{"image", "digest", "os", "arch"},
		nil,
	)
	descPlatformCount = prometheus.NewDesc(
		"kubearch_image_platform_count",
		"Total number of platforms supported by the image.",
		[]string{"image", "digest"},
		nil,
	)
	descMultiArch = prometheus.NewDesc(
		"kubearch_image_multi_arch",
		"1 if the image supports more than one platform, 0 otherwise.",
		[]string{"image", "digest"},
		nil,
	)
)

// Collector implements prometheus.Collector for the image platform store.
type Collector struct {
	store *store.Store
}

func New(s *store.Store) *Collector {
	return &Collector{store: s}
}

func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- descPlatformSupported
	ch <- descPlatformCount
	ch <- descMultiArch
}

func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	for _, info := range c.store.Snapshot() {
		for _, p := range info.Platforms {
			ch <- prometheus.MustNewConstMetric(
				descPlatformSupported,
				prometheus.GaugeValue,
				1,
				info.Ref, info.Digest, p.OS, p.Arch,
			)
		}

		count := float64(len(info.Platforms))
		ch <- prometheus.MustNewConstMetric(descPlatformCount, prometheus.GaugeValue, count, info.Ref, info.Digest)

		multiArch := 0.0
		if count > 1 {
			multiArch = 1.0
		}
		ch <- prometheus.MustNewConstMetric(descMultiArch, prometheus.GaugeValue, multiArch, info.Ref, info.Digest)
	}
}
