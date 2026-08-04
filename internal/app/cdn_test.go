package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
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

type cdnDiscovererFunc func(context.Context) ([]cdn.Domain, error)

func (f cdnDiscovererFunc) ListDomains(ctx context.Context) ([]cdn.Domain, error) { return f(ctx) }

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
	window := newCDNMonitoringWindow(time.Now(), 10*time.Minute, location)

	failed := collectCDNMonitoring(context.Background(), client, domains, window, time.Hour, location, store, metrics, &badMu, bad, nil)
	if len(failed) != 1 || failed.ErrorFor("bad.example.com") == nil || failed.ErrorFor("good-a.example.com") != nil {
		t.Fatalf("unexpected partial errors: %#v", failed)
	}
	if calls != 7 {
		t.Fatalf("calls=%d, want bounded bisection cost 7", calls)
	}
	if values := store.Load(time.Now()); len(values) != 2 {
		t.Fatalf("published resources=%d, want 2 healthy domains", len(values))
	}

	failed = collectCDNMonitoring(context.Background(), client, domains, window, time.Hour, location, store, metrics, &badMu, bad, nil)
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

	window := newCDNMonitoringWindow(time.Now(), 10*time.Minute, location)
	failed := collectCDNMonitoring(context.Background(), client, []string{domain}, window, time.Hour, location, store, metrics, &badMu, bad, nil)
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

	window := newCDNMonitoringWindow(time.Now(), 10*time.Minute, location)
	failed := collectCDNMonitoring(context.Background(), client, domains, window, time.Hour, location, store, metrics, &badMu, bad, nil)
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

