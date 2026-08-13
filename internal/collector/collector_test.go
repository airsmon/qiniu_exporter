package collector

import (
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"qiniu-exporter/internal/qiniu/billing"
	"qiniu-exporter/internal/qiniu/cdn"
	"qiniu-exporter/internal/qiniu/kodo"
	"qiniu-exporter/internal/snapshot"
)

func TestBusinessCollectorsReadOnlyPublishedSnapshots(t *testing.T) {
	registry := prometheus.NewRegistry()
	now := time.Now()

	kodoStore := &snapshot.ResourceStore[[]kodo.GaugeSample]{}
	kodoStore.Publish("capacity/bucket", []kodo.GaugeSample{
		{Kind: kodo.GaugeStorageBytes, Bucket: "bucket", Region: "z0", StorageClass: kodo.StorageClassStandard, Value: 1024},
		{Kind: kodo.GaugeObjects, Bucket: "bucket", Region: "z0", StorageClass: kodo.StorageClassStandard, Value: 2},
		{Kind: kodo.GaugeRequestsPerSecond, Bucket: "bucket", Region: "z0", Operation: kodo.OperationGet, Value: 3},
		{Kind: kodo.GaugeEgressBytesPerSecond, Bucket: "bucket", Region: "z0", Route: kodo.RouteDirect, Value: 4},
	}, snapshot.Meta{CollectedAt: now, StaleAfter: time.Hour})
	kodoStore.Publish("summary/bucket", []kodo.GaugeSample{
		{Kind: kodo.GaugeUsageEgressBytes, Bucket: "bucket", Region: "z0", Route: kodo.RouteDirect, Period: kodo.PeriodCurrentMonth, Value: 8192, DataAt: now},
		{Kind: kodo.GaugeUsageRequests, Bucket: "bucket", Region: "z0", Operation: kodo.OperationPut, Period: kodo.PeriodCurrentMonth, Value: 7, DataAt: now},
	}, snapshot.Meta{CollectedAt: now, StaleAfter: time.Hour})
	kodoInventory := &snapshot.Store[[]kodo.Bucket]{}
	kodoInventory.Publish([]kodo.Bucket{{
		Name: "bucket", Region: "z0", StorageRegion: "East China - Zhejiang", Private: true,
	}}, snapshot.Meta{CollectedAt: now, StaleAfter: time.Hour})
	registry.MustRegister(NewKodo(kodoInventory, kodoStore))

	cdnStores := CDNStores{
		Inventory:  &snapshot.Store[[]cdn.Domain]{},
		Monitoring: &snapshot.ResourceStore[CDNMonitoringSnapshot]{},
		Analytics:  &snapshot.ResourceStore[CDNAnalyticsSnapshot]{},
		Usage:      &snapshot.Store[CDNUsageSnapshot]{},
		TopIPs:     &snapshot.Store[CDNTopIPSnapshot]{},
	}
	cdnStores.Inventory.Publish([]cdn.Domain{{Name: "cdn.example.com", OperatingState: "success", Product: "cdn"}}, snapshot.Meta{CollectedAt: now, StaleAfter: time.Hour})
	cdnStores.Monitoring.Publish("cdn.example.com", CDNMonitoringSnapshot{
		Bandwidth: []cdn.BandwidthSample{{Domain: "cdn.example.com", Region: cdn.RegionChina, BitsPerSecond: 100}},
		Traffic:   []cdn.TrafficSample{{Domain: "cdn.example.com", Region: cdn.RegionChina, BytesPerSecond: 200}},
	}, snapshot.Meta{CollectedAt: now, StaleAfter: time.Hour})
	cdnStores.Analytics.Publish("cdn.example.com", CDNAnalyticsSnapshot{
		Requests: cdn.RequestRateSample{Domain: "cdn.example.com", Region: cdn.RegionGlobal, RequestsPerSecond: 10},
		Statuses: []cdn.StatusCodeRateSample{{Domain: "cdn.example.com", Region: cdn.RegionGlobal, Code: "2xx", ResponsesPerSecond: 9}},
		Cache:    cdn.CacheSample{Domain: "cdn.example.com", HitRequestsPerSecond: 8, MissRequestsPerSecond: 2, RequestHitRatio: 0.8, RequestHitRatioValid: true},
	}, snapshot.Meta{CollectedAt: now, StaleAfter: time.Hour})
	cdnStores.Usage.Publish(CDNUsageSnapshot{DailyTraffic: []CDNDailyTrafficSnapshot{{Date: now.In(cdnCollectorLocation).Format("2006-01-02"), Bytes: 4096}}, Periods: []CDNUsagePeriodSnapshot{{
		Period: CDNUsagePeriodToday,
		Traffic: cdn.TrafficUsageAggregate{
			Domains:      []cdn.DomainTrafficUsage{{Domain: "cdn.example.com", Bytes: 4096, Active: true}},
			AccountBytes: 4096,
		},
		Bandwidth: cdn.BandwidthUsageAggregate{
			Domains:                  []cdn.DomainBandwidthUsage{{Domain: "cdn.example.com", PeakBitsPerSecond: 800}},
			AccountPeakBitsPerSecond: 800,
		},
		HasBandwidth: true,
		Complete:     true,
	}}}, snapshot.Meta{CollectedAt: now, StaleAfter: time.Hour})
	cdnStores.TopIPs.Publish(CDNTopIPSnapshot{
		Date:     now.In(cdnCollectorLocation).Format("2006-01-02"),
		Traffic:  []cdn.TopIPValue{{IP: "192.0.2.1", Value: 2048}},
		Requests: []cdn.TopIPValue{{IP: "2001:db8::1", Value: 42}},
	}, snapshot.Meta{CollectedAt: now, StaleAfter: time.Hour})
	registry.MustRegister(NewCDN(cdnStores))

	billingStores := BillingStores{
		Balance:       &snapshot.Store[billing.BalanceOverview]{},
		Estimate:      &snapshot.Store[BillingEstimate]{},
		DailyEstimate: &snapshot.Store[[]BillingDailyEstimate]{},
		ResourcePacks: &snapshot.Store[[]billing.ResourcePackMonthOverview]{},
		Finalized:     &snapshot.Store[BillingFinalized]{},
		Last12:        &snapshot.Store[BillingFinalizedMonths]{},
	}
	billingStores.Balance.Publish(billing.BalanceOverview{AvailableBalance: billing.Fixed8(1_230_000_000), UnpaidMoney: billing.Fixed8(100_000_000), Currency: "CNY"}, snapshot.Meta{CollectedAt: now, StaleAfter: time.Hour})
	billingStores.DailyEstimate.Publish([]BillingDailyEstimate{{Date: now.AddDate(0, 0, -1), Cost: billing.Fixed8(25_000_000), Currency: "CNY"}}, snapshot.Meta{CollectedAt: now, StaleAfter: time.Hour})
	billingStores.ResourcePacks.Publish([]billing.ResourcePackMonthOverview{{ItemName: "traffic", ZoneName: "global", AvailableTime: "month", Unit: "GB", TotalSurplus: 10, MonthUsed: 2, MonthRemain: 8}}, snapshot.Meta{CollectedAt: now, StaleAfter: time.Hour})
	billingYear := now.In(billingCollectorLocation).Year()
	billingStores.Finalized.Publish(BillingFinalized{
		Detail: billing.BillDetail{TotalMoney: billing.Fixed8(600_000_000), Currency: "CNY"},
		Period: billing.BillingPeriod{Start: time.Date(billingYear, time.June, 1, 0, 0, 0, 0, time.UTC), End: time.Date(billingYear, time.July, 1, 0, 0, 0, 0, time.UTC)},
	}, snapshot.Meta{CollectedAt: now, StaleAfter: time.Hour})
	billingStores.Last12.Publish(BillingFinalizedMonths{
		Months: []BillingFinalizedMonth{
			{Detail: billing.BillDetail{TotalMoney: billing.Fixed8(100_000_000), Currency: "CNY", Items: []billing.BillItem{{Start: time.Date(billingYear, time.January, 3, 0, 0, 0, 0, billingCollectorLocation), End: time.Date(billingYear, time.January, 4, 0, 0, 0, 0, billingCollectorLocation), ItemMoney: billing.Fixed8(40_000_000), Currency: "CNY", BillPeriod: "daily"}}}, Period: billing.BillingPeriod{Start: time.Date(billingYear, time.January, 1, 0, 0, 0, 0, time.UTC), End: time.Date(billingYear, time.February, 1, 0, 0, 0, 0, time.UTC)}},
			{Detail: billing.BillDetail{TotalMoney: billing.Fixed8(200_000_000), Currency: "CNY"}, Period: billing.BillingPeriod{Start: time.Date(billingYear, time.February, 1, 0, 0, 0, 0, time.UTC), End: time.Date(billingYear, time.March, 1, 0, 0, 0, 0, time.UTC)}},
		},
	}, snapshot.Meta{CollectedAt: now, StaleAfter: time.Hour})
	registry.MustRegister(NewBilling(billingStores))

	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	names := make(map[string]int, len(families))
	monthlyLabels := make(map[string]float64)
	kodoInventoryLabels := make(map[string]string)
	for _, family := range families {
		names[family.GetName()] = len(family.Metric)
		if family.GetName() == "qiniu_kodo_bucket_info" {
			for _, label := range family.Metric[0].Label {
				kodoInventoryLabels[label.GetName()] = label.GetValue()
			}
		}
		if family.GetName() == "qiniu_billing_last_12_months_finalized_cost" {
			for _, metric := range family.Metric {
				if len(metric.Label) != 2 {
					t.Fatalf("monthly finalized metric labels = %d, want exactly currency and month", len(metric.Label))
				}
				labels := make(map[string]string, len(metric.Label))
				for _, label := range metric.Label {
					labels[label.GetName()] = label.GetValue()
				}
				if labels["currency"] != "CNY" || (labels["month"] != fmt.Sprintf("%d-01", billingYear) && labels["month"] != fmt.Sprintf("%d-02", billingYear)) {
					t.Fatalf("unexpected monthly finalized labels: %#v", labels)
				}
				monthlyLabels[labels["month"]] = metric.GetGauge().GetValue()
			}
		}
	}
	want := []string{
		"qiniu_kodo_buckets", "qiniu_kodo_bucket_info",
		"qiniu_kodo_storage_bytes", "qiniu_kodo_objects", "qiniu_kodo_requests_per_second", "qiniu_kodo_egress_bytes_per_second",
		"qiniu_kodo_usage_egress_bytes", "qiniu_kodo_usage_requests",
		"qiniu_cdn_domains", "qiniu_cdn_domain_info",
		"qiniu_cdn_monitoring_bandwidth_bits_per_second", "qiniu_cdn_monitoring_traffic_bytes_per_second", "qiniu_cdn_requests_per_second", "qiniu_cdn_http_responses_per_second",
		"qiniu_cdn_usage_traffic_bytes", "qiniu_cdn_usage_peak_bandwidth_bits_per_second", "qiniu_cdn_usage_account_traffic_bytes", "qiniu_cdn_usage_account_daily_traffic_bytes", "qiniu_cdn_usage_account_peak_bandwidth_bits_per_second", "qiniu_cdn_usage_active_domains", "qiniu_cdn_usage_complete",
		"qiniu_cdn_top_client_ip_traffic_bytes", "qiniu_cdn_top_client_ip_requests",
		"qiniu_billing_available_balance", "qiniu_billing_unpaid_amount", "qiniu_billing_resource_pack_records", "qiniu_billing_resource_pack_remaining_ratio",
		"qiniu_billing_last_finalized_cost", "qiniu_billing_last_12_months_finalized_cost",
		"qiniu_billing_estimated_daily_cost", "qiniu_billing_finalized_daily_cost",
	}
	for _, name := range want {
		if names[name] == 0 {
			t.Errorf("metric family %s was not collected", name)
		}
	}
	if got := names["qiniu_billing_last_12_months_finalized_cost"]; got != 2 {
		t.Fatalf("last-12-month monthly finalized series = %d, want 2", got)
	}
	if monthlyLabels[fmt.Sprintf("%d-01", billingYear)] != 1 || monthlyLabels[fmt.Sprintf("%d-02", billingYear)] != 2 {
		t.Fatalf("last-12-month monthly finalized values = %#v", monthlyLabels)
	}
	if got := names["qiniu_kodo_bucket_info"]; got != 1 {
		t.Fatalf("Kodo bucket inventory series = %d, want 1", got)
	}
	wantKodoLabels := map[string]string{
		"bucket": "bucket", "region": "z0", "storage_region": "East China - Zhejiang", "access": "private",
	}
	for name, want := range wantKodoLabels {
		if got := kodoInventoryLabels[name]; got != want {
			t.Fatalf("Kodo bucket inventory label %s = %q, want %q", name, got, want)
		}
	}
	if got := names["qiniu_cdn_domain_info"]; got != 1 {
		t.Fatalf("CDN domain inventory series = %d, want 1", got)
	}
}

