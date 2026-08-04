package collector

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"qiniu-exporter/internal/qiniu/cdn"
	"qiniu-exporter/internal/snapshot"
)

const (
	CDNUsagePeriodLastCompleteHour = "last_complete_hour"
	CDNUsagePeriodToday            = "today"
	CDNUsagePeriodCurrentMonth     = "current_month"
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

// CDNUsagePeriodSnapshot contains an atomic period-to-date usage view. Traffic
// is a Gauge snapshot (not a counter); it resets at the natural period
// boundary. Bandwidth is present only when it was calculated from aligned
// five-minute points.
type CDNUsagePeriodSnapshot struct {
	Period       string
	Traffic      cdn.TrafficUsageAggregate
	Bandwidth    cdn.BandwidthUsageAggregate
	HasBandwidth bool
	Complete     bool
}

type CDNUsageSnapshot struct {
	Periods []CDNUsagePeriodSnapshot
}

type CDNStores struct {
	Inventory  *snapshot.Store[[]cdn.Domain]
	Monitoring *snapshot.ResourceStore[CDNMonitoringSnapshot]
	Analytics  *snapshot.ResourceStore[CDNAnalyticsSnapshot]
	Usage      *snapshot.Store[CDNUsageSnapshot]
}

type CDNCollector struct {
	stores CDNStores

	domains          *prometheus.Desc
	domainInfo       *prometheus.Desc
	bandwidth        *prometheus.Desc
	traffic          *prometheus.Desc
	requests         *prometheus.Desc
	httpResponses    *prometheus.Desc
	cacheRequests    *prometheus.Desc
	cacheTraffic     *prometheus.Desc
	cacheHitRatio    *prometheus.Desc
	trafficHitRatio  *prometheus.Desc
	usageTraffic     *prometheus.Desc
	usageBandwidth   *prometheus.Desc
	accountTraffic   *prometheus.Desc
	accountBandwidth *prometheus.Desc
	activeDomains    *prometheus.Desc
	usageComplete    *prometheus.Desc
}

func NewCDN(stores CDNStores) *CDNCollector {
	return &CDNCollector{
		stores: stores,
		domains: prometheus.NewDesc(
			"qiniu_cdn_domains",
			"Number of CDN domains visible to the configured credentials in the latest successful discovery.",
			nil, nil,
		),
		domainInfo: prometheus.NewDesc(
			"qiniu_cdn_domain_info",
			"Information about a CDN domain visible in the latest successful discovery.",
			[]string{"domain", "operating_state", "product"}, nil,
		),
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
		usageTraffic: prometheus.NewDesc(
			"qiniu_cdn_usage_traffic_bytes",
			"Qiniu CDN traffic bytes in a current reporting period; this Gauge resets at the natural period boundary.",
			[]string{"domain", "period"}, nil,
		),
		usageBandwidth: prometheus.NewDesc(
			"qiniu_cdn_usage_peak_bandwidth_bits_per_second",
			"Peak Qiniu CDN bandwidth for a domain in a current reporting period, calculated from complete five-minute points.",
			[]string{"domain", "period"}, nil,
		),
		accountTraffic: prometheus.NewDesc(
			"qiniu_cdn_usage_account_traffic_bytes",
			"Qiniu CDN traffic bytes across the complete active discovered domain scope in a current reporting period; this Gauge resets at the natural period boundary.",
			[]string{"period"}, nil,
		),
		accountBandwidth: prometheus.NewDesc(
			"qiniu_cdn_usage_account_peak_bandwidth_bits_per_second",
			"Peak Qiniu CDN bandwidth across the complete active discovered domain scope, calculated by summing aligned points before selecting the peak.",
			[]string{"period"}, nil,
		),
		activeDomains: prometheus.NewDesc(
			"qiniu_cdn_usage_active_domains",
			"Number of CDN domains with non-zero traffic in a current reporting period.",
			[]string{"period"}, nil,
		),
		usageComplete: prometheus.NewDesc(
			"qiniu_cdn_usage_complete",
			"Whether the usage period includes every active CDN domain discovered for the account.",
			[]string{"period"}, nil,
		),
	}
}

func (c *CDNCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.domains
	ch <- c.domainInfo
	ch <- c.bandwidth
	ch <- c.traffic
	ch <- c.requests
	ch <- c.httpResponses
	ch <- c.cacheRequests
	ch <- c.cacheTraffic
	ch <- c.cacheHitRatio
	ch <- c.trafficHitRatio
	ch <- c.usageTraffic
	ch <- c.usageBandwidth
	ch <- c.accountTraffic
	ch <- c.accountBandwidth
	ch <- c.activeDomains
	ch <- c.usageComplete
}

func (c *CDNCollector) Collect(ch chan<- prometheus.Metric) {
	now := time.Now()
	if domains, _, ok := c.stores.Inventory.Load(now); ok {
		ch <- prometheus.MustNewConstMetric(c.domains, prometheus.GaugeValue, float64(len(domains)))
		for _, domain := range domains {
			ch <- prometheus.MustNewConstMetric(c.domainInfo, prometheus.GaugeValue, 1, domain.Name, domain.OperatingState, domain.Product)
		}
	}
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
	if c.stores.Usage == nil {
		return
	}
	if usage, _, ok := c.stores.Usage.Load(now); ok {
		for _, period := range usage.Periods {
			active := 0
			for _, domain := range period.Traffic.Domains {
				ch <- prometheus.MustNewConstMetric(c.usageTraffic, prometheus.GaugeValue, domain.Bytes, domain.Domain, period.Period)
				if domain.Active {
					active++
				}
			}
			complete := float64(0)
			if period.Complete {
				complete = 1
				ch <- prometheus.MustNewConstMetric(c.accountTraffic, prometheus.GaugeValue, period.Traffic.AccountBytes, period.Period)
				ch <- prometheus.MustNewConstMetric(c.activeDomains, prometheus.GaugeValue, float64(active), period.Period)
			}
			ch <- prometheus.MustNewConstMetric(c.usageComplete, prometheus.GaugeValue, complete, period.Period)
			if !period.HasBandwidth {
				continue
			}
			for _, domain := range period.Bandwidth.Domains {
				ch <- prometheus.MustNewConstMetric(c.usageBandwidth, prometheus.GaugeValue, domain.PeakBitsPerSecond, domain.Domain, period.Period)
			}
			if period.Complete {
				ch <- prometheus.MustNewConstMetric(c.accountBandwidth, prometheus.GaugeValue, period.Bandwidth.AccountPeakBitsPerSecond, period.Period)
			}
		}
	}
}
