package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"qiniu-exporter/internal/collector"
	"qiniu-exporter/internal/config"
	"qiniu-exporter/internal/poller"
	"qiniu-exporter/internal/qiniu/billing"
	"qiniu-exporter/internal/qiniu/cdn"
	"qiniu-exporter/internal/qiniu/kodo"
	"qiniu-exporter/internal/snapshot"
	"qiniu-exporter/internal/telemetry"
)

type appDoerFunc func(*http.Request) (*http.Response, error)

func (f appDoerFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

type cdnDiscovererFunc func(context.Context) ([]string, error)

func (f cdnDiscovererFunc) ListDomains(ctx context.Context) ([]string, error) { return f(ctx) }

func TestMonitoringIsolatesAndCachesInvalidDomain(t *testing.T) {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	point := time.Now().In(location).Add(-15 * time.Minute).Truncate(cdn.FiveMinuteBucket)
	var calls int
	doer := appDoerFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		var payload struct {
			Domains string `json:"domains"`
		}
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		domains := strings.Split(payload.Domains, ";")
		if slicesContain(domains, "bad.example.com") {
			return appJSONResponse(`{"code":400032,"error":"invalid domain","data":{}}`), nil
		}
		data := make(map[string]cdn.MonitoringRegionSeries, len(domains))
		for _, domain := range domains {
			data[domain] = cdn.MonitoringRegionSeries{China: []float64{300}, Oversea: []float64{150}}
		}
		body, err := json.Marshal(cdn.MonitoringResponse{
			Code: 200, Times: []string{point.Format("2006-01-02 15:04:05")}, Data: data,
		})
		if err != nil {
			t.Fatal(err)
		}
		return appJSONResponse(string(body)), nil
	})
	client, err := cdn.NewClient(doer, "https://fusion.test")
	if err != nil {
		t.Fatal(err)
	}
	store := &snapshot.ResourceStore[collector.CDNMonitoringSnapshot]{}
	metrics := telemetry.New(prometheus.NewRegistry(), "test", "test")
	bad := map[string]error{}
	var badMu sync.RWMutex
	domains := []string{"good-a.example.com", "bad.example.com", "good-b.example.com"}

	failed := collectCDNMonitoring(context.Background(), client, domains, 10*time.Minute, time.Hour, location, store, metrics, &badMu, bad)
	if len(failed) != 1 || failed.ErrorFor("bad.example.com") == nil || failed.ErrorFor("good-a.example.com") != nil {
		t.Fatalf("unexpected partial errors: %#v", failed)
	}
	if calls != 7 {
		t.Fatalf("calls=%d, want bounded bisection cost 7", calls)
	}
	if values := store.Load(time.Now()); len(values) != 2 {
		t.Fatalf("published resources=%d, want 2 healthy domains", len(values))
	}

	failed = collectCDNMonitoring(context.Background(), client, domains, 10*time.Minute, time.Hour, location, store, metrics, &badMu, bad)
	if len(failed) != 1 || failed.ErrorFor("bad.example.com") == nil {
		t.Fatalf("negative cache lost: %#v", failed)
	}
	if calls != 9 {
		t.Fatalf("calls=%d, want one two-request healthy batch after negative cache", calls)
	}
}

