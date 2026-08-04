package collector

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"qiniu-exporter/internal/qiniu/kodo"
	"qiniu-exporter/internal/snapshot"
)

type KodoCollector struct {
	inventory *snapshot.Store[[]kodo.Bucket]
	store     *snapshot.ResourceStore[[]kodo.GaugeSample]

	buckets      *prometheus.Desc
	bucketInfo   *prometheus.Desc
	storageBytes *prometheus.Desc
	objects      *prometheus.Desc
	requests     *prometheus.Desc
	egress       *prometheus.Desc
}

func NewKodo(inventory *snapshot.Store[[]kodo.Bucket], store *snapshot.ResourceStore[[]kodo.GaugeSample]) *KodoCollector {
	return &KodoCollector{
		inventory: inventory,
		store:     store,
		buckets: prometheus.NewDesc(
			"qiniu_kodo_buckets",
			"Number of Kodo buckets visible to the configured credentials in the latest successful discovery.",
			nil, nil,
		),
		bucketInfo: prometheus.NewDesc(
			"qiniu_kodo_bucket_info",
			"Information about a Kodo bucket visible in the latest successful discovery, including its native region ID and access-control state.",
			[]string{"bucket", "region", "storage_region", "access"}, nil,
		),
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
	ch <- c.buckets
	ch <- c.bucketInfo
	ch <- c.storageBytes
	ch <- c.objects
	ch <- c.requests
	ch <- c.egress
}

func (c *KodoCollector) Collect(ch chan<- prometheus.Metric) {
	now := time.Now()
	if buckets, _, ok := c.inventory.Load(now); ok {
		ch <- prometheus.MustNewConstMetric(c.buckets, prometheus.GaugeValue, float64(len(buckets)))
		for _, bucket := range buckets {
			access := "public"
			if bucket.Private {
				access = "private"
			}
			ch <- prometheus.MustNewConstMetric(c.bucketInfo, prometheus.GaugeValue, 1, bucket.Name, bucket.Region, bucket.StorageRegion, access)
		}
	}
	for _, value := range c.store.Load(now) {
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
