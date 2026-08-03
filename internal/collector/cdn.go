package collector

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"qiniu-exporter/internal/qiniu/cdn"
	"qiniu-exporter/internal/snapshot"
)

type CDNMonitoringSnapshot struct {
	Bandwidth []cdn.BandwidthSample
	Traffic   []cdn.TrafficSample
}

type CDNAnalyticsSnapshot struct {
	Requests cdn.RequestRateSample
	Statuses []cdn.StatusCodeRateSample
	Cache    cdn.CacheSample
}

type CDNStores struct {
	Monitoring *snapshot.ResourceStore[CDNMonitoringSnapshot]
	Analytics  *snapshot.ResourceStore[CDNAnalyticsSnapshot]
}

type CDNCollector struct {
	stores CDNStores

	bandwidth       *prometheus.Desc
	traffic         *prometheus.Desc
	requests        *prometheus.Desc
	httpResponses   *prometheus.Desc
	cacheRequests   *prometheus.Desc
	cacheTraffic    *prometheus.Desc
	cacheHitRatio   *prometheus.Desc
	trafficHitRatio *prometheus.Desc
}

func NewCDN(stores CDNStores) *CDNCollector {
	return &CDNCollector{
		stores: stores,
		bandwidth: prometheus.NewDesc(
			"qiniu_cdn_monitoring_bandwidth_bits_per_second",
			"Latest complete Qiniu CDN monitoring bandwidth bucket.",
			[]string{"domain", "region"}, nil,
		),
		traffic: prometheus.NewDesc(
			"qiniu_cdn_monitoring_traffic_bytes_per_second",
			"Average Qiniu CDN monitoring traffic rate in the latest complete five-minute bucket.",
			[]string{"domain", "region"}, nil,
		),
		requests: prometheus.NewDesc(
			"qiniu_cdn_requests_per_second",
			"Average Qiniu CDN request rate in the latest complete five-minute bucket.",
			[]string{"domain", "region"}, nil,
		),
		httpResponses: prometheus.NewDesc(
			"qiniu_cdn_http_responses_per_second",
			"Average Qiniu CDN response rate by API-provided status-code key in the latest complete five-minute bucket.",
			[]string{"domain", "region", "code"}, nil,
		),
		cacheRequests: prometheus.NewDesc(
			"qiniu_cdn_cache_requests_per_second",
			"Average hit or miss request rate in the latest complete Qiniu CDN five-minute bucket.",
			[]string{"domain", "result"}, nil,
		),
		cacheTraffic: prometheus.NewDesc(
			"qiniu_cdn_cache_traffic_bytes_per_second",
			"Average hit or miss traffic rate in the latest complete Qiniu CDN five-minute bucket.",
			[]string{"domain", "result"}, nil,
		),
		cacheHitRatio: prometheus.NewDesc(
			"qiniu_cdn_cache_request_hit_ratio",
			"Request cache-hit ratio in the latest complete Qiniu CDN five-minute bucket.",
			[]string{"domain"}, nil,
		),
		trafficHitRatio: prometheus.NewDesc(
			"qiniu_cdn_cache_traffic_hit_ratio",
			"Traffic cache-hit ratio in the latest complete Qiniu CDN five-minute bucket.",
			[]string{"domain"}, nil,
		),
	}
}

func (c *CDNCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.bandwidth
	ch <- c.traffic
	ch <- c.requests
	ch <- c.httpResponses
	ch <- c.cacheRequests
	ch <- c.cacheTraffic
	ch <- c.cacheHitRatio
	ch <- c.trafficHitRatio
}

func (c *CDNCollector) Collect(ch chan<- prometheus.Metric) {
	now := time.Now()
	for _, value := range c.stores.Monitoring.Load(now) {
		for _, sample := range value.Data.Bandwidth {
			ch <- prometheus.MustNewConstMetric(c.bandwidth, prometheus.GaugeValue, sample.BitsPerSecond, sample.Domain, sample.Region)
		}
		for _, sample := range value.Data.Traffic {
			ch <- prometheus.MustNewConstMetric(c.traffic, prometheus.GaugeValue, sample.BytesPerSecond, sample.Domain, sample.Region)
		}
	}
	for _, value := range c.stores.Analytics.Load(now) {
		analytics := value.Data
		ch <- prometheus.MustNewConstMetric(c.requests, prometheus.GaugeValue, analytics.Requests.RequestsPerSecond, analytics.Requests.Domain, analytics.Requests.Region)
		for _, sample := range analytics.Statuses {
			ch <- prometheus.MustNewConstMetric(c.httpResponses, prometheus.GaugeValue, sample.ResponsesPerSecond, sample.Domain, sample.Region, sample.Code)
		}
		cache := analytics.Cache
		ch <- prometheus.MustNewConstMetric(c.cacheRequests, prometheus.GaugeValue, cache.HitRequestsPerSecond, cache.Domain, "hit")
		ch <- prometheus.MustNewConstMetric(c.cacheRequests, prometheus.GaugeValue, cache.MissRequestsPerSecond, cache.Domain, "miss")
		ch <- prometheus.MustNewConstMetric(c.cacheTraffic, prometheus.GaugeValue, cache.HitTrafficBytesPerSecond, cache.Domain, "hit")
		ch <- prometheus.MustNewConstMetric(c.cacheTraffic, prometheus.GaugeValue, cache.MissTrafficBytesPerSecond, cache.Domain, "miss")
		if cache.RequestHitRatioValid {
			ch <- prometheus.MustNewConstMetric(c.cacheHitRatio, prometheus.GaugeValue, cache.RequestHitRatio, cache.Domain)
		}
		if cache.TrafficHitRatioValid {
			ch <- prometheus.MustNewConstMetric(c.trafficHitRatio, prometheus.GaugeValue, cache.TrafficHitRatio, cache.Domain)
		}
	}
}
