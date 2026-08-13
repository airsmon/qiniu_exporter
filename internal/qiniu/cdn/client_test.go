package cdn

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestClientUsesOnlyFixedP0EndpointsAndBodies(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		wantBody map[string]any
		response string
		call     func(context.Context, *Client) error
	}{
		{
			name: "metering bandwidth",
			path: meteringBandwidthPath,
			wantBody: map[string]any{
				"domains": "a.example.com;b.example.com", "startDate": "2026-02-01",
				"endDate": "2026-02-10", "granularity": "5min",
			},
			response: `{"code":200,"error":"","time":[],"data":{}}`,
			call: func(ctx context.Context, client *Client) error {
				_, err := client.FetchMeteringBandwidth(ctx, meteringTestQuery(GranularityFiveMinutes))
				return err
			},
		},
		{
			name: "metering flux",
			path: meteringFluxPath,
			wantBody: map[string]any{
				"domains": "a.example.com;b.example.com", "startDate": "2026-02-01",
				"endDate": "2026-02-10", "granularity": "day",
			},
			response: `{"code":200,"error":"","time":[],"data":{}}`,
			call: func(ctx context.Context, client *Client) error {
				_, err := client.FetchMeteringFlux(ctx, meteringTestQuery(GranularityDay))
				return err
			},
		},
		{
			name: "monitoring bandwidth",
			path: monitoringBandwidthPath,
			wantBody: map[string]any{
				"domains": "a.example.com;b.example.com", "startDate": "2026-02-10",
				"endDate": "2026-02-10", "granularity": "5min",
			},
			response: `{"code":200,"error":"","time":[],"data":{}}`,
			call: func(ctx context.Context, client *Client) error {
				_, err := client.FetchMonitoringBandwidth(ctx, monitoringTestQuery())
				return err
			},
		},
		{
			name: "monitoring flow",
			path: monitoringFlowPath,
			wantBody: map[string]any{
				"domains": "a.example.com;b.example.com", "startDate": "2026-02-10",
				"endDate": "2026-02-10", "granularity": "5min",
			},
			response: `{"code":200,"error":"","time":[],"data":{}}`,
			call: func(ctx context.Context, client *Client) error {
				_, err := client.FetchMonitoringFlow(ctx, monitoringTestQuery())
				return err
			},
		},
		{
			name: "request count",
			path: requestCountPath,
			wantBody: map[string]any{
				"domains": []any{"a.example.com"}, "startDate": "2026-02-10",
				"endDate": "2026-02-10", "freq": "5min", "region": "global",
			},
			response: `{"code":200,"error":"","data":{"points":[],"reqCount":[]}}`,
			call: func(ctx context.Context, client *Client) error {
				_, err := client.FetchRequestCount(ctx, regionalTestQuery())
				return err
			},
		},
		{
			name: "status codes",
			path: statusCodePath,
			wantBody: map[string]any{
				"domains": []any{"a.example.com"}, "startDate": "2026-02-10",
				"endDate": "2026-02-10", "freq": "5min", "region": "global",
			},
			response: `{"code":200,"error":"","data":{"points":[],"codes":{}}}`,
			call: func(ctx context.Context, client *Client) error {
				_, err := client.FetchStatusCodes(ctx, regionalTestQuery())
				return err
			},
		},
		{
			name: "hit miss",
			path: hitMissPath,
			wantBody: map[string]any{
				"domains": []any{"a.example.com"}, "startDate": "2026-02-10",
				"endDate": "2026-02-10", "freq": "5min",
			},
			response: `{"code":200,"error":"","data":{"points":[],"hit":[],"miss":[],"trafficHit":[],"trafficMiss":[]}}`,
			call: func(ctx context.Context, client *Client) error {
				_, err := client.FetchHitMiss(ctx, domainTestQuery())
				return err
			},
		},
		{
			name: "top IP traffic",
			path: topTrafficIPPath,
			wantBody: map[string]any{
				"domains": []any{"a.example.com", "b.example.com"}, "startDate": "2026-02-10",
				"endDate": "2026-02-10", "region": "global",
			},
			response: `{"code":200,"error":"","data":{"ips":[],"traffic":[]}}`,
			call: func(ctx context.Context, client *Client) error {
				_, err := client.FetchTopIPTraffic(ctx, topIPTestQuery())
				return err
			},
		},
		{
			name: "top IP requests",
			path: topCountIPPath,
			wantBody: map[string]any{
				"domains": []any{"a.example.com", "b.example.com"}, "startDate": "2026-02-10",
				"endDate": "2026-02-10", "region": "global",
			},
			response: `{"code":200,"error":"","data":{"ips":[],"count":[]}}`,
			call: func(ctx context.Context, client *Client) error {
				_, err := client.FetchTopIPRequests(ctx, topIPTestQuery())
				return err
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			doer := doerFunc(func(request *http.Request) (*http.Response, error) {
				if request.Method != http.MethodPost {
					t.Errorf("method = %q, want POST", request.Method)
				}
				if request.URL.Path != test.path {
					t.Errorf("path = %q, want %q", request.URL.Path, test.path)
				}
				if got := request.Header.Get("Content-Type"); got != "application/json" {
					t.Errorf("Content-Type = %q, want application/json", got)
				}

				var gotBody map[string]any
				if err := json.NewDecoder(request.Body).Decode(&gotBody); err != nil {
					t.Errorf("decode request body: %v", err)
				}
				if !reflect.DeepEqual(gotBody, test.wantBody) {
					t.Errorf("body = %#v, want %#v", gotBody, test.wantBody)
				}
				return jsonResponse(http.StatusOK, test.response), nil
			})

			client, err := NewClient(doer, "http://cdn.test")
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			if err := test.call(context.Background(), client); err != nil {
				t.Fatalf("call: %v", err)
			}
		})
	}
}

