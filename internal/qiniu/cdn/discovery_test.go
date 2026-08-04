package cdn

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

func TestDomainDiscoveryClientPaginatesAndReturnsOnlySuccessfulDomains(t *testing.T) {
	requests := 0
	doer := doerFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		if request.Method != http.MethodGet {
			t.Fatalf("method = %q, want GET", request.Method)
		}
		if request.URL.Scheme != "http" || request.URL.Host != "api.test" || request.URL.Path != "/domain" {
			t.Fatalf("URL = %q", request.URL.String())
		}
		if got := request.URL.Query().Get("limit"); got != "1000" {
			t.Fatalf("limit = %q, want 1000", got)
		}
		if got := request.Header.Get("Accept"); got != "application/json" {
			t.Fatalf("Accept = %q, want application/json", got)
		}

		switch requests {
		case 1:
			if _, exists := request.URL.Query()["marker"]; exists {
				t.Fatal("first request must not contain marker")
			}
			return discoveryResponse(`{
				"domains":[
					{"name":"z.example.com","operatingState":"success","product":"cdn","cname":"ignored.qiniudns.com"},
					{"name":"offlined.example.com","operatingState":"offlined"}
				],
				"marker":"next+/="
			}`), nil
		case 2:
			if got := request.URL.Query().Get("marker"); got != "next+/=" {
				t.Fatalf("marker = %q, want next+/=", got)
			}
			return discoveryResponse(`{
				"domains":[
					{"name":"a.example.com","operatingState":"success"},
					{"name":".wildcard.example.com","operatingState":"success"},
					{"name":"dynamic.example.com","operatingState":"success","product":"dcdn"},
					{"name":"pending.example.com","operatingState":"processing"}
				],
				"marker":""
			}`), nil
		default:
			t.Fatalf("unexpected request %d", requests)
			return nil, nil
		}
	})

	client, err := NewDomainDiscoveryClient(doer, "http://api.test/domain")
	if err != nil {
		t.Fatalf("NewDomainDiscoveryClient: %v", err)
	}
	domains, err := client.ListDomains(context.Background())
	if err != nil {
		t.Fatalf("ListDomains: %v", err)
	}
	if want := []string{".wildcard.example.com", "a.example.com", "z.example.com"}; !reflect.DeepEqual(domains, want) {
		t.Fatalf("domains = %#v, want %#v", domains, want)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
}

func TestNewDomainDiscoveryClientValidation(t *testing.T) {
	if _, err := NewDomainDiscoveryClient(nil, ""); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("nil doer error = %v, want ErrInvalidInput", err)
	}

	doer := doerFunc(func(*http.Request) (*http.Response, error) { return nil, nil })
	for _, endpoint := range []string{
		"api.qiniu.com/domain",
		"ftp://api.qiniu.com/domain",
		"https:///domain",
		"https://user@api.qiniu.com/domain",
		"https://api.qiniu.com/",
		"https://api.qiniu.com/domain/extra",
		"https://api.qiniu.com/domain?limit=1",
		"https://api.qiniu.com/domain#fragment",
		"https://api.qiniu.com/%64omain",
	} {
		t.Run(endpoint, func(t *testing.T) {
			if _, err := NewDomainDiscoveryClient(doer, endpoint); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("error = %v, want ErrInvalidInput", err)
			}
		})
	}

	client, err := NewDomainDiscoveryClient(doer, "")
	if err != nil {
		t.Fatalf("default endpoint: %v", err)
	}
	if got := client.endpoint.String(); got != DefaultDomainDiscoveryURL {
		t.Fatalf("endpoint = %q, want %q", got, DefaultDomainDiscoveryURL)
	}
}

