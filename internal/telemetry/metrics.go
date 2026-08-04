package telemetry

import (
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"qiniu-exporter/internal/poller"
)

type Metrics struct {
	resourceMu               sync.Mutex
	resourceStates           map[string]map[string]bool
	resourceLastSuccessTimes map[string]map[string]time.Time
	resourceDataTimes        map[string]map[string]time.Time
	collectorSuccess         *prometheus.GaugeVec
	collectorLastSuccess     *prometheus.GaugeVec
	collectorStaleAfter      *prometheus.GaugeVec
	dataTimestamp            *prometheus.GaugeVec
	collectionDuration       *prometheus.GaugeVec
	apiRequests              *prometheus.CounterVec
	apiDuration              *prometheus.HistogramVec
	rateLimitEvents          *prometheus.CounterVec
	limiterWait              *prometheus.HistogramVec
	schedulerSkipped         *prometheus.CounterVec
	resourceSuccess          *prometheus.GaugeVec
	resourceLastSuccess      *prometheus.GaugeVec
	resourceDataTimestamp    *prometheus.GaugeVec
}

func New(registerer prometheus.Registerer, version, revision string) *Metrics {
	metrics := &Metrics{
		resourceStates:           make(map[string]map[string]bool),
		resourceLastSuccessTimes: make(map[string]map[string]time.Time),
		resourceDataTimes:        make(map[string]map[string]time.Time),
		collectorSuccess: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "qiniu_exporter_collector_success",
			Help: "Whether the latest attempt, or every configured resource's latest attempt, succeeded (1) or failed (0).",
		}, []string{"module", "collector"}),
		collectorLastSuccess: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "qiniu_exporter_collector_last_success_timestamp_seconds",
			Help: "Unix timestamp of the latest success, or the oldest latest-success time across configured resources.",
		}, []string{"module", "collector"}),
		collectorStaleAfter: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "qiniu_exporter_collector_stale_after_seconds",
			Help: "Configured maximum age of published data before it is considered stale.",
		}, []string{"module", "collector"}),
		dataTimestamp: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "qiniu_exporter_data_timestamp_seconds",
			Help: "Unix timestamp represented by the currently published upstream data.",
		}, []string{"module", "collector"}),
		collectionDuration: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "qiniu_exporter_collection_duration_seconds",
			Help: "Duration of the most recent collection attempt.",
		}, []string{"module", "collector"}),
		apiRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "qiniu_exporter_api_requests_total",
			Help: "Qiniu API attempts made by the exporter.",
		}, []string{"service", "endpoint", "result"}),
		apiDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "qiniu_exporter_api_request_duration_seconds",
			Help:    "Duration of Qiniu API attempts.",
			Buckets: prometheus.DefBuckets,
		}, []string{"service", "endpoint"}),
		rateLimitEvents: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "qiniu_exporter_api_rate_limit_events_total",
			Help: "Qiniu API rate-limit responses observed by the exporter.",
		}, []string{"service", "host"}),
		limiterWait: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "qiniu_exporter_api_limiter_wait_duration_seconds",
			Help:    "Time spent waiting for a local API rate-limit token.",
			Buckets: []float64{0.001, 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10},
		}, []string{"service", "host"}),
		schedulerSkipped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "qiniu_exporter_scheduler_skipped_total",
			Help: "Scheduled collections skipped by reason.",
		}, []string{"module", "collector", "reason"}),
		resourceSuccess: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "qiniu_exporter_resource_collector_success",
			Help: "Whether the most recent collection for a configured resource succeeded.",
		}, []string{"module", "collector", "resource"}),
		resourceLastSuccess: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "qiniu_exporter_resource_last_success_timestamp_seconds",
			Help: "Unix timestamp of the most recent successful collection for a resource.",
		}, []string{"module", "collector", "resource"}),
		resourceDataTimestamp: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "qiniu_exporter_resource_data_timestamp_seconds",
			Help: "Unix timestamp represented by the currently published data for a resource.",
		}, []string{"module", "collector", "resource"}),
	}

	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "qiniu_exporter_build_info",
		Help: "Build information for qiniu_exporter.",
	}, []string{"version", "revision", "goversion"})
	registerer.MustRegister(
		buildInfo,
		metrics.collectorSuccess,
		metrics.collectorLastSuccess,
		metrics.collectorStaleAfter,
		metrics.dataTimestamp,
		metrics.collectionDuration,
		metrics.apiRequests,
		metrics.apiDuration,
		metrics.rateLimitEvents,
		metrics.limiterWait,
		metrics.schedulerSkipped,
		metrics.resourceSuccess,
		metrics.resourceLastSuccess,
		metrics.resourceDataTimestamp,
	)
	buildInfo.WithLabelValues(version, revision, runtime.Version()).Set(1)
	return metrics
}