func TestClientRejectsMoreThanFiftyMonitoringDomainsBeforeRequest(t *testing.T) {
	doer := doerFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("Do must not be called")
		return nil, nil
	})
	client, err := NewClient(doer, "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	domains := make([]string, 51)
	for index := range domains {
		domains[index] = "domain-" + strings.Repeat("x", index+1) + ".example.com"
	}

	_, err = client.FetchMonitoringBandwidth(context.Background(), MonitoringQuery{
		Domains: domains, StartDate: "2026-02-10", EndDate: "2026-02-10",
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("error = %v, want ErrInvalidInput", err)
	}
}

func TestClientRejectsInvalidMeteringQueriesBeforeRequest(t *testing.T) {
	doer := doerFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("Do must not be called")
		return nil, nil
	})
	client, err := NewClient(doer, "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	tests := []struct {
		name  string
		query MeteringQuery
	}{
		{name: "missing granularity", query: meteringTestQuery("")},
		{name: "invalid granularity", query: meteringTestQuery("month")},
		{name: "end before start", query: MeteringQuery{
			Domains: []string{"a.example.com"}, StartDate: "2026-02-10", EndDate: "2026-02-09", Granularity: GranularityDay,
		}},
		{name: "range over 31 days", query: MeteringQuery{
			Domains: []string{"a.example.com"}, StartDate: "2026-01-01", EndDate: "2026-02-01", Granularity: GranularityDay,
		}},
	}
	tooMany := meteringTestQuery(GranularityDay)
	tooMany.Domains = make([]string, 51)
	for index := range tooMany.Domains {
		tooMany.Domains[index] = "domain-" + strings.Repeat("x", index+1) + ".example.com"
	}
	tests = append(tests, struct {
		name  string
		query MeteringQuery
	}{name: "more than fifty domains", query: tooMany})

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := client.FetchMeteringFlux(context.Background(), test.query)
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("error = %v, want ErrInvalidInput", err)
			}
		})
	}
}

