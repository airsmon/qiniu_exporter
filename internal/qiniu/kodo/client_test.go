package kodo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestEndpointsForStorageClass(t *testing.T) {
	t.Parallel()

	tests := []struct {
		storageClass StorageClass
		capacity     string
		objects      string
	}{
		{StorageClassStandard, "/v6/space", "/v6/count"},
		{StorageClassIA, "/v6/space_line", "/v6/count_line"},
		{StorageClassIntelligentTiering, "/v6/space_intelligent_tiering", "/v6/count_intelligent_tiering"},
		{StorageClassArchiveIR, "/v6/space_archive_ir", "/v6/count_archive_ir"},
		{StorageClassArchive, "/v6/space_archive", "/v6/count_archive"},
		{StorageClassDeepArchive, "/v6/space_deep_archive", "/v6/count_deep_archive"},
	}

	for _, test := range tests {
		test := test
		t.Run(string(test.storageClass), func(t *testing.T) {
			t.Parallel()
			got, err := EndpointsForStorageClass(test.storageClass)
			if err != nil {
				t.Fatalf("EndpointsForStorageClass() error = %v", err)
			}
			if got.CapacityPath != test.capacity || got.ObjectCountPath != test.objects {
				t.Fatalf("EndpointsForStorageClass() = %+v, want capacity %q objects %q", got, test.capacity, test.objects)
			}
		})
	}
}

func TestCollectP0UsesFixedEndpointsAndTransformsLatestPoint(t *testing.T) {
	t.Parallel()

	arrayFixture := readFixture(t, "array_ok.json")
	var (
		mu   sync.Mutex
		hits = make(map[string]int)
	)
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			t.Errorf("method = %q, want GET", request.Method)
		}
		if got := request.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
			t.Errorf("Content-Type = %q", got)
		}
		query := request.URL.Query()
		if got := query.Get("begin"); got != "20260803100000" {
			t.Errorf("begin = %q", got)
		}
		if got := query.Get("end"); got != "20260803101500" {
			t.Errorf("end = %q", got)
		}
		if got := query.Get("g"); got != "5min" {
			t.Errorf("g = %q", got)
		}

		mu.Lock()
		hits[request.URL.Path]++
		mu.Unlock()
		response.Header().Set("Content-Type", "application/json")

		switch request.URL.Path {
		case "/v6/space", "/v6/count":
			if query.Get("bucket") != "static-bucket" || query.Get("region") != "z0" {
				t.Errorf("storage filters = %q/%q", query.Get("bucket"), query.Get("region"))
			}
			if query.Has("$bucket") || query.Has("$region") {
				t.Errorf("storage request unexpectedly used dollar-prefixed filters")
			}
			_, _ = response.Write(arrayFixture)
		case BlobIOPath:
			assertActivityFilters(t, query.Get("$bucket"), query.Get("$region"))
			switch query.Get("$metric") {
			case "hits":
				if query.Get("select") != "hits" {
					t.Errorf("hits select = %q", query.Get("select"))
				}
				writeValueResponse(response, "hits", 10, 20, 30)
			case "flow_out":
				if query.Get("select") != "flow" {
					t.Errorf("flow_out select = %q", query.Get("select"))
				}
				writeValueResponse(response, "flow", 300, 600, 900)
			case "cdn_flow_out":
				if query.Get("select") != "flow" {
					t.Errorf("cdn_flow_out select = %q", query.Get("select"))
				}
				writeValueResponse(response, "flow", 600, 1200, 1800)
			default:
				t.Errorf("unexpected blob_io metric %q", query.Get("$metric"))
				http.Error(response, "unexpected metric", http.StatusBadRequest)
			}
		case RSPutPath:
			assertActivityFilters(t, query.Get("$bucket"), query.Get("$region"))
			if query.Get("select") != "hits" || query.Has("$metric") {
				t.Errorf("rs_put query = %q", request.URL.RawQuery)
			}
			writeValueResponse(response, "hits", 5, 10, 15)
		default:
			t.Errorf("unexpected endpoint %q", request.URL.Path)
			http.NotFound(response, request)
		}
	})
	client := newTestClient(t, handler)
	query := testQuery()
	samples, err := client.CollectP0(context.Background(), CollectInput{
		Query:          query,
		StorageClasses: []StorageClass{StorageClassStandard},
	})
	if err != nil {
		t.Fatalf("CollectP0() error = %v", err)
	}
	if len(samples) != 6 {
		t.Fatalf("len(samples) = %d, want 6", len(samples))
	}

	want := []GaugeSample{
		{Kind: GaugeStorageBytes, Bucket: query.Bucket, Region: query.Region, StorageClass: StorageClassStandard, Value: 300},
		{Kind: GaugeObjects, Bucket: query.Bucket, Region: query.Region, StorageClass: StorageClassStandard, Value: 300},
		{Kind: GaugeRequestsPerSecond, Bucket: query.Bucket, Region: query.Region, Operation: OperationGet, Value: 0.1},
		{Kind: GaugeRequestsPerSecond, Bucket: query.Bucket, Region: query.Region, Operation: OperationPut, Value: 0.05},
		{Kind: GaugeEgressBytesPerSecond, Bucket: query.Bucket, Region: query.Region, Route: RouteDirect, Value: 3},
		{Kind: GaugeEgressBytesPerSecond, Bucket: query.Bucket, Region: query.Region, Route: RouteCDNOrigin, Value: 6},
	}
	wantDataAt := time.Date(2026, 8, 3, 10, 10, 0, 0, time.FixedZone("UTC+8", 8*60*60))
	for i := range want {
		if samples[i].Kind != want[i].Kind ||
			samples[i].Bucket != want[i].Bucket ||
			samples[i].Region != want[i].Region ||
			samples[i].StorageClass != want[i].StorageClass ||
			samples[i].Operation != want[i].Operation ||
			samples[i].Route != want[i].Route ||
			samples[i].Value != want[i].Value {
			t.Errorf("samples[%d] = %+v, want %+v", i, samples[i], want[i])
		}
		if !samples[i].DataAt.Equal(wantDataAt) {
			t.Errorf("samples[%d].DataAt = %s, want %s", i, samples[i].DataAt, wantDataAt)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if hits["/v6/space"] != 1 || hits["/v6/count"] != 1 || hits[BlobIOPath] != 3 || hits[RSPutPath] != 1 {
		t.Errorf("endpoint hits = %#v", hits)
	}
}

func TestStorageRejectsMismatchedArrays(t *testing.T) {
	t.Parallel()

	fixture := readFixture(t, "array_mismatch.json")
	handler := http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write(fixture)
	})
	client := newTestClient(t, handler)
	_, err := client.Storage(context.Background(), testQuery(), StorageClassStandard)
	if !errors.Is(err, ErrMismatchedArrays) {
		t.Fatalf("Storage() error = %v, want ErrMismatchedArrays", err)
	}
}