func TestBuildCDNUsageSnapshotCalculatesAlignedAccountPeak(t *testing.T) {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	dayStart := time.Date(2026, time.August, 1, 0, 0, 0, 0, location)
	pointCount := 14
	times := make([]string, pointCount)
	flowA, flowB := make([]float64, pointCount), make([]float64, pointCount)
	bandwidthA, bandwidthB := make([]float64, pointCount), make([]float64, pointCount)
	zeros := make([]float64, pointCount)
	for index := range times {
		times[index] = dayStart.Add(time.Duration(index) * cdn.FiveMinuteBucket).Format("2006-01-02 15:04:05")
		flowA[index], flowB[index] = 10, 20
		bandwidthA[index], bandwidthB[index] = 1, 1
	}
	bandwidthA[2] = 100
	bandwidthB[4] = 200

	response := func(domain string, values []float64) cdn.UsageResponse {
		return cdn.UsageResponse{Code: 200, Times: times, Data: map[string]cdn.MonitoringRegionSeries{
			domain: {China: values, Oversea: zeros},
		}}
	}
	round := cdnUsageRound{
		DataEnd: dayStart.Add(70 * time.Minute),
		Bandwidth: []cdn.UsageBatch{
			{Domains: []string{"a.example.com"}, Response: response("a.example.com", bandwidthA)},
			{Domains: []string{"b.example.com"}, Response: response("b.example.com", bandwidthB)},
		},
		Traffic: []cdn.UsageBatch{
			{Domains: []string{"a.example.com"}, Response: response("a.example.com", flowA)},
			{Domains: []string{"b.example.com"}, Response: response("b.example.com", flowB)},
		},
	}
	window := cdnMonitoringWindow{
		SafeBefore: dayStart.Add(70 * time.Minute),
		TodayStart: dayStart,
		HourStart:  dayStart,
		HourEnd:    dayStart.Add(time.Hour),
		StartDate:  "2026-08-01",
		EndDate:    "2026-08-01",
	}

	usage, err := buildCDNUsageSnapshot(
		round, nil, zeroCDNBandwidthUsage([]string{"a.example.com", "b.example.com"}, dayStart),
		window, location, true, true, true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(usage.Periods) != 3 {
		t.Fatalf("usage periods=%d, want 3", len(usage.Periods))
	}
	hour, today, month := usage.Periods[0], usage.Periods[1], usage.Periods[2]
	if hour.Period != collector.CDNUsagePeriodLastCompleteHour || hour.Traffic.AccountBytes != 360 {
		t.Fatalf("last complete hour=%#v, want 360 bytes", hour)
	}
	if !hour.Complete || !today.Complete || !month.Complete {
		t.Fatalf("complete usage round was marked incomplete: %#v", usage.Periods)
	}
	if today.Period != collector.CDNUsagePeriodToday || today.Traffic.AccountBytes != 420 {
		t.Fatalf("today=%#v, want 420 bytes", today)
	}
	if today.Bandwidth.AccountPeakBitsPerSecond != 201 {
		t.Fatalf("account peak=%v, want pointwise peak 201 (not per-domain peak sum 300)", today.Bandwidth.AccountPeakBitsPerSecond)
	}
	if month.Period != collector.CDNUsagePeriodCurrentMonth || month.Traffic.AccountBytes != 420 || !month.HasBandwidth || month.Bandwidth.AccountPeakBitsPerSecond != 201 {
		t.Fatalf("current month=%#v, want 420 bytes and an exact 201 bps five-minute peak", month)
	}
}

func TestBuildCDNUsageSnapshotUsesCommonLaggedDataEnd(t *testing.T) {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	dayStart := time.Date(2026, time.August, 2, 0, 0, 0, 0, location)
	timesA := make([]string, 14)
	timesB := make([]string, 13)
	for index := range timesA {
		timesA[index] = dayStart.Add(time.Duration(index) * cdn.FiveMinuteBucket).Format("2006-01-02 15:04:05")
	}
	copy(timesB, timesA)
	response := func(domain string, times []string) cdn.UsageResponse {
		values := make([]float64, len(times))
		for index := range values {
			values[index] = 1
		}
		return cdn.UsageResponse{Code: 200, Times: times, Data: map[string]cdn.MonitoringRegionSeries{
			domain: {China: values},
		}}
	}
	round := cdnUsageRound{
		DataEnd: dayStart.Add(65 * time.Minute),
		Bandwidth: []cdn.UsageBatch{
			{Domains: []string{"a.example.com"}, Response: response("a.example.com", timesA)},
			{Domains: []string{"b.example.com"}, Response: response("b.example.com", timesB)},
		},
		Traffic: []cdn.UsageBatch{
			{Domains: []string{"a.example.com"}, Response: response("a.example.com", timesA)},
			{Domains: []string{"b.example.com"}, Response: response("b.example.com", timesB)},
		},
	}
	window := cdnMonitoringWindow{
		SafeBefore: dayStart.Add(70 * time.Minute), TodayStart: dayStart,
		HourStart: dayStart, HourEnd: dayStart.Add(time.Hour),
	}

	usage, err := buildCDNUsageSnapshot(round, nil, cdn.BandwidthUsageAggregate{}, window, location, true, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := usage.Periods[1].Traffic.PeriodEnd; !got.Equal(round.DataEnd) {
		t.Fatalf("today period end=%s, want common data end %s", got, round.DataEnd)
	}
	if got := usage.Periods[1].Traffic.AccountBytes; got != 26 {
		t.Fatalf("today traffic=%v, want 13 points across two domains", got)
	}
}

func TestBuildCDNUsageSnapshotPublishesZeroNewDayAtMidnight(t *testing.T) {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	todayStart := time.Date(2026, time.August, 2, 0, 0, 0, 0, location)
	times := make([]string, 12)
	values := make([]float64, 12)
	for index := range times {
		times[index] = todayStart.Add(-time.Hour).Add(time.Duration(index) * cdn.FiveMinuteBucket).Format("2006-01-02 15:04:05")
		values[index] = 10
	}
	response := cdn.UsageResponse{Code: 200, Times: times, Data: map[string]cdn.MonitoringRegionSeries{
		"a.example.com": {China: values},
	}}
	round := cdnUsageRound{
		DataEnd:   todayStart,
		Bandwidth: []cdn.UsageBatch{{Domains: []string{"a.example.com"}, Response: response}},
		Traffic:   []cdn.UsageBatch{{Domains: []string{"a.example.com"}, Response: response}},
	}
	prior := []cdn.UsageBatch{{
		Domains: []string{"a.example.com"},
		Response: cdn.UsageResponse{Code: 200, Times: []string{"2026-08-01 00:00:00"}, Data: map[string]cdn.MonitoringRegionSeries{
			"a.example.com": {China: []float64{500}},
		}},
	}}
	window := cdnMonitoringWindow{
		SafeBefore: todayStart, TodayStart: todayStart,
		HourStart: todayStart.Add(-time.Hour), HourEnd: todayStart,
	}

	usage, err := buildCDNUsageSnapshot(round, prior, cdn.BandwidthUsageAggregate{}, window, location, true, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := usage.Periods[0].Traffic.AccountBytes; got != 120 {
		t.Fatalf("last hour traffic=%v, want 120", got)
	}
	if today := usage.Periods[1]; today.Traffic.AccountBytes != 0 || today.HasBandwidth || !today.Complete {
		t.Fatalf("midnight today snapshot=%#v, want complete zero traffic without a fabricated peak", today)
	}
	if month := usage.Periods[2]; month.Traffic.AccountBytes != 500 {
		t.Fatalf("current-month traffic=%v, want completed prior day 500", month.Traffic.AccountBytes)
	}
}

func TestCDNUsagePeriodsFollowWallClockAcrossMonthRollover(t *testing.T) {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.September, 1, 0, 5, 0, 0, location)
	window := newCDNMonitoringWindow(now, 10*time.Minute, location)
	wantTodayStart := time.Date(2026, time.September, 1, 0, 0, 0, 0, location)
	wantSafeBefore := time.Date(2026, time.August, 31, 23, 55, 0, 0, location)
	if !window.TodayStart.Equal(wantTodayStart) || !window.SafeBefore.Equal(wantSafeBefore) {
		t.Fatalf("window today=%s safe-before=%s, want wall-clock today=%s and lagged data=%s", window.TodayStart, window.SafeBefore, wantTodayStart, wantSafeBefore)
	}
	if window.StartDate != "2026-08-31" || window.EndDate != "2026-08-31" {
		t.Fatalf("query dates=[%s,%s], want lagged monitoring coverage on 2026-08-31", window.StartDate, window.EndDate)
	}

	responseStart := time.Date(2026, time.August, 31, 22, 0, 0, 0, location)
	times := make([]string, 24)
	values := make([]float64, len(times))
	for index := range times {
		times[index] = responseStart.Add(time.Duration(index) * cdn.FiveMinuteBucket).Format("2006-01-02 15:04:05")
		values[index] = 10
	}
	response := cdn.UsageResponse{Code: 200, Times: times, Data: map[string]cdn.MonitoringRegionSeries{
		"a.example.com": {China: values},
	}}
	round := cdnUsageRound{
		DataEnd:   window.SafeBefore,
		Bandwidth: []cdn.UsageBatch{{Domains: []string{"a.example.com"}, Response: response}},
		Traffic:   []cdn.UsageBatch{{Domains: []string{"a.example.com"}, Response: response}},
	}
	usage, err := buildCDNUsageSnapshot(
		round, nil, zeroCDNBandwidthUsage([]string{"a.example.com"}, wantTodayStart),
		window, location, true, true, true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if today := usage.Periods[1]; today.Traffic.AccountBytes != 0 || today.HasBandwidth {
		t.Fatalf("today=%#v, want empty new wall-clock day while source data still belongs to August", today)
	}
	if month := usage.Periods[2]; month.Traffic.AccountBytes != 0 || month.HasBandwidth {
		t.Fatalf("current month=%#v, want empty September instead of relabeled August usage", month)
	}
}

func TestFetchCDNPriorMonthTrafficIsolatesInvalidDomain(t *testing.T) {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
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
			data[domain] = cdn.MonitoringRegionSeries{China: []float64{1, 2}}
		}
		return appJSONValueResponse(t, cdn.UsageResponse{
			Code: 200, Times: []string{"2026-08-01 00:00:00", "2026-08-02 00:00:00"}, Data: data,
		}), nil
	})
	client, err := cdn.NewClient(doer, "https://fusion.test")
	if err != nil {
		t.Fatal(err)
	}
	window := cdnMonitoringWindow{TodayStart: time.Date(2026, time.August, 3, 0, 0, 0, 0, location)}
	got, failed := fetchCDNPriorMonthTraffic(
		context.Background(), client,
		[]string{"good-a.example.com", "bad.example.com", "good-b.example.com"},
		window, location,
	)
	if calls != 5 {
		t.Fatalf("metering calls=%d, want bounded binary isolation cost 5", calls)
	}
	if len(got) != 2 || len(failed) != 1 || failed.ErrorFor("bad.example.com") == nil {
		t.Fatalf("batches=%#v failures=%#v, want two healthy singleton batches and one invalid domain", got, failed)
	}
}

