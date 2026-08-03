package authhttp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qiniu/go-sdk/v7/auth"
	"qiniu-exporter/internal/limiter"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type failingBody struct{}

func (failingBody) Read([]byte) (int, error) { return 0, errors.New("body read failed") }
func (failingBody) Close() error             { return nil }

type observer struct {
	mu          sync.Mutex
	rateLimited int
	results     []string
}

func (o *observer) ObserveAPIRequest(_, _, result string, _ time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.results = append(o.results, result)
}
func (o *observer) ObserveLimiterWait(_, _ string, _ time.Duration) {}
func (o *observer) ObserveRateLimited(_, _ string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.rateLimited++
}

func TestPolicyRejectsUnknownEndpoint(t *testing.T) {
	request, err := http.NewRequest(http.MethodDelete, "https://api.example.test/managed", nil)
	if err != nil {
		t.Fatal(err)
	}
	client := Client{
		Credentials: auth.New("ak", "sk"),
		Policy: Policy{Host: "api.example.test", Endpoints: []Endpoint{{
			Method: http.MethodGet, Path: "/stats", Name: "stats",
		}}},
	}
	if _, err := client.Do(request); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("expected allowlist error, got %v", err)
	}
}

func TestPolicyRequiresHTTPSAndRejectsHostOverride(t *testing.T) {
	policy := Policy{Host: "api.example.test", Endpoints: []Endpoint{{
		Method: http.MethodGet, Path: "/stats", Name: "stats",
	}}}
	for _, test := range []struct {
		name string
		url  string
		host string
	}{
		{name: "plain HTTP", url: "http://api.example.test/stats"},
		{name: "Host override", url: "https://api.example.test/stats", host: "attacker.example.test"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodGet, test.url, nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Host = test.host
			client := Client{Credentials: auth.New("ak", "sk"), Policy: policy}
			if _, err := client.Do(request); err == nil || !strings.Contains(err.Error(), "not allowed") {
				t.Fatalf("expected policy rejection, got %v", err)
			}
		})
	}
}

