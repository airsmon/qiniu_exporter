package cdn

import (
	"fmt"
	"math"
	"time"
)

// Granularity is a Qiniu CDN usage bucket size.
type Granularity string

const (
	GranularityFiveMinutes Granularity = "5min"
	GranularityHour        Granularity = "hour"
	GranularityDay         Granularity = "day"
)

func (granularity Granularity) String() string { return string(granularity) }

func (granularity Granularity) Valid() bool {
	switch granularity {
	case GranularityFiveMinutes, GranularityHour, GranularityDay:
		return true
	default:
		return false
	}
}

// UsageBatch binds one API response to the domains sent in that request. A
// transform accepts multiple batches so account peaks can be calculated from
// aligned points across every domain, rather than by summing batch peaks.
type UsageBatch struct {
	Domains  []string
	Response UsageResponse
}

type DomainTrafficUsage struct {
	Domain       string
	ChinaBytes   float64
	OverseaBytes float64
	Bytes        float64
	Active       bool
}

type TrafficUsageAggregate struct {
	PeriodStart  time.Time
	PeriodEnd    time.Time
	BucketCount  int
	Domains      []DomainTrafficUsage
	AccountBytes float64
}

type DomainBandwidthUsage struct {
	Domain            string
	PeakBitsPerSecond float64
	PeakAt            time.Time
	Active            bool
}

type BandwidthUsageAggregate struct {
	PeriodStart              time.Time
	PeriodEnd                time.Time
	BucketCount              int
	Domains                  []DomainBandwidthUsage
	AccountPeakBitsPerSecond float64
	AccountPeakAt            time.Time
}

// AggregateTrafficUsage sums upstream traffic buckets. Qiniu returns bytes per
// bucket, so this remains a Gauge snapshot and must not be treated as a
// monotonic counter.
func AggregateTrafficUsage(
	batches []UsageBatch,
	granularity Granularity,
	periodStart, periodEnd time.Time,
	location *time.Location,
) (TrafficUsageAggregate, error) {
	var result TrafficUsageAggregate
	validated, err := validateUsageBatches(batches, granularity, periodStart, periodEnd, location)
	if err != nil {
		return result, err
	}

	result.PeriodStart = validated.periodStart
	result.PeriodEnd = validated.periodEnd
	result.BucketCount = len(validated.starts)
	result.Domains = make([]DomainTrafficUsage, 0, len(validated.domains))

	for batchIndex, batch := range batches {
		selected := validated.ranges[batchIndex]
		for _, domain := range batch.Domains {
			usage := DomainTrafficUsage{Domain: domain}
			if series, ok := batch.Response.Data[domain]; ok {
				for point := selected.startIndex; point < selected.endIndex; point++ {
					usage.ChinaBytes, err = addUsageValue(usage.ChinaBytes, optionalSeriesValue(series.China, point))
					if err != nil {
						return TrafficUsageAggregate{}, err
					}
					usage.OverseaBytes, err = addUsageValue(usage.OverseaBytes, optionalSeriesValue(series.Oversea, point))
					if err != nil {
						return TrafficUsageAggregate{}, err
					}
				}
			}
			usage.Bytes, err = addUsageValue(usage.ChinaBytes, usage.OverseaBytes)
			if err != nil {
				return TrafficUsageAggregate{}, err
			}
			usage.Active = usage.Bytes > 0
			result.AccountBytes, err = addUsageValue(result.AccountBytes, usage.Bytes)
			if err != nil {
				return TrafficUsageAggregate{}, err
			}
			result.Domains = append(result.Domains, usage)
		}
	}
	return result, nil
}

