package app

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"qiniu-exporter/internal/collector"
	"qiniu-exporter/internal/poller"
	"qiniu-exporter/internal/qiniu/cdn"
	"qiniu-exporter/internal/snapshot"
	"qiniu-exporter/internal/telemetry"
)

type cdnPartialErrors map[string]error

const cdnMonitoringIsolationAttemptLimit = 16

var errCDNMonitoringIsolationBudget = errors.New("cdn: monitoring isolation attempt budget exhausted")

func (e cdnPartialErrors) Error() string {
	return fmt.Sprintf("cdn: collection failed for %d configured domains", len(e))
}

func (e cdnPartialErrors) ErrorFor(resource string) error { return e[resource] }

func RegisterCDN(
	scheduler *poller.Scheduler,
	client *cdn.Client,
	domains []string,
	statisticsTimezoneVerified bool,
	monitoringUnitsVerified bool,
	sourceLag time.Duration,
	staleAfter time.Duration,
	stores collector.CDNStores,
	metrics *telemetry.Metrics,
) error {
	if !statisticsTimezoneVerified {
		metrics.ObserveSkipped("cdn/analytics", "timezone_unverified")
		metrics.ObserveSkipped("cdn/monitoring", "timezone_unverified")
		return nil
	}
	metrics.InitResources("cdn", "analytics", domains, staleAfter)
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return fmt.Errorf("load CDN statistics timezone: %w", err)
	}

	badDomains := make(map[string]error)
	var badDomainsMu sync.RWMutex
	if monitoringUnitsVerified {
		metrics.InitResources("cdn", "monitoring", domains, staleAfter)
		for batchNumber, batch := range batches(domains, 50) {
			batch := append([]string(nil), batch...)
			if err := scheduler.Add(poller.Job{
				Name:      "cdn/monitoring",
				Resources: batch,
				PhaseKey:  fmt.Sprintf("cdn/monitoring/%d/%s", batchNumber, batch[0]),
				Interval:  5 * time.Minute,
				Timeout:   2 * time.Minute,
				Run: func(ctx context.Context) error {
					failed := collectCDNMonitoring(ctx, client, batch, sourceLag, staleAfter, location, stores.Monitoring, metrics, &badDomainsMu, badDomains)
					if len(failed) > 0 {
						return failed
					}
					return nil
				},
			}); err != nil {
				return err
			}
		}
	} else {
		metrics.ObserveSkipped("cdn/monitoring", "units_unverified")
	}

	for _, configuredDomain := range domains {
		domain := configuredDomain
		if err := scheduler.Add(poller.Job{
			Name:     "cdn/analytics",
			Resource: domain,
			PhaseKey: "cdn/analytics/" + domain,
			Interval: 5 * time.Minute,
			Timeout:  time.Minute,
			Run: func(ctx context.Context) error {
				return collectCDNAnalytics(ctx, client, domain, sourceLag, staleAfter, location, stores.Analytics, metrics, &badDomainsMu, badDomains)
			},
		}); err != nil {
			return err
		}
	}
	return nil
}

func collectCDNAnalytics(
	ctx context.Context,
	client *cdn.Client,
	domain string,
	sourceLag, staleAfter time.Duration,
	location *time.Location,
	store *snapshot.ResourceStore[collector.CDNAnalyticsSnapshot],
	metrics *telemetry.Metrics,
	badMu *sync.RWMutex,
	bad map[string]error,
) error {
	badMu.RLock()
	cachedError := bad[domain]
	badMu.RUnlock()
	if cachedError != nil {
		return cachedError
	}

	safeBefore, date := cdnSafeWindow(time.Now(), sourceLag, location)
	regionalQuery := cdn.RegionalDomainQuery{
		DomainQuery: cdn.DomainQuery{Domain: domain, StartDate: date, EndDate: date},
		Region:      cdn.RegionGlobal,
	}
	requestsResponse, err := client.FetchRequestCount(ctx, regionalQuery)
	if err != nil {
		return cacheInvalidCDNDomain(err, domain, badMu, bad)
	}
	requests, err := cdn.SelectLatestSafeRequestRate5Min(requestsResponse, domain, cdn.RegionGlobal, safeBefore, location)
	if err != nil {
		return err
	}
	statusResponse, err := client.FetchStatusCodes(ctx, regionalQuery)
	if err != nil {
		return cacheInvalidCDNDomain(err, domain, badMu, bad)
	}
	statuses, err := cdn.SelectLatestSafeStatusCodeRates5Min(statusResponse, domain, cdn.RegionGlobal, safeBefore, location)
	if err != nil {
		return err
	}
	statusBucketEnd, err := cdn.SelectLatestSafeAnalyticsBucketEnd5Min(statusResponse.Data.Points, safeBefore, location)
	if err != nil {
		return err
	}
	cacheResponse, err := client.FetchHitMiss(ctx, regionalQuery.DomainQuery)
	if err != nil {
		return cacheInvalidCDNDomain(err, domain, badMu, bad)
	}
	cache, err := cdn.SelectLatestSafeCache5Min(cacheResponse, domain, safeBefore, location)
	if err != nil {
		return err
	}

	bucketEnds := make([]time.Time, 0, len(statuses)+3)
	bucketEnds = append(bucketEnds, requests.BucketEnd, statusBucketEnd)
	for _, status := range statuses {
		bucketEnds = append(bucketEnds, status.BucketEnd)
	}
	bucketEnds = append(bucketEnds, cache.BucketEnd)
	dataAt, err := matchingCDNBucketEnd("analytics", bucketEnds...)
	if err != nil {
		return err
	}
	store.Publish(domain, collector.CDNAnalyticsSnapshot{
		Requests: requests, Statuses: statuses, Cache: cache,
	}, snapshot.Meta{CollectedAt: time.Now(), DataAt: dataAt, StaleAfter: staleAfter})
	metrics.SetResourceDataTimestamp("cdn", "analytics", domain, dataAt)
	return nil
}