func TestCompletedMonthBandwidthBackfillIsChunkedAndCached(t *testing.T) {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	var calls int
	doer := appDoerFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		var payload struct {
			Domains   string `json:"domains"`
			StartDate string `json:"startDate"`
			EndDate   string `json:"endDate"`
		}
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		start, err := time.ParseInLocation("2006-01-02", payload.StartDate, location)
		if err != nil {
			t.Fatal(err)
		}
		lastDay, err := time.ParseInLocation("2006-01-02", payload.EndDate, location)
		if err != nil {
			t.Fatal(err)
		}
		end := lastDay.AddDate(0, 0, 1)
		if days := int(end.Sub(start) / (24 * time.Hour)); days < 1 || days > cdnBandwidthBackfillDays {
			t.Fatalf("backfill request covered %d days, want 1..%d", days, cdnBandwidthBackfillDays)
		}
		points := int(end.Sub(start) / cdn.FiveMinuteBucket)
		times := make([]string, points)
		values := make([]float64, points)
		for index := range times {
			times[index] = start.Add(time.Duration(index) * cdn.FiveMinuteBucket).Format("2006-01-02 15:04:05")
			values[index] = float64(calls)
		}
		data := map[string]cdn.MonitoringRegionSeries{}
		for _, domain := range strings.Split(payload.Domains, ";") {
			data[domain] = cdn.MonitoringRegionSeries{China: values}
		}
		return appJSONValueResponse(t, cdn.UsageResponse{Code: 200, Times: times, Data: data}), nil
	})
	client, err := cdn.NewClient(doer, "https://fusion.test")
	if err != nil {
		t.Fatal(err)
	}
	cache := &cdnBandwidthCache{}
	domains := []string{"a.example.com", "b.example.com"}
	window := cdnMonitoringWindow{TodayStart: time.Date(2026, time.August, 8, 0, 0, 0, 0, location)}

	first, err := loadCompletedMonthBandwidth(context.Background(), client, domains, window, location, cache)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 || first.BucketCount != 7*24*12 || first.AccountPeakBitsPerSecond != 6 {
		t.Fatalf("first backfill calls=%d aggregate=%#v, want three chunks, 2016 points, and 6 bps peak", calls, first)
	}
	if _, err := loadCompletedMonthBandwidth(context.Background(), client, domains, window, location, cache); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("unchanged completed-day boundary made %d calls, want cached total 3", calls)
	}

	window.TodayStart = window.TodayStart.AddDate(0, 0, 1)
	second, err := loadCompletedMonthBandwidth(context.Background(), client, domains, window, location, cache)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 4 || second.BucketCount != 8*24*12 || second.AccountPeakBitsPerSecond != 8 {
		t.Fatalf("incremental backfill calls=%d aggregate=%#v, want one new day and 8 bps peak", calls, second)
	}
}