func (m *Metrics) InitResources(module, collector string, resources []string, staleAfter time.Duration) {
	m.ReplaceResources(module, collector, resources, staleAfter)
}

// ReplaceResources reconciles a collector with a newly discovered resource
// set. State for unchanged resources is preserved; removed label sets are
// deleted and newly discovered resources start unhealthy until collected.
func (m *Metrics) ReplaceResources(module, collector string, resources []string, staleAfter time.Duration) {
	m.resourceMu.Lock()
	defer m.resourceMu.Unlock()
	key := module + "/" + collector
	oldStates := m.resourceStates[key]
	oldLastSuccess := m.resourceLastSuccessTimes[key]
	oldDataTimes := m.resourceDataTimes[key]
	states := make(map[string]bool, len(resources))
	lastSuccess := make(map[string]time.Time, len(resources))
	dataTimes := make(map[string]time.Time, len(resources))
	active := make(map[string]struct{}, len(resources))
	for _, resource := range resources {
		active[resource] = struct{}{}
		states[resource] = oldStates[resource]
		if value, ok := oldLastSuccess[resource]; ok {
			lastSuccess[resource] = value
		}
		if value, ok := oldDataTimes[resource]; ok {
			dataTimes[resource] = value
		}
		m.resourceSuccess.WithLabelValues(module, collector, resource).Set(boolFloat(states[resource]))
	}
	for resource := range oldStates {
		if _, ok := active[resource]; ok {
			continue
		}
		m.resourceSuccess.DeleteLabelValues(module, collector, resource)
		m.resourceLastSuccess.DeleteLabelValues(module, collector, resource)
		m.resourceDataTimestamp.DeleteLabelValues(module, collector, resource)
	}
	m.resourceStates[key] = states
	m.resourceLastSuccessTimes[key] = lastSuccess
	m.resourceDataTimes[key] = dataTimes
	if len(resources) == 0 {
		m.collectorSuccess.DeleteLabelValues(module, collector)
		m.collectorLastSuccess.DeleteLabelValues(module, collector)
		m.collectorStaleAfter.DeleteLabelValues(module, collector)
		m.dataTimestamp.DeleteLabelValues(module, collector)
		return
	}
	m.collectorStaleAfter.WithLabelValues(module, collector).Set(staleAfter.Seconds())
	m.updateResourceAggregatesLocked(module, collector)
	m.updateResourceDataTimestampLocked(module, collector)
}

func (m *Metrics) InitCollector(module, collector string) {
	m.collectorSuccess.WithLabelValues(module, collector).Set(0)
}

func (m *Metrics) SetCollectorStaleAfter(module, collector string, staleAfter time.Duration) {
	m.collectorStaleAfter.WithLabelValues(module, collector).Set(staleAfter.Seconds())
}

func (m *Metrics) ObserveAPIRequest(service, endpoint, result string, duration time.Duration) {
	m.apiRequests.WithLabelValues(service, endpoint, result).Inc()
	m.apiDuration.WithLabelValues(service, endpoint).Observe(duration.Seconds())
}

func (m *Metrics) ObserveLimiterWait(service, host string, duration time.Duration) {
	m.limiterWait.WithLabelValues(service, host).Observe(duration.Seconds())
}

func (m *Metrics) ObserveRateLimited(service, host string) {
	m.rateLimitEvents.WithLabelValues(service, host).Inc()
}

func (m *Metrics) ObserveJob(name string, duration time.Duration, err error) {
	module, collector := splitJob(name)
	m.collectionDuration.WithLabelValues(module, collector).Set(duration.Seconds())
	if err != nil {
		m.collectorSuccess.WithLabelValues(module, collector).Set(0)
		return
	}
	now := float64(time.Now().Unix())
	m.collectorSuccess.WithLabelValues(module, collector).Set(1)
	m.collectorLastSuccess.WithLabelValues(module, collector).Set(now)
}

func (m *Metrics) ObserveResourceJob(name, resource string, duration time.Duration, err error) {
	module, collector := splitJob(name)
	m.collectionDuration.WithLabelValues(module, collector).Set(duration.Seconds())
	m.observeResourceResults(module, collector, map[string]error{resource: err})
}

func (m *Metrics) ObserveResourceBatchJob(name string, resources []string, duration time.Duration, err error) {
	module, collector := splitJob(name)
	m.collectionDuration.WithLabelValues(module, collector).Set(duration.Seconds())
	results := make(map[string]error, len(resources))
	partial, isPartial := err.(poller.PartialResourceError)
	for _, resource := range resources {
		if isPartial {
			results[resource] = partial.ErrorFor(resource)
		} else {
			results[resource] = err
		}
	}
	m.observeResourceResults(module, collector, results)
}

