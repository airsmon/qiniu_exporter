package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBalanceOverviewAliases(t *testing.T) {
	tests := []struct {
		name      string
		fixture   string
		available Fixed8
		currency  string
	}{
		{name: "documented available_balance", fixture: "balance_available.json", available: 123456789, currency: "CNY"},
		{name: "response example balance alias", fixture: "balance_alias.json", available: 987654321, currency: "USD"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := fixtureServer(t, test.fixture, func(t *testing.T, request *http.Request) {
				assertFixedGET(t, request, balanceOverviewPath)
				if request.URL.RawQuery != "" {
					t.Fatalf("query = %q, want empty", request.URL.RawQuery)
				}
			})
			defer server.Close()

			client := mustClient(t, server.Client(), server.URL)
			got, err := client.BalanceOverview(context.Background())
			if err != nil {
				t.Fatalf("BalanceOverview() error = %v", err)
			}
			if got.AvailableBalance != test.available || got.Currency != test.currency {
				t.Fatalf("BalanceOverview() = %+v, want available=%d currency=%s", got, test.available, test.currency)
			}
		})
	}
}

func TestAPIErrorDoesNotExposeUpstreamMessage(t *testing.T) {
	err := (&APIError{Code: 1009, Message: "account-specific upstream detail"}).Error()
	if strings.Contains(err, "account-specific") {
		t.Fatalf("API error exposed upstream message: %q", err)
	}
}

func TestBalanceOverviewRejectsConflictingAliases(t *testing.T) {
	server := fixtureServer(t, "balance_conflict.json", nil)
	defer server.Close()

	client := mustClient(t, server.Client(), server.URL)
	_, err := client.BalanceOverview(context.Background())
	if err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("BalanceOverview() error = %v, want alias conflict", err)
	}
}

func TestBillSnapshot(t *testing.T) {
	server := fixtureServer(t, "bill_snapshot.json", func(t *testing.T, request *http.Request) {
		assertFixedGET(t, request, billSnapshotPath)
		if got, want := request.URL.Query().Get("date"), "2026-08-03T00:00:00"; got != want {
			t.Fatalf("date = %q, want %q", got, want)
		}
	})
	defer server.Close()

	client := mustClient(t, server.Client(), server.URL)
	got, err := client.BillSnapshot(context.Background(), billingTestTime(2026, time.August, 3, 21, 0))
	if err != nil {
		t.Fatalf("BillSnapshot() error = %v", err)
	}
	if got.TotalMoney != 1234567890 || got.Currency != "CNY" {
		t.Fatalf("BillSnapshot() = %+v", got)
	}
}

func TestBillDetail(t *testing.T) {
	server := fixtureServer(t, "bill_detail.json", func(t *testing.T, request *http.Request) {
		assertFixedGET(t, request, billDetailPath)
		if got, want := request.URL.Query().Get("start"), "2026-07-01T00:00:00"; got != want {
			t.Fatalf("start = %q, want %q", got, want)
		}
		if got, want := request.URL.Query().Get("end"), "2026-08-01T00:00:00"; got != want {
			t.Fatalf("end = %q, want %q", got, want)
		}
	})
	defer server.Close()

	client := mustClient(t, server.Client(), server.URL)
	period := BillingPeriod{
		Start: billingTestTime(2026, time.July, 1, 0, 0),
		End:   billingTestTime(2026, time.August, 1, 0, 0),
	}
	got, err := client.BillDetail(context.Background(), period)
	if err != nil {
		t.Fatalf("BillDetail() error = %v", err)
	}
	if got.TotalMoney != 538323000000 || got.Currency != "CNY" {
		t.Fatalf("BillDetail() = %+v", got)
	}
	if len(got.Items) != 2 || got.Items[0].Start.Day() != 3 || got.Items[0].ItemMoney != 120000000 || got.Items[1].End.Month() != time.August {
		t.Fatalf("BillDetail().Items = %+v", got.Items)
	}
}

func TestBillDetailRejectsNonMonthlyPeriodBeforeRequest(t *testing.T) {
	calls := 0
	client := mustClient(t, doerFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("unexpected request")
	}), "https://api.qiniu.test")

	period := BillingPeriod{
		Start: billingTestTime(2026, time.July, 2, 0, 0),
		End:   billingTestTime(2026, time.August, 1, 0, 0),
	}
	_, err := client.BillDetail(context.Background(), period)
	if !errors.Is(err, ErrInvalidMonthlyPeriod) {
		t.Fatalf("BillDetail() error = %v, want ErrInvalidMonthlyPeriod", err)
	}
	if calls != 0 {
		t.Fatalf("Do() calls = %d, want 0", calls)
	}
}

