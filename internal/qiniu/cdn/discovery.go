package cdn

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

const (
	DefaultDomainDiscoveryURL = "https://api.qiniu.com/domain"

	domainDiscoveryPageSize     = 1000
	maxDomainDiscoveryPages     = 100
	maxDomainDiscoveryResources = 10000
	maxDomainDiscoveryMarker    = 4096
)

// DomainDiscoveryClient lists CDN domains through Qiniu's read-only domain
// endpoint. Authentication, retries, and rate limiting belong to Doer.
type DomainDiscoveryClient struct {
	doer     Doer
	endpoint *url.URL
}

// Domain is one CDN resource returned by Qiniu's domain-list endpoint.
// Product is normalized to "cdn" when older responses omit it.
type Domain struct {
	Name           string `json:"name"`
	OperatingState string `json:"operatingState"`
	Product        string `json:"product"`
}

// NewDomainDiscoveryClient creates a read-only CDN domain discovery client.
// An empty endpoint selects DefaultDomainDiscoveryURL.
func NewDomainDiscoveryClient(doer Doer, endpoint string) (*DomainDiscoveryClient, error) {
	if doer == nil {
		return nil, fmt.Errorf("%w: nil HTTP doer", ErrInvalidInput)
	}
	if endpoint == "" {
		endpoint = DefaultDomainDiscoveryURL
	}

	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("%w: parse domain discovery URL: %v", ErrInvalidInput, err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("%w: domain discovery URL must be an absolute HTTP(S) URL", ErrInvalidInput)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "/domain" || parsed.EscapedPath() != "/domain" {
		return nil, fmt.Errorf("%w: domain discovery URL must contain only the /domain path", ErrInvalidInput)
	}

	return &DomainDiscoveryClient{doer: doer, endpoint: parsed}, nil
}

// ListDomains returns every unique CDN domain visible to the supplied
// credentials, including domains that are not currently active. DCDN resources
// are excluded because this exporter module targets the CDN APIs.
func (c *DomainDiscoveryClient) ListDomains(ctx context.Context) ([]Domain, error) {
	domains := make([]Domain, 0)
	seenDomains := make(map[string]struct{})
	seenMarkers := make(map[string]struct{})
	marker := ""

	for pageNumber := 1; pageNumber <= maxDomainDiscoveryPages; pageNumber++ {
		page, err := c.fetchDomainPage(ctx, marker)
		if err != nil {
			return nil, fmt.Errorf("cdn: discover domains page %d: %w", pageNumber, err)
		}
		if len(page.Domains) > domainDiscoveryPageSize {
			return nil, fmt.Errorf("%w: domain discovery page %d contains %d resources, limit is %d", ErrUnexpectedResponse, pageNumber, len(page.Domains), domainDiscoveryPageSize)
		}
		if len(seenDomains)+len(page.Domains) > maxDomainDiscoveryResources {
			return nil, fmt.Errorf("%w: domain discovery exceeds %d resources", ErrUnexpectedResponse, maxDomainDiscoveryResources)
		}

		for _, domain := range page.Domains {
			if err := validateDiscoveredDomain(domain.Name); err != nil {
				return nil, fmt.Errorf("%w: domain discovery page %d: %v", ErrUnexpectedResponse, pageNumber, err)
			}
			if err := validateOperatingState(domain.OperatingState); err != nil {
				return nil, fmt.Errorf("%w: domain discovery page %d: %v", ErrUnexpectedResponse, pageNumber, err)
			}
			if err := validateProduct(domain.Product); err != nil {
				return nil, fmt.Errorf("%w: domain discovery page %d: %v", ErrUnexpectedResponse, pageNumber, err)
			}

			key := strings.ToLower(domain.Name)
			if _, exists := seenDomains[key]; exists {
				return nil, fmt.Errorf("%w: duplicate discovered CDN domain %q", ErrUnexpectedResponse, domain.Name)
			}
			seenDomains[key] = struct{}{}
			// Older list responses omit product. Explicit DCDN entries are not
			// enrolled because the selected statistics endpoints are CDN APIs.
			if domain.Product == "" {
				domain.Product = "cdn"
			}
			if domain.Product != "dcdn" {
				domains = append(domains, domain)
			}
		}

		if page.Marker == "" {
			sort.Slice(domains, func(left, right int) bool {
				return domains[left].Name < domains[right].Name
			})
			return domains, nil
		}
		if err := validateDiscoveryMarker(page.Marker); err != nil {
			return nil, fmt.Errorf("%w: domain discovery page %d: %v", ErrUnexpectedResponse, pageNumber, err)
		}
		if len(page.Domains) == 0 {
			return nil, fmt.Errorf("%w: domain discovery page %d has a continuation marker but no resources", ErrUnexpectedResponse, pageNumber)
		}
		if page.Marker == marker {
			return nil, fmt.Errorf("%w: domain discovery marker did not advance", ErrUnexpectedResponse)
		}
		if _, exists := seenMarkers[page.Marker]; exists {
			return nil, fmt.Errorf("%w: repeated domain discovery marker", ErrUnexpectedResponse)
		}
		seenMarkers[page.Marker] = struct{}{}
		marker = page.Marker

		if len(seenDomains) == maxDomainDiscoveryResources {
			return nil, fmt.Errorf("%w: domain discovery exceeds %d resources", ErrUnexpectedResponse, maxDomainDiscoveryResources)
		}
	}

	return nil, fmt.Errorf("%w: domain discovery exceeds %d pages", ErrUnexpectedResponse, maxDomainDiscoveryPages)
}

type domainDiscoveryPage struct {
	Marker  string
	Domains []Domain
}

func (c *DomainDiscoveryClient) fetchDomainPage(ctx context.Context, marker string) (domainDiscoveryPage, error) {
	requestURL := *c.endpoint
	query := requestURL.Query()
	query.Set("limit", strconv.Itoa(domainDiscoveryPageSize))
	if marker != "" {
		query.Set("marker", marker)
	}
	requestURL.RawQuery = query.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return domainDiscoveryPage{}, fmt.Errorf("cdn: create domain discovery request: %w", err)
	}
	request.Header.Set("Accept", "application/json")

	response, err := c.doer.Do(request)
	if err != nil {
		return domainDiscoveryPage{}, fmt.Errorf("cdn: GET /domain: %w", err)
	}
	if response == nil || response.Body == nil {
		return domainDiscoveryPage{}, fmt.Errorf("%w: nil HTTP response or body", ErrUnexpectedResponse)
	}
	defer response.Body.Close()

	limited := io.LimitReader(response.Body, maxResponseBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return domainDiscoveryPage{}, fmt.Errorf("%w: read domain discovery response: %v", ErrUnexpectedResponse, err)
	}
	if len(body) > maxResponseBytes {
		return domainDiscoveryPage{}, fmt.Errorf("%w: response exceeds %d bytes", ErrUnexpectedResponse, maxResponseBytes)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return domainDiscoveryPage{}, &HTTPStatusError{StatusCode: response.StatusCode}
	}

	var rawPage struct {
		Marker  json.RawMessage `json:"marker"`
		Domains json.RawMessage `json:"domains"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&rawPage); err != nil {
		return domainDiscoveryPage{}, fmt.Errorf("%w: decode domain discovery JSON: %v", ErrUnexpectedResponse, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return domainDiscoveryPage{}, fmt.Errorf("%w: multiple JSON values", ErrUnexpectedResponse)
		}
		return domainDiscoveryPage{}, fmt.Errorf("%w: trailing domain discovery JSON: %v", ErrUnexpectedResponse, err)
	}

	if len(rawPage.Marker) == 0 || bytes.Equal(bytes.TrimSpace(rawPage.Marker), []byte("null")) {
		return domainDiscoveryPage{}, fmt.Errorf("%w: domain discovery response is missing marker", ErrUnexpectedResponse)
	}
	if len(rawPage.Domains) == 0 || bytes.Equal(bytes.TrimSpace(rawPage.Domains), []byte("null")) {
		return domainDiscoveryPage{}, fmt.Errorf("%w: domain discovery response is missing domains", ErrUnexpectedResponse)
	}

	var page domainDiscoveryPage
	if err := json.Unmarshal(rawPage.Marker, &page.Marker); err != nil {
		return domainDiscoveryPage{}, fmt.Errorf("%w: decode domain discovery marker: %v", ErrUnexpectedResponse, err)
	}
	if err := json.Unmarshal(rawPage.Domains, &page.Domains); err != nil {
		return domainDiscoveryPage{}, fmt.Errorf("%w: decode domain discovery domains: %v", ErrUnexpectedResponse, err)
	}
	return page, nil
}

func validateDiscoveredDomain(domain string) error {
	if err := validateDomain(domain, true); err != nil {
		return err
	}
	if len(domain) > 253 || strings.ContainsAny(domain, "/:?# ") {
		return fmt.Errorf("invalid discovered CDN domain %q", domain)
	}
	name := strings.TrimPrefix(domain, ".")
	labels := strings.Split(name, ".")
	if len(labels) < 2 {
		return fmt.Errorf("invalid discovered CDN domain %q", domain)
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("invalid discovered CDN domain %q", domain)
		}
		for _, character := range label {
			if (character < 'a' || character > 'z') &&
				(character < 'A' || character > 'Z') &&
				(character < '0' || character > '9') && character != '-' {
				return fmt.Errorf("invalid discovered CDN domain %q", domain)
			}
		}
	}
	return nil
}

func validateOperatingState(state string) error {
	switch state {
	case "processing", "success", "failed", "frozen", "offlined":
		return nil
	default:
		return fmt.Errorf("invalid operatingState %q", state)
	}
}

func validateProduct(product string) error {
	switch product {
	case "", "cdn", "dcdn":
		return nil
	default:
		return fmt.Errorf("invalid product %q", product)
	}
}

func validateDiscoveryMarker(marker string) error {
	if len(marker) > maxDomainDiscoveryMarker || strings.TrimSpace(marker) != marker || containsControl(marker) {
		return fmt.Errorf("invalid domain discovery marker")
	}
	return nil
}