func TestKodoCollectorOmitsPriorMonthUsageSnapshot(t *testing.T) {
	registry := prometheus.NewRegistry()
	now := time.Now().In(kodoReportingLocation)
	priorMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, kodoReportingLocation).Add(-time.Minute)
	store := &snapshot.ResourceStore[[]kodo.GaugeSample]{}
	store.Publish("summary/bucket", []kodo.GaugeSample{
		{Kind: kodo.GaugeUsageEgressBytes, Bucket: "bucket", Region: "z0", Route: kodo.RouteDirect, Period: kodo.PeriodCurrentMonth, Value: 8192, DataAt: priorMonth},
		{Kind: kodo.GaugeUsageRequests, Bucket: "bucket", Region: "z0", Operation: kodo.OperationPut, Period: kodo.PeriodCurrentMonth, Value: 7, DataAt: priorMonth},
	}, snapshot.Meta{CollectedAt: time.Now(), StaleAfter: 24 * time.Hour})
	registry.MustRegister(NewKodo(&snapshot.Store[[]kodo.Bucket]{}, store))

	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == "qiniu_kodo_usage_egress_bytes" || family.GetName() == "qiniu_kodo_usage_requests" {
			t.Fatalf("prior-month snapshot exposed as current usage: %s", family.GetName())
		}
	}
}

