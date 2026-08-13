package cdn

import (
	"fmt"
	"math"
	"net"
	"sort"
)

const (
	TopIPLimit            = 10
	maxTopIPResponseItems = 100
)

type TopIPValue struct {
	IP    string
	Value float64
}

type TopIPAggregate struct {
	Traffic  []TopIPValue
	Requests []TopIPValue
}

// MergeTopIPResponses sums duplicate IPs across domain batches and retains the
// ten largest values. Each upstream response is already truncated to its own
// Top 100, so the merged account result is intentionally approximate.
func MergeTopIPResponses(trafficResponses, requestResponses []TopIPResponse) (TopIPAggregate, error) {
	traffic, err := mergeTopIPValues(trafficResponses, func(data TopIPData) []float64 { return data.Traffic })
	if err != nil {
		return TopIPAggregate{}, fmt.Errorf("merge top IP traffic: %w", err)
	}
	requests, err := mergeTopIPValues(requestResponses, func(data TopIPData) []float64 { return data.Count })
	if err != nil {
		return TopIPAggregate{}, fmt.Errorf("merge top IP requests: %w", err)
	}
	return TopIPAggregate{Traffic: traffic, Requests: requests}, nil
}

func mergeTopIPValues(responses []TopIPResponse, values func(TopIPData) []float64) ([]TopIPValue, error) {
	totals := make(map[string]float64)
	for _, response := range responses {
		series := values(response.Data)
		if len(response.Data.IPs) > maxTopIPResponseItems {
			return nil, fmt.Errorf("%w: top IP response exceeds %d entries", ErrUnexpectedResponse, maxTopIPResponseItems)
		}
		if len(response.Data.IPs) != len(series) {
			return nil, fmt.Errorf("%w: top IP identity and value lengths differ", ErrSeriesMisaligned)
		}
		for index, ip := range response.Data.IPs {
			parsedIP := net.ParseIP(ip)
			if parsedIP == nil {
				return nil, fmt.Errorf("%w: invalid top IP address", ErrUnexpectedResponse)
			}
			ip = parsedIP.String()
			value := series[index]
			if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
				return nil, fmt.Errorf("%w: invalid top IP value", ErrInvalidValue)
			}
			total := totals[ip] + value
			if math.IsInf(total, 0) {
				return nil, fmt.Errorf("%w: top IP aggregate overflow", ErrInvalidValue)
			}
			totals[ip] = total
		}
	}

	result := make([]TopIPValue, 0, len(totals))
	for ip, value := range totals {
		result = append(result, TopIPValue{IP: ip, Value: value})
	}
	sort.Slice(result, func(left, right int) bool {
		if result[left].Value == result[right].Value {
			return result[left].IP < result[right].IP
		}
		return result[left].Value > result[right].Value
	})
	if len(result) > TopIPLimit {
		result = result[:TopIPLimit]
	}
	return result, nil
}