func TestResourcePackMonthOverview(t *testing.T) {
	server := fixtureServer(t, "resource_packs.json", func(t *testing.T, request *http.Request) {
		assertFixedGET(t, request, resourcePackPath)
		if got := request.URL.Query().Get("page"); got != "1" {
			t.Fatalf("page = %q, want 1", got)
		}
		if got := request.URL.Query().Get("page_size"); got != "200" {
			t.Fatalf("page_size = %q, want 200", got)
		}
	})
	defer server.Close()

	client := mustClient(t, server.Client(), server.URL)
	got, err := client.ResourcePackMonthOverview(context.Background())
	if err != nil {
		t.Fatalf("ResourcePackMonthOverview() error = %v", err)
	}
	want := []ResourcePackMonthOverview{{
		ItemName:      "CDN traffic",
		ZoneName:      "mainland",
		AvailableTime: "all",
		TotalSurplus:  5248,
		MonthUsed:     128,
		MonthRemain:   5120,
		Unit:          "GB",
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ResourcePackMonthOverview() = %+v, want %+v", got, want)
	}
}

func TestResourcePackMonthOverviewFetchesAllPages(t *testing.T) {
	var mu sync.Mutex
	pages := make([]int, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assertFixedGET(t, request, resourcePackPath)
		if got := request.URL.Query().Get("page_size"); got != "200" {
			t.Errorf("page_size = %q, want 200", got)
		}
		page, err := strconv.Atoi(request.URL.Query().Get("page"))
		if err != nil {
			t.Errorf("invalid page: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		pages = append(pages, page)
		mu.Unlock()

		count := resourcePackPageSize
		if page == 2 {
			count = 1
		}
		items := make([]ResourcePackMonthOverview, count)
		for index := range items {
			items[index].ItemName = fmt.Sprintf("item-%d-%d", page, index)
		}
		writer.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(writer).Encode(map[string]any{"code": 0, "data": items}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer server.Close()

	client := mustClient(t, server.Client(), server.URL)
	got, err := client.ResourcePackMonthOverview(context.Background())
	if err != nil {
		t.Fatalf("ResourcePackMonthOverview() error = %v", err)
	}
	if len(got) != 201 {
		t.Fatalf("len(items) = %d, want 201", len(got))
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(pages, []int{1, 2}) {
		t.Fatalf("pages = %v, want [1 2]", pages)
	}
}

func TestResourcePackMonthOverviewIsAllOrNone(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		page := request.URL.Query().Get("page")
		if page == "2" {
			writeFixture(t, writer, "api_error.json")
			return
		}
		items := make([]ResourcePackMonthOverview, resourcePackPageSize)
		writer.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(writer).Encode(map[string]any{"code": 0, "data": items}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer server.Close()

	client := mustClient(t, server.Client(), server.URL)
	got, err := client.ResourcePackMonthOverview(context.Background())
	if got != nil {
		t.Fatalf("items = %#v, want nil on page failure", got)
	}
	var apiError *APIError
	if !errors.As(err, &apiError) || apiError.Code != 1009 {
		t.Fatalf("error = %v, want APIError code 1009", err)
	}
}

func TestResourcePackMonthOverviewPreservesSuccessfulEmptyResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, `{"code":0,"data":[]}`)
	}))
	defer server.Close()

	client := mustClient(t, server.Client(), server.URL)
	got, err := client.ResourcePackMonthOverview(context.Background())
	if err != nil {
		t.Fatalf("ResourcePackMonthOverview() error = %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("items = %#v, want non-nil empty slice", got)
	}
}

func TestResourcePackMonthOverviewCapsPagination(t *testing.T) {
	items := make([]ResourcePackMonthOverview, resourcePackPageSize)
	body, err := json.Marshal(map[string]any{"code": 0, "data": items})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	client := mustClient(t, doerFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body))}, nil
	}), "https://api.qiniu.test")
	result, err := client.ResourcePackMonthOverview(context.Background())
	if !errors.Is(err, ErrPaginationLimit) || result != nil {
		t.Fatalf("result=%v error=%v, want nil ErrPaginationLimit", result, err)
	}
	if calls != resourcePackMaxPages {
		t.Fatalf("calls=%d, want %d", calls, resourcePackMaxPages)
	}
}

