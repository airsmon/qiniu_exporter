package cdn

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

const FiveMinuteBucket = 5 * time.Minute

const (
	RegionGlobal  = "global"
	RegionChina   = "china"
	RegionOversea = "oversea"
)

var (
	ErrSeriesMisaligned = errors.New("cdn: response series are misaligned")
	ErrInvalidTimestamp = errors.New("cdn: invalid response timestamp")
	ErrInvalidValue     = errors.New("cdn: invalid response value")
	ErrNoSafePoint      = errors.New("cdn: no completed safe 5-minute point")
	ErrNonContinuous    = errors.New("cdn: response points are not continuous 5-minute intervals")
)

type BandwidthSample struct {
	Domain        string
	Region        string
	BitsPerSecond float64
	BucketStart   time.Time
	BucketEnd     time.Time
}

type TrafficSample struct {
	Domain         string
	Region         string
	BytesPerSecond float64
	BucketStart    time.Time
	BucketEnd      time.Time
}

type RequestRateSample struct {
	Domain            string
	Region            string
	RequestsPerSecond float64
	BucketStart       time.Time
	BucketEnd         time.Time
}

type StatusCodeRateSample struct {
	Domain             string
	Region             string
	Code               string
	ResponsesPerSecond float64
	BucketStart        time.Time
	BucketEnd          time.Time
}

type CacheSample struct {
	Domain                    string
	Region                    string
	HitRequestsPerSecond      float64
	MissRequestsPerSecond     float64
	HitTrafficBytesPerSecond  float64
	MissTrafficBytesPerSecond float64
	RequestHitRatio           float64
	TrafficHitRatio           float64
	RequestHitRatioValid      bool
	TrafficHitRatioValid      bool
	BucketStart               time.Time
	BucketEnd                 time.Time
}

// SelectLatestSafeBandwidth5Min validates the complete response and returns
// the latest bucket whose end is not after safeBefore. Timestamps are parsed in
// location because the upstream format carries no timezone.
func SelectLatestSafeBandwidth5Min(response MonitoringResponse, domains []string, safeBefore time.Time, location *time.Location) ([]BandwidthSample, error) {
	index, starts, err := validateMonitoringResponse(response, domains, safeBefore, location)
	if err != nil {
		return nil, err
	}

	result := make([]BandwidthSample, 0, len(domains)*2)
	for _, domain := range domains {
		china, oversea := float64(0), float64(0)
		if series, ok := response.Data[domain]; ok {
			china = optionalSeriesValue(series.China, index)
			oversea = optionalSeriesValue(series.Oversea, index)
		}
		result = append(result,
			BandwidthSample{
				Domain:        domain,
				Region:        RegionChina,
				BitsPerSecond: china,
				BucketStart:   starts[index],
				BucketEnd:     starts[index].Add(FiveMinuteBucket),
			},
			BandwidthSample{
				Domain:        domain,
				Region:        RegionOversea,
				BitsPerSecond: oversea,
				BucketStart:   starts[index],
				BucketEnd:     starts[index].Add(FiveMinuteBucket),
			},
		)
	}
	return result, nil
}

// SelectLatestSafeTraffic5Min converts each monitoring-flow bucket from bytes
// per five minutes to its average bytes per second.
func SelectLatestSafeTraffic5Min(response MonitoringResponse, domains []string, safeBefore time.Time, location *time.Location) ([]TrafficSample, error) {
	index, starts, err := validateMonitoringResponse(response, domains, safeBefore, location)
	if err != nil {
		return nil, err
	}

	bucketSeconds := FiveMinuteBucket.Seconds()
	result := make([]TrafficSample, 0, len(domains)*2)
	for _, domain := range domains {
		china, oversea := float64(0), float64(0)
		if series, ok := response.Data[domain]; ok {
			china = optionalSeriesValue(series.China, index)
			oversea = optionalSeriesValue(series.Oversea, index)
		}
		result = append(result,
			TrafficSample{
				Domain:         domain,
				Region:         RegionChina,
				BytesPerSecond: china / bucketSeconds,
				BucketStart:    starts[index],
				BucketEnd:      starts[index].Add(FiveMinuteBucket),
			},
			TrafficSample{
				Domain:         domain,
				Region:         RegionOversea,
				BytesPerSecond: oversea / bucketSeconds,
				BucketStart:    starts[index],
				BucketEnd:      starts[index].Add(FiveMinuteBucket),
			},
		)
	}
	return result, nil
}

func SelectLatestSafeRequestRate5Min(response RequestCountResponse, domain, region string, safeBefore time.Time, location *time.Location) (RequestRateSample, error) {
	var result RequestRateSample
	if err := checkBusinessCode(response.Code, response.Error); err != nil {
		return result, err
	}
	if err := validateOutputIdentity(domain, region); err != nil {
		return result, err
	}
	index, starts, err := latestSafeIndex(response.Data.Points, analyticsTimeLayout, safeBefore, location)
	if err != nil {
		return result, err
	}
	if err := validateSeries("reqCount", response.Data.ReqCount, len(starts)); err != nil {
		return result, err
	}
	result = RequestRateSample{
		Domain:            domain,
		Region:            region,
		RequestsPerSecond: response.Data.ReqCount[index] / FiveMinuteBucket.Seconds(),
		BucketStart:       starts[index],
		BucketEnd:         starts[index].Add(FiveMinuteBucket),
	}
	return result, nil
}

