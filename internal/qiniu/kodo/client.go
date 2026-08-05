package kodo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	BlobIOPath       = "/v6/blob_io"
	RSPutPath        = "/v6/rs_put"
	maxResponseBytes = 4 << 20
	qiniuTimeLayout  = "20060102150405"
)

// Client queries only the fixed read-only Kodo statistics endpoints defined
// by this package. Authentication, retries, and rate limiting belong to Doer.
type Client struct {
	doer    Doer
	baseURL *url.URL
}

func NewClient(doer Doer, baseURL string) (*Client, error) {
	if doer == nil {
		return nil, fmt.Errorf("%w: doer is required", ErrInvalidInput)
	}
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("%w: parse base URL: %v", ErrInvalidInput, err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("%w: base URL must be an absolute HTTP URL", ErrInvalidInput)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, fmt.Errorf("%w: base URL must not contain credentials, path, query, or fragment", ErrInvalidInput)
	}
	parsed.Path = ""
	return &Client{doer: doer, baseURL: parsed}, nil
}

// CollectP0 atomically fetches storage, object, request, and egress samples for
// one static bucket. It returns no partial samples when any request fails.
func (c *Client) CollectP0(ctx context.Context, input CollectInput) ([]GaugeSample, error) {
	if err := validateQuery(input.Query); err != nil {
		return nil, err
	}
	if len(input.StorageClasses) == 0 {
		return nil, fmt.Errorf("%w: at least one storage class is required", ErrInvalidInput)
	}

	seen := make(map[StorageClass]struct{}, len(input.StorageClasses))
	for _, storageClass := range input.StorageClasses {
		if _, ok := seen[storageClass]; ok {
			return nil, fmt.Errorf("%w: duplicate storage class %q", ErrInvalidInput, storageClass)
		}
		if _, err := EndpointsForStorageClass(storageClass); err != nil {
			return nil, err
		}
		seen[storageClass] = struct{}{}
	}

	samples := make([]GaugeSample, 0, 2*len(input.StorageClasses)+4)
	for _, storageClass := range input.StorageClasses {
		storage, err := c.Storage(ctx, input.Query, storageClass)
		if err != nil {
			return nil, fmt.Errorf("collect storage %s: %w", storageClass, err)
		}
		samples = append(samples, storage)

		objects, err := c.Objects(ctx, input.Query, storageClass)
		if err != nil {
			return nil, fmt.Errorf("collect objects %s: %w", storageClass, err)
		}
		samples = append(samples, objects)
	}

	getRequests, err := c.GETRequests(ctx, input.Query)
	if err != nil {
		return nil, fmt.Errorf("collect GET requests: %w", err)
	}
	samples = append(samples, getRequests)

	putRequests, err := c.PUTRequests(ctx, input.Query)
	if err != nil {
		return nil, fmt.Errorf("collect PUT requests: %w", err)
	}
	samples = append(samples, putRequests)

	directEgress, err := c.DirectEgress(ctx, input.Query)
	if err != nil {
		return nil, fmt.Errorf("collect direct egress: %w", err)
	}
	samples = append(samples, directEgress)

	cdnOriginEgress, err := c.CDNOriginEgress(ctx, input.Query)
	if err != nil {
		return nil, fmt.Errorf("collect CDN origin egress: %w", err)
	}
	samples = append(samples, cdnOriginEgress)
	return samples, nil
}

func (c *Client) Storage(ctx context.Context, query Query, storageClass StorageClass) (GaugeSample, error) {
	endpoints, err := EndpointsForStorageClass(storageClass)
	if err != nil {
		return GaugeSample{}, err
	}
	point, err := c.fetchArrayPoint(ctx, endpoints.CapacityPath, query)
	if err != nil {
		return GaugeSample{}, err
	}
	sample := sampleFromPoint(GaugeStorageBytes, query, point)
	sample.StorageClass = storageClass
	return sample, nil
}