func TestCDNCollectorOmitsIncompleteAccountUsage(t *testing.T) {
	registry := prometheus.NewRegistry()
	now := time.Now()
	stores := CDNStores{
		Inventory:  &snapshot.Store[[]cdn.Domain]{},
		Monitoring: &snapshot.ResourceStore[CDNMonitoringSnapshot]{},
		Analytics:  &snapshot.ResourceStore[CDNAnalyticsSnapshot]{},
		Usage:      &snapshot.Store[CDNUsageSnapshot]{},
		TopIPs:     &snapshot.Store[CDNTopIPSnapshot]{},
	}
	stores.Usage.Publish(CDNUsageSnapshot{Periods: []CDNUsagePeriodSnapshot{{
		Period: CDNUsagePeriodToday,
		Traffic: cdn.TrafficUsageAggregate{
			Domains:      []cdn.DomainTrafficUsage{{Domain: "healthy.example.com", Bytes: 100, Active: true}},
			AccountBytes: 100,
		},
		Complete: false,
	}}}, snapshot.Meta{CollectedAt: now, StaleAfter: time.Hour})
	registry.MustRegister(NewCDN(stores))

	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	foundDomain, foundComplete := false, false
	for _, family := range families {
		switch family.GetName() {
		case "qiniu_cdn_usage_traffic_bytes":
			foundDomain = len(family.Metric) == 1 && family.Metric[0].Gauge.GetValue() == 100
		case "qiniu_cdn_usage_complete":
			foundComplete = len(family.Metric) == 1 && family.Metric[0].Gauge.GetValue() == 0
		case "qiniu_cdn_usage_account_traffic_bytes", "qiniu_cdn_usage_active_domains":
			t.Fatalf("incomplete usage exposed account aggregate %s", family.GetName())
		}
	}
	if !foundDomain || !foundComplete {
		t.Fatalf("incomplete usage domain=%v complete=%v, want per-domain data and complete=0", foundDomain, foundComplete)
	}
}