// SelectLatestSafeAnalyticsBucketEnd5Min returns the end of the latest safe
// analytics bucket, including when an endpoint has no value series to carry it.
func SelectLatestSafeAnalyticsBucketEnd5Min(points []string, safeBefore time.Time, location *time.Location) (time.Time, error) {
	index, starts, err := latestSafeIndex(points, analyticsTimeLayout, safeBefore, location)
	if err != nil {
		return time.Time{}, err
	}
	return starts[index].Add(FiveMinuteBucket), nil
}

func SelectLatestSafeStatusCodeRates5Min(response StatusCodeResponse, domain, region string, safeBefore time.Time, location *time.Location) ([]StatusCodeRateSample, error) {
	if err := checkBusinessCode(response.Code, response.Error); err != nil {
		return nil, err
	}
	if err := validateOutputIdentity(domain, region); err != nil {
		return nil, err
	}
	index, starts, err := latestSafeIndex(response.Data.Points, analyticsTimeLayout, safeBefore, location)
	if err != nil {
		return nil, err
	}
	if response.Data.Codes == nil {
		return nil, fmt.Errorf("%w: missing status code series", ErrUnexpectedResponse)
	}

	codes := make([]string, 0, len(response.Data.Codes))
	aggregatedClasses := make(map[byte]bool, 5)
	exactClasses := make(map[byte]bool, 5)
	for code, values := range response.Data.Codes {
		if !validStatusCodeKey(code) {
			return nil, fmt.Errorf("%w: invalid status code key", ErrUnexpectedResponse)
		}
		if err := validateSeries("codes["+code+"]", values, len(starts)); err != nil {
			return nil, err
		}
		if code[1] == 'x' {
			aggregatedClasses[code[0]] = true
		} else {
			exactClasses[code[0]] = true
		}
		codes = append(codes, code)
	}
	for class := range aggregatedClasses {
		if exactClasses[class] {
			return nil, fmt.Errorf("%w: status class %cxx mixes aggregate and exact codes", ErrUnexpectedResponse, class)
		}
	}
	sort.Strings(codes)

	result := make([]StatusCodeRateSample, 0, len(codes))
	for _, code := range codes {
		result = append(result, StatusCodeRateSample{
			Domain:             domain,
			Region:             region,
			Code:               code,
			ResponsesPerSecond: response.Data.Codes[code][index] / FiveMinuteBucket.Seconds(),
			BucketStart:        starts[index],
			BucketEnd:          starts[index].Add(FiveMinuteBucket),
		})
	}
	return result, nil
}

// SelectLatestSafeCache5Min emits global cache statistics because hitmiss does
// not accept a region selector.
func SelectLatestSafeCache5Min(response HitMissResponse, domain string, safeBefore time.Time, location *time.Location) (CacheSample, error) {
	var result CacheSample
	if err := checkBusinessCode(response.Code, response.Error); err != nil {
		return result, err
	}
	if err := validateOutputIdentity(domain, RegionGlobal); err != nil {
		return result, err
	}
	index, starts, err := latestSafeIndex(response.Data.Points, analyticsTimeLayout, safeBefore, location)
	if err != nil {
		return result, err
	}
	series := []struct {
		name   string
		values []float64
	}{
		{name: "hit", values: response.Data.Hit},
		{name: "miss", values: response.Data.Miss},
		{name: "trafficHit", values: response.Data.TrafficHit},
		{name: "trafficMiss", values: response.Data.TrafficMiss},
	}
	for _, item := range series {
		if err := validateSeries(item.name, item.values, len(starts)); err != nil {
			return result, err
		}
	}

	requestRatio, requestRatioValid := nonnegativeRatio(response.Data.Hit[index], response.Data.Miss[index])
	trafficRatio, trafficRatioValid := nonnegativeRatio(response.Data.TrafficHit[index], response.Data.TrafficMiss[index])
	result = CacheSample{
		Domain:                    domain,
		Region:                    RegionGlobal,
		HitRequestsPerSecond:      response.Data.Hit[index] / FiveMinuteBucket.Seconds(),
		MissRequestsPerSecond:     response.Data.Miss[index] / FiveMinuteBucket.Seconds(),
		HitTrafficBytesPerSecond:  response.Data.TrafficHit[index] / FiveMinuteBucket.Seconds(),
		MissTrafficBytesPerSecond: response.Data.TrafficMiss[index] / FiveMinuteBucket.Seconds(),
		RequestHitRatio:           requestRatio,
		TrafficHitRatio:           trafficRatio,
		RequestHitRatioValid:      requestRatioValid,
		TrafficHitRatioValid:      trafficRatioValid,
		BucketStart:               starts[index],
		BucketEnd:                 starts[index].Add(FiveMinuteBucket),
	}
	return result, nil
}

