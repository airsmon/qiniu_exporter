package kodo

import (
	"fmt"
	"math"
	"time"
)

// SelectLatestSafe5Min returns the newest safe snapshot point. Capacity and
// object counts are point-in-time gauges, so they do not require a preceding
// bucket.
func SelectLatestSafe5Min(points []Point, safeBefore time.Time) (Point, error) {
	latestSafe, err := latestSafe5MinIndex(points, safeBefore)
	if err != nil {
		return Point{}, err
	}
	return points[latestSafe], nil
}

// SelectLatestSafeRate5Min returns the newest safe interval point and verifies
// that its immediately preceding bucket exists. It deliberately does not fall
// back to an older pair when the newest safe bucket has a gap: the caller
// should keep its previous snapshot and surface the collection failure.
func SelectLatestSafeRate5Min(points []Point, safeBefore time.Time) (Point, error) {
	latestSafe, err := latestSafe5MinIndex(points, safeBefore)
	if err != nil {
		return Point{}, err
	}
	if latestSafe < 1 {
		return Point{}, ErrInsufficientPoints
	}
	if points[latestSafe].Time.Sub(points[latestSafe-1].Time) != BucketWidth {
		return Point{}, fmt.Errorf(
			"%w: %s follows %s",
			ErrNonContinuous,
			points[latestSafe].Time.Format(time.RFC3339),
			points[latestSafe-1].Time.Format(time.RFC3339),
		)
	}
	return points[latestSafe], nil
}

func latestSafe5MinIndex(points []Point, safeBefore time.Time) (int, error) {
	if safeBefore.IsZero() {
		return -1, fmt.Errorf("%w: safe-before time is required", ErrInvalidInput)
	}
	if len(points) == 0 {
		return -1, ErrInsufficientPoints
	}

	latestSafe := -1
	for i, point := range points {
		if point.Time.IsZero() {
			return -1, fmt.Errorf("%w: point %d has zero time", ErrUnexpectedResponse, i)
		}
		if point.Time.UnixNano()%BucketWidth.Nanoseconds() != 0 {
			return -1, fmt.Errorf("%w: point %d is not aligned to five minutes", ErrUnexpectedResponse, i)
		}
		if math.IsNaN(point.Value) || math.IsInf(point.Value, 0) || point.Value < 0 {
			return -1, fmt.Errorf("%w: point %d has invalid value", ErrUnexpectedResponse, i)
		}
		if i > 0 && !point.Time.After(points[i-1].Time) {
			return -1, fmt.Errorf("%w: timestamps are not strictly increasing", ErrNonContinuous)
		}
		if !point.Time.Add(BucketWidth).After(safeBefore) {
			latestSafe = i
		}
	}

	if latestSafe < 0 {
		return -1, ErrInsufficientPoints
	}
	return latestSafe, nil
}

func sampleFromPoint(kind GaugeKind, query Query, point Point) GaugeSample {
	return GaugeSample{
		Kind:   kind,
		Bucket: query.Bucket,
		Region: query.Region,
		Value:  point.Value,
		DataAt: point.Time,
	}
}

func rateSampleFromPoint(kind GaugeKind, query Query, point Point) GaugeSample {
	sample := sampleFromPoint(kind, query, point)
	sample.Value /= BucketWidth.Seconds()
	return sample
}
