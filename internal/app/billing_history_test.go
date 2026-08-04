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
	if !got.CurrentYearComplete || len(got.CurrentYear.Months) != 6 || len(client.calls) != 6 {
		t.Fatalf("first collection: complete=%t months=%d calls=%d, want true/6/6", got.CurrentYearComplete, len(got.CurrentYear.Months), len(client.calls))
	}
	if monthKey(got.Latest.Period) != "2026-06" {
		t.Fatalf("latest finalized month = %s, want 2026-06", monthKey(got.Latest.Period))
	}

	if _, err := cache.collect(context.Background(), client, beforeCutoff); err != nil {
		t.Fatal(err)
	}
	if len(client.calls) != 6 {
		t.Fatalf("unchanged period made %d calls, want 6", len(client.calls))
	}

	afterCutoff := shanghaiTestTime(2026, time.August, 5, 8, 30)
	got, err = cache.collect(context.Background(), client, afterCutoff)
	if err != nil {
		t.Fatal(err)
	}
	if !got.CurrentYearComplete || len(got.CurrentYear.Months) != 7 || len(client.calls) != 7 {
		t.Fatalf("incremental collection: complete=%t months=%d calls=%d, want true/7/7", got.CurrentYearComplete, len(got.CurrentYear.Months), len(client.calls))
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
	if partial.CurrentYearComplete {
		t.Fatal("partial current-year history was marked complete")
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
	if _, exists := cache.months["2025-12"]; exists {
		t.Fatal("partial current-year failure retained a prior-year cache entry")
	}

	result, err := cache.collect(context.Background(), client, now)
	if err != nil {
		t.Fatal(err)
	}
	if !result.CurrentYearComplete || len(result.CurrentYear.Months) != 6 {
		t.Fatalf("recovered result: complete=%t month count=%d, want true/6", result.CurrentYearComplete, len(result.CurrentYear.Months))
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

func TestFinalizedHistoryCacheDropsPriorYearFromCurrentYearSeries(t *testing.T) {
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
	if !result.CurrentYearComplete || len(result.CurrentYear.Months) != 0 {
		t.Fatalf("new-year result: complete=%t current months=%d, want true/0", result.CurrentYearComplete, len(result.CurrentYear.Months))
	}
	if monthKey(result.Latest.Period) != "2026-11" {
		t.Fatalf("latest finalized period = %s, want 2026-11", monthKey(result.Latest.Period))
	}
	if len(client.calls) != callCount {
		t.Fatalf("new-year cache reuse made %d extra calls, want 0", len(client.calls)-callCount)
	}
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