func TestGETRequestsRejectsMissingBucket(t *testing.T) {
	t.Parallel()

	fixture := readFixture(t, "values_gap.json")
	handler := http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write(fixture)
	})
	client := newTestClient(t, handler)
	_, err := client.GETRequests(context.Background(), testQuery())
	if !errors.Is(err, ErrNonContinuous) {
		t.Fatalf("GETRequests() error = %v, want ErrNonContinuous", err)
	}
}

func TestCurrentMonthUsageUsesDailyBucketsAndSumsSparsePoints(t *testing.T) {
	t.Parallel()

	query := testMonthToDateQuery()
	tests := []struct {
		name       string
		call       func(*Client) (GaugeSample, error)
		path       string
		selectName string
		metric     string
		valueName  string
		wantKind   GaugeKind
		wantRoute  Route
		wantOp     Operation
	}{
		{
			name: "direct egress",
			call: func(client *Client) (GaugeSample, error) {
				return client.CurrentMonthDirectEgress(context.Background(), query)
			},
			path:       BlobIOPath,
			selectName: "flow",
			metric:     "flow_out",
			valueName:  "flow",
			wantKind:   GaugeUsageEgressBytes,
			wantRoute:  RouteDirect,
		},
		{
			name: "put requests",
			call: func(client *Client) (GaugeSample, error) {
				return client.CurrentMonthPUTRequests(context.Background(), query)
			},
			path:       RSPutPath,
			selectName: "hits",
			valueName:  "hits",
			wantKind:   GaugeUsageRequests,
			wantOp:     OperationPut,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodGet {
					t.Errorf("method = %q, want GET", request.Method)
				}
				if request.URL.Path != test.path {
					t.Errorf("path = %q, want %q", request.URL.Path, test.path)
				}
				params := request.URL.Query()
				if got := params.Get("begin"); got != "20260801000000" {
					t.Errorf("begin = %q", got)
				}
				if got := params.Get("end"); got != "20260805101500" {
					t.Errorf("end = %q", got)
				}
				if got := params.Get("g"); got != "day" {
					t.Errorf("g = %q, want day", got)
				}
				if got := params.Get("select"); got != test.selectName {
					t.Errorf("select = %q, want %q", got, test.selectName)
				}
				if got := params.Get("$metric"); got != test.metric {
					t.Errorf("$metric = %q, want %q", got, test.metric)
				}
				if params.Get("$bucket") != query.Bucket || params.Get("$region") != query.Region {
					t.Errorf("filters = %q/%q", params.Get("$bucket"), params.Get("$region"))
				}
				if params.Has("bucket") || params.Has("region") {
					t.Errorf("request unexpectedly used non-dollar filters")
				}
				response.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(response, `[
  {"time":"2026-08-01T00:00:00+08:00","values":{"%s":10}},
  {"time":"2026-08-03T00:00:00+08:00","values":{"%s":20}},
  {"time":"2026-08-05T00:00:00+08:00","values":{"%s":30}}
]`, test.valueName, test.valueName, test.valueName)
			})

			got, err := test.call(newTestClient(t, handler))
			if err != nil {
				t.Fatalf("current-month call error = %v", err)
			}
			want := GaugeSample{
				Kind:      test.wantKind,
				Bucket:    query.Bucket,
				Region:    query.Region,
				Operation: test.wantOp,
				Route:     test.wantRoute,
				Period:    PeriodCurrentMonth,
				Value:     60,
				DataAt:    query.End,
			}
			if got != want {
				t.Fatalf("current-month sample = %+v, want %+v", got, want)
			}
		})
	}
}