func TestMonitoringRejectsMismatchedBucketEnds(t *testing.T) {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	firstPoint := time.Now().In(location).Add(-20 * time.Minute).Truncate(cdn.FiveMinuteBucket)
	domain := "cdn.example.com"
	calls := 0
	doer := appDoerFunc(func(*http.Request) (*http.Response, error) {
		calls++
		point := firstPoint
		if calls == 2 {
			point = point.Add(cdn.FiveMinuteBucket)
		}
		return appJSONValueResponse(t, cdn.MonitoringResponse{
			Code:  200,
			Times: []string{point.Format("2006-01-02 15:04:05")},
			Data: map[string]cdn.MonitoringRegionSeries{
				domain: {China: []float64{300}, Oversea: []float64{150}},
			},
		}), nil
	})
	client, err := cdn.NewClient(doer, "https://fusion.test")
	if err != nil {
		t.Fatal(err)
	}
	store := &snapshot.ResourceStore[collector.CDNMonitoringSnapshot]{}
	metrics := telemetry.New(prometheus.NewRegistry(), "test", "test")
	bad := map[string]error{}
	var badMu sync.RWMutex

	failed := collectCDNMonitoring(context.Background(), client, []string{domain}, 10*time.Minute, time.Hour, location, store, metrics, &badMu, bad)
	if !errors.Is(failed.ErrorFor(domain), cdn.ErrSeriesMisaligned) {
		t.Fatalf("error=%v, want ErrSeriesMisaligned", failed.ErrorFor(domain))
	}
	if values := store.Load(time.Now()); len(values) != 0 {
		t.Fatalf("published resources=%d, want 0", len(values))
	}
	if len(bad) != 0 {
		t.Fatalf("negative cache=%v, want empty", bad)
	}
}

func TestAnalyticsRejectsMismatchedBucketEnds(t *testing.T) {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	firstPoint := time.Now().In(location).Add(-20 * time.Minute).Truncate(cdn.FiveMinuteBucket)
	calls := 0
	doer := appDoerFunc(func(*http.Request) (*http.Response, error) {
		calls++
		switch calls {
		case 1:
			return appJSONValueResponse(t, cdn.RequestCountResponse{
				Code: 200,
				Data: cdn.RequestCountData{Points: []string{firstPoint.Format("2006-01-02-15-04")}, ReqCount: []float64{300}},
			}), nil
		case 2:
			return appJSONValueResponse(t, cdn.StatusCodeResponse{
				Code: 200,
				Data: cdn.StatusCodeData{
					Points: []string{firstPoint.Add(cdn.FiveMinuteBucket).Format("2006-01-02-15-04")},
					Codes:  map[string][]float64{"2xx": {300}},
				},
			}), nil
		default:
			return appJSONValueResponse(t, cdn.HitMissResponse{
				Code: 200,
				Data: cdn.HitMissData{
					Points: []string{firstPoint.Format("2006-01-02-15-04")},
					Hit:    []float64{250}, Miss: []float64{50}, TrafficHit: []float64{900}, TrafficMiss: []float64{100},
				},
			}), nil
		}
	})
	client, err := cdn.NewClient(doer, "https://fusion.test")
	if err != nil {
		t.Fatal(err)
	}
	store := &snapshot.ResourceStore[collector.CDNAnalyticsSnapshot]{}
	metrics := telemetry.New(prometheus.NewRegistry(), "test", "test")
	bad := map[string]error{}
	var badMu sync.RWMutex

	err = collectCDNAnalytics(context.Background(), client, "cdn.example.com", 10*time.Minute, time.Hour, location, store, metrics, &badMu, bad)
	if !errors.Is(err, cdn.ErrSeriesMisaligned) {
		t.Fatalf("error=%v, want ErrSeriesMisaligned", err)
	}
	if values := store.Load(time.Now()); len(values) != 0 {
		t.Fatalf("published resources=%d, want 0", len(values))
	}
	if len(bad) != 0 {
		t.Fatalf("negative cache=%v, want empty", bad)
	}
}

