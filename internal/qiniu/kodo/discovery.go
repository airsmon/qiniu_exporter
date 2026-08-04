package kodo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

const (
	DefaultDiscoveryBaseURL = "https://uc.qiniuapi.com"
	BucketsPath             = "/buckets"

	bucketDiscoveryPageSize  = 100
	maxDiscoveredBuckets     = 200
	maxBucketDiscoveryMarker = 4096
)

// Bucket identifies one Kodo bucket and its storage region.
type Bucket struct {
	Name   string `json:"name"`
	Region string `json:"region"`
}

// DiscoveryClient calls only Qiniu's read-only paginated bucket-list API.
// Authentication, retries, and rate limiting belong to Doer.
type DiscoveryClient struct {
	doer    Doer
	baseURL *url.URL
}

// NewDiscoveryClient creates a Kodo discovery client. An empty baseURL uses
// the public Qiniu UC endpoint.
func NewDiscoveryClient(doer Doer, baseURL string) (*DiscoveryClient, error) {
	if doer == nil {
		return nil, fmt.Errorf("%w: discovery doer is required", ErrInvalidInput)
	}
	if baseURL == "" {
		baseURL = DefaultDiscoveryBaseURL
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("%w: parse discovery base URL: %v", ErrInvalidInput, err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("%w: discovery base URL must be an absolute HTTP URL", ErrInvalidInput)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, fmt.Errorf("%w: discovery base URL must not contain credentials, path, query, or fragment", ErrInvalidInput)
	}
	parsed.Path = ""
	return &DiscoveryClient{doer: doer, baseURL: parsed}, nil
}

// ListBuckets returns every visible bucket and region. It uses the same v4
// paginated listing API as the official Qiniu Go SDK, avoiding a per-bucket
// region lookup.
func (c *DiscoveryClient) ListBuckets(ctx context.Context) ([]Bucket, error) {
	buckets := make([]Bucket, 0)
	seenBuckets := make(map[string]struct{})
	seenMarkers := make(map[string]struct{})
	marker := ""

	for {
		params := url.Values{
			"apiVersion": {"v4"},
			"limit":      {strconv.Itoa(bucketDiscoveryPageSize)},
		}
		if marker != "" {
			params.Set("marker", marker)
		}
		var page bucketDiscoveryPage
		if err := c.getJSON(ctx, params, &page); err != nil {
			return nil, fmt.Errorf("list Kodo buckets: %w", err)
		}
		if page.IsTruncated == nil || page.Buckets == nil {
			return nil, fmt.Errorf("%w: bucket page is missing required fields", ErrUnexpectedResponse)
		}
		if len(page.Buckets) > bucketDiscoveryPageSize {
			return nil, fmt.Errorf("%w: bucket page exceeds %d resources", ErrUnexpectedResponse, bucketDiscoveryPageSize)
		}
		if len(buckets)+len(page.Buckets) > maxDiscoveredBuckets {
			return nil, fmt.Errorf("%w: bucket discovery exceeds %d resources", ErrUnexpectedResponse, maxDiscoveredBuckets)
		}
		for index, bucket := range page.Buckets {
			if !validResourceIdentifier(bucket.Name) || !validResourceIdentifier(bucket.Region) {
				return nil, fmt.Errorf("%w: bucket %d has an invalid name or region", ErrUnexpectedResponse, index)
			}
			if _, exists := seenBuckets[bucket.Name]; exists {
				return nil, fmt.Errorf("%w: bucket discovery contains duplicate bucket %q", ErrUnexpectedResponse, bucket.Name)
			}
			seenBuckets[bucket.Name] = struct{}{}
			buckets = append(buckets, bucket)
		}

		if !*page.IsTruncated {
			slices.SortFunc(buckets, func(left, right Bucket) int {
				if result := strings.Compare(left.Name, right.Name); result != 0 {
					return result
				}
				return strings.Compare(left.Region, right.Region)
			})
			return buckets, nil
		}
		if len(page.Buckets) == 0 || !validDiscoveryMarker(page.NextMarker) {
			return nil, fmt.Errorf("%w: truncated bucket page has an invalid continuation marker", ErrUnexpectedResponse)
		}
		if page.NextMarker == marker {
			return nil, fmt.Errorf("%w: bucket discovery marker did not advance", ErrUnexpectedResponse)
		}
		if _, exists := seenMarkers[page.NextMarker]; exists {
			return nil, fmt.Errorf("%w: bucket discovery marker repeated", ErrUnexpectedResponse)
		}
		seenMarkers[page.NextMarker] = struct{}{}
		marker = page.NextMarker
		if len(buckets) == maxDiscoveredBuckets {
			return nil, fmt.Errorf("%w: bucket discovery exceeds %d resources", ErrUnexpectedResponse, maxDiscoveredBuckets)
		}
	}
}

type bucketDiscoveryPage struct {
	NextMarker  string   `json:"next_marker"`
	IsTruncated *bool    `json:"is_truncated"`
	Buckets     []Bucket `json:"buckets"`
}

func (c *DiscoveryClient) getJSON(ctx context.Context, params url.Values, target any) error {
	requestURL := *c.baseURL
	requestURL.Path = BucketsPath
	requestURL.RawQuery = params.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return fmt.Errorf("kodo discovery: construct request: %w", err)
	}
	request.Header.Set("Accept", "application/json")

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
		return fmt.Errorf("%w: read discovery body: %v", ErrUnexpectedResponse, err)
	}
	if len(body) > maxResponseBytes {
		return fmt.Errorf("%w: discovery response exceeds %d bytes", ErrUnexpectedResponse, maxResponseBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("%w: decode discovery JSON: %v", ErrUnexpectedResponse, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("%w: multiple discovery JSON values", ErrUnexpectedResponse)
		}
		return fmt.Errorf("%w: trailing discovery JSON: %v", ErrUnexpectedResponse, err)
	}
	return nil
}

func validResourceIdentifier(value string) bool {
	return value != "" && !strings.Contains(value, "/") && strings.IndexFunc(value, func(character rune) bool {
		return unicode.IsSpace(character) || unicode.IsControl(character)
	}) < 0
}

func validDiscoveryMarker(marker string) bool {
	return marker != "" && len(marker) <= maxBucketDiscoveryMarker && strings.TrimSpace(marker) == marker && strings.IndexFunc(marker, unicode.IsControl) < 0
}