func TestCurrentMonthUsageTreatsEmptyArrayAsZero(t *testing.T) {
	t.Parallel()

	query := testMonthToDateQuery()
	handler := http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte("[]"))
	})
	client := newTestClient(t, handler)

	got, err := client.CurrentMonthDirectEgress(context.Background(), query)
	if err != nil {
		t.Fatalf("CurrentMonthDirectEgress() error = %v", err)
	}
	if got.Value != 0 || got.Period != PeriodCurrentMonth || !got.DataAt.Equal(query.End) {
		t.Fatalf("CurrentMonthDirectEgress() = %+v", got)
	}
}

func TestCurrentMonthUsageRejectsInvalidQueriesBeforeRequest(t *testing.T) {
	t.Parallel()

	location := time.FixedZone("UTC+8", 8*60*60)
	valid := testMonthToDateQuery()
	tests := []struct {
		name   string
		mutate func(*MonthToDateQuery)
	}{
		{"missing bucket", func(query *MonthToDateQuery) { query.Bucket = "" }},
		{"bucket whitespace", func(query *MonthToDateQuery) { query.Bucket = " bucket" }},
		{"missing region", func(query *MonthToDateQuery) { query.Region = "" }},
		{"zero begin", func(query *MonthToDateQuery) { query.Begin = time.Time{} }},
		{"zero end", func(query *MonthToDateQuery) { query.End = time.Time{} }},
		{"begin after first day", func(query *MonthToDateQuery) { query.Begin = query.Begin.Add(24 * time.Hour) }},
		{"begin after end", func(query *MonthToDateQuery) { query.End = query.Begin.Add(-BucketWidth) }},
		{"end in next month", func(query *MonthToDateQuery) { query.End = time.Date(2026, 9, 1, 0, 0, 0, 0, location) }},
		{"unaligned end", func(query *MonthToDateQuery) { query.End = query.End.Add(time.Minute) }},
		{"different timezone", func(query *MonthToDateQuery) { query.End = query.End.In(time.UTC) }},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			called := false
			client := newTestClient(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				called = true
			}))
			query := valid
			test.mutate(&query)
			_, err := client.CurrentMonthDirectEgress(context.Background(), query)
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("CurrentMonthDirectEgress() error = %v, want ErrInvalidInput", err)
			}
			if called {
				t.Fatal("invalid query made an HTTP request")
			}
		})
	}
}