func TestCompletedMonthBandwidthBackfillResumesAfterFailedChunk(t *testing.T) {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	var starts []string
	failSecondChunk := true
	doer := appDoerFunc(func(req *http.Request) (*http.Response, error) {
		var payload struct {
			Domains   string `json:"domains"`
			StartDate string `json:"startDate"`
			EndDate   string `json:"endDate"`
		}
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		starts = append(starts, payload.StartDate)
		if payload.StartDate == "2026-08-04" && failSecondChunk {
			failSecondChunk = false
			return appJSONResponse(`{"code":503,"error":"temporary failure","data":{}}`), nil
		}
		start, err := time.ParseInLocation("2006-01-02", payload.StartDate, location)
		if err != nil {
			t.Fatal(err)
		}
		lastDay, err := time.ParseInLocation("2006-01-02", payload.EndDate, location)
		if err != nil {
			t.Fatal(err)
		}
		end := lastDay.AddDate(0, 0, 1)
		points := int(end.Sub(start) / cdn.FiveMinuteBucket)
		times := make([]string, points)
		values := make([]float64, points)
		for index := range times {
			times[index] = start.Add(time.Duration(index) * cdn.FiveMinuteBucket).Format("2006-01-02 15:04:05")
			values[index] = float64(len(starts))
		}
		data := make(map[string]cdn.MonitoringRegionSeries)
		for _, domain := range strings.Split(payload.Domains, ";") {
			data[domain] = cdn.MonitoringRegionSeries{China: values}
		}
		return appJSONValueResponse(t, cdn.UsageResponse{Code: 200, Times: times, Data: data}), nil
	})
	client, err := cdn.NewClient(doer, "https://fusion.test")
	if err != nil {
		t.Fatal(err)
	}
	cache := &cdnBandwidthCache{}
	domains := []string{"a.example.com", "b.example.com"}
	monthStart := time.Date(2026, time.August, 1, 0, 0, 0, 0, location)
	window := cdnMonitoringWindow{TodayStart: time.Date(2026, time.August, 8, 0, 0, 0, 0, location)}

	if _, err := loadCompletedMonthBandwidth(context.Background(), client, domains, window, location, cache); err == nil {
		t.Fatal("backfill succeeded despite the injected second-chunk failure")
	}
	if !cache.PeriodEnd.Equal(time.Date(2026, time.August, 4, 0, 0, 0, 0, location)) || cache.Aggregate.BucketCount != 3*24*12 {
		t.Fatalf("cache after failure=%#v, want first successful three-day chunk committed", cache)
	}
	if !cache.MonthStart.Equal(monthStart) {
		t.Fatalf("cache month start=%s, want %s", cache.MonthStart, monthStart)
	}

	got, err := loadCompletedMonthBandwidth(context.Background(), client, domains, window, location, cache)
	if err != nil {
		t.Fatal(err)
	}
	wantStarts := []string{"2026-08-01", "2026-08-04", "2026-08-04", "2026-08-07"}
	if !slices.Equal(starts, wantStarts) {
		t.Fatalf("backfill starts=%v, want %v; retry must resume at the failed chunk", starts, wantStarts)
	}
	if got.BucketCount != 7*24*12 || !cache.PeriodEnd.Equal(window.TodayStart) {
		t.Fatalf("resumed aggregate=%#v cache-end=%s, want seven completed days", got, cache.PeriodEnd)
	}
}

