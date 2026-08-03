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
	}
	billingStores.Balance.Publish(billing.BalanceOverview{AvailableBalance: billing.Fixed8(1_230_000_000), UnpaidMoney: billing.Fixed8(100_000_000), Currency: "CNY"}, snapshot.Meta{CollectedAt: now, StaleAfter: time.Hour})
	billingStores.ResourcePacks.Publish([]billing.ResourcePackMonthOverview{{ItemName: "traffic", ZoneName: "global", AvailableTime: "month", Unit: "GB", TotalSurplus: 10, MonthUsed: 2, MonthRemain: 8}}, snapshot.Meta{CollectedAt: now, StaleAfter: time.Hour})
	registry.MustRegister(NewBilling(billingStores))

	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	names := make(map[string]int, len(families))
	for _, family := range families {
		names[family.GetName()] = len(family.Metric)
	}
	want := []string{
		"qiniu_kodo_storage_bytes", "qiniu_kodo_objects", "qiniu_kodo_requests_per_second", "qiniu_kodo_egress_bytes_per_second",
		"qiniu_cdn_monitoring_bandwidth_bits_per_second", "qiniu_cdn_monitoring_traffic_bytes_per_second", "qiniu_cdn_requests_per_second", "qiniu_cdn_http_responses_per_second",
		"qiniu_billing_available_balance", "qiniu_billing_unpaid_amount", "qiniu_billing_resource_pack_records", "qiniu_billing_resource_pack_remaining_ratio",
	}
	for _, name := range want {
		if names[name] == 0 {
			t.Errorf("metric family %s was not collected", name)
		}
	}
}
