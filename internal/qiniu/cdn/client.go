package cdn

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	DefaultBaseURL = "https://fusion.qiniuapi.com"

	meteringBandwidthPath   = "/v2/tune/bandwidth"
	meteringFluxPath        = "/v2/tune/flux"
	monitoringBandwidthPath = "/v2/tune/monitoring/bandwidth"
	monitoringFlowPath      = "/v2/tune/monitoring/flow"
	requestCountPath        = "/v2/tune/loganalyze/reqcount"
	statusCodePath          = "/v2/tune/loganalyze/statuscode"
	hitMissPath             = "/v2/tune/loganalyze/hitmiss"
	topTrafficIPPath        = "/v2/tune/loganalyze/toptrafficip"
	topCountIPPath          = "/v2/tune/loganalyze/topcountip"

	maxUsageDomains     = 50
	maxAnalyticsDomains = 100
	maxResponseBytes    = 16 << 20
)

var (
	ErrInvalidInput       = errors.New("cdn: invalid input")
	ErrUnexpectedResponse = errors.New("cdn: unexpected response")
)

// Doer is normally implemented by the shared QBox-signing HTTP transport.
// Client deliberately performs no authentication of its own.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

type Client struct {
	doer    Doer
	baseURL string
}

// NewClient creates a client that can call only the fixed CDN P0 statistics
// endpoints in this package. An empty baseURL selects DefaultBaseURL.
func NewClient(doer Doer, baseURL string) (*Client, error) {
	if doer == nil {
		return nil, fmt.Errorf("%w: nil HTTP doer", ErrInvalidInput)
	}
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}

	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("%w: parse base URL: %v", ErrInvalidInput, err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("%w: base URL must be an absolute HTTP(S) URL", ErrInvalidInput)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, fmt.Errorf("%w: base URL must not contain credentials, path, query, or fragment", ErrInvalidInput)
	}

	return &Client{doer: doer, baseURL: strings.TrimRight(baseURL, "/")}, nil
}

type MonitoringQuery struct {
	Domains   []string
	StartDate string
	EndDate   string
}

// MeteringQuery selects billing-grade CDN traffic buckets.
// Granularity is explicit even though Qiniu defaults an omitted value to 5min.
type MeteringQuery struct {
	Domains     []string
	StartDate   string
	EndDate     string
	Granularity Granularity
}

type DomainQuery struct {
	Domain    string
	StartDate string
	EndDate   string
}

type RegionalDomainQuery struct {
	DomainQuery
	Region string
}

type TopIPQuery struct {
	Domains   []string
	StartDate string
	EndDate   string
	Region    string
}

// MonitoringResponse is the official response shape shared by monitoring
// bandwidth and monitoring flow.
type MonitoringResponse struct {
	Code  int                               `json:"code"`
	Error string                            `json:"error"`
	Times []string                          `json:"time"`
	Data  map[string]MonitoringRegionSeries `json:"data"`
}

type MonitoringRegionSeries struct {
	China   []float64 `json:"china"`
	Oversea []float64 `json:"oversea"`
}

// UsageResponse and UsageRegionSeries name the response shared by metering
// and monitoring endpoints without breaking the existing monitoring API.
type UsageResponse = MonitoringResponse
type UsageRegionSeries = MonitoringRegionSeries

type RequestCountResponse struct {
	Code  int              `json:"code"`
	Error string           `json:"error"`
	Data  RequestCountData `json:"data"`
}

type RequestCountData struct {
	Points   []string  `json:"points"`
	ReqCount []float64 `json:"reqCount"`
}

type StatusCodeResponse struct {
	Code  int            `json:"code"`
	Error string         `json:"error"`
	Data  StatusCodeData `json:"data"`
}

type StatusCodeData struct {
	Points []string             `json:"points"`
	Codes  map[string][]float64 `json:"codes"`
}

type HitMissResponse struct {
	Code  int         `json:"code"`
	Error string      `json:"error"`
	Data  HitMissData `json:"data"`
}

type HitMissData struct {
	Points      []string  `json:"points"`
	Hit         []float64 `json:"hit"`
	Miss        []float64 `json:"miss"`
	TrafficHit  []float64 `json:"trafficHit"`
	TrafficMiss []float64 `json:"trafficMiss"`
}

