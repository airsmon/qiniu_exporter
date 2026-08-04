package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"qiniu-exporter/internal/collector"
	"qiniu-exporter/internal/config"
	"qiniu-exporter/internal/poller"
	"qiniu-exporter/internal/qiniu/cdn"
	"qiniu-exporter/internal/qiniu/kodo"
	"qiniu-exporter/internal/snapshot"
	"qiniu-exporter/internal/telemetry"
)

type discoveryJobResult struct {
	name string
	err  error
}

type discoveryObserver struct {
	metrics *telemetry.Metrics
	results chan discoveryJobResult
}

func (o *discoveryObserver) ObserveJob(name string, duration time.Duration, err error) {
	o.metrics.ObserveJob(name, duration, err)
	if name == "kodo/discovery" || name == "cdn/discovery" {
		o.results <- discoveryJobResult{name: name, err: err}
	}
}

func (o *discoveryObserver) ObserveSkipped(name, reason string) {
	o.metrics.ObserveSkipped(name, reason)
}

func (o *discoveryObserver) ObserveResourceBatchJob(name string, resources []string, duration time.Duration, err error) {
	o.metrics.ObserveResourceBatchJob(name, resources, duration, err)
}

type retryingKodoDiscoverer struct {
	listCalls  int
	firstError error
}

func (d *retryingKodoDiscoverer) ListBuckets(context.Context) ([]kodo.Bucket, error) {
	d.listCalls++
	if d.listCalls == 1 {
		return nil, d.firstError
	}
	return []kodo.Bucket{
		{Name: "bucket", Region: "z0", StorageRegion: "East China - Zhejiang", Private: false},
		{Name: "bucket", Region: "z1", StorageRegion: "North China - Hebei", Private: true},
	}, nil
}

type retryingCDNDiscoverer struct {
	calls      int
	firstError error
}

func (d *retryingCDNDiscoverer) ListDomains(context.Context) ([]cdn.Domain, error) {
	d.calls++
	if d.calls == 1 {
		return nil, d.firstError
	}
	return []cdn.Domain{
		{Name: "a.example.com", OperatingState: "success", Product: "cdn"},
		{Name: "b.example.com", OperatingState: "success", Product: "cdn"},
	}, nil
}

func TestKodoStartupDiscoveryFailureIsNonfatalAndRetried(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := telemetry.New(registry, "test", "test")
	observer := &discoveryObserver{metrics: metrics, results: make(chan discoveryJobResult, 4)}
	scheduler := poller.New(observer)
	cfg := discoveryRegistrationConfig()
	// The short discovery interval keeps this scheduler integration test fast.
	// Its matching test-only QPS value avoids triggering admission on that
	// deliberately unrealistic interval.
	cfg.Collection.Intervals.Discovery = config.Duration(20 * time.Millisecond)
	cfg.Collection.KodoMaxQPS = 1_000
	wantErr := errors.New("temporary Kodo discovery failure")
	discoverer := &retryingKodoDiscoverer{firstError: wantErr}

	inventory := &snapshot.Store[[]kodo.Bucket]{}
	if err := RegisterKodo(scheduler, nil, discoverer, cfg, inventory, &snapshot.ResourceStore[[]kodo.GaugeSample]{}, metrics); err != nil {
		t.Fatalf("registration should not perform startup discovery: %v", err)
	}
	if discoverer.listCalls != 0 {
		t.Fatalf("discovery ran during registration: calls=%d", discoverer.listCalls)
	}

	ctx, cancel := context.WithCancel(context.Background())
	scheduler.Run(ctx)
	first := waitForDiscoveryJob(t, observer.results)
	second := waitForDiscoveryJob(t, observer.results)
	cancel()
	scheduler.Wait()

	if first.name != "kodo/discovery" || !errors.Is(first.err, wantErr) {
		t.Fatalf("first discovery result=%#v, want temporary failure", first)
	}
	if second.name != "kodo/discovery" || second.err != nil {
		t.Fatalf("second discovery result=%#v, want success", second)
	}
	if discoverer.listCalls < 2 {
		t.Fatalf("discovery calls=%d, want at least 2", discoverer.listCalls)
	}
	if buckets, _, ok := inventory.Load(time.Now().Add(time.Hour)); !ok || len(buckets) != 2 {
		t.Fatalf("Kodo last-good inventory = %#v, ok=%v, want 2 retained buckets", buckets, ok)
	}
	assertAppGauge(t, registry, "qiniu_exporter_collector_success", map[string]string{
		"module": "kodo", "collector": "discovery",
	}, 1)
	for _, collectorName := range []string{"capacity", "activity"} {
		for _, resource := range []string{"bucket/z0", "bucket/z1"} {
			assertAppGauge(t, registry, "qiniu_exporter_resource_collector_success", map[string]string{
				"module": "kodo", "collector": collectorName, "resource": resource,
			}, 0)
		}
	}
}

