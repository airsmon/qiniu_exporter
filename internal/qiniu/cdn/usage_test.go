package cdn

import (
	"errors"
	"math"
	"reflect"
	"testing"
	"time"
)

func TestAggregateTrafficUsageAcrossBatchesAndRegions(t *testing.T) {
	batches := []UsageBatch{
		{
			Domains: []string{"a.example.com", "inactive.example.com"},
			Response: usageResponse(
				[]string{"2026-02-10 10:00:00", "2026-02-10 10:05:00", "2026-02-10 10:10:00"},
				map[string]UsageRegionSeries{
					"a.example.com": {China: []float64{100, 200, 300}, Oversea: []float64{10, 20, 30}},
				},
			),
		},
		{
			Domains: []string{"b.example.com"},
			Response: usageResponse(
				[]string{"2026-02-10 10:00:00", "2026-02-10 10:05:00", "2026-02-10 10:10:00"},
				map[string]UsageRegionSeries{
					"b.example.com": {China: []float64{400, 500, 600}, Oversea: []float64{40, 50, 60}},
				},
			),
		},
	}

	got, err := AggregateTrafficUsage(
		batches,
		GranularityFiveMinutes,
		usageTime(10, 0),
		usageTime(10, 15),
		time.UTC,
	)
	if err != nil {
		t.Fatalf("AggregateTrafficUsage: %v", err)
	}
	wantDomains := []DomainTrafficUsage{
		{Domain: "a.example.com", ChinaBytes: 600, OverseaBytes: 60, Bytes: 660, Active: true},
		{Domain: "inactive.example.com"},
		{Domain: "b.example.com", ChinaBytes: 1500, OverseaBytes: 150, Bytes: 1650, Active: true},
	}
	if !reflect.DeepEqual(got.Domains, wantDomains) {
		t.Fatalf("domains = %#v, want %#v", got.Domains, wantDomains)
	}
	if got.AccountBytes != 2310 || got.BucketCount != 3 {
		t.Fatalf("aggregate = %#v", got)
	}
	if !got.PeriodStart.Equal(usageTime(10, 0)) || !got.PeriodEnd.Equal(usageTime(10, 15)) {
		t.Fatalf("period = [%v,%v)", got.PeriodStart, got.PeriodEnd)
	}
}

func TestAggregateTrafficUsageSupportsDayBuckets(t *testing.T) {
	response := usageResponse(
		[]string{"2026-02-01 00:00:00", "2026-02-02 00:00:00", "2026-02-03 00:00:00"},
		map[string]UsageRegionSeries{
			"a.example.com": {China: []float64{1, 2, 100}, Oversea: []float64{3, 4, 100}},
		},
	)
	start := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 2, 3, 0, 0, 0, 0, time.UTC)

	got, err := AggregateTrafficUsage(
		[]UsageBatch{{Domains: []string{"a.example.com"}, Response: response}},
		GranularityDay,
		start,
		end,
		time.UTC,
	)
	if err != nil {
		t.Fatalf("AggregateTrafficUsage: %v", err)
	}
	if got.AccountBytes != 10 || got.BucketCount != 2 {
		t.Fatalf("aggregate = %#v, want 10 bytes across two complete days", got)
	}
}

func TestUsageAggregatesTreatEmptyRegionSeriesAsZero(t *testing.T) {
	times := []string{"2026-02-10 10:00:00", "2026-02-10 10:05:00"}
	batches := []UsageBatch{{
		Domains: []string{"a.example.com"},
		Response: usageResponse(times, map[string]UsageRegionSeries{
			"a.example.com": {China: []float64{100, 200}, Oversea: []float64{}},
		}),
	}}

	traffic, err := AggregateTrafficUsage(batches, GranularityFiveMinutes, usageTime(10, 0), usageTime(10, 10), time.UTC)
	if err != nil {
		t.Fatalf("AggregateTrafficUsage: %v", err)
	}
	if traffic.AccountBytes != 300 || traffic.Domains[0].OverseaBytes != 0 {
		t.Fatalf("traffic = %#v, want 300 china bytes and zero oversea bytes", traffic)
	}

	bandwidth, err := AggregateBandwidthUsage(batches, GranularityFiveMinutes, usageTime(10, 0), usageTime(10, 10), time.UTC)
	if err != nil {
		t.Fatalf("AggregateBandwidthUsage: %v", err)
	}
	if bandwidth.AccountPeakBitsPerSecond != 200 {
		t.Fatalf("bandwidth = %#v, want 200 bps account peak", bandwidth)
	}
}