func TestResponseEnvelopeValidation(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		check  func(error) bool
		wanted string
	}{
		{
			name:   "missing code",
			body:   `{"data":{"total_money":1,"currency":"CNY"}}`,
			check:  func(err error) bool { return errors.Is(err, ErrMissingEnvelopeCode) },
			wanted: "ErrMissingEnvelopeCode",
		},
		{
			name:   "nonzero code",
			body:   `{"code":1001,"message":"BaseBillGetFailed","data":{}}`,
			check:  isAPIErrorCode(1001),
			wanted: "APIError code 1001",
		},
		{
			name:   "missing data",
			body:   `{"code":0}`,
			check:  func(err error) bool { return errors.Is(err, ErrMissingEnvelopeData) },
			wanted: "ErrMissingEnvelopeData",
		},
		{
			name:   "null data",
			body:   `{"code":0,"data":null}`,
			check:  func(err error) bool { return errors.Is(err, ErrMissingEnvelopeData) },
			wanted: "ErrMissingEnvelopeData",
		},
		{
			name:   "trailing JSON",
			body:   `{"code":0,"data":{"total_money":1,"currency":"CNY"}} {}`,
			check:  func(err error) bool { return err != nil && strings.Contains(err.Error(), "trailing JSON") },
			wanted: "trailing JSON error",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(writer, test.body)
			}))
			defer server.Close()

			client := mustClient(t, server.Client(), server.URL)
			_, err := client.BillSnapshot(context.Background(), billingTestTime(2026, time.August, 1, 0, 0))
			if !test.check(err) {
				t.Fatalf("error = %v, want %s", err, test.wanted)
			}
		})
	}
}

func TestHTTPStatusMustBeOK(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := mustClient(t, server.Client(), server.URL)
	_, err := client.BalanceOverview(context.Background())
	var statusError *HTTPError
	if !errors.As(err, &statusError) || statusError.StatusCode != http.StatusNoContent {
		t.Fatalf("error = %v, want HTTPError 204", err)
	}
}

func TestNewClientValidation(t *testing.T) {
	if _, err := NewClient(nil, ""); !errors.Is(err, ErrNilDoer) {
		t.Fatalf("NewClient(nil) error = %v, want ErrNilDoer", err)
	}

	invalid := []string{
		"ftp://api.qiniu.com",
		"https://",
		"https://user@example.com",
		"https://api.qiniu.com/proxy",
		"https://api.qiniu.com?",
		"https://api.qiniu.com?path=other",
		"https://api.qiniu.com#fragment",
	}
	for _, baseURL := range invalid {
		t.Run(baseURL, func(t *testing.T) {
			_, err := NewClient(doerFunc(func(*http.Request) (*http.Response, error) { return nil, nil }), baseURL)
			if !errors.Is(err, ErrInvalidBaseURL) {
				t.Fatalf("NewClient() error = %v, want ErrInvalidBaseURL", err)
			}
		})
	}
}

type doerFunc func(*http.Request) (*http.Response, error)

func (function doerFunc) Do(request *http.Request) (*http.Response, error) {
	return function(request)
}

func fixtureServer(t *testing.T, fixture string, assertRequest func(*testing.T, *http.Request)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if assertRequest != nil {
			assertRequest(t, request)
		}
		writeFixture(t, writer, fixture)
	}))
}

func writeFixture(t *testing.T, writer http.ResponseWriter, fixture string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", fixture))
	if err != nil {
		t.Errorf("read fixture: %v", err)
		writer.WriteHeader(http.StatusInternalServerError)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	if _, err := writer.Write(body); err != nil {
		t.Errorf("write fixture: %v", err)
	}
}

func assertFixedGET(t *testing.T, request *http.Request, path string) {
	t.Helper()
	if request.Method != http.MethodGet {
		t.Fatalf("method = %s, want GET", request.Method)
	}
	if request.URL.Path != path {
		t.Fatalf("path = %q, want %q", request.URL.Path, path)
	}
	if got := request.Header.Get("Accept"); got != "application/json" {
		t.Fatalf("Accept = %q, want application/json", got)
	}
	if got := request.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
		t.Fatalf("Content-Type = %q, want application/x-www-form-urlencoded", got)
	}
}

func mustClient(t *testing.T, doer Doer, baseURL string) *Client {
	t.Helper()
	client, err := NewClient(doer, baseURL)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	return client
}

func isAPIErrorCode(want int) func(error) bool {
	return func(err error) bool {
		var apiError *APIError
		return errors.As(err, &apiError) && apiError.Code == want
	}
}