func TestBillingCollectorIncludesPriorYearInRollingWindow(t *testing.T) {
	now := time.Now()
	stores := BillingStores{
		Balance:       &snapshot.Store[billing.BalanceOverview]{},
		Estimate:      &snapshot.Store[BillingEstimate]{},
		DailyEstimate: &snapshot.Store[[]BillingDailyEstimate]{},
		ResourcePacks: &snapshot.Store[[]billing.ResourcePackMonthOverview]{},
		Finalized:     &snapshot.Store[BillingFinalized]{},
		Last12:        &snapshot.Store[BillingFinalizedMonths]{},
	}
	stores.Last12.Publish(BillingFinalizedMonths{
		Months: []BillingFinalizedMonth{{
			Detail: billing.BillDetail{TotalMoney: billing.Fixed8(100_000_000), Currency: "CNY"},
			Period: billing.BillingPeriod{Start: now.AddDate(-1, 0, 0), End: now.AddDate(-1, 1, 0)},
		}},
	}, snapshot.Meta{CollectedAt: now, StaleAfter: time.Hour})

	registry := prometheus.NewRegistry()
	registry.MustRegister(NewBilling(stores))
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, family := range families {
		found = found || family.GetName() == "qiniu_billing_last_12_months_finalized_cost"
	}
	if !found {
		t.Fatal("in-range prior-year monthly finalized series was not exported")
	}
}

func TestFinalizedDailyCostsExcludeMonthlyItems(t *testing.T) {
	location := billingCollectorLocation
	detail := billing.BillDetail{Currency: "CNY", Items: []billing.BillItem{
		{Start: time.Date(2026, time.July, 3, 0, 0, 0, 0, location), End: time.Date(2026, time.July, 4, 0, 0, 0, 0, location), ItemMoney: 100_000_000, Currency: "CNY", BillPeriod: "daily"},
		{Start: time.Date(2026, time.July, 3, 0, 0, 0, 0, location), End: time.Date(2026, time.July, 4, 0, 0, 0, 0, location), ItemMoney: 50_000_000, Currency: "CNY", BillPeriod: "daily"},
		{Start: time.Date(2026, time.July, 1, 0, 0, 0, 0, location), End: time.Date(2026, time.August, 1, 0, 0, 0, 0, location), ItemMoney: 900_000_000, Currency: "CNY", BillPeriod: "monthly"},
	}}
	got := finalizedDailyCosts(detail)
	if len(got) != 1 || got[0].Date.Format("2006-01-02") != "2026-07-03" || got[0].Cost != 150_000_000 {
		t.Fatalf("finalizedDailyCosts() = %#v", got)
	}
}