func TestConcurrencySlotCoversResponseBodyRead(t *testing.T) {
	limit, err := limiter.New(1_000_000, 1)
	if err != nil {
		t.Fatal(err)
	}
	reader, writer := io.Pipe()
	entered := make(chan int, 2)
	var attempts atomic.Int32
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		attempt := int(attempts.Add(1))
		entered <- attempt
		body := io.NopCloser(strings.NewReader(`{"ok":true}`))
		if attempt == 1 {
			body = reader
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body, Request: req}, nil
	})
	client := Client{
		Credentials: auth.New("ak", "sk"), TokenType: auth.TokenQiniu,
		Policy:      Policy{Host: "api.example.test", Endpoints: []Endpoint{{Method: http.MethodGet, Path: "/stats", Name: "stats"}}},
		HostLimiter: limit, Transport: transport,
	}

	results := make(chan error, 2)
	call := func() {
		request, requestErr := http.NewRequest(http.MethodGet, "https://api.example.test/stats", nil)
		if requestErr != nil {
			results <- requestErr
			return
		}
		response, callErr := client.Do(request)
		if response != nil {
			response.Body.Close()
		}
		results <- callErr
	}
	go call()
	if got := <-entered; got != 1 {
		t.Fatalf("first transport attempt=%d", got)
	}
	go call()
	select {
	case got := <-entered:
		t.Fatalf("second transport attempt %d entered before the first response body completed", got)
	case <-time.After(25 * time.Millisecond):
	}
	if _, err := writer.Write([]byte(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-entered:
		if got != 2 {
			t.Fatalf("second transport attempt=%d", got)
		}
	case <-time.After(time.Second):
		t.Fatal("second request did not enter after the first body completed")
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
}

func TestRetryReplaysBodyAndResigns(t *testing.T) {
	var attempts int
	var bodies []string
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		attempts++
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, string(body))
		if !strings.HasPrefix(req.Header.Get("Authorization"), "Qiniu ") {
			t.Fatalf("request was not signed: %q", req.Header.Get("Authorization"))
		}
		status := http.StatusOK
		if attempts == 1 {
			status = http.StatusInternalServerError
		}
		return &http.Response{
			StatusCode: status,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewBufferString(`{"code":0}`)),
			Request:    req,
		}, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.example.test/stats", bytes.NewBufferString(`{"bucket":"b"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	firstLimit, err := limiter.NewRate(0.5)
	if err != nil {
		t.Fatal(err)
	}
	retryLimit, err := limiter.NewRate(1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	client := Client{
		Service:     "test",
		Credentials: auth.New("ak", "sk"),
		TokenType:   auth.TokenQiniu,
		Policy: Policy{Host: "api.example.test", Endpoints: []Endpoint{{
			Method: http.MethodPost, Path: "/stats", Name: "stats",
		}}},
		Transport:           transport,
		FirstAttemptLimiter: firstLimit,
		RetryLimiter:        retryLimit,
		MaxRetries:          1,
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if attempts != 2 {
		t.Fatalf("attempts=%d, want 2", attempts)
	}
	if bodies[0] != bodies[1] || bodies[0] != `{"bucket":"b"}` {
		t.Fatalf("request body was not replayed: %#v", bodies)
	}
}

func TestRetryAttemptUsesSeparateLimiter(t *testing.T) {
	retryLimit, err := limiter.NewRate(1)
	if err != nil {
		t.Fatal(err)
	}
	release, _, err := retryLimit.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	release()

	client := Client{RetryLimiter: retryLimit}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := client.acquire(ctx, false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("retry acquire error=%v, want context deadline", err)
	}
}

func Test403024IsClassifiedAsRateLimit(t *testing.T) {
	parsed, _ := url.Parse("https://api.example.test/stats")
	request := &http.Request{Method: http.MethodGet, URL: parsed, Header: make(http.Header)}
	observed := &observer{}
	client := Client{
		Service:     "test",
		Credentials: auth.New("ak", "sk"),
		TokenType:   auth.TokenQiniu,
		Policy: Policy{Host: "api.example.test", Endpoints: []Endpoint{{
			Method: http.MethodGet, Path: "/stats", Name: "stats",
		}}},
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"code":403024}`)),
				Request:    req,
			}, nil
		}),
		Observer: observed,
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if observed.rateLimited != 1 || len(observed.results) != 1 || observed.results[0] != "rate_limited" {
		t.Fatalf("unexpected observations: rate_limited=%d results=%v", observed.rateLimited, observed.results)
	}
}

func TestResponseBodyTransportFailureIsRetried(t *testing.T) {
	attempts := 0
	client := Client{
		Credentials: auth.New("ak", "sk"), TokenType: auth.TokenQiniu, MaxRetries: 1,
		Policy: Policy{Host: "api.example.test", Endpoints: []Endpoint{{Method: http.MethodGet, Path: "/stats", Name: "stats"}}},
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			attempts++
			var body io.ReadCloser = failingBody{}
			if attempts == 2 {
				body = io.NopCloser(strings.NewReader(`{"ok":true}`))
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body, Request: req}, nil
		}),
	}
	request, err := http.NewRequest(http.MethodGet, "https://api.example.test/stats", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if attempts != 2 {
		t.Fatalf("attempts=%d, want 2", attempts)
	}
}

func TestBusinessEnvelopeIsClassified(t *testing.T) {
	request, err := http.NewRequest(http.MethodGet, "https://api.example.test/stats", nil)
	if err != nil {
		t.Fatal(err)
	}
	observed := &observer{}
	expected := 200
	client := Client{
		Service: "test", Credentials: auth.New("ak", "sk"), TokenType: auth.TokenQiniu,
		Policy: Policy{Host: "api.example.test", Endpoints: []Endpoint{{Method: http.MethodGet, Path: "/stats", Name: "stats"}}},
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":400032,"error":"invalid"}`)), Request: req}, nil
		}),
		Observer: observed, ExpectedBusinessCode: &expected,
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if len(observed.results) != 1 || observed.results[0] != "api_error" {
		t.Fatalf("results=%v, want api_error", observed.results)
	}
}

func TestQBoxSigning(t *testing.T) {
	request, err := http.NewRequest(http.MethodPost, "https://fusion.example.test/stats", bytes.NewBufferString(`{"domains":["a.example.com"]}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	client := Client{
		Service: "cdn", Credentials: auth.New("ak", "sk"), TokenType: auth.TokenQBox,
		Policy: Policy{Host: "fusion.example.test", Endpoints: []Endpoint{{Method: http.MethodPost, Path: "/stats", Name: "stats"}}},
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if !strings.HasPrefix(req.Header.Get("Authorization"), "QBox ak:") {
				t.Fatalf("unexpected QBox authorization: %q", req.Header.Get("Authorization"))
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":200}`)), Request: req}, nil
		}),
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
}