func TestCurrentMonthUsageRejectsUnsafeDailyResponsesWithoutLeakingValues(t *testing.T) {
	t.Parallel()

	query := testMonthToDateQuery()
	tests := []struct {
		name string
		body string
	}{
		{"null response", "null"},
		{"before begin", `[{"time":"2026-07-31T00:00:00+08:00","values":{"flow":1}}]`},
		{"at end", `[{"time":"2026-08-05T10:15:00+08:00","values":{"flow":1}}]`},
		{"not day aligned", `[{"time":"2026-08-02T00:05:00+08:00","values":{"flow":1}}]`},
		{"duplicate day", `[{"time":"2026-08-02T00:00:00+08:00","values":{"flow":1}},{"time":"2026-08-02T00:00:00+08:00","values":{"flow":2}}]`},
		{"out of order", `[{"time":"2026-08-03T00:00:00+08:00","values":{"flow":1}},{"time":"2026-08-02T00:00:00+08:00","values":{"flow":2}}]`},
		{"negative value", `[{"time":"2026-08-02T00:00:00+08:00","values":{"flow":-1}}]`},
		{"non finite value", `[{"time":"2026-08-02T00:00:00+08:00","values":{"flow":1e999}}]`},
		{"sum overflow", `[{"time":"2026-08-02T00:00:00+08:00","values":{"flow":1e308}},{"time":"2026-08-03T00:00:00+08:00","values":{"flow":1e308}}]`},
		{"secret timestamp", `[{"time":"credential-shaped-secret","values":{"flow":1}}]`},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			client := newTestClient(t, http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				response.Header().Set("Content-Type", "application/json")
				_, _ = response.Write([]byte(test.body))
			}))
			_, err := client.CurrentMonthDirectEgress(context.Background(), query)
			if !errors.Is(err, ErrUnexpectedResponse) && !errors.Is(err, ErrNonContinuous) {
				t.Fatalf("CurrentMonthDirectEgress() error = %v, want a response validation error", err)
			}
			if strings.Contains(err.Error(), "credential-shaped-secret") || strings.Contains(err.Error(), "1e999") {
				t.Fatalf("error exposed response data: %v", err)
			}
		})
	}
}

func TestResponseErrorsDoNotEchoUntrustedTimestampOrNumber(t *testing.T) {
	badTimestamp := "tenant-token-instead-of-time"
	_, err := (valueResponse{{Time: badTimestamp, Values: map[string]json.Number{"hits": "1"}}}).points("hits")
	if err == nil || strings.Contains(err.Error(), badTimestamp) {
		t.Fatalf("timestamp error exposed upstream data: %v", err)
	}

	badNumber := strings.Repeat("9", 1_000)
	_, err = (arrayResponse{Times: []int64{1}, Datas: []json.Number{json.Number(badNumber)}}).points()
	if err == nil || strings.Contains(err.Error(), badNumber) {
		t.Fatalf("number error exposed upstream data: %v", err)
	}
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %q: %v", name, err)
	}
	return contents
}

type handlerDoer struct {
	handler http.Handler
}

func (d handlerDoer) Do(request *http.Request) (*http.Response, error) {
	recorder := httptest.NewRecorder()
	d.handler.ServeHTTP(recorder, request)
	return recorder.Result(), nil
}

func newTestClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	client, err := NewClient(handlerDoer{handler: handler}, "https://kodo.test")
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	return client
}

func testQuery() Query {
	location := time.FixedZone("UTC+8", 8*60*60)
	return Query{
		Bucket:     "static-bucket",
		Region:     "z0",
		Begin:      time.Date(2026, 8, 3, 10, 0, 0, 0, location),
		End:        time.Date(2026, 8, 3, 10, 15, 0, 0, location),
		SafeBefore: time.Date(2026, 8, 3, 10, 15, 0, 0, location),
	}
}

func testMonthToDateQuery() MonthToDateQuery {
	location := time.FixedZone("UTC+8", 8*60*60)
	return MonthToDateQuery{
		Bucket: "static-bucket",
		Region: "z0",
		Begin:  time.Date(2026, 8, 1, 0, 0, 0, 0, location),
		End:    time.Date(2026, 8, 5, 10, 15, 0, 0, location),
	}
}

func assertActivityFilters(t *testing.T, bucket, region string) {
	t.Helper()
	if bucket != "static-bucket" || region != "z0" {
		t.Errorf("activity filters = %q/%q", bucket, region)
	}
}

func writeValueResponse(response http.ResponseWriter, name string, values ...int) {
	_, _ = fmt.Fprintf(response, `[
  {"time":"2026-08-03T10:00:00+08:00","values":{"%s":%d}},
  {"time":"2026-08-03T10:05:00+08:00","values":{"%s":%d}},
  {"time":"2026-08-03T10:10:00+08:00","values":{"%s":%d}}
]`, name, values[0], name, values[1], name, values[2])
}
