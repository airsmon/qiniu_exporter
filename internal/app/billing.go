package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"

	"qiniu-exporter/internal/collector"
	"qiniu-exporter/internal/config"
	"qiniu-exporter/internal/poller"
	"qiniu-exporter/internal/qiniu/billing"
	"qiniu-exporter/internal/snapshot"
	"qiniu-exporter/internal/telemetry"
)

type BillingStaleness struct {
	Balance   time.Duration
	Daily     time.Duration
	Finalized time.Duration
}

type billDetailClient interface {
	BillDetail(context.Context, billing.BillingPeriod) (billing.BillDetail, error)
}

type finalizedHistoryCache struct {
	mu     sync.Mutex
	months map[string]collector.BillingFinalizedMonth
}

type finalizedCollection struct {
	Latest              collector.BillingFinalized
	CurrentYear         collector.BillingFinalizedYear
	CurrentYearComplete bool
}

func (cache *finalizedHistoryCache) collect(ctx context.Context, client billDetailClient, now time.Time) (finalizedCollection, error) {
	cache.mu.Lock()
	defer cache.mu.Unlock()

	if cache.months == nil {
		cache.months = make(map[string]collector.BillingFinalizedMonth)
	}
	periods := billing.CurrentYearFinalizedPeriods(now)
	result := finalizedCollection{
		CurrentYear: collector.BillingFinalizedYear{Year: now.In(billingHistoryLocation).Year()},
	}
	latestPeriod := billing.SelectPeriods(now).Finalized
	latest, err := cache.fetch(ctx, client, latestPeriod)
	if err != nil {
		result.CurrentYearComplete = len(periods) == 0
		return result, err
	}
	result.Latest = collector.BillingFinalized{Detail: latest.Detail, Period: latest.Period}

	var historyErr error
	for _, period := range periods {
		if _, err := cache.fetch(ctx, client, period); err != nil {
			historyErr = err
			break
		}
	}

	currentYear := make([]collector.BillingFinalizedMonth, 0, len(periods))
	keep := make(map[string]collector.BillingFinalizedMonth, len(periods)+1)
	for _, period := range periods {
		key := finalizedPeriodKey(period)
		if month, ok := cache.months[key]; ok {
			currentYear = append(currentYear, month)
			keep[key] = month
		}
	}
	keep[finalizedPeriodKey(latestPeriod)] = latest
	cache.months = keep
	if historyErr == nil {
		result.CurrentYearComplete = true
	}
	result.CurrentYear.Months = currentYear
	return result, historyErr
}

func (cache *finalizedHistoryCache) fetch(ctx context.Context, client billDetailClient, period billing.BillingPeriod) (collector.BillingFinalizedMonth, error) {
	key := finalizedPeriodKey(period)
	if month, ok := cache.months[key]; ok {
		return month, nil
	}
	value, err := client.BillDetail(ctx, period)
	if err != nil {
		return collector.BillingFinalizedMonth{}, err
	}
	if err := validateCurrency(value.Currency); err != nil {
		return collector.BillingFinalizedMonth{}, err
	}
	month := collector.BillingFinalizedMonth{Detail: value, Period: period}
	cache.months[key] = month
	return month, nil
}

func finalizedPeriodKey(period billing.BillingPeriod) string {
	return period.Start.Format("2006-01")
}

var billingHistoryLocation = time.FixedZone("Asia/Shanghai", 8*60*60)

