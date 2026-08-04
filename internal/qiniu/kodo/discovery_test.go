package kodo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestDiscoveryClientPaginatesBucketsWithRegions(t *testing.T) {
	t.Parallel()
	calls := 0
	client := newDiscoveryTestClient(t, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		calls++
		if request.Method != http.MethodGet || request.URL.Path != BucketsPath {
			t.Errorf("request = %s %s, want GET %s", request.Method, request.URL.Path, BucketsPath)
		}
		if got := request.URL.Query().Get("apiVersion"); got != "v4" {
			t.Errorf("apiVersion = %q, want v4", got)
		}
		if got := request.URL.Query().Get("limit"); got != "100" {
			t.Errorf("limit = %q, want 100", got)
		}
		if request.Header.Get("Accept") != "application/json" {
			t.Errorf("Accept = %q, want application/json", request.Header.Get("Accept"))
		}
		switch calls {
		case 1:
			if _, exists := request.URL.Query()["marker"]; exists {
				t.Error("first request unexpectedly contains marker")
			}
			_, _ = io.WriteString(response, `{"next_marker":"next+/=","is_truncated":true,"buckets":[{"name":"bucket-b","region":"z2","private":true},{"name":"bucket-a","region":"z0","private":false}]}`)
		case 2:
			if got := request.URL.Query().Get("marker"); got != "next+/=" {
				t.Errorf("marker = %q, want next+/=", got)
			}
			_, _ = io.WriteString(response, `{"next_marker":"","is_truncated":false,"buckets":[{"name":"bucket-c","region":"na0","private":false}]}`)
		default:
			t.Errorf("unexpected request %d", calls)
		}
	}))

	got, err := client.ListBuckets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Bucket{
		{Name: "bucket-a", Region: "z0", StorageRegion: "East China - Zhejiang", Private: false},
		{Name: "bucket-b", Region: "z2", StorageRegion: "South China - Guangdong", Private: true},
		{Name: "bucket-c", Region: "na0", StorageRegion: "North America - Los Angeles", Private: false},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buckets = %#v, want %#v", got, want)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

func TestStorageRegionNameUsesOfficialMappingAndPreservesUnknownIDs(t *testing.T) {
	t.Parallel()
	for regionID, want := range map[string]string{
		"z0":             "East China - Zhejiang",
		"cn-east-2":      "East China - Zhejiang 2",
		"z1":             "North China - Hebei",
		"z2":             "South China - Guangdong",
		"na0":            "North America - Los Angeles",
		"as0":            "Asia Pacific - Singapore",
		"ap-southeast-2": "Asia Pacific - Hanoi",
		"ap-southeast-3": "Asia Pacific - Ho Chi Minh City",
		"future-region":  "future-region",
	} {
		if got := storageRegionName(regionID); got != want {
			t.Errorf("storageRegionName(%q) = %q, want %q", regionID, got, want)
		}
	}
}

func TestNewDiscoveryClientValidation(t *testing.T) {
	t.Parallel()
	doer := handlerDoer{handler: http.NotFoundHandler()}
	for _, test := range []struct {
		name    string
		doer    Doer
		baseURL string
	}{
		{name: "nil doer", baseURL: "https://uc.test"},
		{name: "relative URL", doer: doer, baseURL: "uc.test"},
		{name: "URL path", doer: doer, baseURL: "https://uc.test/path"},
		{name: "URL query", doer: doer, baseURL: "https://uc.test?x=1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewDiscoveryClient(test.doer, test.baseURL); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("error = %v, want ErrInvalidInput", err)
			}
		})
	}
	client, err := NewDiscoveryClient(doer, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := client.baseURL.String(); got != DefaultDiscoveryBaseURL {
		t.Fatalf("default URL = %q, want %q", got, DefaultDiscoveryBaseURL)
	}
}

