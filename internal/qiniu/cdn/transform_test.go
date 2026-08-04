package cdn

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSelectLatestSafeMonitoringFiveMinuteSamples(t *testing.T) {
	location := time.FixedZone("UTC+8", 8*60*60)
	safeBefore := time.Date(2026, 2, 10, 10, 14, 0, 0, location)
	response := MonitoringResponse{
		Code:  200,
		Times: []string{"2026-02-10 10:00:00", "2026-02-10 10:05:00", "2026-02-10 10:10:00"},
		Data: map[string]MonitoringRegionSeries{
			"a.example.com": {China: []float64{100, 200, 300}, Oversea: []float64{10, 20, 30}},
			"b.example.com": {China: []float64{400, 500, 600}, Oversea: []float64{40, 50, 60}},
		},
	}
	domains := []string{"b.example.com", "a.example.com"}

	bandwidth, err := SelectLatestSafeBandwidth5Min(response, domains, safeBefore, location)
	if err != nil {
		t.Fatalf("SelectLatestSafeBandwidth5Min: %v", err)
	}
	wantBandwidth := []BandwidthSample{
		{Domain: "b.example.com", Region: RegionChina, BitsPerSecond: 500, BucketStart: bucketTime(location, 10, 5), BucketEnd: bucketTime(location, 10, 10)},
		{Domain: "b.example.com", Region: RegionOversea, BitsPerSecond: 50, BucketStart: bucketTime(location, 10, 5), BucketEnd: bucketTime(location, 10, 10)},
		{Domain: "a.example.com", Region: RegionChina, BitsPerSecond: 200, BucketStart: bucketTime(location, 10, 5), BucketEnd: bucketTime(location, 10, 10)},
		{Domain: "a.example.com", Region: RegionOversea, BitsPerSecond: 20, BucketStart: bucketTime(location, 10, 5), BucketEnd: bucketTime(location, 10, 10)},
	}
	if !reflect.DeepEqual(bandwidth, wantBandwidth) {
		t.Fatalf("bandwidth = %#v, want %#v", bandwidth, wantBandwidth)
	}

	traffic, err := SelectLatestSafeTraffic5Min(response, domains, safeBefore, location)
	if err != nil {
		t.Fatalf("SelectLatestSafeTraffic5Min: %v", err)
	}
	if got, want := traffic[0].BytesPerSecond, 500.0/300; got != want {
		t.Fatalf("traffic bytes/s = %v, want %v", got, want)
	}
	if traffic[0].Domain != "b.example.com" || traffic[0].Region != RegionChina {
		t.Fatalf("traffic identity = %s/%s", traffic[0].Domain, traffic[0].Region)
	}
}