func TestCDNStartupDiscoveryFailureIsNonfatalAndRetried(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := telemetry.New(registry, "test", "test")
	observer := &discoveryObserver{metrics: metrics, results: make(chan discoveryJobResult, 4)}
	scheduler := poller.New(observer)
	cfg := discoveryRegistrationConfig()
	cfg.Collection.Intervals.Discovery = config.Duration(20 * time.Millisecond)
	wantErr := errors.New("temporary CDN discovery failure")
	discoverer := &retryingCDNDiscoverer{firstError: wantErr}
	stores := collector.CDNStores{
		Inventory:  &snapshot.Store[[]cdn.Domain]{},
		Monitoring: &snapshot.ResourceStore[collector.CDNMonitoringSnapshot]{},
		Analytics:  &snapshot.ResourceStore[collector.CDNAnalyticsSnapshot]{},
		Usage:      &snapshot.Store[collector.CDNUsageSnapshot]{},
	}

	if err := RegisterCDN(scheduler, nil, discoverer, cfg, stores, metrics); err != nil {
		t.Fatalf("registration should not perform startup discovery: %v", err)
	}
	if discoverer.calls != 0 {
		t.Fatalf("discovery ran during registration: calls=%d", discoverer.calls)
	}

	ctx, cancel := context.WithCancel(context.Background())
	scheduler.Run(ctx)
	first := waitForDiscoveryJob(t, observer.results)
	second := waitForDiscoveryJob(t, observer.results)
	cancel()
	scheduler.Wait()

	if first.name != "cdn/discovery" || !errors.Is(first.err, wantErr) {
		t.Fatalf("first discovery result=%#v, want temporary failure", first)
	}
	if second.name != "cdn/discovery" || second.err != nil {
		t.Fatalf("second discovery result=%#v, want success", second)
	}
	if discoverer.calls < 2 {
		t.Fatalf("discovery calls=%d, want at least 2", discoverer.calls)
	}
	if domains, _, ok := stores.Inventory.Load(time.Now().Add(time.Hour)); !ok || len(domains) != 2 {
		t.Fatalf("CDN last-good inventory = %#v, ok=%v, want 2 retained domains", domains, ok)
	}
	assertAppGauge(t, registry, "qiniu_exporter_collector_success", map[string]string{
		"module": "cdn", "collector": "discovery",
	}, 1)
	for _, collectorName := range []string{"monitoring", "analytics"} {
		for _, resource := range []string{"a.example.com", "b.example.com"} {
			assertAppGauge(t, registry, "qiniu_exporter_resource_collector_success", map[string]string{
				"module": "cdn", "collector": collectorName, "resource": resource,
			}, 0)
		}
	}
}

func TestKodoInventoryIsPublishedWhileStatisticsAreGated(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := telemetry.New(registry, "test", "test")
	observer := &discoveryObserver{metrics: metrics, results: make(chan discoveryJobResult, 1)}
	scheduler := poller.New(observer)
	cfg := discoveryRegistrationConfig()
	cfg.Kodo.StatisticsTimezoneVerified = false
	discoverer := &retryingKodoDiscoverer{}
	discoverer.listCalls = 1 // Skip the fake discoverer's first-error branch.
	inventory := &snapshot.Store[[]kodo.Bucket]{}
	statistics := &snapshot.ResourceStore[[]kodo.GaugeSample]{}
	registry.MustRegister(collector.NewKodo(inventory, statistics))

	if err := RegisterKodo(scheduler, nil, discoverer, cfg, inventory, statistics, metrics); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	scheduler.Run(ctx)
	result := waitForDiscoveryJob(t, observer.results)
	cancel()
	scheduler.Wait()
	if result.err != nil {
		t.Fatalf("Kodo discovery error = %v", result.err)
	}
	assertAppGauge(t, registry, "qiniu_kodo_buckets", nil, 2)
	assertAppGauge(t, registry, "qiniu_kodo_bucket_info", map[string]string{
		"bucket": "bucket", "region": "z0", "storage_region": "East China - Zhejiang", "access": "public",
	}, 1)
	assertMetricFamilyAbsent(t, registry, "qiniu_kodo_storage_bytes")
}