func TestDomainDiscoveryClientRejectsMalformedPages(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "invalid JSON", body: `{`},
		{name: "multiple JSON values", body: `{"domains":[],"marker":""} {}`},
		{name: "missing marker", body: `{"domains":[]}`},
		{name: "null marker", body: `{"domains":[],"marker":null}`},
		{name: "wrong marker type", body: `{"domains":[],"marker":7}`},
		{name: "missing domains", body: `{"marker":""}`},
		{name: "null domains", body: `{"domains":null,"marker":""}`},
		{name: "wrong domains type", body: `{"domains":{},"marker":""}`},
		{name: "empty domain", body: `{"domains":[{"name":"","operatingState":"success"}],"marker":""}`},
		{name: "invalid domain", body: `{"domains":[{"name":"https://bad.example.com","operatingState":"success"}],"marker":""}`},
		{name: "invalid DNS name", body: `{"domains":[{"name":"bad_domain.example.com","operatingState":"success"}],"marker":""}`},
		{name: "missing state", body: `{"domains":[{"name":"a.example.com"}],"marker":""}`},
		{name: "invalid state", body: `{"domains":[{"name":"a.example.com","operatingState":" success"}],"marker":""}`},
		{name: "unknown state", body: `{"domains":[{"name":"a.example.com","operatingState":"unknown"}],"marker":""}`},
		{name: "unknown product", body: `{"domains":[{"name":"a.example.com","operatingState":"success","product":"other"}],"marker":""}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := mustDomainDiscoveryClient(t, doerFunc(func(*http.Request) (*http.Response, error) {
				return discoveryResponse(test.body), nil
			}))
			if _, err := client.ListDomains(context.Background()); !errors.Is(err, ErrUnexpectedResponse) {
				t.Fatalf("error = %v, want ErrUnexpectedResponse", err)
			}
		})
	}
}

func TestDomainDiscoveryClientRejectsInvalidPagination(t *testing.T) {
	tests := []struct {
		name string
		run  func(*testing.T) Doer
	}{
		{
			name: "empty page with continuation",
			run: func(*testing.T) Doer {
				return doerFunc(func(*http.Request) (*http.Response, error) {
					return discoveryResponse(`{"domains":[],"marker":"next"}`), nil
				})
			},
		},
		{
			name: "marker has whitespace",
			run: func(*testing.T) Doer {
				return doerFunc(func(*http.Request) (*http.Response, error) {
					return discoveryResponse(`{"domains":[{"name":"a.example.com","operatingState":"success"}],"marker":" next"}`), nil
				})
			},
		},
		{
			name: "marker does not advance",
			run: func(t *testing.T) Doer {
				calls := 0
				return doerFunc(func(*http.Request) (*http.Response, error) {
					calls++
					if calls == 1 {
						return discoveryResponse(`{"domains":[{"name":"a.example.com","operatingState":"success"}],"marker":"same"}`), nil
					}
					return discoveryResponse(`{"domains":[{"name":"b.example.com","operatingState":"success"}],"marker":"same"}`), nil
				})
			},
		},
		{
			name: "marker cycle",
			run: func(t *testing.T) Doer {
				calls := 0
				return doerFunc(func(*http.Request) (*http.Response, error) {
					calls++
					marker := []string{"one", "two", "one"}[calls-1]
					body := fmt.Sprintf(`{"domains":[{"name":"d%d.example.com","operatingState":"success"}],"marker":%q}`, calls, marker)
					return discoveryResponse(body), nil
				})
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := mustDomainDiscoveryClient(t, test.run(t))
			if _, err := client.ListDomains(context.Background()); !errors.Is(err, ErrUnexpectedResponse) {
				t.Fatalf("error = %v, want ErrUnexpectedResponse", err)
			}
		})
	}
}

func TestDomainDiscoveryClientRejectsDuplicateDomains(t *testing.T) {
	calls := 0
	client := mustDomainDiscoveryClient(t, doerFunc(func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return discoveryResponse(`{"domains":[{"name":"A.example.com","operatingState":"offlined"}],"marker":"next"}`), nil
		}
		return discoveryResponse(`{"domains":[{"name":"a.example.com","operatingState":"success"}],"marker":""}`), nil
	}))

	if _, err := client.ListDomains(context.Background()); !errors.Is(err, ErrUnexpectedResponse) {
		t.Fatalf("error = %v, want ErrUnexpectedResponse", err)
	}
}

func TestDomainDiscoveryClientEnforcesPageAndResourceBounds(t *testing.T) {
	t.Run("page size", func(t *testing.T) {
		entries := make([]string, 0, domainDiscoveryPageSize+1)
		for index := 0; index <= domainDiscoveryPageSize; index++ {
			entries = append(entries, fmt.Sprintf(`{"name":"d%04d.example.com","operatingState":"success"}`, index))
		}
		body := `{"domains":[` + strings.Join(entries, ",") + `],"marker":""}`
		client := mustDomainDiscoveryClient(t, doerFunc(func(*http.Request) (*http.Response, error) {
			return discoveryResponse(body), nil
		}))
		if _, err := client.ListDomains(context.Background()); !errors.Is(err, ErrUnexpectedResponse) {
			t.Fatalf("error = %v, want ErrUnexpectedResponse", err)
		}
	})

	t.Run("resources", func(t *testing.T) {
		page := 0
		client := mustDomainDiscoveryClient(t, doerFunc(func(*http.Request) (*http.Response, error) {
			entries := make([]string, 0, domainDiscoveryPageSize)
			for index := 0; index < domainDiscoveryPageSize; index++ {
				entries = append(entries, fmt.Sprintf(`{"name":"p%02d-%04d.example.com","operatingState":"success"}`, page, index))
			}
			page++
			body := fmt.Sprintf(`{"domains":[%s],"marker":"page-%d"}`, strings.Join(entries, ","), page)
			return discoveryResponse(body), nil
		}))
		if _, err := client.ListDomains(context.Background()); !errors.Is(err, ErrUnexpectedResponse) {
			t.Fatalf("error = %v, want ErrUnexpectedResponse", err)
		}
		if page != maxDomainDiscoveryResources/domainDiscoveryPageSize {
			t.Fatalf("pages = %d, want %d", page, maxDomainDiscoveryResources/domainDiscoveryPageSize)
		}
	})

	t.Run("pages", func(t *testing.T) {
		page := 0
		client := mustDomainDiscoveryClient(t, doerFunc(func(*http.Request) (*http.Response, error) {
			page++
			body := fmt.Sprintf(`{"domains":[{"name":"page-%d.example.com","operatingState":"success"}],"marker":"marker-%d"}`, page, page)
			return discoveryResponse(body), nil
		}))
		if _, err := client.ListDomains(context.Background()); !errors.Is(err, ErrUnexpectedResponse) {
			t.Fatalf("error = %v, want ErrUnexpectedResponse", err)
		}
		if page != maxDomainDiscoveryPages {
			t.Fatalf("pages = %d, want %d", page, maxDomainDiscoveryPages)
		}
	})
}

func TestDomainDiscoveryClientPropagatesTransportAndHTTPFailures(t *testing.T) {
	transportErr := errors.New("network unavailable")
	client := mustDomainDiscoveryClient(t, doerFunc(func(*http.Request) (*http.Response, error) {
		return nil, transportErr
	}))
	if _, err := client.ListDomains(context.Background()); !errors.Is(err, transportErr) {
		t.Fatalf("transport error = %v", err)
	}

	client = mustDomainDiscoveryClient(t, doerFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusTooManyRequests, "rate limited"), nil
	}))
	_, err := client.ListDomains(context.Background())
	var statusError *HTTPStatusError
	if !errors.As(err, &statusError) || statusError.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("HTTP error = %v, want status 429", err)
	}

	client = mustDomainDiscoveryClient(t, doerFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK}, nil
	}))
	if _, err := client.ListDomains(context.Background()); !errors.Is(err, ErrUnexpectedResponse) {
		t.Fatalf("nil body error = %v, want ErrUnexpectedResponse", err)
	}
}

func mustDomainDiscoveryClient(t *testing.T, doer Doer) *DomainDiscoveryClient {
	t.Helper()
	client, err := NewDomainDiscoveryClient(doer, "http://api.test/domain")
	if err != nil {
		t.Fatalf("NewDomainDiscoveryClient: %v", err)
	}
	return client
}

func discoveryResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     http.StatusText(http.StatusOK),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
