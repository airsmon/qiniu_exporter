package collector

import (
	"sort"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"qiniu-exporter/internal/qiniu/billing"
	"qiniu-exporter/internal/snapshot"
)

type BillingEstimate struct {
	Snapshot    billing.BillSnapshot
	PeriodStart time.Time
	PeriodEnd   time.Time
}

type BillingDailyEstimate struct {
	Date     time.Time
	Cost     billing.Fixed8
	Currency string
}

type BillingFinalized struct {
	Detail billing.BillDetail
	Period billing.BillingPeriod
}

type BillingFinalizedMonth struct {
	Detail billing.BillDetail
	Period billing.BillingPeriod
}

type BillingFinalizedMonths struct {
	Months []BillingFinalizedMonth
}

type BillingStores struct {
	Balance       *snapshot.Store[billing.BalanceOverview]
	Estimate      *snapshot.Store[BillingEstimate]
	DailyEstimate *snapshot.Store[[]BillingDailyEstimate]
	ResourcePacks *snapshot.Store[[]billing.ResourcePackMonthOverview]
	Finalized     *snapshot.Store[BillingFinalized]
	Last12        *snapshot.Store[BillingFinalizedMonths]
}

type BillingCollector struct {
	stores BillingStores

	availableBalance      *prometheus.Desc
	unpaidAmount          *prometheus.Desc
	estimatedCost         *prometheus.Desc
	estimatedDailyCost    *prometheus.Desc
	estimatePeriodStart   *prometheus.Desc
	estimatePeriodEnd     *prometheus.Desc
	resourcePackRecords   *prometheus.Desc
	resourcePackTotal     *prometheus.Desc
	resourcePackUsed      *prometheus.Desc
	resourcePackRemaining *prometheus.Desc
	resourcePackRatio     *prometheus.Desc
	finalizedCost         *prometheus.Desc
	finalizedPeriodStart  *prometheus.Desc
	last12MonthlyCost     *prometheus.Desc
	finalizedDailyCost    *prometheus.Desc
}

func NewBilling(stores BillingStores) *BillingCollector {
	resourceLabels := []string{"item", "zone", "available_time", "unit"}
	return &BillingCollector{
		stores: stores,
		availableBalance: prometheus.NewDesc(
			"qiniu_billing_available_balance",
			"Current available account balance in the currency's major unit.",
			[]string{"currency"}, nil,
		),
		unpaidAmount: prometheus.NewDesc(
			"qiniu_billing_unpaid_amount",
			"Current unpaid account amount in the currency's major unit.",
			[]string{"currency"}, nil,
		),
		estimatedCost: prometheus.NewDesc(
			"qiniu_billing_estimated_cost",
			"Estimated cost for the accompanying left-closed, right-open billing period, in the currency's major unit.",
			[]string{"currency"}, nil,
		),
		estimatedDailyCost: prometheus.NewDesc(
			"qiniu_billing_estimated_daily_cost",
			"Estimated incremental cost for one completed Asia/Shanghai day, derived from adjacent cumulative snapshots, in the currency's major unit.",
			[]string{"currency", "date"}, nil,
		),
		estimatePeriodStart: prometheus.NewDesc(
			"qiniu_billing_estimate_period_start_timestamp_seconds",
			"Unix timestamp at the start of the current estimated-cost period.",
			nil, nil,
		),
		estimatePeriodEnd: prometheus.NewDesc(
			"qiniu_billing_estimate_period_end_timestamp_seconds",
			"Unix timestamp at the exclusive end of the current estimated-cost period.",
			nil, nil,
		),
		resourcePackRecords: prometheus.NewDesc(
			"qiniu_billing_resource_pack_records",
			"Number of records in the most recent complete resource-pack month overview.",
			nil, nil,
		),
		resourcePackTotal: prometheus.NewDesc(
			"qiniu_billing_resource_pack_total",
			"Total available quantity in a current-month resource-pack overview; never aggregate across unit.",
			resourceLabels, nil,
		),
		resourcePackUsed: prometheus.NewDesc(
			"qiniu_billing_resource_pack_used",
			"Used quantity in a current-month resource-pack overview; never aggregate across unit.",
			resourceLabels, nil,
		),
		resourcePackRemaining: prometheus.NewDesc(
			"qiniu_billing_resource_pack_remaining",
			"Remaining quantity in a current-month resource-pack overview; never aggregate across unit.",
			resourceLabels, nil,
		),
		resourcePackRatio: prometheus.NewDesc(
			"qiniu_billing_resource_pack_remaining_ratio",
			"Remaining fraction of a current-month resource-pack overview.",
			resourceLabels, nil,
		),
		finalizedCost: prometheus.NewDesc(
			"qiniu_billing_last_finalized_cost",
			"Total cost of the most recently available finalized month, in the currency's major unit.",
			[]string{"currency"}, nil,
		),
		finalizedPeriodStart: prometheus.NewDesc(
			"qiniu_billing_last_finalized_period_start_timestamp_seconds",
			"Unix timestamp at the start of the most recently available finalized month.",
			nil, nil,
		),
		last12MonthlyCost: prometheus.NewDesc(
			"qiniu_billing_last_12_months_finalized_cost",
			"Finalized cost for one of the latest twelve available Asia/Shanghai billing months, in the currency's major unit.",
			[]string{"currency", "month"}, nil,
		),
		finalizedDailyCost: prometheus.NewDesc(
			"qiniu_billing_finalized_daily_cost",
			"Finalized cost assigned to one Asia/Shanghai day from daily-billed v2 bill-detail items; monthly-billed items are intentionally excluded.",
			[]string{"currency", "date"}, nil,
		),
	}
}

