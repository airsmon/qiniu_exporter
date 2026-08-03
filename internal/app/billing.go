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

	var finalizedMu sync.Mutex
	var lastFinalized time.Time
	if err := scheduler.Add(poller.Job{
		Name:       "billing/finalized",
		Next:       nextShanghaiTime(8, 30),
		Timeout:    time.Minute,
		RunOnStart: true,
		Run: func(ctx context.Context) error {
			period := billing.SelectPeriods(time.Now()).Finalized
			finalizedMu.Lock()
			alreadyCollected := period.Start.Equal(lastFinalized)
			finalizedMu.Unlock()
			if alreadyCollected {
				return nil
			}
			value, err := client.BillDetail(ctx, period)
			if err != nil {
				return err
			}
			if err := validateCurrency(value.Currency); err != nil {
				return err
			}
			stores.Finalized.Publish(collector.BillingFinalized{Detail: value, Period: period}, snapshot.Meta{
				CollectedAt: time.Now(), DataAt: period.End, StaleAfter: stale.Finalized,
			})
			metrics.SetDataTimestamp("billing", "finalized", period.End)
			finalizedMu.Lock()
			lastFinalized = period.Start
			finalizedMu.Unlock()
			return nil
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
