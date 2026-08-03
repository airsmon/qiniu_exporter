package collector

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"qiniu-exporter/internal/qiniu/kodo"
	"qiniu-exporter/internal/snapshot"
)

type KodoCollector struct {
	store *snapshot.ResourceStore[[]kodo.GaugeSample]

	storageBytes *prometheus.Desc
	objects      *prometheus.Desc
	requests     *prometheus.Desc
	egress       *prometheus.Desc
}

func NewKodo(store *snapshot.ResourceStore[[]kodo.GaugeSample]) *KodoCollector {
	return &KodoCollector{
		store: store,
		storageBytes: prometheus.NewDesc(
			"qiniu_kodo_storage_bytes",
			"Latest complete Kodo storage capacity reported by Qiniu.",
			[]string{"bucket", "region", "storage_class"}, nil,
		),
		objects: prometheus.NewDesc(
			"qiniu_kodo_objects",
			"Latest complete Kodo object count reported by Qiniu.",
			[]string{"bucket", "region", "storage_class"}, nil,
		),
		requests: prometheus.NewDesc(
			"qiniu_kodo_requests_per_second",
			"Average Kodo customer request rate in the latest complete five-minute bucket; the exporter does not issue object PUTs.",
			[]string{"bucket", "region", "operation"}, nil,
		),
		egress: prometheus.NewDesc(
			"qiniu_kodo_egress_bytes_per_second",
			"Average Kodo egress rate in the latest complete five-minute bucket.",
			[]string{"bucket", "region", "route"}, nil,
		),
	}
}

func (c *KodoCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.storageBytes
	ch <- c.objects
	ch <- c.requests
	ch <- c.egress
}

func (c *KodoCollector) Collect(ch chan<- prometheus.Metric) {
	for _, value := range c.store.Load(time.Now()) {
		for _, sample := range value.Data {
			switch sample.Kind {
			case kodo.GaugeStorageBytes:
				ch <- prometheus.MustNewConstMetric(c.storageBytes, prometheus.GaugeValue, sample.Value, sample.Bucket, sample.Region, string(sample.StorageClass))
			case kodo.GaugeObjects:
				ch <- prometheus.MustNewConstMetric(c.objects, prometheus.GaugeValue, sample.Value, sample.Bucket, sample.Region, string(sample.StorageClass))
			case kodo.GaugeRequestsPerSecond:
				ch <- prometheus.MustNewConstMetric(c.requests, prometheus.GaugeValue, sample.Value, sample.Bucket, sample.Region, string(sample.Operation))
			case kodo.GaugeEgressBytesPerSecond:
				ch <- prometheus.MustNewConstMetric(c.egress, prometheus.GaugeValue, sample.Value, sample.Bucket, sample.Region, string(sample.Route))
			}
		}
	}
}