// AggregateBandwidthUsage calculates five-minute domain and account peaks.
// Coarser Qiniu bandwidth buckets are deliberately rejected because the public
// API does not define whether hour/day values are maxima or averages.
func AggregateBandwidthUsage(
	batches []UsageBatch,
	granularity Granularity,
	periodStart, periodEnd time.Time,
	location *time.Location,
) (BandwidthUsageAggregate, error) {
	var result BandwidthUsageAggregate
	if granularity != GranularityFiveMinutes {
		return result, fmt.Errorf("%w: bandwidth peaks require 5min granularity", ErrInvalidInput)
	}
	validated, err := validateUsageBatches(batches, granularity, periodStart, periodEnd, location)
	if err != nil {
		return result, err
	}

	result.PeriodStart = validated.periodStart
	result.PeriodEnd = validated.periodEnd
	result.BucketCount = len(validated.starts)
	result.Domains = make([]DomainBandwidthUsage, 0, len(validated.domains))
	domainIndexes := make(map[string]int, len(validated.domains))
	firstPoint := validated.starts[0]
	for _, domain := range validated.domains {
		domainIndexes[domain] = len(result.Domains)
		result.Domains = append(result.Domains, DomainBandwidthUsage{Domain: domain, PeakAt: firstPoint})
	}
	result.AccountPeakAt = firstPoint

	for point := range validated.starts {
		accountAtPoint := float64(0)
		for batchIndex, batch := range batches {
			responsePoint := validated.ranges[batchIndex].startIndex + point
			for _, domain := range batch.Domains {
				bandwidthAtPoint := float64(0)
				if series, ok := batch.Response.Data[domain]; ok {
					bandwidthAtPoint, err = addUsageValue(
						optionalSeriesValue(series.China, responsePoint),
						optionalSeriesValue(series.Oversea, responsePoint),
					)
					if err != nil {
						return BandwidthUsageAggregate{}, err
					}
				}
				index := domainIndexes[domain]
				if point == 0 || bandwidthAtPoint > result.Domains[index].PeakBitsPerSecond {
					result.Domains[index].PeakBitsPerSecond = bandwidthAtPoint
					result.Domains[index].PeakAt = validated.starts[point]
				}
				if bandwidthAtPoint > 0 {
					result.Domains[index].Active = true
				}
				accountAtPoint, err = addUsageValue(accountAtPoint, bandwidthAtPoint)
				if err != nil {
					return BandwidthUsageAggregate{}, err
				}
			}
		}
		if point == 0 || accountAtPoint > result.AccountPeakBitsPerSecond {
			result.AccountPeakBitsPerSecond = accountAtPoint
			result.AccountPeakAt = validated.starts[point]
		}
	}
	return result, nil
}

type validatedUsageBatches struct {
	starts      []time.Time
	ranges      []usageBatchRange
	periodStart time.Time
	periodEnd   time.Time
	domains     []string
}

type usageBatchRange struct {
	startIndex int
	endIndex   int
}

func validateUsageBatches(
	batches []UsageBatch,
	granularity Granularity,
	periodStart, periodEnd time.Time,
	location *time.Location,
) (validatedUsageBatches, error) {
	var result validatedUsageBatches
	if len(batches) == 0 {
		return result, fmt.Errorf("%w: usage batches must not be empty", ErrInvalidInput)
	}
	if !granularity.Valid() {
		return result, fmt.Errorf("%w: invalid usage granularity", ErrInvalidInput)
	}
	if location == nil {
		return result, fmt.Errorf("%w: nil timestamp location", ErrInvalidInput)
	}

	seenDomains := make(map[string]struct{})
	for batchIndex, batch := range batches {
		if err := validateUsageDomains(batch.Domains); err != nil {
			return result, err
		}
		for _, domain := range batch.Domains {
			if _, exists := seenDomains[domain]; exists {
				return result, fmt.Errorf("%w: duplicate domain across usage batches", ErrInvalidInput)
			}
			seenDomains[domain] = struct{}{}
			result.domains = append(result.domains, domain)
		}

		starts, err := validateUsageResponse(batch.Response, batch.Domains, granularity, location)
		if err != nil {
			return validatedUsageBatches{}, err
		}
		startIndex, endIndex, normalizedStart, normalizedEnd, err := usagePeriodIndexes(
			starts, granularity, periodStart, periodEnd, location,
		)
		if err != nil {
			return validatedUsageBatches{}, err
		}
		selectedStarts := starts[startIndex:endIndex]
		if batchIndex == 0 {
			result.starts = append([]time.Time(nil), selectedStarts...)
			result.periodStart = normalizedStart
			result.periodEnd = normalizedEnd
		} else if !sameUsagePoints(result.starts, selectedStarts) {
			return validatedUsageBatches{}, fmt.Errorf("%w: usage batch period axes differ", ErrSeriesMisaligned)
		}
		result.ranges = append(result.ranges, usageBatchRange{startIndex: startIndex, endIndex: endIndex})
	}
	return result, nil
}

