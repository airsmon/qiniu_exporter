package authhttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/qiniu/go-sdk/v7/auth"
	"qiniu-exporter/internal/limiter"
)

const maxObservedBody = 32 << 20

type Endpoint struct {
	Method string
	Path   string
	Name   string
}

type Policy struct {
	Host      string
	Endpoints []Endpoint
}

func (p Policy) endpoint(req *http.Request) (string, error) {
	if !strings.EqualFold(req.URL.Scheme, "https") {
		return "", fmt.Errorf("authhttp: scheme %q is not allowed", req.URL.Scheme)
	}
	if !strings.EqualFold(req.URL.Host, p.Host) {
		return "", fmt.Errorf("authhttp: host %q is not allowed", req.URL.Host)
	}
	if req.Host != "" && !strings.EqualFold(req.Host, p.Host) {
		return "", fmt.Errorf("authhttp: Host override %q is not allowed", req.Host)
	}
	for _, endpoint := range p.Endpoints {
		if req.Method == endpoint.Method && req.URL.Path == endpoint.Path {
			return endpoint.Name, nil
		}
	}
	return "", fmt.Errorf("authhttp: endpoint %s %s is not allowed", req.Method, req.URL.Path)
}

type Observer interface {
	ObserveAPIRequest(service, endpoint, result string, duration time.Duration)
	ObserveLimiterWait(service, host string, duration time.Duration)
	ObserveRateLimited(service, host string)
}

type Client struct {
	Service      string
	Credentials  *auth.Credentials
	TokenType    auth.TokenType
	AddQiniuDate bool
	Policy       Policy
	// FirstAttemptLimiter applies the normal collection budget only to attempt
	// zero. RetryLimiter separately caps attempt > 0. HostLimiter and
	// EndpointLimiter remain hard caps for every attempt.
	FirstAttemptLimiter *limiter.Limiter
	RetryLimiter        *limiter.Limiter
	HostLimiter         *limiter.Limiter
	EndpointLimiter     *limiter.Limiter
	Transport           http.RoundTripper
	Observer            Observer
	MaxRetries          int
	// ExpectedBusinessCode enables envelope classification for APIs whose
	// successful JSON response always contains a numeric code field.
	ExpectedBusinessCode *int
}

func (c *Client) Do(req *http.Request) (*http.Response, error) {
	if c.Credentials == nil {
		return nil, errors.New("authhttp: credentials are required")
	}
	endpoint, err := c.Policy.endpoint(req)
	if err != nil {
		return nil, err
	}
	if req.Body != nil && req.GetBody == nil {
		return nil, errors.New("authhttp: request body must be replayable")
	}
	transport := c.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	retries := c.MaxRetries
	if retries < 0 {
		retries = 0
	}
	if retries > 2 {
		retries = 2
	}

	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 {
			if err := waitBackoff(req.Context(), attempt); err != nil {
				return nil, err
			}
		}

		attemptReq, err := cloneRequest(req)
		if err != nil {
			return nil, err
		}
		release, err := c.acquire(req.Context(), attempt == 0)
		if err != nil {
			return nil, err
		}
		attemptReq.Header.Del("Authorization")
		if c.AddQiniuDate {
			attemptReq.Header.Set("X-Qiniu-Date", time.Now().UTC().Format("20060102T150405Z"))
		}
		if err := c.Credentials.AddToken(c.TokenType, attemptReq); err != nil {
			release()
			return nil, fmt.Errorf("authhttp: sign request: %w", err)
		}
		started := time.Now()
		resp, doErr := transport.RoundTrip(attemptReq)

		if doErr != nil {
			duration := time.Since(started)
			release()
			if resp != nil && resp.Body != nil {
				resp.Body.Close()
			}
			if c.Observer != nil {
				c.Observer.ObserveAPIRequest(c.Service, endpoint, classify(nil, doErr), duration)
			}
			lastErr = doErr
			if req.Context().Err() != nil {
				return nil, req.Context().Err()
			}
			if attempt < retries {
				continue
			}
			return nil, doErr
		}
		if resp == nil || resp.Body == nil {
			duration := time.Since(started)
			release()
			if c.Observer != nil {
				c.Observer.ObserveAPIRequest(c.Service, endpoint, "transport_error", duration)
			}
			return nil, errors.New("authhttp: transport returned nil response or body")
		}

		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxObservedBody+1))
		resp.Body.Close()
		if readErr != nil {
			duration := time.Since(started)
			release()
			if c.Observer != nil {
				c.Observer.ObserveAPIRequest(c.Service, endpoint, "transport_error", duration)
			}
			lastErr = fmt.Errorf("authhttp: read response body: %w", readErr)
			if req.Context().Err() != nil {
				return nil, req.Context().Err()
			}
			if attempt < retries {
				continue
			}
			return nil, lastErr
		}
		if len(body) > maxObservedBody {
			duration := time.Since(started)
			release()
			if c.Observer != nil {
				c.Observer.ObserveAPIRequest(c.Service, endpoint, "decode_error", duration)
			}
			return nil, fmt.Errorf("authhttp: response exceeds %d bytes", maxObservedBody)
		}
		resp.Body = io.NopCloser(bytes.NewReader(body))

		rateLimited, retryAfter := inspectRateLimit(resp, body)
		result := classify(resp, nil)
		if resp.StatusCode >= 200 && resp.StatusCode < 300 && c.ExpectedBusinessCode != nil {
			businessCode, ok, decodeErr := inspectBusinessCode(body)
			if decodeErr != nil || !ok {
				result = "decode_error"
			} else if businessCode != *c.ExpectedBusinessCode {
				result = "api_error"
				if businessCode == 403024 {
					rateLimited = true
				}
			}
		}
		if rateLimited {
			result = "rate_limited"
		}
		duration := time.Since(started)
		if c.Observer != nil {
			c.Observer.ObserveAPIRequest(c.Service, endpoint, result, duration)
		}
		if rateLimited {
			c.onRateLimited(retryAfter)
		}
		release()

		if attempt < retries && (rateLimited || resp.StatusCode >= 500) {
			resp.Body.Close()
			continue
		}
		return resp, nil
	}
	return nil, lastErr
}

