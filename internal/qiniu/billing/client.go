package billing

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
	DefaultBaseURL = "https://api.qiniu.com"

	balanceOverviewPath = "/billing-api/v1/account/balance-overview"
	billSnapshotPath    = "/billing-api/v2/bill/snapshot"
	resourcePackPath    = "/billing-api/v1/respack/month-overview"
	billDetailPath      = "/billing-api/v2/bill/detail"

	resourcePackPageSize = 200
	resourcePackMaxPages = 50
	maxResponseBytes     = 16 << 20
)

var (
	ErrNilDoer              = errors.New("billing: nil HTTP doer")
	ErrInvalidBaseURL       = errors.New("billing: invalid base URL")
	ErrMissingEnvelopeCode  = errors.New("billing: response envelope is missing code")
	ErrMissingEnvelopeData  = errors.New("billing: successful response envelope is missing data")
	ErrInvalidMonthlyPeriod = errors.New("billing: bill detail period must be one Asia/Shanghai calendar month")
	ErrPaginationLimit      = errors.New("billing: resource-pack pagination exceeds 50 pages")
)

// Doer is implemented by http.Client and by the shared signing transport.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// APIError is a Qiniu response envelope with a non-zero business code.
type APIError struct {
	Code    int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("billing: Qiniu API error code %d", e.Code)
}

// HTTPError reports a non-200 response without retaining a potentially
// sensitive response body.
type HTTPError struct {
	StatusCode int
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("billing: unexpected HTTP status %d", e.StatusCode)
}

// Client calls exactly the four read-only finance endpoints represented by
// its exported methods. Signing is intentionally delegated to the Doer.
type Client struct {
	doer    Doer
	baseURL *url.URL
}

// NewClient constructs a read-only finance client. An empty baseURL selects
// DefaultBaseURL; tests may provide an httptest server URL.
func NewClient(doer Doer, baseURL string) (*Client, error) {
	if doer == nil {
		return nil, ErrNilDoer
	}
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}

	parsed, err := url.Parse(baseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return nil, ErrInvalidBaseURL
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = ""
	return &Client{doer: doer, baseURL: parsed}, nil
}

// BalanceOverview fetches the current read-only account balance snapshot.
func (c *Client) BalanceOverview(ctx context.Context) (BalanceOverview, error) {
	var data balanceData
	if err := c.get(ctx, balanceOverviewPath, nil, &data); err != nil {
		return BalanceOverview{}, err
	}
	return data.value()
}

// BillSnapshot fetches the estimated aggregate cost for snapshotDate.
func (c *Client) BillSnapshot(ctx context.Context, snapshotDate time.Time) (BillSnapshot, error) {
	query := url.Values{"date": []string{formatBillingTime(snapshotDate)}}
	var data billSummaryData
	if err := c.get(ctx, billSnapshotPath, query, &data); err != nil {
		return BillSnapshot{}, err
	}
	if err := data.validate(); err != nil {
		return BillSnapshot{}, err
	}
	return BillSnapshot{TotalMoney: *data.TotalMoney, Currency: data.Currency}, nil
}

// ResourcePackMonthOverview fetches every page of the current-month resource
// package overview. It returns no partial results if any page fails.
func (c *Client) ResourcePackMonthOverview(ctx context.Context) ([]ResourcePackMonthOverview, error) {
	all := make([]ResourcePackMonthOverview, 0)
	for page := 1; page <= resourcePackMaxPages; page++ {
		query := url.Values{
			"page":      []string{fmt.Sprintf("%d", page)},
			"page_size": []string{fmt.Sprintf("%d", resourcePackPageSize)},
		}
		var items []ResourcePackMonthOverview
		if err := c.get(ctx, resourcePackPath, query, &items); err != nil {
			return nil, err
		}
		all = append(all, items...)
		if len(items) < resourcePackPageSize {
			return all, nil
		}
	}
	return nil, ErrPaginationLimit
}