func TestMonitoringIsolationAttemptBudgetDoesNotCacheUnresolvedDomains(t *testing.T) {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	doer := appDoerFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return appJSONResponse(`{"code":400032,"error":"invalid domain","data":{}}`), nil
	})
	client, err := cdn.NewClient(doer, "https://fusion.test")
	if err != nil {
		t.Fatal(err)
	}
	domains := make([]string, 50)
	for index := range domains {
		domains[index] = fmt.Sprintf("domain-%02d.example.com", index)
	}
	store := &snapshot.ResourceStore[collector.CDNMonitoringSnapshot]{}
	metrics := telemetry.New(prometheus.NewRegistry(), "test", "test")
	bad := map[string]error{}
	var badMu sync.RWMutex

	failed := collectCDNMonitoring(context.Background(), client, domains, 10*time.Minute, time.Hour, location, store, metrics, &badMu, bad)
	if calls != cdnMonitoringIsolationAttemptLimit {
		t.Fatalf("batch attempts=%d, want %d", calls, cdnMonitoringIsolationAttemptLimit)
	}
	if len(failed) != len(domains) {
		t.Fatalf("failed domains=%d, want %d", len(failed), len(domains))
	}
	unresolved := 0
	for _, domain := range domains {
		if !errors.Is(failed.ErrorFor(domain), errCDNMonitoringIsolationBudget) {
			continue
		}
		unresolved++
		if _, cached := bad[domain]; cached {
			t.Fatalf("unresolved domain %q was added to negative cache", domain)
		}
	}
	if unresolved == 0 {
		t.Fatal("attempt budget did not leave any domain unresolved")
	}
	if len(bad) == 0 || len(bad) >= len(domains) {
		t.Fatalf("negative cache size=%d, want only directly isolated singleton domains", len(bad))
	}
}

func TestUnverifiedMonitoringUnitsMakeNoMonitoringRequest(t *testing.T) {
	calls := 0
	client, err := cdn.NewClient(appDoerFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, context.Canceled
	}), "https://fusion.test")
	if err != nil {
		t.Fatal(err)
	}
	registry := prometheus.NewRegistry()
	metrics := telemetry.New(registry, "test", "test")
	scheduler := poller.New(metrics)
	stores := collector.CDNStores{
		Monitoring: &snapshot.ResourceStore[collector.CDNMonitoringSnapshot]{},
		Analytics:  &snapshot.ResourceStore[collector.CDNAnalyticsSnapshot]{},
	}
	cfg := testRealtimeConfig()
	cfg.CDN.StatisticsTimezoneVerified = true
	cfg.CDN.MonitoringUnitsVerified = false
	discoverer := cdnDiscovererFunc(func(context.Context) ([]string, error) { return []string{"cdn.example.com"}, nil })
	if err := RegisterCDN(scheduler, client, discoverer, cfg, stores, metrics); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("monitoring requests=%d before scheduler start, want 0", calls)
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	foundSkipped := false
	for _, family := range families {
		if family.GetName() == "qiniu_exporter_scheduler_skipped_total" && len(family.Metric) == 1 && family.Metric[0].Counter.GetValue() == 1 {
			foundSkipped = true
		}
		if family.GetName() == "qiniu_exporter_collector_success" {
			for _, metric := range family.Metric {
				labels := map[string]string{}
				for _, label := range metric.Label {
					labels[label.GetName()] = label.GetValue()
				}
				if labels["module"] == "cdn" && labels["collector"] == "monitoring" {
					t.Fatal("disabled monitoring collector must not expose a permanent failure state")
				}
			}
		}
	}
	if !foundSkipped {
		t.Fatal("units_unverified skip metric was not published")
	}
}

func TestUnverifiedStatisticsTimezoneMakesNoCDNRequestOrFailureState(t *testing.T) {
	calls := 0
	client, err := cdn.NewClient(appDoerFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, context.Canceled
	}), "https://fusion.test")
	if err != nil {
		t.Fatal(err)
	}
	registry := prometheus.NewRegistry()
	metrics := telemetry.New(registry, "test", "test")
	stores := collector.CDNStores{
		Monitoring: &snapshot.ResourceStore[collector.CDNMonitoringSnapshot]{},
		Analytics:  &snapshot.ResourceStore[collector.CDNAnalyticsSnapshot]{},
	}
	cfg := testRealtimeConfig()
	cfg.CDN.StatisticsTimezoneVerified = false
	cfg.CDN.MonitoringUnitsVerified = true
	discoverer := cdnDiscovererFunc(func(context.Context) ([]string, error) { return []string{"cdn.example.com"}, nil })
	if err := RegisterCDN(poller.New(metrics), client, discoverer, cfg, stores, metrics); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("requests=%d, want 0", calls)
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == "qiniu_exporter_collector_success" {
			for _, metric := range family.Metric {
				labels := map[string]string{}
				for _, label := range metric.Label {
					labels[label.GetName()] = label.GetValue()
				}
				if labels["module"] == "cdn" && labels["collector"] != "discovery" {
					t.Fatal("timezone-gated CDN statistics collector exposed a permanent failure state")
				}
			}
		}
	}
}

