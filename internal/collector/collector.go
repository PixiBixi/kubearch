package collector

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/PixiBixi/kubearch/internal/store"
)

// descSet groups the three image metric descriptors for one label variant.
type descSet struct {
	platformSupported *prometheus.Desc
	platformCount     *prometheus.Desc
	multiArch         *prometheus.Desc
}

var (
	// withDigest is the default label set. noDigest exists because the digest
	// label is a cardinality trap: every rebuild of the same tag mints a new
	// time series that Prometheus never garbage-collects on its own.
	// WithDigestLabel(false) switches a Collector over to it.
	withDigest = descSet{
		platformSupported: prometheus.NewDesc(
			"kubearch_image_platform_supported",
			"Whether the image supports the given platform (value is always 1 when the entry exists).",
			[]string{"image", "digest", "os", "arch"},
			nil,
		),
		platformCount: prometheus.NewDesc(
			"kubearch_image_platform_count",
			"Total number of platforms supported by the image.",
			[]string{"image", "digest"},
			nil,
		),
		multiArch: prometheus.NewDesc(
			"kubearch_image_multi_arch",
			"1 if the image supports more than one platform, 0 otherwise.",
			[]string{"image", "digest"},
			nil,
		),
	}
	noDigest = descSet{
		platformSupported: prometheus.NewDesc(
			"kubearch_image_platform_supported",
			"Whether the image supports the given platform (value is always 1 when the entry exists).",
			[]string{"image", "os", "arch"},
			nil,
		),
		platformCount: prometheus.NewDesc(
			"kubearch_image_platform_count",
			"Total number of platforms supported by the image.",
			[]string{"image"},
			nil,
		),
		multiArch: prometheus.NewDesc(
			"kubearch_image_multi_arch",
			"1 if the image supports more than one platform, 0 otherwise.",
			[]string{"image"},
			nil,
		),
	}
)

// Collector implements prometheus.Collector for the image platform store.
type Collector struct {
	store       *store.Store
	digestLabel bool
}

// Option configures a Collector constructed via New.
type Option func(*Collector)

// WithDigestLabel controls whether the digest label is attached to the image
// metric families. The default (true) matches historical behaviour; set to
// false to avoid a new time series on every image rebuild of the same tag.
func WithDigestLabel(enabled bool) Option {
	return func(c *Collector) {
		c.digestLabel = enabled
	}
}

// New builds a Collector for store s. The digest label is included by
// default; pass WithDigestLabel(false) to omit it.
func New(s *store.Store, opts ...Option) *Collector {
	c := &Collector{store: s, digestLabel: true}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

func (c *Collector) descs() descSet {
	if c.digestLabel {
		return withDigest
	}
	return noDigest
}

func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	d := c.descs()
	ch <- d.platformSupported
	ch <- d.platformCount
	ch <- d.multiArch
}

func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	d := c.descs()
	for _, info := range c.store.Snapshot() {
		if c.digestLabel {
			for _, p := range info.Platforms {
				ch <- prometheus.MustNewConstMetric(d.platformSupported, prometheus.GaugeValue, 1, info.Ref, info.Digest, p.OS, p.Arch)
			}

			count := float64(len(info.Platforms))
			ch <- prometheus.MustNewConstMetric(d.platformCount, prometheus.GaugeValue, count, info.Ref, info.Digest)

			multiArch := 0.0
			if count > 1 {
				multiArch = 1.0
			}
			ch <- prometheus.MustNewConstMetric(d.multiArch, prometheus.GaugeValue, multiArch, info.Ref, info.Digest)
			continue
		}

		for _, p := range info.Platforms {
			ch <- prometheus.MustNewConstMetric(d.platformSupported, prometheus.GaugeValue, 1, info.Ref, p.OS, p.Arch)
		}

		count := float64(len(info.Platforms))
		ch <- prometheus.MustNewConstMetric(d.platformCount, prometheus.GaugeValue, count, info.Ref)

		multiArch := 0.0
		if count > 1 {
			multiArch = 1.0
		}
		ch <- prometheus.MustNewConstMetric(d.multiArch, prometheus.GaugeValue, multiArch, info.Ref)
	}
}