func RegisterBilling(
	scheduler *poller.Scheduler,
	client *billing.Client,
	resourcePackAllowlist []config.ResourcePackAllowlist,
	stores collector.BillingStores,
	stale BillingStaleness,
	metrics *telemetry.Metrics,
) error {
	metrics.InitCollector("billing", "balance")
	metrics.SetCollectorStaleAfter("billing", "balance", stale.Balance)
	metrics.SetCollectorStaleAfter("billing", "estimate", stale.Daily)
	metrics.InitCollector("billing", "finalized")
	metrics.SetCollectorStaleAfter("billing", "finalized", stale.Finalized)
	if err := scheduler.Add(poller.Job{
		Name:       "billing/balance",
		Interval:   time.Hour,
		Timeout:    time.Minute,
		RunOnStart: true,
		Run: func(ctx context.Context) error {
			value, err := client.BalanceOverview(ctx)
			if err != nil {
				return err
			}
			if err := validateCurrency(value.Currency); err != nil {
				return err
			}
			stores.Balance.Publish(value, snapshot.Meta{CollectedAt: time.Now(), StaleAfter: stale.Balance})
			return nil
		},
	}); err != nil {
		return err
	}

	if err := scheduler.Add(poller.Job{
		Name:    "billing/estimate",
		Next:    nextShanghaiTime(8, 15),
		Timeout: time.Minute,
		RunOnStartWhen: func(now time.Time) bool {
			return billing.SelectPeriods(now).SnapshotReady
		},
		Run: func(ctx context.Context) error {
			periods := billing.SelectPeriods(time.Now())
			if !periods.SnapshotReady {
				return errors.New("billing snapshot is not ready")
			}
			value, err := client.BillSnapshot(ctx, periods.SnapshotDate)
			if err != nil {
				return err
			}
			if err := validateCurrency(value.Currency); err != nil {
				return err
			}
			periodStart, periodEnd := estimatePeriod(periods.SnapshotDate)
			stores.Estimate.Publish(collector.BillingEstimate{
				Snapshot: value, PeriodStart: periodStart, PeriodEnd: periodEnd,
			}, snapshot.Meta{CollectedAt: time.Now(), DataAt: periodEnd, StaleAfter: stale.Daily})
			metrics.SetDataTimestamp("billing", "estimate", periodEnd)
			return nil
		},
	}); err != nil {
		return err
	}

	if len(resourcePackAllowlist) > 0 {
		metrics.SetCollectorStaleAfter("billing", "resource_packs", stale.Daily)
		if err := scheduler.Add(poller.Job{
			Name:    "billing/resource_packs",
			Next:    nextShanghaiTime(8, 16),
			Timeout: 2 * time.Minute,
			RunOnStartWhen: func(now time.Time) bool {
				return !now.Before(todayShanghaiTimeAt(now, 8, 15))
			},
			Run: func(ctx context.Context) error {
				packs, err := client.ResourcePackMonthOverview(ctx)
				if err != nil {
					return err
				}
				if err := validateResourcePacks(packs, resourcePackAllowlist); err != nil {
					return err
				}
				stores.ResourcePacks.Publish(packs, snapshot.Meta{CollectedAt: time.Now(), StaleAfter: stale.Daily})
				return nil
			},
		}); err != nil {
			return err
		}
	} else {
		metrics.ObserveSkipped("billing/resource_packs", "allowlist_empty")
	}

	finalizedCache := &finalizedHistoryCache{}
	if err := scheduler.Add(poller.Job{
		Name:       "billing/finalized",
		Next:       nextShanghaiTime(8, 30),
		Timeout:    time.Minute,
		RunOnStart: true,
		Run: func(ctx context.Context) error {
			now := time.Now()
			value, err := finalizedCache.collect(ctx, client, now)
			if !value.Latest.Period.Start.IsZero() {
				stores.Finalized.Publish(value.Latest, snapshot.Meta{
					CollectedAt: now, DataAt: value.Latest.Period.End, StaleAfter: stale.Finalized,
				})
				metrics.SetDataTimestamp("billing", "finalized", value.Latest.Period.End)
			}
			if value.CurrentYearComplete {
				stores.CurrentYear.Publish(value.CurrentYear, snapshot.Meta{
					CollectedAt: now, StaleAfter: stale.Finalized,
				})
			}
			return err
		},
	}); err != nil {
		return err
	}
	return nil
}

func estimatePeriod(snapshotDate time.Time) (time.Time, time.Time) {
	end := snapshotDate
	start := time.Date(end.Year(), end.Month(), 1, 0, 0, 0, 0, end.Location())
	if end.Day() == 1 {
		start = start.AddDate(0, -1, 0)
	}
	return start, end
}

func nextShanghaiTime(hour, minute int) func(time.Time) time.Duration {
	return func(now time.Time) time.Duration {
		target := todayShanghaiTimeAt(now, hour, minute)
		if !target.After(now) {
			target = target.AddDate(0, 0, 1)
		}
		return target.Sub(now)
	}
}

func todayShanghaiTimeAt(now time.Time, hour, minute int) time.Time {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		panic("Asia/Shanghai timezone unavailable: " + err.Error())
	}
	local := now.In(location)
	return time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, location)
}

func validateCurrency(value string) error {
	if value != "CNY" && value != "USD" {
		return fmt.Errorf("billing: unsupported currency code")
	}
	return nil
}

func validateResourcePacks(packs []billing.ResourcePackMonthOverview, allowlist []config.ResourcePackAllowlist) error {
	if len(packs) > 10_000 {
		return errors.New("billing: resource-pack response exceeds 10000 records")
	}
	seen := make(map[string]struct{}, len(packs))
	allowed := make(map[string]struct{}, len(allowlist))
	for _, pack := range allowlist {
		allowed[strings.Join([]string{pack.Item, pack.Zone, pack.AvailableTime, pack.Unit}, "\x00")] = struct{}{}
	}
	for index, pack := range packs {
		labels := []string{pack.ItemName, pack.ZoneName, pack.AvailableTime, pack.Unit}
		for _, label := range labels {
			if label == "" || len(label) > 128 || strings.IndexFunc(label, unicode.IsControl) >= 0 {
				return fmt.Errorf("billing: resource-pack record %d has invalid labels", index)
			}
		}
		if pack.TotalSurplus < 0 || pack.MonthUsed < 0 || pack.MonthRemain < 0 || pack.MonthUsed > pack.TotalSurplus || pack.MonthRemain > pack.TotalSurplus {
			return fmt.Errorf("billing: resource-pack record %d has invalid values", index)
		}
		key := strings.Join(labels, "\x00")
		if _, exists := allowed[key]; !exists {
			return fmt.Errorf("billing: resource-pack record %d is not in the configured label allowlist", index)
		}
		if _, exists := seen[key]; exists {
			return fmt.Errorf("billing: resource-pack record %d duplicates a label set", index)
		}
		seen[key] = struct{}{}
	}
	return nil
}