func (c *Client) Objects(ctx context.Context, query Query, storageClass StorageClass) (GaugeSample, error) {
	endpoints, err := EndpointsForStorageClass(storageClass)
	if err != nil {
		return GaugeSample{}, err
	}
	point, err := c.fetchArrayPoint(ctx, endpoints.ObjectCountPath, query)
	if err != nil {
		return GaugeSample{}, err
	}
	sample := sampleFromPoint(GaugeObjects, query, point)
	sample.StorageClass = storageClass
	return sample, nil
}

func (c *Client) GETRequests(ctx context.Context, query Query) (GaugeSample, error) {
	point, err := c.fetchValuePoint(ctx, BlobIOPath, query, "hits", "hits", "hits")
	if err != nil {
		return GaugeSample{}, err
	}
	sample := rateSampleFromPoint(GaugeRequestsPerSecond, query, point)
	sample.Operation = OperationGet
	return sample, nil
}

func (c *Client) PUTRequests(ctx context.Context, query Query) (GaugeSample, error) {
	point, err := c.fetchValuePoint(ctx, RSPutPath, query, "hits", "hits", "")
	if err != nil {
		return GaugeSample{}, err
	}
	sample := rateSampleFromPoint(GaugeRequestsPerSecond, query, point)
	sample.Operation = OperationPut
	return sample, nil
}

func (c *Client) DirectEgress(ctx context.Context, query Query) (GaugeSample, error) {
	point, err := c.fetchValuePoint(ctx, BlobIOPath, query, "flow", "flow", "flow_out")
	if err != nil {
		return GaugeSample{}, err
	}
	sample := rateSampleFromPoint(GaugeEgressBytesPerSecond, query, point)
	sample.Route = RouteDirect
	return sample, nil
}

func (c *Client) CDNOriginEgress(ctx context.Context, query Query) (GaugeSample, error) {
	point, err := c.fetchValuePoint(ctx, BlobIOPath, query, "flow", "flow", "cdn_flow_out")
	if err != nil {
		return GaugeSample{}, err
	}
	sample := rateSampleFromPoint(GaugeEgressBytesPerSecond, query, point)
	sample.Route = RouteCDNOrigin
	return sample, nil
}

func (c *Client) CurrentMonthDirectEgress(ctx context.Context, query MonthToDateQuery) (GaugeSample, error) {
	value, err := c.fetchMonthToDateUsage(ctx, BlobIOPath, query, "flow", "flow", "flow_out")
	if err != nil {
		return GaugeSample{}, err
	}
	return GaugeSample{
		Kind:   GaugeUsageEgressBytes,
		Bucket: query.Bucket,
		Region: query.Region,
		Route:  RouteDirect,
		Period: PeriodCurrentMonth,
		Value:  value,
		DataAt: query.End,
	}, nil
}

func (c *Client) CurrentMonthPUTRequests(ctx context.Context, query MonthToDateQuery) (GaugeSample, error) {
	value, err := c.fetchMonthToDateUsage(ctx, RSPutPath, query, "hits", "hits", "")
	if err != nil {
		return GaugeSample{}, err
	}
	return GaugeSample{
		Kind:      GaugeUsageRequests,
		Bucket:    query.Bucket,
		Region:    query.Region,
		Operation: OperationPut,
		Period:    PeriodCurrentMonth,
		Value:     value,
		DataAt:    query.End,
	}, nil
}

func (c *Client) fetchArrayPoint(ctx context.Context, path string, query Query) (Point, error) {
	if err := validateQuery(query); err != nil {
		return Point{}, err
	}
	params := commonParams(query)
	params.Set("bucket", query.Bucket)
	params.Set("region", query.Region)

	var response arrayResponse
	if err := c.getJSON(ctx, path, params, &response); err != nil {
		return Point{}, err
	}
	points, err := response.points()
	if err != nil {
		return Point{}, err
	}
	return SelectLatestSafe5Min(points, query.SafeBefore)
}