// BillDetail fetches the aggregate finalized cost for exactly one Shanghai
// calendar month.
func (c *Client) BillDetail(ctx context.Context, period BillingPeriod) (BillDetail, error) {
	if !validMonthlyPeriod(period) {
		return BillDetail{}, ErrInvalidMonthlyPeriod
	}
	query := url.Values{
		"start": []string{formatBillingTime(period.Start)},
		"end":   []string{formatBillingTime(period.End)},
	}
	var data billSummaryData
	if err := c.get(ctx, billDetailPath, query, &data); err != nil {
		return BillDetail{}, err
	}
	if err := data.validate(); err != nil {
		return BillDetail{}, err
	}
	return BillDetail{TotalMoney: *data.TotalMoney, Currency: data.Currency}, nil
}

func (c *Client) get(ctx context.Context, path string, query url.Values, dst any) error {
	endpoint := *c.baseURL
	endpoint.Path = c.baseURL.Path + path
	endpoint.RawPath = ""
	endpoint.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return fmt.Errorf("billing: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.doer.Do(req)
	if err != nil {
		return fmt.Errorf("billing: perform request: %w", err)
	}
	if resp == nil {
		return errors.New("billing: HTTP doer returned a nil response")
	}
	if resp.Body == nil {
		return errors.New("billing: HTTP response has a nil body")
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return &HTTPError{StatusCode: resp.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("billing: read response: %w", err)
	}
	if len(body) > maxResponseBytes {
		return fmt.Errorf("billing: response exceeds %d bytes", maxResponseBytes)
	}
	if err := decodeEnvelope(bytes.NewReader(body), dst); err != nil {
		return fmt.Errorf("billing: decode response: %w", err)
	}
	return nil
}

type responseEnvelope struct {
	Code    *int            `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func decodeEnvelope(reader io.Reader, dst any) error {
	decoder := json.NewDecoder(reader)
	var envelope responseEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		return err
	}
	if envelope.Code == nil {
		return ErrMissingEnvelopeCode
	}
	if *envelope.Code != 0 {
		return &APIError{Code: *envelope.Code, Message: envelope.Message}
	}
	if len(envelope.Data) == 0 || bytes.Equal(bytes.TrimSpace(envelope.Data), []byte("null")) {
		return ErrMissingEnvelopeData
	}
	if err := json.Unmarshal(envelope.Data, dst); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("billing: response contains trailing JSON data")
	}
	return nil
}

type balanceData struct {
	AvailableBalance    *Fixed8 `json:"available_balance"`
	Balance             *Fixed8 `json:"balance"`
	CashBalance         *Fixed8 `json:"cash_balance"`
	PresentBalance      *Fixed8 `json:"present_balance"`
	CreditLine          *Fixed8 `json:"credit_line"`
	UnpaidMoney         *Fixed8 `json:"unpaid_money"`
	EstimatedBillsMoney *Fixed8 `json:"estimated_bills_money"`
	Currency            string  `json:"currency"`
}

func (data balanceData) value() (BalanceOverview, error) {
	if data.AvailableBalance == nil && data.Balance == nil {
		return BalanceOverview{}, errors.New("billing: balance response is missing available_balance/balance")
	}
	if data.AvailableBalance != nil && data.Balance != nil && *data.AvailableBalance != *data.Balance {
		return BalanceOverview{}, errors.New("billing: available_balance and balance conflict")
	}
	if data.UnpaidMoney == nil || data.Currency == "" {
		return BalanceOverview{}, errors.New("billing: balance response is missing required fields")
	}

	available := data.AvailableBalance
	if available == nil {
		available = data.Balance
	}
	return BalanceOverview{
		AvailableBalance:    *available,
		CashBalance:         fixed8OrZero(data.CashBalance),
		PresentBalance:      fixed8OrZero(data.PresentBalance),
		CreditLine:          fixed8OrZero(data.CreditLine),
		UnpaidMoney:         *data.UnpaidMoney,
		EstimatedBillsMoney: fixed8OrZero(data.EstimatedBillsMoney),
		Currency:            data.Currency,
	}, nil
}

func fixed8OrZero(value *Fixed8) Fixed8 {
	if value == nil {
		return 0
	}
	return *value
}

type billSummaryData struct {
	TotalMoney *Fixed8 `json:"total_money"`
	Currency   string  `json:"currency"`
}

func (data billSummaryData) validate() error {
	if data.TotalMoney == nil || data.Currency == "" {
		return errors.New("billing: bill response is missing required fields")
	}
	return nil
}
