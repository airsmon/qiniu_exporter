package billing

import "time"

const (
	shanghaiUTCOffset = 8 * 60 * 60
	snapshotReadyHour = 8
	snapshotReadyMin  = 15
)

var shanghaiLocation = time.FixedZone("Asia/Shanghai", shanghaiUTCOffset)

// SelectPeriods selects finance periods using Asia/Shanghai calendar rules.
//
// A snapshot for today is selected at or after 08:15. Before then, the most
// recent safe date is yesterday, except on the first day of a month because
// the snapshot API only accepts dates in the current month. On days 1-4, the
// month before last is the latest period expected to be finalized; starting
// on day 5, the immediately preceding month is selected.
func SelectPeriods(now time.Time) PeriodSelection {
	localNow := now.In(shanghaiLocation)
	today := billingMidnight(localNow)
	readyAt := today.Add(snapshotReadyHour*time.Hour + snapshotReadyMin*time.Minute)

	selection := PeriodSelection{}
	if !localNow.Before(readyAt) {
		selection.SnapshotDate = today
		selection.SnapshotReady = true
	} else if localNow.Day() > 1 {
		selection.SnapshotDate = today.AddDate(0, 0, -1)
		selection.SnapshotReady = true
	}

	currentMonth := time.Date(localNow.Year(), localNow.Month(), 1, 0, 0, 0, 0, shanghaiLocation)
	if localNow.Day() <= 4 {
		selection.Finalized.End = currentMonth.AddDate(0, -1, 0)
		selection.Finalized.Start = currentMonth.AddDate(0, -2, 0)
	} else {
		selection.Finalized.End = currentMonth
		selection.Finalized.Start = currentMonth.AddDate(0, -1, 0)
	}

	return selection
}

// CurrentYearFinalizedPeriods returns the finalized months in the current
// Asia/Shanghai calendar year, oldest first. The current month is never
// included because Qiniu finalizes a monthly bill after that month ends.
func CurrentYearFinalizedPeriods(now time.Time) []BillingPeriod {
	localNow := now.In(shanghaiLocation)
	yearStart := time.Date(localNow.Year(), time.January, 1, 0, 0, 0, 0, shanghaiLocation)
	latest := SelectPeriods(now).Finalized
	if latest.Start.Before(yearStart) {
		return nil
	}

	periods := make([]BillingPeriod, 0, int(latest.Start.Month()))
	for start := yearStart; !start.After(latest.Start); start = start.AddDate(0, 1, 0) {
		periods = append(periods, BillingPeriod{Start: start, End: start.AddDate(0, 1, 0)})
	}
	return periods
}

func billingMidnight(t time.Time) time.Time {
	local := t.In(shanghaiLocation)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, shanghaiLocation)
}

func formatBillingTime(t time.Time) string {
	return billingMidnight(t).Format("2006-01-02T15:04:05")
}

func validMonthlyPeriod(period BillingPeriod) bool {
	start := period.Start.In(shanghaiLocation)
	end := period.End.In(shanghaiLocation)
	if !start.Equal(billingMidnight(start)) || start.Day() != 1 {
		return false
	}
	if !end.Equal(billingMidnight(end)) || end.Day() != 1 {
		return false
	}
	return end.Equal(start.AddDate(0, 1, 0))
}
