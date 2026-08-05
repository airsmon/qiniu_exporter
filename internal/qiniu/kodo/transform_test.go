package kodo

import (
	"errors"
	"testing"
	"time"
)

func TestSelectLatestSafe5MinAllowsSingleSnapshotPoint(t *testing.T) {
	t.Parallel()

	pointTime := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	got, err := SelectLatestSafe5Min(
		[]Point{{Time: pointTime, Value: 42}},
		pointTime.Add(BucketWidth),
	)
	if err != nil {
		t.Fatalf("SelectLatestSafe5Min() error = %v", err)
	}
	if !got.Time.Equal(pointTime) || got.Value != 42 {
		t.Fatalf("SelectLatestSafe5Min() = %+v", got)
	}
}

func TestSelectLatestSafeRate5MinRequiresContinuousPredecessor(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	_, err := SelectLatestSafeRate5Min([]Point{
		{Time: start, Value: 1},
		{Time: start.Add(2 * BucketWidth), Value: 2},
	}, start.Add(3*BucketWidth))
	if !errors.Is(err, ErrNonContinuous) {
		t.Fatalf("SelectLatestSafeRate5Min() error = %v, want ErrNonContinuous", err)
	}
}

func TestSumMonthToDateDailyUsageAllowsSparseDaysAndPartialCurrentDay(t *testing.T) {
	t.Parallel()

	query := testMonthToDateQuery()
	got, err := sumMonthToDateDailyUsage([]Point{
		{Time: time.Date(2026, 8, 1, 0, 0, 0, 0, query.Begin.Location()), Value: 10},
		{Time: time.Date(2026, 8, 3, 0, 0, 0, 0, query.Begin.Location()), Value: 20},
		{Time: time.Date(2026, 8, 5, 0, 0, 0, 0, query.Begin.Location()), Value: 30},
	}, query)
	if err != nil {
		t.Fatalf("sumMonthToDateDailyUsage() error = %v", err)
	}
	if got != 60 {
		t.Fatalf("sumMonthToDateDailyUsage() = %v, want 60", got)
	}
}