func (c *Client) fetchValuePoint(
	ctx context.Context,
	path string,
	query Query,
	selectValue string,
	responseValue string,
	metric string,
) (Point, error) {
	if err := validateQuery(query); err != nil {
		return Point{}, err
	}
	params := commonParams(query)
	params.Set("$bucket", query.Bucket)
	params.Set("$region", query.Region)
	params.Set("select", selectValue)
	if metric != "" {
		params.Set("$metric", metric)
	}

	var response valueResponse
	if err := c.getJSON(ctx, path, params, &response); err != nil {
		return Point{}, err
	}
	points, err := response.points(responseValue)
	if err != nil {
		return Point{}, err
	}
	return SelectLatestSafeRate5Min(points, query.SafeBefore)
}

func (c *Client) fetchMonthToDateUsage(
	ctx context.Context,
	path string,
	query MonthToDateQuery,
	selectValue string,
	responseValue string,
	metric string,
) (float64, error) {
	if err := validateMonthToDateQuery(query); err != nil {
		return 0, err
	}
	params := monthToDateParams(query)
	params.Set("$bucket", query.Bucket)
	params.Set("$region", query.Region)
	params.Set("select", selectValue)
	if metric != "" {
		params.Set("$metric", metric)
	}

	var response valueResponse
	if err := c.getJSON(ctx, path, params, &response); err != nil {
		return 0, err
	}
	points, err := response.points(responseValue)
	if err != nil {
		return 0, err
	}
	return sumMonthToDateDailyUsage(points, query)
}