func TestAggregateBandwidthUsageCalculatesAccountPeakFromAlignedPoints(t *testing.T) {
	times := []string{"2026-02-10 10:00:00", "2026-02-10 10:05:00"}
	batches := []UsageBatch{
		{
			Domains: []string{"a.example.com", "inactive.example.com"},
			Response: usageResponse(times, map[string]UsageRegionSeries{
				"a.example.com": {China: []float64{90, 0}, Oversea: []float64{10, 0}},
			}),
		},
		{
			Domains: []string{"b.example.com"},
			Response: usageResponse(times, map[string]UsageRegionSeries{
				"b.example.com": {China: []float64{0, 80}, Oversea: []float64{0, 20}},
			}),
		},
	}

	got, err := AggregateBandwidthUsage(
		batches,
		GranularityFiveMinutes,
		usageTime(10, 0),
		usageTime(10, 10),
		time.UTC,
	)
	if err != nil {
		t.Fatalf("AggregateBandwidthUsage: %v", err)
	}
	if got.AccountPeakBitsPerSecond != 100 {
		t.Fatalf("account peak = %v, want 100 (not sum of domain peaks 200)", got.AccountPeakBitsPerSecond)
	}
	if !got.AccountPeakAt.Equal(usageTime(10, 0)) {
		t.Fatalf("account peak at = %v, want first equal peak", got.AccountPeakAt)
	}
	wantDomains := []DomainBandwidthUsage{
		{Domain: "a.example.com", PeakBitsPerSecond: 100, PeakAt: usageTime(10, 0), Active: true},
		{Domain: "inactive.example.com", PeakAt: usageTime(10, 0)},
		{Domain: "b.example.com", PeakBitsPerSecond: 100, PeakAt: usageTime(10, 5), Active: true},
	}
	if !reflect.DeepEqual(got.Domains, wantDomains) {
		t.Fatalf("domains = %#v, want %#v", got.Domains, wantDomains)
	}
}

func TestAggregateBandwidthUsageRejectsCoarseBuckets(t *testing.T) {
	_, err := AggregateBandwidthUsage(
		[]UsageBatch{{Domains: []string{"a.example.com"}, Response: UsageResponse{Code: 200}}},
		GranularityDay,
		time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC),
		time.UTC,
	)
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("error = %v, want ErrInvalidInput", err)
	}
}

func TestUsageAggregatesRejectInvalidOrMisalignedResponses(t *testing.T) {
	validTimes := []string{"2026-02-10 10:00:00", "2026-02-10 10:05:00"}
	tests := []struct {
		name    string
		batches []UsageBatch
		start   time.Time
		end     time.Time
		want    error
	}{
		{
			name: "unexpected domain",
			batches: []UsageBatch{{
				Domains: []string{"a.example.com"},
				Response: usageResponse(validTimes, map[string]UsageRegionSeries{
					"other.example.com": {China: []float64{1, 1}, Oversea: []float64{0, 0}},
				}),
			}},
			start: usageTime(10, 0), end: usageTime(10, 10), want: ErrSeriesMisaligned,
		},
		{
			name: "series length",
			batches: []UsageBatch{{
				Domains: []string{"a.example.com"},
				Response: usageResponse(validTimes, map[string]UsageRegionSeries{
					"a.example.com": {China: []float64{1}, Oversea: []float64{0, 0}},
				}),
			}},
			start: usageTime(10, 0), end: usageTime(10, 10), want: ErrSeriesMisaligned,
		},
		{
			name: "negative value",
			batches: []UsageBatch{{
				Domains: []string{"a.example.com"},
				Response: usageResponse(validTimes, map[string]UsageRegionSeries{
					"a.example.com": {China: []float64{-1, 0}, Oversea: []float64{0, 0}},
				}),
			}},
			start: usageTime(10, 0), end: usageTime(10, 10), want: ErrInvalidValue,
		},
		{
			name: "batch time axes",
			batches: []UsageBatch{
				{Domains: []string{"a.example.com"}, Response: usageResponse(validTimes, nil)},
				{Domains: []string{"b.example.com"}, Response: usageResponse(validTimes[:1], nil)},
			},
			start: usageTime(10, 0), end: usageTime(10, 10), want: ErrSeriesMisaligned,
		},
		{
			name:    "period not fully covered",
			batches: []UsageBatch{{Domains: []string{"a.example.com"}, Response: usageResponse(validTimes[:1], nil)}},
			start:   usageTime(10, 0), end: usageTime(10, 10), want: ErrSeriesMisaligned,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := AggregateTrafficUsage(test.batches, GranularityFiveMinutes, test.start, test.end, time.UTC)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestUsageAggregateRejectsOverflow(t *testing.T) {
	response := usageResponse(
		[]string{"2026-02-10 10:00:00"},
		map[string]UsageRegionSeries{
			"a.example.com": {China: []float64{math.MaxFloat64}, Oversea: []float64{math.MaxFloat64}},
		},
	)
	_, err := AggregateTrafficUsage(
		[]UsageBatch{{Domains: []string{"a.example.com"}, Response: response}},
		GranularityFiveMinutes,
		usageTime(10, 0),
		usageTime(10, 5),
		time.UTC,
	)
	if !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("error = %v, want ErrInvalidValue", err)
	}
}

func usageResponse(times []string, data map[string]UsageRegionSeries) UsageResponse {
	return UsageResponse{Code: 200, Times: times, Data: data}
}

func usageTime(hour, minute int) time.Time {
	return time.Date(2026, 2, 10, hour, minute, 0, 0, time.UTC)
}