func TestSelectLatestSafeMonitoringTreatsOmittedRequestedDomainAsZero(t *testing.T) {
	response := MonitoringResponse{
		Code:  200,
		Times: []string{"2026-02-10 10:00:00"},
		Data: map[string]MonitoringRegionSeries{
			"active.example.com": {China: []float64{300}, Oversea: []float64{0}},
		},
	}
	domains := []string{"active.example.com", "idle.example.com"}
	bandwidth, err := SelectLatestSafeBandwidth5Min(response, domains, bucketTimeUTC(10, 5), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if len(bandwidth) != 4 || bandwidth[2].Domain != "idle.example.com" || bandwidth[2].BitsPerSecond != 0 || bandwidth[3].BitsPerSecond != 0 {
		t.Fatalf("bandwidth=%#v, want explicit zero samples for omitted requested domain", bandwidth)
	}
	traffic, err := SelectLatestSafeTraffic5Min(response, domains, bucketTimeUTC(10, 5), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if traffic[2].BytesPerSecond != 0 || traffic[3].BytesPerSecond != 0 {
		t.Fatalf("traffic=%#v, want explicit zero samples for omitted requested domain", traffic)
	}
}

func TestSelectLatestSafeMonitoringTreatsEmptyRegionSeriesAsZero(t *testing.T) {
	response := MonitoringResponse{
		Code:  200,
		Times: []string{"2026-02-10 10:00:00", "2026-02-10 10:05:00"},
		Data: map[string]MonitoringRegionSeries{
			"a.example.com": {China: []float64{300, 600}, Oversea: []float64{}},
		},
	}
	domains := []string{"a.example.com"}

	bandwidth, err := SelectLatestSafeBandwidth5Min(response, domains, bucketTimeUTC(10, 10), time.UTC)
	if err != nil {
		t.Fatalf("SelectLatestSafeBandwidth5Min: %v", err)
	}
	if bandwidth[0].BitsPerSecond != 600 || bandwidth[1].BitsPerSecond != 0 {
		t.Fatalf("bandwidth = %#v, want china=600 and oversea=0", bandwidth)
	}

	traffic, err := SelectLatestSafeTraffic5Min(response, domains, bucketTimeUTC(10, 10), time.UTC)
	if err != nil {
		t.Fatalf("SelectLatestSafeTraffic5Min: %v", err)
	}
	if traffic[0].BytesPerSecond != 2 || traffic[1].BytesPerSecond != 0 {
		t.Fatalf("traffic = %#v, want china=2 B/s and oversea=0", traffic)
	}
}

func TestSelectLatestSafeRequestRateFiveMinutes(t *testing.T) {
	location := time.UTC
	response := RequestCountResponse{
		Code: 200,
		Data: RequestCountData{
			Points:   []string{"2026-02-10-10-00", "2026-02-10-10-05", "2026-02-10-10-10"},
			ReqCount: []float64{300, 600, 900},
		},
	}

	sample, err := SelectLatestSafeRequestRate5Min(response, "a.example.com", RegionGlobal, bucketTimeUTC(10, 14), location)
	if err != nil {
		t.Fatalf("SelectLatestSafeRequestRate5Min: %v", err)
	}
	if sample.RequestsPerSecond != 2 {
		t.Fatalf("requests/s = %v, want 2", sample.RequestsPerSecond)
	}
	if !sample.BucketStart.Equal(bucketTimeUTC(10, 5)) || !sample.BucketEnd.Equal(bucketTimeUTC(10, 10)) {
		t.Fatalf("bucket = [%v,%v)", sample.BucketStart, sample.BucketEnd)
	}
}

func TestSelectLatestSafeStatusCodeRatesFiveMinutes(t *testing.T) {
	response := StatusCodeResponse{
		Code: 200,
		Data: StatusCodeData{
			Points: []string{"2026-02-10-10-00", "2026-02-10-10-05"},
			Codes: map[string][]float64{
				"404": {30, 60},
				"2xx": {300, 600},
			},
		},
	}

	samples, err := SelectLatestSafeStatusCodeRates5Min(response, "a.example.com", RegionGlobal, bucketTimeUTC(10, 10), time.UTC)
	if err != nil {
		t.Fatalf("SelectLatestSafeStatusCodeRates5Min: %v", err)
	}
	if len(samples) != 2 || samples[0].Code != "2xx" || samples[1].Code != "404" {
		t.Fatalf("codes = %#v, want deterministic [2xx 404]", samples)
	}
	if samples[0].ResponsesPerSecond != 2 || samples[1].ResponsesPerSecond != 0.2 {
		t.Fatalf("response rates = [%v %v]", samples[0].ResponsesPerSecond, samples[1].ResponsesPerSecond)
	}
}

func TestSelectLatestSafeAnalyticsBucketEndWithoutValueSeries(t *testing.T) {
	got, err := SelectLatestSafeAnalyticsBucketEnd5Min(
		[]string{"2026-02-10-10-00", "2026-02-10-10-05"},
		bucketTimeUTC(10, 10),
		time.UTC,
	)
	if err != nil {
		t.Fatal(err)
	}
	if want := bucketTimeUTC(10, 10); !got.Equal(want) {
		t.Fatalf("bucket end=%v, want %v", got, want)
	}
}

func TestStatusCodesRejectAggregateAndExactOverlap(t *testing.T) {
	response := StatusCodeResponse{
		Code: 200,
		Data: StatusCodeData{
			Points: []string{"2026-02-10-10-00"},
			Codes:  map[string][]float64{"5xx": {1}, "500": {1}},
		},
	}
	_, err := SelectLatestSafeStatusCodeRates5Min(response, "a.example.com", RegionGlobal, bucketTimeUTC(10, 5), time.UTC)
	if !errors.Is(err, ErrUnexpectedResponse) {
		t.Fatalf("error=%v, want ErrUnexpectedResponse", err)
	}
}

func TestSelectLatestSafeCacheFiveMinutes(t *testing.T) {
	response := HitMissResponse{
		Code: 200,
		Data: HitMissData{
			Points:      []string{"2026-02-10-10-00", "2026-02-10-10-05"},
			Hit:         []float64{100, 240},
			Miss:        []float64{100, 60},
			TrafficHit:  []float64{1000, 2700},
			TrafficMiss: []float64{1000, 300},
		},
	}

	sample, err := SelectLatestSafeCache5Min(response, "a.example.com", bucketTimeUTC(10, 10), time.UTC)
	if err != nil {
		t.Fatalf("SelectLatestSafeCache5Min: %v", err)
	}
	if sample.Region != RegionGlobal || sample.HitRequestsPerSecond != 0.8 || sample.MissRequestsPerSecond != 0.2 {
		t.Fatalf("request sample = %#v", sample)
	}
	if !sample.RequestHitRatioValid || math.Abs(sample.RequestHitRatio-0.8) > 1e-12 {
		t.Fatalf("request hit ratio = %v (valid=%v)", sample.RequestHitRatio, sample.RequestHitRatioValid)
	}
	if !sample.TrafficHitRatioValid || math.Abs(sample.TrafficHitRatio-0.9) > 1e-12 {
		t.Fatalf("traffic hit ratio = %v (valid=%v)", sample.TrafficHitRatio, sample.TrafficHitRatioValid)
	}
}

func TestCacheZeroDenominatorsDoNotCreateNaN(t *testing.T) {
	response := HitMissResponse{
		Code: 200,
		Data: HitMissData{
			Points:      []string{"2026-02-10-10-00"},
			Hit:         []float64{0},
			Miss:        []float64{0},
			TrafficHit:  []float64{0},
			TrafficMiss: []float64{0},
		},
	}
	sample, err := SelectLatestSafeCache5Min(response, "a.example.com", bucketTimeUTC(10, 5), time.UTC)
	if err != nil {
		t.Fatalf("SelectLatestSafeCache5Min: %v", err)
	}
	if sample.RequestHitRatioValid || sample.TrafficHitRatioValid {
		t.Fatalf("zero denominators must produce invalid ratios: %#v", sample)
	}
	if math.IsNaN(sample.RequestHitRatio) || math.IsNaN(sample.TrafficHitRatio) {
		t.Fatalf("ratios must remain finite: %#v", sample)
	}
}

func TestTransformsRejectMisalignedSeries(t *testing.T) {
	response := MonitoringResponse{
		Code:  200,
		Times: []string{"2026-02-10 10:00:00", "2026-02-10 10:05:00"},
		Data: map[string]MonitoringRegionSeries{
			"a.example.com": {China: []float64{1}, Oversea: []float64{1, 2}},
		},
	}
	_, err := SelectLatestSafeBandwidth5Min(response, []string{"a.example.com"}, bucketTimeUTC(10, 10), time.UTC)
	if !errors.Is(err, ErrSeriesMisaligned) {
		t.Fatalf("error = %v, want ErrSeriesMisaligned", err)
	}
}

func TestTransformsRejectInvalidTimes(t *testing.T) {
	tests := []struct {
		name   string
		points []string
		want   error
	}{
		{name: "not aligned", points: []string{"2026-02-10-10-01"}, want: ErrInvalidTimestamp},
		{name: "gap", points: []string{"2026-02-10-10-00", "2026-02-10-10-10"}, want: ErrNonContinuous},
		{name: "out of order", points: []string{"2026-02-10-10-05", "2026-02-10-10-00"}, want: ErrNonContinuous},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := RequestCountResponse{
				Code: 200,
				Data: RequestCountData{Points: test.points, ReqCount: make([]float64, len(test.points))},
			}
			_, err := SelectLatestSafeRequestRate5Min(response, "a.example.com", RegionGlobal, bucketTimeUTC(11, 0), time.UTC)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestTransformsRejectNegativeAndNonFiniteValues(t *testing.T) {
	for _, value := range []float64{-1, math.NaN(), math.Inf(1)} {
		response := RequestCountResponse{
			Code: 200,
			Data: RequestCountData{Points: []string{"2026-02-10-10-00"}, ReqCount: []float64{value}},
		}
		_, err := SelectLatestSafeRequestRate5Min(response, "a.example.com", RegionGlobal, bucketTimeUTC(10, 5), time.UTC)
		if !errors.Is(err, ErrInvalidValue) {
			t.Errorf("value %v: error = %v, want ErrInvalidValue", value, err)
		}
	}
}

func TestTransformsRejectUnsafeOnlyPoint(t *testing.T) {
	response := RequestCountResponse{
		Code: 200,
		Data: RequestCountData{Points: []string{"2026-02-10-10-00"}, ReqCount: []float64{1}},
	}
	_, err := SelectLatestSafeRequestRate5Min(response, "a.example.com", RegionGlobal, bucketTimeUTC(10, 4), time.UTC)
	if !errors.Is(err, ErrNoSafePoint) {
		t.Fatalf("error = %v, want ErrNoSafePoint", err)
	}
}

func TestTransformErrorsDoNotEchoUntrustedResponseKeysOrTimestamps(t *testing.T) {
	secret := "other-tenant-secret.example.com"
	monitoring := MonitoringResponse{
		Code:  200,
		Times: []string{"2026-02-10 10:00:00"},
		Data: map[string]MonitoringRegionSeries{
			secret: {China: []float64{1}, Oversea: []float64{1}},
		},
	}
	_, err := SelectLatestSafeBandwidth5Min(monitoring, []string{"allowed.example.com"}, bucketTimeUTC(10, 5), time.UTC)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("unexpected-domain error exposed an upstream key: %v", err)
	}

	badTimestamp := "tenant-token-instead-of-time"
	analytics := RequestCountResponse{Code: 200, Data: RequestCountData{Points: []string{badTimestamp}, ReqCount: []float64{1}}}
	_, err = SelectLatestSafeRequestRate5Min(analytics, "allowed.example.com", RegionGlobal, bucketTimeUTC(10, 5), time.UTC)
	if err == nil || strings.Contains(err.Error(), badTimestamp) {
		t.Fatalf("timestamp error exposed upstream data: %v", err)
	}
}

func bucketTime(location *time.Location, hour, minute int) time.Time {
	return time.Date(2026, 2, 10, hour, minute, 0, 0, location)
}

func bucketTimeUTC(hour, minute int) time.Time {
	return bucketTime(time.UTC, hour, minute)
}