func inspectBusinessCode(body []byte) (int, bool, error) {
	var envelope struct {
		Code *int `json:"code"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return 0, false, err
	}
	if envelope.Code == nil {
		return 0, false, nil
	}
	return *envelope.Code, true, nil
}

func (c *Client) acquire(ctx context.Context, firstAttempt bool) (func(), error) {
	releases := make([]func(), 0, 3)
	acquired := make(map[*limiter.Limiter]struct{}, 3)
	started := time.Now()
	hasLimiter := false
	observeWait := func() {
		if hasLimiter && c.Observer != nil {
			c.Observer.ObserveLimiterWait(c.Service, c.Policy.Host, time.Since(started))
		}
	}
	acquire := func(l *limiter.Limiter) error {
		if l == nil {
			return nil
		}
		if _, exists := acquired[l]; exists {
			return nil
		}
		hasLimiter = true
		release, _, err := l.Acquire(ctx)
		if err != nil {
			return err
		}
		acquired[l] = struct{}{}
		releases = append(releases, release)
		return nil
	}
	releaseAll := func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}
	if firstAttempt {
		if err := acquire(c.FirstAttemptLimiter); err != nil {
			observeWait()
			return nil, err
		}
	} else {
		if err := acquire(c.RetryLimiter); err != nil {
			observeWait()
			return nil, err
		}
	}
	if err := acquire(c.HostLimiter); err != nil {
		releaseAll()
		observeWait()
		return nil, err
	}
	if c.EndpointLimiter != c.HostLimiter {
		if err := acquire(c.EndpointLimiter); err != nil {
			releaseAll()
			observeWait()
			return nil, err
		}
	}
	observeWait()
	return releaseAll, nil
}

func (c *Client) onRateLimited(retryAfter time.Duration) {
	if c.HostLimiter != nil {
		c.HostLimiter.OnRateLimited(retryAfter)
	}
	if c.EndpointLimiter != nil && c.EndpointLimiter != c.HostLimiter {
		c.EndpointLimiter.OnRateLimited(retryAfter)
	}
	if c.Observer != nil {
		c.Observer.ObserveRateLimited(c.Service, c.Policy.Host)
	}
}

func cloneRequest(req *http.Request) (*http.Request, error) {
	clone := req.Clone(req.Context())
	if req.Body == nil {
		return clone, nil
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, fmt.Errorf("authhttp: replay body: %w", err)
	}
	clone.Body = body
	return clone, nil
}

func classify(resp *http.Response, err error) string {
	if err != nil {
		return "transport_error"
	}
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return "rate_limited"
	case resp.StatusCode >= 500:
		return "http_5xx"
	case resp.StatusCode >= 400:
		return "http_4xx"
	default:
		return "success"
	}
}

func inspectRateLimit(resp *http.Response, body []byte) (bool, time.Duration) {
	rateLimited := resp.StatusCode == http.StatusTooManyRequests
	if resp.StatusCode == http.StatusForbidden {
		var envelope struct {
			Code json.RawMessage `json:"code"`
		}
		if json.Unmarshal(body, &envelope) == nil {
			code := strings.Trim(string(envelope.Code), "\"")
			rateLimited = code == "403024"
		}
	}
	return rateLimited, parseRetryAfter(resp.Header.Get("Retry-After"))
}

func parseRetryAfter(value string) time.Duration {
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		if delay := time.Until(when); delay > 0 {
			return delay
		}
	}
	return 0
}

func waitBackoff(ctx context.Context, attempt int) error {
	maximum := time.Second << min(attempt-1, 4)
	delay := time.Duration(rand.Int63n(int64(maximum) + 1))
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