func validateUsageResponse(
	response UsageResponse,
	domains []string,
	granularity Granularity,
	location *time.Location,
) ([]time.Time, error) {
	if err := checkBusinessCode(response.Code, response.Error); err != nil {
		return nil, err
	}
	expected := make(map[string]struct{}, len(domains))
	for _, domain := range domains {
		expected[domain] = struct{}{}
	}
	for domain := range response.Data {
		if _, ok := expected[domain]; !ok {
			return nil, fmt.Errorf("%w: response contains a domain outside the requested batch", ErrSeriesMisaligned)
		}
	}

	starts, err := parseUsagePoints(response.Times, granularity, location)
	if err != nil {
		return nil, err
	}
	for _, domain := range domains {
		series, ok := response.Data[domain]
		if !ok {
			continue
		}
		if err := validateOptionalSeries(domain+".china", series.China, len(starts)); err != nil {
			return nil, err
		}
		if err := validateOptionalSeries(domain+".oversea", series.Oversea, len(starts)); err != nil {
			return nil, err
		}
	}
	return starts, nil
}

func parseUsagePoints(rawPoints []string, granularity Granularity, location *time.Location) ([]time.Time, error) {
	if len(rawPoints) == 0 {
		return nil, ErrNoSafePoint
	}
	starts := make([]time.Time, len(rawPoints))
	for index, raw := range rawPoints {
		parsed, err := time.ParseInLocation(monitoringTimeLayout, raw, location)
		if err != nil || parsed.Format(monitoringTimeLayout) != raw {
			return nil, fmt.Errorf("%w: point %d cannot be parsed", ErrInvalidTimestamp, index)
		}
		if !granularity.aligned(parsed) {
			return nil, fmt.Errorf("%w: point %d is not %s aligned", ErrInvalidTimestamp, index, granularity)
		}
		if index > 0 && !parsed.Equal(granularity.bucketEnd(starts[index-1])) {
			return nil, fmt.Errorf("%w: points %d and %d", ErrNonContinuous, index-1, index)
		}
		starts[index] = parsed
	}
	return starts, nil
}

func usagePeriodIndexes(
	starts []time.Time,
	granularity Granularity,
	periodStart, periodEnd time.Time,
	location *time.Location,
) (int, int, time.Time, time.Time, error) {
	if periodStart.IsZero() || periodEnd.IsZero() {
		return 0, 0, time.Time{}, time.Time{}, fmt.Errorf("%w: usage period bounds must be non-zero", ErrInvalidInput)
	}
	start := periodStart.In(location)
	end := periodEnd.In(location)
	if !start.Before(end) {
		return 0, 0, time.Time{}, time.Time{}, fmt.Errorf("%w: usage period end must follow start", ErrInvalidInput)
	}
	if !granularity.aligned(start) || !granularity.aligned(end) {
		return 0, 0, time.Time{}, time.Time{}, fmt.Errorf("%w: usage period is not %s aligned", ErrInvalidInput, granularity)
	}

	startIndex := -1
	for index, point := range starts {
		if point.Equal(start) {
			startIndex = index
			break
		}
	}
	if startIndex < 0 {
		return 0, 0, time.Time{}, time.Time{}, fmt.Errorf("%w: response does not cover usage period start", ErrSeriesMisaligned)
	}

	cursor := start
	endIndex := startIndex
	for cursor.Before(end) {
		if endIndex >= len(starts) || !starts[endIndex].Equal(cursor) {
			return 0, 0, time.Time{}, time.Time{}, fmt.Errorf("%w: response does not fully cover usage period", ErrSeriesMisaligned)
		}
		cursor = granularity.bucketEnd(cursor)
		endIndex++
	}
	if !cursor.Equal(end) {
		return 0, 0, time.Time{}, time.Time{}, fmt.Errorf("%w: usage period has a partial bucket", ErrInvalidInput)
	}
	return startIndex, endIndex, start, end, nil
}

func (granularity Granularity) aligned(value time.Time) bool {
	if value.Second() != 0 || value.Nanosecond() != 0 {
		return false
	}
	switch granularity {
	case GranularityFiveMinutes:
		return value.Minute()%5 == 0
	case GranularityHour:
		return value.Minute() == 0
	case GranularityDay:
		return value.Hour() == 0 && value.Minute() == 0
	default:
		return false
	}
}

func (granularity Granularity) bucketEnd(start time.Time) time.Time {
	switch granularity {
	case GranularityFiveMinutes:
		return start.Add(FiveMinuteBucket)
	case GranularityHour:
		return start.Add(time.Hour)
	case GranularityDay:
		return start.AddDate(0, 0, 1)
	default:
		return time.Time{}
	}
}

func sameUsagePoints(left, right []time.Time) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if !left[index].Equal(right[index]) {
			return false
		}
	}
	return true
}

func addUsageValue(left, right float64) (float64, error) {
	result := left + right
	if math.IsNaN(result) || math.IsInf(result, 0) {
		return 0, fmt.Errorf("%w: usage aggregate overflow", ErrInvalidValue)
	}
	return result, nil
}