func (c *BillingCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.availableBalance
	ch <- c.unpaidAmount
	ch <- c.estimatedCost
	ch <- c.estimatedDailyCost
	ch <- c.estimatePeriodStart
	ch <- c.estimatePeriodEnd
	ch <- c.resourcePackRecords
	ch <- c.resourcePackTotal
	ch <- c.resourcePackUsed
	ch <- c.resourcePackRemaining
	ch <- c.resourcePackRatio
	ch <- c.finalizedCost
	ch <- c.finalizedPeriodStart
	ch <- c.last12MonthlyCost
	ch <- c.finalizedDailyCost
}

func (c *BillingCollector) Collect(ch chan<- prometheus.Metric) {
	now := time.Now()
	if balance, _, ok := c.stores.Balance.Load(now); ok {
		ch <- prometheus.MustNewConstMetric(c.availableBalance, prometheus.GaugeValue, balance.AvailableBalance.MajorUnits(), balance.Currency)
		ch <- prometheus.MustNewConstMetric(c.unpaidAmount, prometheus.GaugeValue, balance.UnpaidMoney.MajorUnits(), balance.Currency)
	}
	if estimate, _, ok := c.stores.Estimate.Load(now); ok {
		ch <- prometheus.MustNewConstMetric(c.estimatedCost, prometheus.GaugeValue, estimate.Snapshot.TotalMoney.MajorUnits(), estimate.Snapshot.Currency)
		ch <- prometheus.MustNewConstMetric(c.estimatePeriodStart, prometheus.GaugeValue, float64(estimate.PeriodStart.Unix()))
		ch <- prometheus.MustNewConstMetric(c.estimatePeriodEnd, prometheus.GaugeValue, float64(estimate.PeriodEnd.Unix()))
	}
	if c.stores.DailyEstimate != nil {
		if daily, _, ok := c.stores.DailyEstimate.Load(now); ok {
			for _, value := range daily {
				ch <- prometheus.MustNewConstMetric(c.estimatedDailyCost, prometheus.GaugeValue, value.Cost.MajorUnits(), value.Currency, value.Date.Format("2006-01-02"))
			}
		}
	}
	if packs, _, ok := c.stores.ResourcePacks.Load(now); ok {
		ch <- prometheus.MustNewConstMetric(c.resourcePackRecords, prometheus.GaugeValue, float64(len(packs)))
		for _, pack := range packs {
			labels := []string{pack.ItemName, pack.ZoneName, pack.AvailableTime, pack.Unit}
			ch <- prometheus.MustNewConstMetric(c.resourcePackTotal, prometheus.GaugeValue, float64(pack.TotalSurplus), labels...)
			ch <- prometheus.MustNewConstMetric(c.resourcePackUsed, prometheus.GaugeValue, float64(pack.MonthUsed), labels...)
			ch <- prometheus.MustNewConstMetric(c.resourcePackRemaining, prometheus.GaugeValue, float64(pack.MonthRemain), labels...)
			ratio := float64(0)
			if pack.TotalSurplus > 0 {
				ratio = float64(pack.MonthRemain) / float64(pack.TotalSurplus)
			}
			ch <- prometheus.MustNewConstMetric(c.resourcePackRatio, prometheus.GaugeValue, ratio, labels...)
		}
	}
	if finalized, _, ok := c.stores.Finalized.Load(now); ok {
		ch <- prometheus.MustNewConstMetric(c.finalizedCost, prometheus.GaugeValue, finalized.Detail.TotalMoney.MajorUnits(), finalized.Detail.Currency)
		ch <- prometheus.MustNewConstMetric(c.finalizedPeriodStart, prometheus.GaugeValue, float64(finalized.Period.Start.Unix()))
	}
	if last12, _, ok := c.stores.Last12.Load(now); ok {
		for _, month := range last12.Months {
			ch <- prometheus.MustNewConstMetric(
				c.last12MonthlyCost,
				prometheus.GaugeValue,
				month.Detail.TotalMoney.MajorUnits(),
				month.Detail.Currency,
				month.Period.Start.Format("2006-01"),
			)
			for _, daily := range finalizedDailyCosts(month.Detail) {
				ch <- prometheus.MustNewConstMetric(c.finalizedDailyCost, prometheus.GaugeValue, daily.Cost.MajorUnits(), daily.Currency, daily.Date.Format("2006-01-02"))
			}
		}
	}
}

func finalizedDailyCosts(detail billing.BillDetail) []BillingDailyEstimate {
	totals := make(map[string]billing.Fixed8)
	for _, item := range detail.Items {
		if !item.End.Equal(item.Start.AddDate(0, 0, 1)) {
			continue
		}
		totals[item.Start.Format("2006-01-02")] += item.ItemMoney
	}
	result := make([]BillingDailyEstimate, 0, len(totals))
	for date, cost := range totals {
		parsed, _ := time.ParseInLocation("2006-01-02", date, billingCollectorLocation)
		result = append(result, BillingDailyEstimate{Date: parsed, Cost: cost, Currency: detail.Currency})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Date.Before(result[j].Date) })
	return result
}

var billingCollectorLocation = time.FixedZone("Asia/Shanghai", 8*60*60)