func TestBillingScheduleHelpers(t *testing.T) {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	monthFirst := time.Date(2026, time.August, 1, 0, 0, 0, 0, location)
	start, end := estimatePeriod(monthFirst)
	if !start.Equal(time.Date(2026, time.July, 1, 0, 0, 0, 0, location)) || !end.Equal(monthFirst) {
		t.Fatalf("month-first estimate period = [%s,%s)", start, end)
	}
	now := time.Date(2026, time.August, 3, 7, 30, 0, 0, location)
	if got := nextShanghaiTime(8, 15)(now); got != 45*time.Minute {
		t.Fatalf("next daily delay=%s, want 45m", got)
	}
}

func TestKodoQueryUsesShanghaiAlignedSafeWindow(t *testing.T) {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 3, 12, 17, 0, 0, location)
	query := kodoQuery(now, 10*time.Minute, kodo.Bucket{Name: "bucket", Region: "z0"}, location)
	wantEnd := time.Date(2026, time.August, 3, 12, 5, 0, 0, location)
	if !query.End.Equal(wantEnd) || query.End.Sub(query.Begin) != 15*time.Minute || !query.SafeBefore.Equal(query.End) {
		t.Fatalf("unexpected Kodo query: %#v", query)
	}
}

func TestBillingLabelAllowlists(t *testing.T) {
	if err := validateCurrency("CNY"); err != nil {
		t.Fatal(err)
	}
	if err := validateCurrency("EUR"); err == nil {
		t.Fatal("undocumented currency must be rejected")
	}
	allowed := []config.ResourcePackAllowlist{{Item: "CDN traffic", Zone: "mainland", AvailableTime: "all", Unit: "GB"}}
	packs := []billing.ResourcePackMonthOverview{{
		ItemName: "CDN traffic", ZoneName: "mainland", AvailableTime: "all", Unit: "GB", TotalSurplus: 10, MonthUsed: 2, MonthRemain: 8,
	}}
	if err := validateResourcePacks(packs, allowed); err != nil {
		t.Fatal(err)
	}
	packs[0].ZoneName = "unconfigured"
	if err := validateResourcePacks(packs, allowed); err == nil || !strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("unconfigured resource-pack labels were accepted: %v", err)
	}
}

func appJSONResponse(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewBufferString(body))}
}

func appJSONValueResponse(t *testing.T, value any) *http.Response {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return appJSONResponse(string(body))
}

func slicesContain(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func testRealtimeConfig() *config.Config {
	return &config.Config{
		Kodo: config.KodoConfig{Enabled: true, StorageClasses: []string{"standard"}},
		CDN:  config.CDNConfig{Enabled: true},
		Collection: config.CollectionConfig{
			SourceLag: config.Duration(10 * time.Minute),
			Intervals: config.IntervalConfig{
				Discovery: config.Duration(time.Hour), CDNMonitoring: config.Duration(30 * time.Minute), CDNAnalytics: config.Duration(30 * time.Minute),
				KodoCapacity: config.Duration(30 * time.Minute), KodoActivity: config.Duration(30 * time.Minute),
			},
			StaleAfter:              config.StaleAfterConfig{Realtime: config.Duration(time.Hour)},
			KodoMaxQPS:              1,
			CDNFusionMaxQPS:         5,
			FirstRequestUtilization: 0.8,
		},
	}
}