func TestPartialMonitoringDoesNotReplaceFullBandwidthCache(t *testing.T) {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	dayStart := time.Date(2026, time.August, 1, 0, 0, 0, 0, location)
	times := make([]string, 13)
	values := make([]float64, len(times))
	for index := range times {
		times[index] = dayStart.Add(time.Duration(index) * cdn.FiveMinuteBucket).Format("2006-01-02 15:04:05")
		values[index] = 1
	}
	response := cdn.UsageResponse{Code: 200, Times: times, Data: map[string]cdn.MonitoringRegionSeries{
		"a.example.com": {China: values},
	}}
	round := cdnUsageRound{
		DataEnd:   dayStart.Add(65 * time.Minute),
		Bandwidth: []cdn.UsageBatch{{Domains: []string{"a.example.com"}, Response: response}},
		Traffic:   []cdn.UsageBatch{{Domains: []string{"a.example.com"}, Response: response}},
	}
	fullScope := []string{"a.example.com", "b.example.com"}
	cache := &cdnBandwidthCache{
		MonthStart: dayStart,
		PeriodEnd:  dayStart,
		Domains:    append([]string(nil), fullScope...),
		Aggregate:  zeroCDNBandwidthUsage(fullScope, dayStart),
	}
	requests := 0
	client, err := cdn.NewClient(appDoerFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return nil, errors.New("unexpected request")
	}), "https://fusion.test")
	if err != nil {
		t.Fatal(err)
	}
	catalog := newResourceCatalog(fullScope)
	store := &snapshot.Store[collector.CDNUsageSnapshot]{}
	metrics := telemetry.New(prometheus.NewRegistry(), "test", "test")
	var scopeMu sync.Mutex
	err = collectAndPublishCDNUsage(
		context.Background(), client, round, cdnPartialErrors{"b.example.com": errors.New("monitoring failed")},
		fullScope, catalog,
		cdnMonitoringWindow{SafeBefore: round.DataEnd, TodayStart: dayStart}, location, time.Hour,
		store, cache, &scopeMu, metrics,
	)
	if err == nil {
		t.Fatal("partial monitoring round unexpectedly reported success")
	}
	if requests != 0 {
		t.Fatalf("metering requests=%d, want no monthly bandwidth backfill for a partial scope", requests)
	}
	if !slices.Equal(cache.Domains, fullScope) || !cache.MonthStart.Equal(dayStart) || !cache.PeriodEnd.Equal(dayStart) {
		t.Fatalf("full-scope bandwidth cache was replaced by partial monitoring: %#v", cache)
	}
	usage, _, ok := store.Load(round.DataEnd)
	if !ok || len(usage.Periods) != 3 || usage.Periods[2].HasBandwidth || usage.Periods[2].Complete {
		t.Fatalf("partial usage snapshot=%#v, want published incomplete traffic without monthly bandwidth", usage)
	}
}

