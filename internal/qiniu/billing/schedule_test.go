package billing

import (
	"testing"
	"time"
)

func TestSelectPeriods(t *testing.T) {
	tests := []struct {
		name          string
		now           time.Time
		snapshotReady bool
		snapshot      string
		finalizedFrom string
		finalizedTo   string
	}{
		{
			name:          "first day before snapshot cutoff",
			now:           billingTestTime(2026, time.August, 1, 8, 14),
			snapshotReady: false,
			finalizedFrom: "2026-06-01",
			finalizedTo:   "2026-07-01",
		},
		{
			name:          "first day at snapshot cutoff",
			now:           billingTestTime(2026, time.August, 1, 8, 15),
			snapshotReady: true,
			snapshot:      "2026-08-01",
			finalizedFrom: "2026-06-01",
			finalizedTo:   "2026-07-01",
		},
		{
			name:          "third day before snapshot cutoff",
			now:           billingTestTime(2026, time.August, 3, 7, 59),
			snapshotReady: true,
			snapshot:      "2026-08-02",
			finalizedFrom: "2026-06-01",
			finalizedTo:   "2026-07-01",
		},
		{
			name:          "fourth day keeps month before last",
			now:           billingTestTime(2026, time.August, 4, 23, 59),
			snapshotReady: true,
			snapshot:      "2026-08-04",
			finalizedFrom: "2026-06-01",
			finalizedTo:   "2026-07-01",
		},
		{
			name:          "fifth day selects last month",
			now:           billingTestTime(2026, time.August, 5, 0, 0),
			snapshotReady: true,
			snapshot:      "2026-08-04",
			finalizedFrom: "2026-07-01",
			finalizedTo:   "2026-08-01",
		},
		{
			name:          "year boundary",
			now:           billingTestTime(2027, time.January, 3, 8, 15),
			snapshotReady: true,
			snapshot:      "2027-01-03",
			finalizedFrom: "2026-11-01",
			finalizedTo:   "2026-12-01",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := SelectPeriods(test.now)
			if got.SnapshotReady != test.snapshotReady {
				t.Fatalf("SnapshotReady = %t, want %t", got.SnapshotReady, test.snapshotReady)
			}
			if !test.snapshotReady {
				if !got.SnapshotDate.IsZero() {
					t.Fatalf("SnapshotDate = %s, want zero", got.SnapshotDate)
				}
			} else if dateString(got.SnapshotDate) != test.snapshot {
				t.Fatalf("SnapshotDate = %s, want %s", dateString(got.SnapshotDate), test.snapshot)
			}
			if dateString(got.Finalized.Start) != test.finalizedFrom {
				t.Fatalf("Finalized.Start = %s, want %s", dateString(got.Finalized.Start), test.finalizedFrom)
			}
			if dateString(got.Finalized.End) != test.finalizedTo {
				t.Fatalf("Finalized.End = %s, want %s", dateString(got.Finalized.End), test.finalizedTo)
			}
		})
	}
}

func TestSelectPeriodsUsesShanghaiTime(t *testing.T) {
	// 00:14 UTC is 08:14 on the first day in Shanghai.
	got := SelectPeriods(time.Date(2026, time.August, 1, 0, 14, 0, 0, time.UTC))
	if got.SnapshotReady {
		t.Fatal("snapshot unexpectedly ready before 08:15 Asia/Shanghai")
	}
}

func TestCurrentYearFinalizedPeriods(t *testing.T) {
	tests := []struct {
		name string
		now  time.Time
		want []string
	}{
		{
			name: "before previous month is finalized",
			now:  billingTestTime(2026, time.August, 4, 23, 59),
			want: []string{"2026-01", "2026-02", "2026-03", "2026-04", "2026-05", "2026-06"},
		},
		{
			name: "after previous month is finalized",
			now:  billingTestTime(2026, time.August, 5, 0, 0),
			want: []string{"2026-01", "2026-02", "2026-03", "2026-04", "2026-05", "2026-06", "2026-07"},
		},
		{
			name: "new year has no finalized current-year month",
			now:  billingTestTime(2027, time.January, 5, 8, 30),
			want: nil,
		},
		{
			name: "February fourth still has no finalized current-year month",
			now:  billingTestTime(2027, time.February, 4, 23, 59),
			want: nil,
		},
		{
			name: "February fifth includes January",
			now:  billingTestTime(2027, time.February, 5, 0, 0),
			want: []string{"2027-01"},
		},
		{
			name: "December has at most eleven finalized months",
			now:  billingTestTime(2027, time.December, 5, 8, 30),
			want: []string{"2027-01", "2027-02", "2027-03", "2027-04", "2027-05", "2027-06", "2027-07", "2027-08", "2027-09", "2027-10", "2027-11"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			periods := CurrentYearFinalizedPeriods(test.now)
			if len(periods) != len(test.want) {
				t.Fatalf("period count = %d, want %d", len(periods), len(test.want))
			}
			for index, period := range periods {
				if got := period.Start.In(shanghaiLocation).Format("2006-01"); got != test.want[index] {
					t.Fatalf("period %d start = %s, want %s", index, got, test.want[index])
				}
				if !period.End.Equal(period.Start.AddDate(0, 1, 0)) {
					t.Fatalf("period %d is not exactly one month: %#v", index, period)
				}
			}
		})
	}
}

func TestCurrentYearFinalizedPeriodsUsesShanghaiBoundary(t *testing.T) {
	// 16:00 UTC on February 4 is midnight on February 5 in Shanghai, when
	// January becomes the latest expected finalized month.
	periods := CurrentYearFinalizedPeriods(time.Date(2027, time.February, 4, 16, 0, 0, 0, time.UTC))
	if len(periods) != 1 || dateString(periods[0].Start) != "2027-01-01" {
		t.Fatalf("periods at Shanghai cutoff = %#v, want January 2027", periods)
	}
}

func billingTestTime(year int, month time.Month, day, hour, minute int) time.Time {
	return time.Date(year, month, day, hour, minute, 0, 0, shanghaiLocation)
}

func dateString(value time.Time) string {
	return value.In(shanghaiLocation).Format("2006-01-02")
}
