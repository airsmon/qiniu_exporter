package collector

import (
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
	registry.MustRegister(NewKodo(kodoStore))

	cdnStores := CDNStores{
		Monitoring: &snapshot.ResourceStore[CDNMonitoringSnapshot]{},
		Analytics:  &snapshot.ResourceStore[CDNAnalyticsSnapshot]{},
	}
	cdnStores.Monitoring.Publish("cdn.example.com", CDNMonitoringSnapshot{
		Bandwidth: []cdn.BandwidthSample{{Domain: "cdn.example.com", Region: cdn.RegionChina, BitsPerSecond: 100}},
		Traffic:   []cdn.TrafficSample{{Domain: "cdn.example.com", Region: cdn.RegionChina, BytesPerSecond: 200}},
	}, snapshot.Meta{CollectedAt: now, StaleAfter: time.Hour})
	cdnStores.Analytics.Publish("cdn.example.com", CDNAnalyticsSnapshot{
		Requests: cdn.RequestRateSample{Domain: "cdn.example.com", Region: cdn.RegionGlobal, RequestsPerSecond: 10},
		Statuses: []cdn.StatusCodeRateSample{{Domain: "cdn.example.com", Region: cdn.RegionGlobal, Code: "2xx", ResponsesPerSecond: 9}},
		Cache:    cdn.CacheSample{Domain: "cdn.example.com", HitRequestsPerSecond: 8, MissRequestsPerSecond: 2, RequestHitRatio: 0.8, RequestHitRatioValid: true},
	}, snapshot.Meta{CollectedAt: now, StaleAfter: time.Hour})
	registry.MustRegister(NewCDN(cdnStores))

	billingStores := BillingStores{
		Balance:       &snapshot.Store[billing.BalanceOverview]{},
		Estimate:      &snapshot.Store[BillingEstimate]{},
		ResourcePacks: &snapshot.Store[[]billing.ResourcePackMonthOverview]{},
		Finalized:     &snapshot.Store[BillingFinalized]{},
		CurrentYear:   &snapshot.Store[BillingFinalizedYear]{},
	}
	billingStores.Balance.Publish(billing.BalanceOverview{AvailableBalance: billing.Fixed8(1_230_000_000), UnpaidMoney: billing.Fixed8(100_000_000), Currency: "CNY"}, snapshot.Meta{CollectedAt: now, StaleAfter: time.Hour})
	billingStores.ResourcePacks.Publish([]billing.ResourcePackMonthOverview{{ItemName: "traffic", ZoneName: "global", AvailableTime: "month", Unit: "GB", TotalSurplus: 10, MonthUsed: 2, MonthRemain: 8}}, snapshot.Meta{CollectedAt: now, StaleAfter: time.Hour})
	billingYear := now.In(billingCollectorLocation).Year()
	billingStores.Finalized.Publish(BillingFinalized{
		Detail: billing.BillDetail{TotalMoney: billing.Fixed8(600_000_000), Currency: "CNY"},
		Period: billing.BillingPeriod{Start: time.Date(billingYear, time.June, 1, 0, 0, 0, 0, time.UTC), End: time.Date(billingYear, time.July, 1, 0, 0, 0, 0, time.UTC)},
	}, snapshot.Meta{CollectedAt: now, StaleAfter: time.Hour})
	billingStores.CurrentYear.Publish(BillingFinalizedYear{
		Year: billingYear,
		Months: []BillingFinalizedMonth{
			{Detail: billing.BillDetail{TotalMoney: billing.Fixed8(100_000_000), Currency: "CNY"}, Period: billing.BillingPeriod{Start: time.Date(billingYear, time.January, 1, 0, 0, 0, 0, time.UTC), End: time.Date(billingYear, time.February, 1, 0, 0, 0, 0, time.UTC)}},
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
	for _, family := range families {
		names[family.GetName()] = len(family.Metric)
		if family.GetName() == "qiniu_billing_current_year_monthly_finalized_cost" {
			for _, metric := range family.Metric {
				if len(metric.Label) != 2 {
					t.Fatalf("monthly finalized metric labels = %d, want exactly currency and month", len(metric.Label))
				}
				labels := make(map[string]string, len(metric.Label))
				for _, label := range metric.Label {
					labels[label.GetName()] = label.GetValue()
				}
				if labels["currency"] != "CNY" || (labels["month"] != "01" && labels["month"] != "02") {
					t.Fatalf("unexpected monthly finalized labels: %#v", labels)
				}
				monthlyLabels[labels["month"]] = metric.GetGauge().GetValue()
			}
		}
	}
	want := []string{
		"qiniu_kodo_storage_bytes", "qiniu_kodo_objects", "qiniu_kodo_requests_per_second", "qiniu_kodo_egress_bytes_per_second",
		"qiniu_cdn_monitoring_bandwidth_bits_per_second", "qiniu_cdn_monitoring_traffic_bytes_per_second", "qiniu_cdn_requests_per_second", "qiniu_cdn_http_responses_per_second",
		"qiniu_billing_available_balance", "qiniu_billing_unpaid_amount", "qiniu_billing_resource_pack_records", "qiniu_billing_resource_pack_remaining_ratio",
		"qiniu_billing_last_finalized_cost", "qiniu_billing_current_year_monthly_finalized_cost",
	}
	for _, name := range want {
		if names[name] == 0 {
			t.Errorf("metric family %s was not collected", name)
		}
	}
	if got := names["qiniu_billing_current_year_monthly_finalized_cost"]; got != 2 {
		t.Fatalf("current-year monthly finalized series = %d, want 2", got)
	}
	if monthlyLabels["01"] != 1 || monthlyLabels["02"] != 2 {
		t.Fatalf("current-year monthly finalized values = %#v, want 01=1 and 02=2", monthlyLabels)
	}
}

func TestBillingCollectorHidesPriorYearHistory(t *testing.T) {
	now := time.Now()
	stores := BillingStores{
		Balance:       &snapshot.Store[billing.BalanceOverview]{},
		Estimate:      &snapshot.Store[BillingEstimate]{},
		ResourcePacks: &snapshot.Store[[]billing.ResourcePackMonthOverview]{},
		Finalized:     &snapshot.Store[BillingFinalized]{},
		CurrentYear:   &snapshot.Store[BillingFinalizedYear]{},
	}
	stores.CurrentYear.Publish(BillingFinalizedYear{
		Year: now.In(billingCollectorLocation).Year() - 1,
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
	for _, family := range families {
		if family.GetName() == "qiniu_billing_current_year_monthly_finalized_cost" {
			t.Fatal("prior-year monthly finalized series was exported")
		}
	}
}