const (
	monitoringTimeLayout = "2006-01-02 15:04:05"
	analyticsTimeLayout  = "2006-01-02-15-04"
)

func validateMonitoringResponse(response MonitoringResponse, domains []string, safeBefore time.Time, location *time.Location) (int, []time.Time, error) {
	if err := checkBusinessCode(response.Code, response.Error); err != nil {
		return 0, nil, err
	}
	if len(domains) == 0 || len(domains) > maxUsageDomains {
		return 0, nil, fmt.Errorf("%w: expected domains must contain 1 to %d entries", ErrInvalidInput, maxUsageDomains)
	}
	expected := make(map[string]struct{}, len(domains))
	for _, domain := range domains {
		if err := validateDomain(domain, true); err != nil {
			return 0, nil, err
		}
		if _, exists := expected[domain]; exists {
			return 0, nil, fmt.Errorf("%w: duplicate expected domain %q", ErrInvalidInput, domain)
		}
		expected[domain] = struct{}{}
	}
	for domain := range response.Data {
		if _, ok := expected[domain]; !ok {
			return 0, nil, fmt.Errorf("%w: response contains a domain outside the configured allowlist", ErrSeriesMisaligned)
		}
	}

	index, starts, err := latestSafeIndex(response.Times, monitoringTimeLayout, safeBefore, location)
	if err != nil {
		return 0, nil, err
	}
	for _, domain := range domains {
		series, ok := response.Data[domain]
		if !ok {
			continue
		}
		if err := validateOptionalSeries(domain+".china", series.China, len(starts)); err != nil {
			return 0, nil, err
		}
		if err := validateOptionalSeries(domain+".oversea", series.Oversea, len(starts)); err != nil {
			return 0, nil, err
		}
	}
	return index, starts, nil
}

func latestSafeIndex(rawPoints []string, layout string, safeBefore time.Time, location *time.Location) (int, []time.Time, error) {
	if location == nil {
		return 0, nil, fmt.Errorf("%w: nil timestamp location", ErrInvalidInput)
	}
	if safeBefore.IsZero() {
		return 0, nil, fmt.Errorf("%w: zero safeBefore", ErrInvalidInput)
	}
	if len(rawPoints) == 0 {
		return 0, nil, ErrNoSafePoint
	}

	starts := make([]time.Time, len(rawPoints))
	latest := -1
	for i, raw := range rawPoints {
		parsed, err := time.ParseInLocation(layout, raw, location)
		if err != nil || parsed.Format(layout) != raw {
			return 0, nil, fmt.Errorf("%w: point %d cannot be parsed", ErrInvalidTimestamp, i)
		}
		if parsed.Second() != 0 || parsed.Nanosecond() != 0 || parsed.Minute()%5 != 0 {
			return 0, nil, fmt.Errorf("%w: point %d is not 5-minute aligned", ErrInvalidTimestamp, i)
		}
		if i > 0 && parsed.Sub(starts[i-1]) != FiveMinuteBucket {
			return 0, nil, fmt.Errorf("%w: points %d and %d", ErrNonContinuous, i-1, i)
		}
		starts[i] = parsed
		if !parsed.Add(FiveMinuteBucket).After(safeBefore) {
			latest = i
		}
	}
	if latest < 0 {
		return 0, nil, ErrNoSafePoint
	}
	return latest, starts, nil
}

func validateSeries(name string, values []float64, want int) error {
	if len(values) != want {
		return fmt.Errorf("%w: %s has %d values for %d points", ErrSeriesMisaligned, name, len(values), want)
	}
	for i, value := range values {
		if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			return fmt.Errorf("%w: %s[%d]=%v", ErrInvalidValue, name, i, value)
		}
	}
	return nil
}

// Qiniu omits an unused geographic series as an empty array while retaining
// the domain and shared time axis. Treat that representation as an all-zero
// series; a non-empty series must still align exactly with the time axis.
func validateOptionalSeries(name string, values []float64, want int) error {
	if len(values) == 0 {
		return nil
	}
	return validateSeries(name, values, want)
}

func optionalSeriesValue(values []float64, index int) float64 {
	if len(values) == 0 {
		return 0
	}
	return values[index]
}

func validateOutputIdentity(domain, region string) error {
	if err := validateDomain(domain, true); err != nil {
		return err
	}
	if region == "" || strings.TrimSpace(region) != region || containsControl(region) {
		return fmt.Errorf("%w: invalid output region", ErrInvalidInput)
	}
	return nil
}

func validStatusCodeKey(code string) bool {
	if len(code) != 3 || code[0] < '1' || code[0] > '5' {
		return false
	}
	if code[1] == 'x' && code[2] == 'x' {
		return true
	}
	return code[1] >= '0' && code[1] <= '9' && code[2] >= '0' && code[2] <= '9'
}

func nonnegativeRatio(numerator, other float64) (float64, bool) {
	if numerator == 0 && other == 0 {
		return 0, false
	}
	if numerator >= other {
		return 1 / (1 + other/numerator), true
	}
	ratio := numerator / other
	return ratio / (1 + ratio), true
}
