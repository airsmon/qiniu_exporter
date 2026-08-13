package billing

import "time"

// BalanceOverview is the read-only account balance snapshot returned by
// Qiniu. All monetary fields retain Qiniu's Fixed8 representation.
type BalanceOverview struct {
	AvailableBalance    Fixed8
	CashBalance         Fixed8
	PresentBalance      Fixed8
	CreditLine          Fixed8
	UnpaidMoney         Fixed8
	EstimatedBillsMoney Fixed8
	Currency            string
}

// BillSnapshot is the aggregate estimated cost for the requested snapshot
// date. The snapshot period is selected separately with SelectPeriods.
type BillSnapshot struct {
	TotalMoney Fixed8
	Currency   string
}

// ResourcePackMonthOverview is one current-month resource package aggregate.
type ResourcePackMonthOverview struct {
	ItemName      string `json:"item_name"`
	ZoneName      string `json:"zone_name"`
	AvailableTime string `json:"available_time"`
	TotalSurplus  int64  `json:"total_surplus"`
	MonthUsed     int64  `json:"month_used"`
	MonthRemain   int64  `json:"month_remain"`
	Unit          string `json:"respack_unit"`
}

// BillDetail is the aggregate cost for one finalized monthly billing period.
type BillDetail struct {
	TotalMoney Fixed8
	Currency   string
	Items      []BillItem
}

// BillItem is one finalized billing line. Version 2 preserves exact daily
// boundaries for daily-billed items; monthly-billed items retain a month span.
type BillItem struct {
	Start      time.Time
	End        time.Time
	ItemMoney  Fixed8
	Currency   string
	BillPeriod string
}

// BillingPeriod is a left-closed, right-open calendar-month period in
// Asia/Shanghai.
type BillingPeriod struct {
	Start time.Time
	End   time.Time
}

// PeriodSelection contains the safe daily snapshot date, when one is
// available, and the most recent period expected to be finalized.
type PeriodSelection struct {
	SnapshotDate  time.Time
	SnapshotReady bool
	Finalized     BillingPeriod
}