type TopIPResponse struct {
	Code  int       `json:"code"`
	Error string    `json:"error"`
	Data  TopIPData `json:"data"`
}

type TopIPData struct {
	IPs     []string  `json:"ips"`
	Traffic []float64 `json:"traffic"`
	Count   []float64 `json:"count"`
}

// HTTPStatusError reports a non-2xx HTTP response without retaining a
// potentially sensitive response body.
type HTTPStatusError struct {
	StatusCode int
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("cdn: unexpected HTTP status %d", e.StatusCode)
}

func (e *HTTPStatusError) Unwrap() error { return ErrUnexpectedResponse }

// APIError reports an HTTP-successful response whose Qiniu business code is
// not 200.
type APIError struct {
	Code    int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("cdn: API response code %d", e.Code)
}

func (e *APIError) Unwrap() error { return ErrUnexpectedResponse }

func (c *Client) FetchMonitoringBandwidth(ctx context.Context, query MonitoringQuery) (MonitoringResponse, error) {
	return c.fetchMonitoring(ctx, monitoringBandwidthPath, query)
}

func (c *Client) FetchMonitoringFlow(ctx context.Context, query MonitoringQuery) (MonitoringResponse, error) {
	return c.fetchMonitoring(ctx, monitoringFlowPath, query)
}

func (c *Client) FetchMeteringFlux(ctx context.Context, query MeteringQuery) (UsageResponse, error) {
	return c.fetchMetering(ctx, meteringFluxPath, query)
}

func (c *Client) FetchMeteringBandwidth(ctx context.Context, query MeteringQuery) (UsageResponse, error) {
	return c.fetchMetering(ctx, meteringBandwidthPath, query)
}

func (c *Client) FetchRequestCount(ctx context.Context, query RegionalDomainQuery) (RequestCountResponse, error) {
	var result RequestCountResponse
	if err := validateRegionalDomainQuery(query); err != nil {
		return result, err
	}
	payload := regionalAnalyticsRequest{
		Domains:   []string{query.Domain},
		StartDate: query.StartDate,
		EndDate:   query.EndDate,
		Freq:      "5min",
		Region:    query.Region,
	}
	if err := c.postJSON(ctx, requestCountPath, payload, &result); err != nil {
		return result, err
	}
	return result, checkBusinessCode(result.Code, result.Error)
}

func (c *Client) FetchStatusCodes(ctx context.Context, query RegionalDomainQuery) (StatusCodeResponse, error) {
	var result StatusCodeResponse
	if err := validateRegionalDomainQuery(query); err != nil {
		return result, err
	}
	payload := regionalAnalyticsRequest{
		Domains:   []string{query.Domain},
		StartDate: query.StartDate,
		EndDate:   query.EndDate,
		Freq:      "5min",
		Region:    query.Region,
	}
	if err := c.postJSON(ctx, statusCodePath, payload, &result); err != nil {
		return result, err
	}
	return result, checkBusinessCode(result.Code, result.Error)
}

func (c *Client) FetchHitMiss(ctx context.Context, query DomainQuery) (HitMissResponse, error) {
	var result HitMissResponse
	if err := validateDomainQuery(query); err != nil {
		return result, err
	}
	payload := analyticsRequest{
		Domains:   []string{query.Domain},
		StartDate: query.StartDate,
		EndDate:   query.EndDate,
		Freq:      "5min",
	}
	if err := c.postJSON(ctx, hitMissPath, payload, &result); err != nil {
		return result, err
	}
	return result, checkBusinessCode(result.Code, result.Error)
}

func (c *Client) FetchTopIPTraffic(ctx context.Context, query TopIPQuery) (TopIPResponse, error) {
	return c.fetchTopIPs(ctx, topTrafficIPPath, query)
}

func (c *Client) FetchTopIPRequests(ctx context.Context, query TopIPQuery) (TopIPResponse, error) {
	return c.fetchTopIPs(ctx, topCountIPPath, query)
}