func TestValidateDateRangeCountsInclusiveCalendarDays(t *testing.T) {
	if err := validateDateRange("2026-01-01", "2026-01-31", 31); err != nil {
		t.Fatalf("31 inclusive days were rejected: %v", err)
	}
	if err := validateDateRange("2026-01-01", "2026-02-01", 31); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("32 inclusive days error=%v, want ErrInvalidInput", err)
	}
}

func TestClientReturnsBusinessCodeError(t *testing.T) {
	doer := doerFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"code":400032,"error":"invalid domain","data":{}}`), nil
	})
	client, err := NewClient(doer, "http://cdn.test")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	_, err = client.FetchHitMiss(context.Background(), domainTestQuery())
	var apiError *APIError
	if !errors.As(err, &apiError) {
		t.Fatalf("error = %v, want *APIError", err)
	}
	if apiError.Code != 400032 || apiError.Message != "invalid domain" {
		t.Fatalf("APIError = %#v", apiError)
	}
	if got := err.Error(); got != "cdn: API response code 400032" || strings.Contains(got, apiError.Message) {
		t.Fatalf("error string = %q, want code without upstream message", got)
	}
	if !errors.Is(err, ErrUnexpectedResponse) {
		t.Fatalf("error = %v, want ErrUnexpectedResponse", err)
	}
}

func TestClientDecodesOfficialResponseShapes(t *testing.T) {
	responses := map[string]string{
		meteringBandwidthPath:   `{"code":200,"error":"","time":["2026-02-10 00:00:00"],"data":{"a.example.com":{"china":[123],"oversea":[45]}}}`,
		meteringFluxPath:        `{"code":200,"error":"","time":["2026-02-10 00:00:00"],"data":{"a.example.com":{"china":[456],"oversea":[78]}}}`,
		monitoringBandwidthPath: `{"code":200,"error":"","time":["2026-02-10 10:00:00"],"data":{"a.example.com":{"china":[123],"oversea":[45]}}}`,
		requestCountPath:        `{"code":200,"error":"","data":{"points":["2026-02-10-10-00"],"reqCount":[300]}}`,
		statusCodePath:          `{"code":200,"error":"","data":{"points":["2026-02-10-10-00"],"codes":{"2xx":[299],"404":[1]}}}`,
		hitMissPath:             `{"code":200,"error":"","data":{"points":["2026-02-10-10-00"],"hit":[250],"miss":[50],"trafficHit":[900],"trafficMiss":[100]}}`,
	}
	doer := doerFunc(func(request *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, responses[request.URL.Path]), nil
	})
	client, err := NewClient(doer, "http://cdn.test")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	monitoring, err := client.FetchMonitoringBandwidth(context.Background(), MonitoringQuery{
		Domains: []string{"a.example.com"}, StartDate: "2026-02-10", EndDate: "2026-02-10",
	})
	if err != nil {
		t.Fatalf("FetchMonitoringBandwidth: %v", err)
	}
	if len(monitoring.Times) != 1 || len(monitoring.Data["a.example.com"].China) != 1 || monitoring.Times[0] != "2026-02-10 10:00:00" || monitoring.Data["a.example.com"].China[0] != 123 {
		t.Fatalf("monitoring response = %#v, error = %v", monitoring, err)
	}

	meteringBandwidth, err := client.FetchMeteringBandwidth(context.Background(), MeteringQuery{
		Domains: []string{"a.example.com"}, StartDate: "2026-02-10", EndDate: "2026-02-10", Granularity: GranularityFiveMinutes,
	})
	if err != nil {
		t.Fatalf("FetchMeteringBandwidth: %v", err)
	}
	if len(meteringBandwidth.Times) != 1 || meteringBandwidth.Data["a.example.com"].China[0] != 123 {
		t.Fatalf("metering bandwidth response = %#v, error = %v", meteringBandwidth, err)
	}

	metering, err := client.FetchMeteringFlux(context.Background(), MeteringQuery{
		Domains: []string{"a.example.com"}, StartDate: "2026-02-10", EndDate: "2026-02-10", Granularity: GranularityDay,
	})
	if err != nil {
		t.Fatalf("FetchMeteringFlux: %v", err)
	}
	if len(metering.Times) != 1 || metering.Times[0] != "2026-02-10 00:00:00" || metering.Data["a.example.com"].China[0] != 456 {
		t.Fatalf("metering response = %#v, error = %v", metering, err)
	}

	requests, err := client.FetchRequestCount(context.Background(), regionalTestQuery())
	if err != nil {
		t.Fatalf("FetchRequestCount: %v", err)
	}
	if len(requests.Data.Points) != 1 || len(requests.Data.ReqCount) != 1 || requests.Data.Points[0] != "2026-02-10-10-00" || requests.Data.ReqCount[0] != 300 {
		t.Fatalf("request count response = %#v, error = %v", requests, err)
	}

	statuses, err := client.FetchStatusCodes(context.Background(), regionalTestQuery())
	if err != nil {
		t.Fatalf("FetchStatusCodes: %v", err)
	}
	if len(statuses.Data.Codes["2xx"]) != 1 || len(statuses.Data.Codes["404"]) != 1 || statuses.Data.Codes["2xx"][0] != 299 || statuses.Data.Codes["404"][0] != 1 {
		t.Fatalf("status code response = %#v, error = %v", statuses, err)
	}

	cache, err := client.FetchHitMiss(context.Background(), domainTestQuery())
	if err != nil {
		t.Fatalf("FetchHitMiss: %v", err)
	}
	if len(cache.Data.Hit) != 1 || len(cache.Data.TrafficMiss) != 1 || cache.Data.Hit[0] != 250 || cache.Data.TrafficMiss[0] != 100 {
		t.Fatalf("hit/miss response = %#v, error = %v", cache, err)
	}
}

func TestClientReturnsHTTPStatusError(t *testing.T) {
	doer := doerFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusTooManyRequests, "rate limited"), nil
	})
	client, err := NewClient(doer, "http://cdn.test")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	_, err = client.FetchRequestCount(context.Background(), regionalTestQuery())
	var statusError *HTTPStatusError
	if !errors.As(err, &statusError) || statusError.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("error = %v, want HTTP 429 error", err)
	}
}

func monitoringTestQuery() MonitoringQuery {
	return MonitoringQuery{
		Domains:   []string{"a.example.com", "b.example.com"},
		StartDate: "2026-02-10",
		EndDate:   "2026-02-10",
	}
}

func meteringTestQuery(granularity Granularity) MeteringQuery {
	return MeteringQuery{
		Domains:     []string{"a.example.com", "b.example.com"},
		StartDate:   "2026-02-01",
		EndDate:     "2026-02-10",
		Granularity: granularity,
	}
}

func domainTestQuery() DomainQuery {
	return DomainQuery{Domain: "a.example.com", StartDate: "2026-02-10", EndDate: "2026-02-10"}
}

func regionalTestQuery() RegionalDomainQuery {
	return RegionalDomainQuery{DomainQuery: domainTestQuery(), Region: RegionGlobal}
}

func topIPTestQuery() TopIPQuery {
	return TopIPQuery{
		Domains: []string{"a.example.com", "b.example.com"}, StartDate: "2026-02-10", EndDate: "2026-02-10", Region: RegionGlobal,
	}
}

type doerFunc func(*http.Request) (*http.Response, error)

func (function doerFunc) Do(request *http.Request) (*http.Response, error) {
	return function(request)
}

func jsonResponse(statusCode int, body string) *http.Response {
	return &http.Response{
		StatusCode: statusCode,
		Status:     http.StatusText(statusCode),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