func (c *Client) getJSON(ctx context.Context, path string, params url.Values, target any) error {
	requestURL := *c.baseURL
	requestURL.Path = path
	requestURL.RawQuery = params.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return fmt.Errorf("kodo: construct request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	response, err := c.doer.Do(request)
	if err != nil {
		return err
	}
	if response == nil || response.Body == nil {
		return fmt.Errorf("%w: nil HTTP response or body", ErrUnexpectedResponse)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return &HTTPStatusError{StatusCode: response.StatusCode}
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("%w: read body: %v", ErrUnexpectedResponse, err)
	}
	if len(body) > maxResponseBytes {
		return fmt.Errorf("%w: response exceeds %d bytes", ErrUnexpectedResponse, maxResponseBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
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

func commonParams(query Query) url.Values {
	return url.Values{
		"begin": {query.Begin.Format(qiniuTimeLayout)},
		"end":   {query.End.Format(qiniuTimeLayout)},
		"g":     {"5min"},
	}
}

func monthToDateParams(query MonthToDateQuery) url.Values {
	return url.Values{
		"begin": {query.Begin.Format(qiniuTimeLayout)},
		"end":   {query.End.Format(qiniuTimeLayout)},
		"g":     {"day"},
	}
}

func validateQuery(query Query) error {
	if query.Bucket == "" || strings.TrimSpace(query.Bucket) != query.Bucket {
		return fmt.Errorf("%w: bucket is required without surrounding whitespace", ErrInvalidInput)
	}
	if query.Region == "" || strings.TrimSpace(query.Region) != query.Region {
		return fmt.Errorf("%w: region is required without surrounding whitespace", ErrInvalidInput)
	}
	if query.Begin.IsZero() || query.End.IsZero() || query.SafeBefore.IsZero() {
		return fmt.Errorf("%w: begin, end, and safe-before times are required", ErrInvalidInput)
	}
	if !query.Begin.Before(query.End) {
		return fmt.Errorf("%w: begin must be before end", ErrInvalidInput)
	}
	if query.End.Sub(query.Begin) < 2*BucketWidth {
		return fmt.Errorf("%w: query window must cover at least two buckets", ErrInvalidInput)
	}
	if query.SafeBefore.After(query.End) {
		return fmt.Errorf("%w: safe-before must not be after end", ErrInvalidInput)
	}
	if query.Begin.UnixNano()%BucketWidth.Nanoseconds() != 0 || query.End.UnixNano()%BucketWidth.Nanoseconds() != 0 {
		return fmt.Errorf("%w: begin and end must align to five minutes", ErrInvalidInput)
	}
	return nil
}

func validateMonthToDateQuery(query MonthToDateQuery) error {
	if query.Bucket == "" || strings.TrimSpace(query.Bucket) != query.Bucket {
		return fmt.Errorf("%w: bucket is required without surrounding whitespace", ErrInvalidInput)
	}
	if query.Region == "" || strings.TrimSpace(query.Region) != query.Region {
		return fmt.Errorf("%w: region is required without surrounding whitespace", ErrInvalidInput)
	}
	if query.Begin.IsZero() || query.End.IsZero() {
		return fmt.Errorf("%w: begin and end times are required", ErrInvalidInput)
	}
	if query.Begin.Location().String() != query.End.Location().String() {
		return fmt.Errorf("%w: begin and end must use the same timezone", ErrInvalidInput)
	}
	beginYear, beginMonth, beginDay := query.Begin.Date()
	if beginDay != 1 || query.Begin.Hour() != 0 || query.Begin.Minute() != 0 || query.Begin.Second() != 0 || query.Begin.Nanosecond() != 0 {
		return fmt.Errorf("%w: begin must be the first day of the month at midnight", ErrInvalidInput)
	}
	if !query.Begin.Before(query.End) {
		return fmt.Errorf("%w: begin must be before end", ErrInvalidInput)
	}
	endYear, endMonth, _ := query.End.Date()
	if endYear != beginYear || endMonth != beginMonth {
		return fmt.Errorf("%w: end must be within the begin month", ErrInvalidInput)
	}
	if query.End.Minute()%5 != 0 || query.End.Second() != 0 || query.End.Nanosecond() != 0 {
		return fmt.Errorf("%w: end must align to five minutes", ErrInvalidInput)
	}
	return nil
}

type arrayResponse struct {
	Times []int64       `json:"times"`
	Datas []json.Number `json:"datas"`
}

func (r arrayResponse) points() ([]Point, error) {
	if r.Times == nil || r.Datas == nil {
		return nil, fmt.Errorf("%w: times and datas are required", ErrUnexpectedResponse)
	}
	if len(r.Times) != len(r.Datas) {
		return nil, ErrMismatchedArrays
	}
	points := make([]Point, len(r.Times))
	for i := range r.Times {
		value, err := parseCount(r.Datas[i])
		if err != nil {
			return nil, fmt.Errorf("%w: datas[%d] is invalid", ErrUnexpectedResponse, i)
		}
		points[i] = Point{Time: time.Unix(r.Times[i], 0).UTC(), Value: value}
	}
	return points, nil
}

type valueResponse []struct {
	Time   string                 `json:"time"`
	Values map[string]json.Number `json:"values"`
}

func (r valueResponse) points(valueName string) ([]Point, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: response array is required", ErrUnexpectedResponse)
	}
	points := make([]Point, len(r))
	for i, item := range r {
		when, err := time.Parse(time.RFC3339Nano, item.Time)
		if err != nil {
			return nil, fmt.Errorf("%w: point %d has invalid time", ErrUnexpectedResponse, i)
		}
		if item.Values == nil {
			return nil, fmt.Errorf("%w: point %d has no values", ErrUnexpectedResponse, i)
		}
		rawValue, ok := item.Values[valueName]
		if !ok {
			return nil, fmt.Errorf("%w: point %d has no %q value", ErrUnexpectedResponse, i, valueName)
		}
		value, err := parseCount(rawValue)
		if err != nil {
			return nil, fmt.Errorf("%w: point %d value %q is invalid", ErrUnexpectedResponse, i, valueName)
		}
		points[i] = Point{Time: when, Value: value}
	}
	return points, nil
}

func parseCount(number json.Number) (float64, error) {
	if number.String() == "" {
		return 0, fmt.Errorf("missing number")
	}
	value, err := strconv.ParseFloat(number.String(), 64)
	if err != nil {
		return 0, fmt.Errorf("invalid number")
	}
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || math.Trunc(value) != value {
		return 0, fmt.Errorf("expected a non-negative integer")
	}
	return value, nil
}