type monitoringRequest struct {
	Domains     string `json:"domains"`
	StartDate   string `json:"startDate"`
	EndDate     string `json:"endDate"`
	Granularity string `json:"granularity"`
}

type analyticsRequest struct {
	Domains   []string `json:"domains"`
	StartDate string   `json:"startDate"`
	EndDate   string   `json:"endDate"`
	Freq      string   `json:"freq"`
}

type regionalAnalyticsRequest struct {
	Domains   []string `json:"domains"`
	StartDate string   `json:"startDate"`
	EndDate   string   `json:"endDate"`
	Freq      string   `json:"freq"`
	Region    string   `json:"region"`
}

type topIPRequest struct {
	Domains   []string `json:"domains"`
	StartDate string   `json:"startDate"`
	EndDate   string   `json:"endDate"`
	Region    string   `json:"region"`
}

func (c *Client) fetchTopIPs(ctx context.Context, path string, query TopIPQuery) (TopIPResponse, error) {
	var result TopIPResponse
	if err := validateTopIPQuery(query); err != nil {
		return result, err
	}
	payload := topIPRequest{
		Domains: query.Domains, StartDate: query.StartDate, EndDate: query.EndDate, Region: query.Region,
	}
	if err := c.postJSON(ctx, path, payload, &result); err != nil {
		return result, err
	}
	return result, checkBusinessCode(result.Code, result.Error)
}

func (c *Client) fetchMonitoring(ctx context.Context, path string, query MonitoringQuery) (MonitoringResponse, error) {
	var result MonitoringResponse
	if err := validateMonitoringQuery(query); err != nil {
		return result, err
	}
	payload := monitoringRequest{
		Domains:     strings.Join(query.Domains, ";"),
		StartDate:   query.StartDate,
		EndDate:     query.EndDate,
		Granularity: "5min",
	}
	if err := c.postJSON(ctx, path, payload, &result); err != nil {
		return result, err
	}
	return result, checkBusinessCode(result.Code, result.Error)
}

func (c *Client) fetchMetering(ctx context.Context, path string, query MeteringQuery) (UsageResponse, error) {
	var result UsageResponse
	if err := validateMeteringQuery(query); err != nil {
		return result, err
	}
	payload := monitoringRequest{
		Domains:     strings.Join(query.Domains, ";"),
		StartDate:   query.StartDate,
		EndDate:     query.EndDate,
		Granularity: query.Granularity.String(),
	}
	if err := c.postJSON(ctx, path, payload, &result); err != nil {
		return result, err
	}
	return result, checkBusinessCode(result.Code, result.Error)
}