func TestUsageScopeChangeCannotRaceOldSnapshotPublish(t *testing.T) {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	dayStart := time.Date(2026, time.August, 1, 0, 0, 0, 0, location)
	times := make([]string, 13)
	values := make([]float64, len(times))
	for index := range times {
		times[index] = dayStart.Add(time.Duration(index) * cdn.FiveMinuteBucket).Format("2006-01-02 15:04:05")
		values[index] = 1
	}
	response := cdn.UsageResponse{Code: 200, Times: times, Data: map[string]cdn.MonitoringRegionSeries{
		"old.example.com": {China: values},
	}}
	round := cdnUsageRound{
		DataEnd:   dayStart.Add(65 * time.Minute),
		Bandwidth: []cdn.UsageBatch{{Domains: []string{"old.example.com"}, Response: response}},
		Traffic:   []cdn.UsageBatch{{Domains: []string{"old.example.com"}, Response: response}},
	}
	client, err := cdn.NewClient(appDoerFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("unexpected request")
	}), "https://fusion.test")
	if err != nil {
		t.Fatal(err)
	}
	metrics := telemetry.New(prometheus.NewRegistry(), "test", "test")

	for iteration := 0; iteration < 200; iteration++ {
		oldScope := []string{"old.example.com"}
		newScope := []string{"new.example.com"}
		catalog := newResourceCatalog(oldScope)
		store := &snapshot.Store[collector.CDNUsageSnapshot]{}
		cache := &cdnBandwidthCache{}
		var scopeMu sync.Mutex
		start := make(chan struct{})
		errCh := make(chan error, 1)
		var changed sync.WaitGroup
		changed.Add(1)
		go func() {
			<-start
			scopeMu.Lock()
			if !slices.Equal(catalog.Snapshot(), newScope) {
				store.Clear()
			}
			catalog.Replace(newScope)
			scopeMu.Unlock()
			changed.Done()
		}()
		go func() {
			<-start
			errCh <- collectAndPublishCDNUsage(
				context.Background(), client, round, nil, oldScope, catalog,
				cdnMonitoringWindow{SafeBefore: round.DataEnd, TodayStart: dayStart}, location, time.Hour,
				store, cache, &scopeMu, metrics,
			)
		}()
		close(start)
		changed.Wait()
		<-errCh
		if store.HasValue() {
			t.Fatalf("iteration %d retained an old-scope usage snapshot after discovery installed the new scope", iteration)
		}
	}
}