func TestCDNInventoryIncludesInactiveDomainsWhileStatisticsAreGated(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := telemetry.New(registry, "test", "test")
	observer := &discoveryObserver{metrics: metrics, results: make(chan discoveryJobResult, 1)}
	scheduler := poller.New(observer)
	cfg := discoveryRegistrationConfig()
	cfg.CDN.StatisticsTimezoneVerified = false
	discoverer := cdnDiscovererFunc(func(context.Context) ([]cdn.Domain, error) {
		return []cdn.Domain{
			{Name: "active.example.com", OperatingState: "success", Product: "cdn"},
			{Name: "offlined.example.com", OperatingState: "offlined", Product: "cdn"},
		}, nil
	})
	stores := collector.CDNStores{
		Inventory:  &snapshot.Store[[]cdn.Domain]{},
		Monitoring: &snapshot.ResourceStore[collector.CDNMonitoringSnapshot]{},
		Analytics:  &snapshot.ResourceStore[collector.CDNAnalyticsSnapshot]{},
		Usage:      &snapshot.Store[collector.CDNUsageSnapshot]{},
	}
	registry.MustRegister(collector.NewCDN(stores))

	if err := RegisterCDN(scheduler, nil, discoverer, cfg, stores, metrics); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	scheduler.Run(ctx)
	result := waitForDiscoveryJob(t, observer.results)
	cancel()
	scheduler.Wait()
	if result.err != nil {
		t.Fatalf("CDN discovery error = %v", result.err)
	}
	assertAppGauge(t, registry, "qiniu_cdn_domains", nil, 2)
	assertAppGauge(t, registry, "qiniu_cdn_domain_info", map[string]string{
		"domain": "offlined.example.com", "operating_state": "offlined", "product": "cdn",
	}, 1)
	assertMetricFamilyAbsent(t, registry, "qiniu_cdn_requests_per_second")
}

func discoveryRegistrationConfig() *config.Config {
	return &config.Config{
		Kodo: config.KodoConfig{Enabled: true, StatisticsTimezoneVerified: true, StorageClasses: []string{"standard"}},
		CDN:  config.CDNConfig{Enabled: true, StatisticsTimezoneVerified: true, MonitoringUnitsVerified: true},
		Collection: config.CollectionConfig{
			SourceLag: config.Duration(10 * time.Minute),
			Intervals: config.IntervalConfig{
				Discovery: config.Duration(time.Hour), KodoCapacity: config.Duration(time.Hour), KodoActivity: config.Duration(time.Hour),
				CDNMonitoring: config.Duration(time.Hour), CDNAnalytics: config.Duration(time.Hour),
			},
			StaleAfter:              config.StaleAfterConfig{Realtime: config.Duration(2 * time.Hour)},
			KodoMaxQPS:              1,
			CDNFusionMaxQPS:         5,
			FirstRequestUtilization: 0.8,
		},
	}
}

func waitForDiscoveryJob(t *testing.T, results <-chan discoveryJobResult) discoveryJobResult {
	t.Helper()
	select {
	case result := <-results:
		return result
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for discovery job")
		return discoveryJobResult{}
	}
}

func assertAppGauge(t *testing.T, registry *prometheus.Registry, familyName string, labels map[string]string, want float64) {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != familyName {
			continue
		}
		for _, metric := range family.Metric {
			matched := true
			for name, wantLabel := range labels {
				found := false
				for _, label := range metric.Label {
					if label.GetName() == name && label.GetValue() == wantLabel {
						found = true
						break
					}
				}
				if !found {
					matched = false
					break
				}
			}
			if matched {
				if got := metric.GetGauge().GetValue(); got != want {
					t.Fatalf("gauge %s%v=%v, want %v", familyName, labels, got, want)
				}
				return
			}
		}
	}
	t.Fatalf("gauge %s%v not found", familyName, labels)
}

func assertMetricFamilyAbsent(t *testing.T, registry *prometheus.Registry, familyName string) {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == familyName {
			t.Fatalf("metric family %s must be absent", familyName)
		}
	}
}
