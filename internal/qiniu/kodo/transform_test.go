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