func TestListBucketsRejectsMalformedPages(t *testing.T) {
	t.Parallel()
	entries := make([]string, bucketDiscoveryPageSize+1)
	for index := range entries {
		entries[index] = fmt.Sprintf(`{"name":"bucket-%03d","region":"z0","private":false}`, index)
	}
	tests := []struct {
		name string
		body string
	}{
		{name: "null", body: `null`},
		{name: "object", body: `{}`},
		{name: "null buckets", body: `{"is_truncated":false,"buckets":null}`},
		{name: "missing truncated", body: `{"buckets":[]}`},
		{name: "empty name", body: `{"is_truncated":false,"buckets":[{"name":"","region":"z0","private":false}]}`},
		{name: "empty region", body: `{"is_truncated":false,"buckets":[{"name":"bucket","region":"","private":false}]}`},
		{name: "slash in name", body: `{"is_truncated":false,"buckets":[{"name":"bad/name","region":"z0","private":false}]}`},
		{name: "missing private", body: `{"is_truncated":false,"buckets":[{"name":"bucket","region":"z0"}]}`},
		{name: "null private", body: `{"is_truncated":false,"buckets":[{"name":"bucket","region":"z0","private":null}]}`},
		{name: "duplicate", body: `{"is_truncated":false,"buckets":[{"name":"bucket","region":"z0","private":false},{"name":"bucket","region":"z1","private":true}]}`},
		{name: "page size", body: `{"is_truncated":false,"buckets":[` + strings.Join(entries, ",") + `]}`},
		{name: "trailing value", body: `{"is_truncated":false,"buckets":[]} {}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			client := newDiscoveryTestClient(t, http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(response, test.body)
			}))
			if _, err := client.ListBuckets(context.Background()); !errors.Is(err, ErrUnexpectedResponse) {
				t.Fatalf("error = %v, want ErrUnexpectedResponse", err)
			}
		})
	}
}

func TestListBucketsRejectsInvalidPaginationAndResourceOverflow(t *testing.T) {
	t.Parallel()
	t.Run("truncated empty page", func(t *testing.T) {
		client := newDiscoveryTestClient(t, discoveryHandler(`{"next_marker":"next","is_truncated":true,"buckets":[]}`))
		if _, err := client.ListBuckets(context.Background()); !errors.Is(err, ErrUnexpectedResponse) {
			t.Fatalf("error = %v, want ErrUnexpectedResponse", err)
		}
	})
	t.Run("invalid marker", func(t *testing.T) {
		client := newDiscoveryTestClient(t, discoveryHandler(`{"next_marker":" next","is_truncated":true,"buckets":[{"name":"bucket","region":"z0","private":false}]}`))
		if _, err := client.ListBuckets(context.Background()); !errors.Is(err, ErrUnexpectedResponse) {
			t.Fatalf("error = %v, want ErrUnexpectedResponse", err)
		}
	})
	t.Run("marker does not advance", func(t *testing.T) {
		calls := 0
		client := newDiscoveryTestClient(t, http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
			calls++
			_, _ = fmt.Fprintf(response, `{"next_marker":"same","is_truncated":true,"buckets":[{"name":"bucket-%d","region":"z0","private":false}]}`, calls)
		}))
		if _, err := client.ListBuckets(context.Background()); !errors.Is(err, ErrUnexpectedResponse) {
			t.Fatalf("error = %v, want ErrUnexpectedResponse", err)
		}
	})
	t.Run("resource ceiling", func(t *testing.T) {
		page := 0
		client := newDiscoveryTestClient(t, http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
			entries := make([]string, bucketDiscoveryPageSize)
			for index := range entries {
				entries[index] = fmt.Sprintf(`{"name":"bucket-%d-%03d","region":"z0","private":false}`, page, index)
			}
			page++
			_, _ = fmt.Fprintf(response, `{"next_marker":"page-%d","is_truncated":true,"buckets":[%s]}`, page, strings.Join(entries, ","))
		}))
		if _, err := client.ListBuckets(context.Background()); !errors.Is(err, ErrUnexpectedResponse) {
			t.Fatalf("error = %v, want ErrUnexpectedResponse", err)
		}
		if page != maxDiscoveredBuckets/bucketDiscoveryPageSize {
			t.Fatalf("pages = %d, want %d", page, maxDiscoveredBuckets/bucketDiscoveryPageSize)
		}
	})
}

func TestDiscoveryClientPropagatesContextAndHTTPFailures(t *testing.T) {
	t.Parallel()
	doer := discoveryDoerFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
	client, err := NewDiscoveryClient(doer, "https://uc.test")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.ListBuckets(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}

	secret := "upstream-secret-that-must-not-be-returned"
	client = newDiscoveryTestClient(t, http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		http.Error(response, secret, http.StatusForbidden)
	}))
	_, err = client.ListBuckets(context.Background())
	var statusError *HTTPStatusError
	if !errors.As(err, &statusError) || statusError.StatusCode != http.StatusForbidden || strings.Contains(err.Error(), secret) {
		t.Fatalf("HTTP error = %v, want redacted status 403", err)
	}
}

type discoveryDoerFunc func(*http.Request) (*http.Response, error)

func (function discoveryDoerFunc) Do(request *http.Request) (*http.Response, error) {
	return function(request)
}

func newDiscoveryTestClient(t *testing.T, handler http.Handler) *DiscoveryClient {
	t.Helper()
	client, err := NewDiscoveryClient(handlerDoer{handler: handler}, "https://uc.test")
	if err != nil {
		t.Fatalf("NewDiscoveryClient: %v", err)
	}
	return client
}

func discoveryHandler(body string) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(response, body)
	})
}