func (m *Metrics) observeResourceResults(module, collector string, results map[string]error) {
	m.resourceMu.Lock()
	defer m.resourceMu.Unlock()
	key := module + "/" + collector
	states, ok := m.resourceStates[key]
	if !ok {
		return
	}
	lastSuccessTimes, ok := m.resourceLastSuccessTimes[key]
	if !ok {
		lastSuccessTimes = map[string]time.Time{}
		m.resourceLastSuccessTimes[key] = lastSuccessTimes
	}
	now := time.Now()
	for resource, err := range results {
		if _, configured := states[resource]; !configured {
			// Ignore a late result from a collection round that started before
			// discovery removed this resource.
			continue
		}
		states[resource] = err == nil
		if err != nil {
			m.resourceSuccess.WithLabelValues(module, collector, resource).Set(0)
		} else {
			lastSuccessTimes[resource] = now
			m.resourceSuccess.WithLabelValues(module, collector, resource).Set(1)
			m.resourceLastSuccess.WithLabelValues(module, collector, resource).Set(float64(now.Unix()))
		}
	}
	m.updateResourceAggregatesLocked(module, collector)
}

func (m *Metrics) ObserveSkipped(name, reason string) {
	module, collector := splitJob(name)
	m.schedulerSkipped.WithLabelValues(module, collector, reason).Inc()
}

func (m *Metrics) SetDataTimestamp(module, collector string, timestamp time.Time) {
	if timestamp.IsZero() {
		return
	}
	m.dataTimestamp.WithLabelValues(module, collector).Set(float64(timestamp.Unix()))
}

func (m *Metrics) SetResource(module, collector, resource string, err error, dataAt time.Time) {
	if err == nil {
		m.SetResourceDataTimestamp(module, collector, resource, dataAt)
	}
}

func (m *Metrics) SetResourceDataTimestamp(module, collector, resource string, dataAt time.Time) {
	if dataAt.IsZero() {
		return
	}
	m.resourceMu.Lock()
	defer m.resourceMu.Unlock()
	key := module + "/" + collector
	states, configuredSet := m.resourceStates[key]
	if !configuredSet {
		return
	}
	if _, configured := states[resource]; !configured {
		return
	}
	m.resourceDataTimestamp.WithLabelValues(module, collector, resource).Set(float64(dataAt.Unix()))
	times, ok := m.resourceDataTimes[key]
	if !ok {
		times = map[string]time.Time{}
		m.resourceDataTimes[key] = times
	}
	times[resource] = dataAt
	m.updateResourceDataTimestampLocked(module, collector)
}

func (m *Metrics) updateResourceAggregatesLocked(module, collector string) {
	key := module + "/" + collector
	states := m.resourceStates[key]
	if len(states) == 0 {
		m.collectorSuccess.DeleteLabelValues(module, collector)
		m.collectorLastSuccess.DeleteLabelValues(module, collector)
		return
	}
	lastSuccessTimes := m.resourceLastSuccessTimes[key]
	var oldest time.Time
	for resource, successful := range states {
		value, exists := lastSuccessTimes[resource]
		if !exists {
			m.collectorSuccess.WithLabelValues(module, collector).Set(0)
			m.collectorLastSuccess.DeleteLabelValues(module, collector)
			return
		}
		if !successful {
			m.collectorSuccess.WithLabelValues(module, collector).Set(0)
			return
		}
		if oldest.IsZero() || value.Before(oldest) {
			oldest = value
		}
	}
	m.collectorSuccess.WithLabelValues(module, collector).Set(1)
	m.collectorLastSuccess.WithLabelValues(module, collector).Set(float64(oldest.Unix()))
}

func (m *Metrics) updateResourceDataTimestampLocked(module, collector string) {
	key := module + "/" + collector
	states := m.resourceStates[key]
	times := m.resourceDataTimes[key]
	if len(states) == 0 {
		m.dataTimestamp.DeleteLabelValues(module, collector)
		return
	}
	var oldest time.Time
	for resource := range states {
		value, exists := times[resource]
		if !exists {
			m.dataTimestamp.DeleteLabelValues(module, collector)
			return
		}
		if oldest.IsZero() || value.Before(oldest) {
			oldest = value
		}
	}
	m.dataTimestamp.WithLabelValues(module, collector).Set(float64(oldest.Unix()))
}

func boolFloat(value bool) float64 {
	if value {
		return 1
	}
	return 0
}

func splitJob(name string) (string, string) {
	module, collector, ok := strings.Cut(name, "/")
	if !ok || module == "" || collector == "" {
		return "unknown", name
	}
	return module, collector
}