func TestMergeCDNBandwidthUsagePreservesPointwiseAccountPeak(t *testing.T) {
	start := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	prior := cdn.BandwidthUsageAggregate{
		PeriodStart: start, PeriodEnd: start.Add(time.Hour), BucketCount: 12,
		Domains: []cdn.DomainBandwidthUsage{
			{Domain: "a.example.com", PeakBitsPerSecond: 100, PeakAt: start, Active: true},
			{Domain: "b.example.com", PeakBitsPerSecond: 20, PeakAt: start, Active: true},
		},
		AccountPeakBitsPerSecond: 110, AccountPeakAt: start,
	}
	current := cdn.BandwidthUsageAggregate{
		PeriodStart: prior.PeriodEnd, PeriodEnd: prior.PeriodEnd.Add(time.Hour), BucketCount: 12,
		Domains: []cdn.DomainBandwidthUsage{
			{Domain: "a.example.com", PeakBitsPerSecond: 10, PeakAt: prior.PeriodEnd, Active: true},
			{Domain: "b.example.com", PeakBitsPerSecond: 200, PeakAt: prior.PeriodEnd, Active: true},
		},
		AccountPeakBitsPerSecond: 205, AccountPeakAt: prior.PeriodEnd,
	}
	got, err := mergeCDNBandwidthUsage(prior, current)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountPeakBitsPerSecond != 205 || got.Domains[0].PeakBitsPerSecond != 100 || got.Domains[1].PeakBitsPerSecond != 200 {
		t.Fatalf("merged bandwidth=%#v", got)
	}
}

func TestMergeCDNTrafficUsageCombinesCompletedDaysAndToday(t *testing.T) {
	start := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	prior := cdn.TrafficUsageAggregate{
		PeriodStart: start, PeriodEnd: start.AddDate(0, 0, 2), BucketCount: 2,
		Domains:      []cdn.DomainTrafficUsage{{Domain: "cdn.example.com", ChinaBytes: 100, OverseaBytes: 20, Bytes: 120, Active: true}},
		AccountBytes: 120,
	}
	today := cdn.TrafficUsageAggregate{
		PeriodStart: prior.PeriodEnd, PeriodEnd: prior.PeriodEnd.Add(time.Hour), BucketCount: 12,
		Domains:      []cdn.DomainTrafficUsage{{Domain: "cdn.example.com", ChinaBytes: 30, OverseaBytes: 10, Bytes: 40, Active: true}},
		AccountBytes: 40,
	}
	got, err := mergeCDNTrafficUsage(prior, today)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountBytes != 160 || got.Domains[0].Bytes != 160 || got.BucketCount != 14 || !got.PeriodStart.Equal(start) || !got.PeriodEnd.Equal(today.PeriodEnd) {
		t.Fatalf("merged usage=%#v", got)
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
		Inventory:  &snapshot.Store[[]cdn.Domain]{},
		Monitoring: &snapshot.ResourceStore[collector.CDNMonitoringSnapshot]{},
		Analytics:  &snapshot.ResourceStore[collector.CDNAnalyticsSnapshot]{},
		Usage:      &snapshot.Store[collector.CDNUsageSnapshot]{},
	}
	cfg := testRealtimeConfig()
	cfg.CDN.StatisticsTimezoneVerified = true
	cfg.CDN.MonitoringUnitsVerified = false
	discoverer := cdnDiscovererFunc(func(context.Context) ([]cdn.Domain, error) {
		return []cdn.Domain{{Name: "cdn.example.com", OperatingState: "success", Product: "cdn"}}, nil
	})
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
		if family.GetName() == "qiniu_exporter_scheduler_skipped_total" {
			for _, metric := range family.Metric {
				labels := map[string]string{}
				for _, label := range metric.Label {
					labels[label.GetName()] = label.GetValue()
				}
				if labels["module"] == "cdn" && labels["collector"] == "monitoring" && labels["reason"] == "units_unverified" && metric.Counter.GetValue() == 1 {
					foundSkipped = true
				}
			}
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
		Inventory:  &snapshot.Store[[]cdn.Domain]{},
		Monitoring: &snapshot.ResourceStore[collector.CDNMonitoringSnapshot]{},
		Analytics:  &snapshot.ResourceStore[collector.CDNAnalyticsSnapshot]{},
		Usage:      &snapshot.Store[collector.CDNUsageSnapshot]{},
	}
	cfg := testRealtimeConfig()
	cfg.CDN.StatisticsTimezoneVerified = false
	cfg.CDN.MonitoringUnitsVerified = true
	discoverer := cdnDiscovererFunc(func(context.Context) ([]cdn.Domain, error) {
		return []cdn.Domain{{Name: "cdn.example.com", OperatingState: "success", Product: "cdn"}}, nil
	})
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