func cacheInvalidCDNDomain(err error, domain string, badMu *sync.RWMutex, bad map[string]error) error {
	var apiError *cdn.APIError
	if errors.As(err, &apiError) && apiError.Code == 400032 {
		badMu.Lock()
		bad[domain] = err
		badMu.Unlock()
	}
	return err
}

func collectCDNMonitoring(
	ctx context.Context,
	client *cdn.Client,
	domains []string,
	sourceLag, staleAfter time.Duration,
	location *time.Location,
	store *snapshot.ResourceStore[collector.CDNMonitoringSnapshot],
	metrics *telemetry.Metrics,
	badMu *sync.RWMutex,
	bad map[string]error,
) cdnPartialErrors {
	failed := make(cdnPartialErrors)
	active := make([]string, 0, len(domains))
	badMu.RLock()
	for _, domain := range domains {
		if err, exists := bad[domain]; exists {
			failed[domain] = err
		} else {
			active = append(active, domain)
		}
	}
	badMu.RUnlock()
	if len(active) == 0 {
		return failed
	}

	attempts := 0
	var collect func([]string)
	collect = func(current []string) {
		if attempts >= cdnMonitoringIsolationAttemptLimit {
			for _, domain := range current {
				failed[domain] = errCDNMonitoringIsolationBudget
			}
			return
		}
		attempts++
		values, dataAt, err := fetchCDNMonitoring(ctx, client, current, sourceLag, location)
		if err == nil {
			for domain, value := range values {
				store.Publish(domain, value, snapshot.Meta{CollectedAt: time.Now(), DataAt: dataAt, StaleAfter: staleAfter})
				metrics.SetResourceDataTimestamp("cdn", "monitoring", domain, dataAt)
			}
			return
		}
		var apiError *cdn.APIError
		if errors.As(err, &apiError) && apiError.Code == 400032 && len(current) > 1 {
			middle := len(current) / 2
			collect(current[:middle])
			collect(current[middle:])
			return
		}
		for _, domain := range current {
			failed[domain] = err
		}
		if errors.As(err, &apiError) && apiError.Code == 400032 && len(current) == 1 {
			badMu.Lock()
			bad[current[0]] = err
			badMu.Unlock()
		}
	}
	collect(active)
	return failed
}

func fetchCDNMonitoring(
	ctx context.Context,
	client *cdn.Client,
	domains []string,
	sourceLag time.Duration,
	location *time.Location,
) (map[string]collector.CDNMonitoringSnapshot, time.Time, error) {
	safeBefore, date := cdnSafeWindow(time.Now(), sourceLag, location)
	query := cdn.MonitoringQuery{Domains: domains, StartDate: date, EndDate: date}
	bandwidthResponse, err := client.FetchMonitoringBandwidth(ctx, query)
	if err != nil {
		return nil, time.Time{}, err
	}
	bandwidth, err := cdn.SelectLatestSafeBandwidth5Min(bandwidthResponse, domains, safeBefore, location)
	if err != nil {
		return nil, time.Time{}, err
	}
	flowResponse, err := client.FetchMonitoringFlow(ctx, query)
	if err != nil {
		return nil, time.Time{}, err
	}
	traffic, err := cdn.SelectLatestSafeTraffic5Min(flowResponse, domains, safeBefore, location)
	if err != nil {
		return nil, time.Time{}, err
	}

	result := make(map[string]collector.CDNMonitoringSnapshot, len(domains))
	bucketEnds := make([]time.Time, 0, len(bandwidth)+len(traffic))
	for _, sample := range bandwidth {
		value := result[sample.Domain]
		value.Bandwidth = append(value.Bandwidth, sample)
		result[sample.Domain] = value
		bucketEnds = append(bucketEnds, sample.BucketEnd)
	}
	for _, sample := range traffic {
		value := result[sample.Domain]
		value.Traffic = append(value.Traffic, sample)
		result[sample.Domain] = value
		bucketEnds = append(bucketEnds, sample.BucketEnd)
	}
	dataAt, err := matchingCDNBucketEnd("monitoring", bucketEnds...)
	if err != nil {
		return nil, time.Time{}, err
	}
	return result, dataAt, nil
}

func matchingCDNBucketEnd(dataset string, bucketEnds ...time.Time) (time.Time, error) {
	if len(bucketEnds) == 0 || bucketEnds[0].IsZero() {
		return time.Time{}, fmt.Errorf("%w: %s response has no selected bucket", cdn.ErrSeriesMisaligned, dataset)
	}
	selected := bucketEnds[0]
	for _, bucketEnd := range bucketEnds[1:] {
		if bucketEnd.IsZero() || !bucketEnd.Equal(selected) {
			return time.Time{}, fmt.Errorf("%w: %s responses selected different bucket ends", cdn.ErrSeriesMisaligned, dataset)
		}
	}
	return selected, nil
}

func cdnSafeWindow(now time.Time, sourceLag time.Duration, location *time.Location) (time.Time, string) {
	safeBefore := now.In(location).Add(-sourceLag).Truncate(cdn.FiveMinuteBucket)
	return safeBefore, safeBefore.Format("2006-01-02")
}

func batches(values []string, size int) [][]string {
	result := make([][]string, 0, (len(values)+size-1)/size)
	for start := 0; start < len(values); start += size {
		end := min(start+size, len(values))
		result = append(result, values[start:end])
	}
	return result
}