func (c *Client) postJSON(ctx context.Context, path string, payload, result any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("cdn: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("cdn: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.doer.Do(req)
	if err != nil {
		return fmt.Errorf("cdn: POST %s: %w", path, err)
	}
	if resp == nil || resp.Body == nil {
		return fmt.Errorf("%w: nil HTTP response or body", ErrUnexpectedResponse)
	}
	defer resp.Body.Close()

	limited := io.LimitReader(resp.Body, maxResponseBytes+1)
	responseBody, err := io.ReadAll(limited)
	if err != nil {
		return fmt.Errorf("%w: read response: %v", ErrUnexpectedResponse, err)
	}
	if len(responseBody) > maxResponseBytes {
		return fmt.Errorf("%w: response exceeds %d bytes", ErrUnexpectedResponse, maxResponseBytes)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return &HTTPStatusError{StatusCode: resp.StatusCode}
	}

	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	if err := decoder.Decode(result); err != nil {
		return fmt.Errorf("%w: decode JSON: %v", ErrUnexpectedResponse, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("%w: multiple JSON values", ErrUnexpectedResponse)
		}
		return fmt.Errorf("%w: trailing JSON: %v", ErrUnexpectedResponse, err)
	}
	return nil
}

func checkBusinessCode(code int, message string) error {
	if code != http.StatusOK {
		return &APIError{Code: code, Message: message}
	}
	return nil
}

func validateMonitoringQuery(query MonitoringQuery) error {
	if err := validateUsageDomains(query.Domains); err != nil {
		return err
	}
	return validateDateRange(query.StartDate, query.EndDate, 31)
}

func validateMeteringQuery(query MeteringQuery) error {
	if err := validateUsageDomains(query.Domains); err != nil {
		return err
	}
	if !query.Granularity.Valid() {
		return fmt.Errorf("%w: invalid metering granularity", ErrInvalidInput)
	}
	return validateDateRange(query.StartDate, query.EndDate, 31)
}

func validateTopIPQuery(query TopIPQuery) error {
	if len(query.Domains) == 0 || len(query.Domains) > maxAnalyticsDomains {
		return fmt.Errorf("%w: top IP domains must contain 1 to %d entries", ErrInvalidInput, maxAnalyticsDomains)
	}
	seen := make(map[string]struct{}, len(query.Domains))
	for _, domain := range query.Domains {
		if err := validateDomain(domain, true); err != nil {
			return err
		}
		if _, exists := seen[domain]; exists {
			return fmt.Errorf("%w: duplicate top IP domain", ErrInvalidInput)
		}
		seen[domain] = struct{}{}
	}
	if query.Region != RegionGlobal && query.Region != RegionChina && query.Region != RegionOversea {
		return fmt.Errorf("%w: invalid top IP region", ErrInvalidInput)
	}
	return validateDateRange(query.StartDate, query.EndDate, 30)
}

func validateUsageDomains(domains []string) error {
	if len(domains) == 0 || len(domains) > maxUsageDomains {
		return fmt.Errorf("%w: usage domains must contain 1 to %d entries", ErrInvalidInput, maxUsageDomains)
	}
	seen := make(map[string]struct{}, len(domains))
	for _, domain := range domains {
		if err := validateDomain(domain, true); err != nil {
			return err
		}
		if _, exists := seen[domain]; exists {
			return fmt.Errorf("%w: duplicate domain %q", ErrInvalidInput, domain)
		}
		seen[domain] = struct{}{}
	}
	return nil
}

func validateDomainQuery(query DomainQuery) error {
	if err := validateDomain(query.Domain, true); err != nil {
		return err
	}
	return validateDateRange(query.StartDate, query.EndDate, 30)
}

func validateRegionalDomainQuery(query RegionalDomainQuery) error {
	if err := validateDomainQuery(query.DomainQuery); err != nil {
		return err
	}
	if query.Region == "" || strings.TrimSpace(query.Region) != query.Region || containsControl(query.Region) {
		return fmt.Errorf("%w: region must be non-empty and contain no surrounding whitespace or control characters", ErrInvalidInput)
	}
	return nil
}

func validateDomain(domain string, rejectSemicolon bool) error {
	if domain == "" || strings.TrimSpace(domain) != domain || containsControl(domain) {
		return fmt.Errorf("%w: domain must be non-empty and contain no surrounding whitespace or control characters", ErrInvalidInput)
	}
	if rejectSemicolon && strings.Contains(domain, ";") {
		return fmt.Errorf("%w: domain %q contains the monitoring separator", ErrInvalidInput, domain)
	}
	return nil
}

func validateDateRange(startText, endText string, maxInclusiveDays int) error {
	const dateLayout = "2006-01-02"
	start, err := time.Parse(dateLayout, startText)
	if err != nil || start.Format(dateLayout) != startText {
		return fmt.Errorf("%w: invalid startDate %q", ErrInvalidInput, startText)
	}
	end, err := time.Parse(dateLayout, endText)
	if err != nil || end.Format(dateLayout) != endText {
		return fmt.Errorf("%w: invalid endDate %q", ErrInvalidInput, endText)
	}
	if end.Before(start) {
		return fmt.Errorf("%w: endDate precedes startDate", ErrInvalidInput)
	}
	// startDate and endDate are both included by Qiniu. A 31-day request can
	// therefore have at most 30 midnights between its endpoints.
	if end.Sub(start) >= time.Duration(maxInclusiveDays)*24*time.Hour {
		return fmt.Errorf("%w: date range exceeds %d inclusive days", ErrInvalidInput, maxInclusiveDays)
	}
	return nil
}

func containsControl(value string) bool {
	for _, r := range value {
		if r < ' ' || r == '\u007f' {
			return true
		}
	}
	return false
}
