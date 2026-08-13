package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"qiniu-exporter/internal/collector"
	"qiniu-exporter/internal/qiniu/billing"
)

func TestFinalizedHistoryCacheBackfillsOnceAndThenAddsOneMonth(t *testing.T) {
	client := &fakeBillDetailClient{}
	cache := &finalizedHistoryCache{}

	beforeCutoff := shanghaiTestTime(2026, time.August, 4, 23, 59)
	got, err := cache.collect(context.Background(), client, beforeCutoff)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Last12Complete || len(got.Last12.Months) != 12 || len(client.calls) != 12 {
		t.Fatalf("first collection: complete=%t months=%d calls=%d, want true/12/12", got.Last12Complete, len(got.Last12.Months), len(client.calls))
	}
	if monthKey(got.Latest.Period) != "2026-06" {
		t.Fatalf("latest finalized month = %s, want 2026-06", monthKey(got.Latest.Period))
	}

	if _, err := cache.collect(context.Background(), client, beforeCutoff); err != nil {
		t.Fatal(err)
	}
	if len(client.calls) != 12 {
		t.Fatalf("unchanged period made %d calls, want 12", len(client.calls))
	}

	afterCutoff := shanghaiTestTime(2026, time.August, 5, 8, 30)
	got, err = cache.collect(context.Background(), client, afterCutoff)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Last12Complete || len(got.Last12.Months) != 12 || len(client.calls) != 13 {
		t.Fatalf("incremental collection: complete=%t months=%d calls=%d, want true/12/13", got.Last12Complete, len(got.Last12.Months), len(client.calls))
	}
	if gotMonth := monthKey(client.calls[len(client.calls)-1]); gotMonth != "2026-07" {
		t.Fatalf("incremental request month = %s, want 2026-07", gotMonth)
	}
}

func TestFinalizedHistoryCacheRetriesOnlyMissingMonths(t *testing.T) {
	client := &fakeBillDetailClient{failOnce: map[string]bool{"2026-03": true}}
	cache := &finalizedHistoryCache{months: map[string]collector.BillingFinalizedMonth{
		"2025-12": {
			Detail: billing.BillDetail{TotalMoney: billing.Fixed8(100_000_000), Currency: "CNY"},
			Period: billing.BillingPeriod{Start: shanghaiTestTime(2025, time.December, 1, 0, 0), End: shanghaiTestTime(2026, time.January, 1, 0, 0)},
		},
	}}
	now := shanghaiTestTime(2026, time.August, 4, 8, 30)

	partial, err := cache.collect(context.Background(), client, now)
	if err == nil {
		t.Fatal("first collection unexpectedly succeeded")
	}
	if partial.Last12Complete {
		t.Fatal("partial last-12-month history was marked complete")
	}
	if monthKey(partial.Latest.Period) != "2026-06" {
		t.Fatalf("latest month was not retained during history failure: %#v", partial.Latest)
	}
	if got := client.counts["2026-01"]; got != 1 {
		t.Fatalf("January calls after failure = %d, want 1", got)
	}
	if got := client.counts["2026-02"]; got != 1 {
		t.Fatalf("February calls after failure = %d, want 1", got)
	}
	if _, exists := cache.months["2025-12"]; !exists {
		t.Fatal("partial last-12-month failure dropped an in-range prior-year cache entry")
	}

	result, err := cache.collect(context.Background(), client, now)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Last12Complete || len(result.Last12.Months) != 12 {
		t.Fatalf("recovered result: complete=%t month count=%d, want true/12", result.Last12Complete, len(result.Last12.Months))
	}
	if got := client.counts["2026-01"]; got != 1 {
		t.Fatalf("January was fetched again: calls=%d", got)
	}
	if got := client.counts["2026-02"]; got != 1 {
		t.Fatalf("February was fetched again: calls=%d", got)
	}
	if got := client.counts["2026-03"]; got != 2 {
		t.Fatalf("March retry calls = %d, want 2", got)
	}
}

func TestFinalizedHistoryCacheKeepsRollingWindowAcrossYearBoundary(t *testing.T) {
	client := &fakeBillDetailClient{}
	cache := &finalizedHistoryCache{}

	if _, err := cache.collect(context.Background(), client, shanghaiTestTime(2026, time.December, 5, 8, 30)); err != nil {
		t.Fatal(err)
	}
	callCount := len(client.calls)
	result, err := cache.collect(context.Background(), client, shanghaiTestTime(2027, time.January, 3, 8, 30))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Last12Complete || len(result.Last12.Months) != 12 {
		t.Fatalf("new-year result: complete=%t months=%d, want true/12", result.Last12Complete, len(result.Last12.Months))
	}
	if monthKey(result.Latest.Period) != "2026-11" {
		t.Fatalf("latest finalized period = %s, want 2026-11", monthKey(result.Latest.Period))
	}
	if len(client.calls) != callCount {
		t.Fatalf("new-year cache reuse made %d extra calls, want 0", len(client.calls)-callCount)
	}
}

func TestCollectDailyEstimatesBackfillsMonthAndReusesCache(t *testing.T) {
	date := shanghaiTestTime(2026, time.August, 13, 0, 0)
	client := &fakeBillSnapshotClient{}
	cache := map[string]billing.BillSnapshot{
		"2026-08-13": {TotalMoney: 1_200_000_000, Currency: "CNY"},
	}
	got, err := collectDailyEstimates(context.Background(), client, date, cache)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 12 || len(client.calls) != 11 || got[0].Date.Day() != 1 || got[11].Date.Day() != 12 {
		t.Fatalf("first backfill: values=%d calls=%d first=%v last=%v", len(got), len(client.calls), got[0].Date, got[11].Date)
	}
	for _, value := range got {
		if value.Cost != 100_000_000 || value.Currency != "CNY" {
			t.Fatalf("daily estimate = %#v, want 1 CNY", value)
		}
	}
	if _, err := collectDailyEstimates(context.Background(), client, date, cache); err != nil {
		t.Fatal(err)
	}
	if len(client.calls) != 11 {
		t.Fatalf("cached backfill made %d calls, want 11", len(client.calls))
	}
}

type fakeBillSnapshotClient struct{ calls []time.Time }

func (client *fakeBillSnapshotClient) BillSnapshot(_ context.Context, date time.Time) (billing.BillSnapshot, error) {
	client.calls = append(client.calls, date)
	return billing.BillSnapshot{TotalMoney: billing.Fixed8(date.Day()-1) * 100_000_000, Currency: "CNY"}, nil
}

type fakeBillDetailClient struct {
	calls    []billing.BillingPeriod
	counts   map[string]int
	failOnce map[string]bool
}

func (client *fakeBillDetailClient) BillDetail(_ context.Context, period billing.BillingPeriod) (billing.BillDetail, error) {
	key := monthKey(period)
	client.calls = append(client.calls, period)
	if client.counts == nil {
		client.counts = make(map[string]int)
	}
	client.counts[key]++
	if client.failOnce[key] {
		delete(client.failOnce, key)
		return billing.BillDetail{}, errors.New("temporary billing error")
	}
	return billing.BillDetail{
		TotalMoney: billing.Fixed8(int64(period.Start.Month()) * 100_000_000),
		Currency:   "CNY",
	}, nil
}

func shanghaiTestTime(year int, month time.Month, day, hour, minute int) time.Time {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		panic(err)
	}
	return time.Date(year, month, day, hour, minute, 0, 0, location)
}

func monthKey(period billing.BillingPeriod) string {
	return period.Start.Format("2006-01")
}
