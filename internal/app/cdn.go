package app

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"sync"
	"time"

	"qiniu-exporter/internal/collector"
	"qiniu-exporter/internal/config"
	"qiniu-exporter/internal/poller"
	"qiniu-exporter/internal/qiniu/cdn"
	"qiniu-exporter/internal/snapshot"
	"qiniu-exporter/internal/telemetry"
)

type CDNDiscoverer interface {
	ListDomains(context.Context) ([]cdn.Domain, error)
}

type cdnPartialErrors map[string]error

const cdnMonitoringIsolationAttemptLimit = 16

const cdnBandwidthBackfillDays = 3

var errCDNMonitoringIsolationBudget = errors.New("cdn: monitoring isolation attempt budget exhausted")

type cdnMonitoringWindow struct {
	SafeBefore time.Time
	TodayStart time.Time
	HourStart  time.Time
	HourEnd    time.Time
	StartDate  string
	EndDate    string
}

type cdnUsageRound struct {
	Bandwidth []cdn.UsageBatch
	Traffic   []cdn.UsageBatch
	DataEnd   time.Time
}

type cdnBandwidthCache struct {
	MonthStart time.Time
	PeriodEnd  time.Time
	Domains    []string
	Aggregate  cdn.BandwidthUsageAggregate
}

func (e cdnPartialErrors) Error() string {
	return fmt.Sprintf("cdn: collection failed for %d configured domains", len(e))
}

func (e cdnPartialErrors) ErrorFor(resource string) error { return e[resource] }

func RegisterCDN(
	scheduler *poller.Scheduler,
	client *cdn.Client,
	discoverer CDNDiscoverer,
	cfg *config.Config,
	stores collector.CDNStores,
	metrics *telemetry.Metrics,
) error {
	if discoverer == nil {
		return fmt.Errorf("CDN discoverer is required")
	}
	if stores.Inventory == nil {
		return fmt.Errorf("CDN inventory store is required")
	}
	if stores.Usage == nil {
		return fmt.Errorf("CDN usage store is required")
	}
	if !cfg.CDN.StatisticsTimezoneVerified {
		metrics.ObserveSkipped("cdn/analytics", "timezone_unverified")
		metrics.ObserveSkipped("cdn/monitoring", "timezone_unverified")
		metrics.ObserveSkipped("cdn/usage", "timezone_unverified")
	}
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return fmt.Errorf("load CDN statistics timezone: %w", err)
	}

	badDomains := make(map[string]error)
	var badDomainsMu sync.RWMutex
	catalog := newResourceCatalog[string](nil)
	var usageScopeMu sync.Mutex
	bandwidthCache := &cdnBandwidthCache{}
	reconcile := func(ctx context.Context) error {
		domainInventory, err := discoverer.ListDomains(ctx)
		if err != nil {
			return err
		}
		domains := make([]string, 0, len(domainInventory))
		for _, domain := range domainInventory {
			if domain.OperatingState == "success" {
				domains = append(domains, domain.Name)
			}
		}
		sort.Strings(domains)
		if err := cfg.ValidateCDNResourceCounts(len(domainInventory), len(domains)); err != nil {
			return err
		}
		stores.Monitoring.Retain(domains)
		stores.Analytics.Retain(domains)
		stores.Inventory.Publish(domainInventory, snapshot.Meta{
			CollectedAt: time.Now(),
		})
		badDomainsMu.Lock()
		clear(badDomains)
		badDomainsMu.Unlock()
		if cfg.CDN.StatisticsTimezoneVerified {
			metrics.ReplaceResources("cdn", "analytics", domains, cfg.Collection.StaleAfter.Realtime.Value())
			if cfg.CDN.MonitoringUnitsVerified {
				metrics.ReplaceResources("cdn", "monitoring", domains, cfg.Collection.StaleAfter.Realtime.Value())
			}
		}
		usageScopeMu.Lock()
		scopeChanged := !slices.Equal(catalog.Snapshot(), domains)
		if scopeChanged {
			stores.Usage.Clear()
			if cfg.CDN.StatisticsTimezoneVerified && cfg.CDN.MonitoringUnitsVerified {
				metrics.InitCollector("cdn", "usage")
			}
		}
		catalog.Replace(domains)
		usageScopeMu.Unlock()
		return nil
	}

	metrics.InitCollector("cdn", "discovery")
	if err := scheduler.Add(poller.Job{
		Name: "cdn/discovery", PhaseKey: "cdn/discovery", Interval: cfg.Collection.Intervals.Discovery.Value(),
		Timeout: discoveryTimeout(cfg.Collection.Intervals.Discovery.Value()), RunOnStart: true, Run: reconcile,
	}); err != nil {
		return err
	}
	if !cfg.CDN.StatisticsTimezoneVerified {
		return nil
	}
	if !cfg.CDN.MonitoringUnitsVerified {
		metrics.ObserveSkipped("cdn/monitoring", "units_unverified")
		metrics.ObserveSkipped("cdn/usage", "units_unverified")
	} else if err := scheduler.Add(poller.Job{
		Name: "cdn/monitoring", PhaseKey: "cdn/monitoring", Interval: cfg.Collection.Intervals.CDNMonitoring.Value(),
		Timeout: collectionTimeout(cfg.Collection.Intervals.CDNMonitoring.Value()),
		RunResources: func(ctx context.Context) ([]string, error) {
			domains := catalog.Snapshot()
			if len(domains) == 0 {
				metrics.ObserveSkipped("cdn/usage", "no_resources")
				return nil, poller.Skip("no_resources")
			}
			resources := append([]string(nil), domains...)
			failed := make(cdnPartialErrors)
			window := newCDNMonitoringWindow(time.Now(), cfg.Collection.SourceLag.Value(), location)
			usageRound := cdnUsageRound{}
			usageStarted := time.Now()
			for _, batch := range batches(domains, 50) {
				batchFailed := collectCDNMonitoring(ctx, client, batch, window, cfg.Collection.StaleAfter.Realtime.Value(), location, stores.Monitoring, metrics, &badDomainsMu, badDomains, &usageRound)
				for domain, err := range batchFailed {
					failed[domain] = err
				}
			}
			usageErr := collectAndPublishCDNUsage(
				ctx, client, usageRound, failed, resources, catalog, window, location,
				cfg.Collection.StaleAfter.Realtime.Value(), stores.Usage, bandwidthCache, &usageScopeMu, metrics,
			)
			metrics.ObserveJob("cdn/usage", time.Since(usageStarted), usageErr)
			if len(failed) > 0 {
				return resources, failed
			}
			return resources, nil
		},
	}); err != nil {
		return err
	}
	if cfg.CDN.MonitoringUnitsVerified {
		metrics.InitCollector("cdn", "usage")
		metrics.SetCollectorStaleAfter("cdn", "usage", cfg.Collection.StaleAfter.Realtime.Value())
	}

	if err := scheduler.Add(poller.Job{
		Name: "cdn/analytics", PhaseKey: "cdn/analytics", Interval: cfg.Collection.Intervals.CDNAnalytics.Value(),
		Timeout: collectionTimeout(cfg.Collection.Intervals.CDNAnalytics.Value()),
		RunResources: func(ctx context.Context) ([]string, error) {
			domains := catalog.Snapshot()
			if len(domains) == 0 {
				return nil, poller.Skip("no_resources")
			}
			resources := append([]string(nil), domains...)
			failed := make(cdnPartialErrors)
			for _, domain := range domains {
				if err := collectCDNAnalytics(ctx, client, domain, cfg.Collection.SourceLag.Value(), cfg.Collection.StaleAfter.Realtime.Value(), location, stores.Analytics, metrics, &badDomainsMu, badDomains); err != nil {
					failed[domain] = err
				}
			}
			if len(failed) > 0 {
				return resources, failed
			}
			return resources, nil
		},
	}); err != nil {
		return err
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
	window cdnMonitoringWindow,
	staleAfter time.Duration,
	location *time.Location,
	store *snapshot.ResourceStore[collector.CDNMonitoringSnapshot],
	metrics *telemetry.Metrics,
	badMu *sync.RWMutex,
	bad map[string]error,
	usageRound *cdnUsageRound,
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
		values, dataAt, bandwidthResponse, flowResponse, err := fetchCDNMonitoring(ctx, client, current, window, location)
		if err == nil {
			for domain, value := range values {
				store.Publish(domain, value, snapshot.Meta{CollectedAt: time.Now(), DataAt: dataAt, StaleAfter: staleAfter})
				metrics.SetResourceDataTimestamp("cdn", "monitoring", domain, dataAt)
			}
			if usageRound != nil {
				requested := append([]string(nil), current...)
				usageRound.Bandwidth = append(usageRound.Bandwidth, cdn.UsageBatch{Domains: requested, Response: bandwidthResponse})
				usageRound.Traffic = append(usageRound.Traffic, cdn.UsageBatch{Domains: requested, Response: flowResponse})
				if usageRound.DataEnd.IsZero() || dataAt.Before(usageRound.DataEnd) {
					usageRound.DataEnd = dataAt
				}
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
	window cdnMonitoringWindow,
	location *time.Location,
) (map[string]collector.CDNMonitoringSnapshot, time.Time, cdn.MonitoringResponse, cdn.MonitoringResponse, error) {
	query := cdn.MonitoringQuery{Domains: domains, StartDate: window.StartDate, EndDate: window.EndDate}
	bandwidthResponse, err := client.FetchMonitoringBandwidth(ctx, query)
	if err != nil {
		return nil, time.Time{}, cdn.MonitoringResponse{}, cdn.MonitoringResponse{}, err
	}
	bandwidth, err := cdn.SelectLatestSafeBandwidth5Min(bandwidthResponse, domains, window.SafeBefore, location)
	if err != nil {
		return nil, time.Time{}, cdn.MonitoringResponse{}, cdn.MonitoringResponse{}, err
	}
	flowResponse, err := client.FetchMonitoringFlow(ctx, query)
	if err != nil {
		return nil, time.Time{}, cdn.MonitoringResponse{}, cdn.MonitoringResponse{}, err
	}
	traffic, err := cdn.SelectLatestSafeTraffic5Min(flowResponse, domains, window.SafeBefore, location)
	if err != nil {
		return nil, time.Time{}, cdn.MonitoringResponse{}, cdn.MonitoringResponse{}, err
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
		return nil, time.Time{}, cdn.MonitoringResponse{}, cdn.MonitoringResponse{}, err
	}
	return result, dataAt, bandwidthResponse, flowResponse, nil
}

func newCDNMonitoringWindow(now time.Time, sourceLag time.Duration, location *time.Location) cdnMonitoringWindow {
	safeBefore, endDate := cdnSafeWindow(now, sourceLag, location)
	wallClock := now.In(location)
	todayStart := time.Date(wallClock.Year(), wallClock.Month(), wallClock.Day(), 0, 0, 0, 0, location)
	hourEnd := time.Date(safeBefore.Year(), safeBefore.Month(), safeBefore.Day(), safeBefore.Hour(), 0, 0, 0, location)
	hourStart := hourEnd.Add(-time.Hour)
	queryStart := todayStart
	// Include one extra hour so a slightly lagging upstream batch can still
	// produce the previous complete hour after all batches converge on their
	// common latest timestamp.
	laggedHourStart := hourStart.Add(-time.Hour)
	if laggedHourStart.Before(queryStart) {
		queryStart = laggedHourStart
	}
	return cdnMonitoringWindow{
		SafeBefore: safeBefore,
		TodayStart: todayStart,
		HourStart:  hourStart,
		HourEnd:    hourEnd,
		StartDate:  queryStart.Format("2006-01-02"),
		EndDate:    endDate,
	}
}

func fetchCDNPriorMonthTraffic(
	ctx context.Context,
	client *cdn.Client,
	domains []string,
	window cdnMonitoringWindow,
	location *time.Location,
) ([]cdn.UsageBatch, cdnPartialErrors) {
	monthStart := time.Date(window.TodayStart.Year(), window.TodayStart.Month(), 1, 0, 0, 0, 0, location)
	if !monthStart.Before(window.TodayStart) {
		return nil, nil
	}
	return fetchCDNMeteringBatches(
		ctx, domains, monthStart.Format("2006-01-02"), window.TodayStart.AddDate(0, 0, -1).Format("2006-01-02"), cdn.GranularityDay,
		client.FetchMeteringFlux,
	)
}

type cdnMeteringFetch func(context.Context, cdn.MeteringQuery) (cdn.UsageResponse, error)

func fetchCDNMeteringBatches(
	ctx context.Context,
	domains []string,
	startDate, endDate string,
	granularity cdn.Granularity,
	fetch cdnMeteringFetch,
) ([]cdn.UsageBatch, cdnPartialErrors) {
	result := make([]cdn.UsageBatch, 0, (len(domains)+49)/50)
	failed := make(cdnPartialErrors)
	for _, batch := range batches(domains, 50) {
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
			response, err := fetch(ctx, cdn.MeteringQuery{
				Domains: current, StartDate: startDate, EndDate: endDate, Granularity: granularity,
			})
			if err == nil {
				result = append(result, cdn.UsageBatch{Domains: append([]string(nil), current...), Response: response})
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
		}
		collect(batch)
	}
	return result, failed
}

func loadCompletedMonthBandwidth(
	ctx context.Context,
	client *cdn.Client,
	domains []string,
	window cdnMonitoringWindow,
	location *time.Location,
	cache *cdnBandwidthCache,
) (cdn.BandwidthUsageAggregate, error) {
	monthStart := time.Date(window.TodayStart.Year(), window.TodayStart.Month(), 1, 0, 0, 0, 0, location)
	targetEnd := window.TodayStart
	if cache == nil {
		return cdn.BandwidthUsageAggregate{}, fmt.Errorf("cdn: nil monthly bandwidth cache")
	}

	candidate := *cache
	if !candidate.MonthStart.Equal(monthStart) || !slices.Equal(candidate.Domains, domains) || candidate.PeriodEnd.Before(monthStart) || candidate.PeriodEnd.After(targetEnd) {
		candidate = cdnBandwidthCache{
			MonthStart: monthStart,
			PeriodEnd:  monthStart,
			Domains:    append([]string(nil), domains...),
			Aggregate:  zeroCDNBandwidthUsage(domains, monthStart),
		}
		*cache = candidate
	}

	for candidate.PeriodEnd.Before(targetEnd) {
		cursor := candidate.PeriodEnd
		chunkEnd := cursor.AddDate(0, 0, cdnBandwidthBackfillDays)
		if chunkEnd.After(targetEnd) {
			chunkEnd = targetEnd
		}
		metering, failed := fetchCDNMeteringBatches(
			ctx, domains, cursor.Format("2006-01-02"), chunkEnd.AddDate(0, 0, -1).Format("2006-01-02"), cdn.GranularityFiveMinutes,
			client.FetchMeteringBandwidth,
		)
		if len(failed) > 0 {
			return cdn.BandwidthUsageAggregate{}, failed
		}
		segment, err := cdn.AggregateBandwidthUsage(metering, cdn.GranularityFiveMinutes, cursor, chunkEnd, location)
		if err != nil {
			return cdn.BandwidthUsageAggregate{}, fmt.Errorf("aggregate completed current-month bandwidth: %w", err)
		}
		candidate.Aggregate, err = mergeCDNBandwidthUsage(candidate.Aggregate, segment)
		if err != nil {
			return cdn.BandwidthUsageAggregate{}, err
		}
		candidate.PeriodEnd = chunkEnd
		// Persist every fully fetched and validated chunk. A later transient
		// failure can then resume from this boundary instead of replaying the
		// entire month.
		*cache = candidate
	}
	return candidate.Aggregate, nil
}

func collectAndPublishCDNUsage(
	ctx context.Context,
	client *cdn.Client,
	round cdnUsageRound,
	monitoringFailed cdnPartialErrors,
	resourceScope []string,
	catalog *resourceCatalog[string],
	window cdnMonitoringWindow,
	location *time.Location,
	staleAfter time.Duration,
	store *snapshot.Store[collector.CDNUsageSnapshot],
	bandwidthCache *cdnBandwidthCache,
	usageScopeMu *sync.Mutex,
	metrics *telemetry.Metrics,
) error {
	if round.DataEnd.IsZero() {
		return fmt.Errorf("%w: CDN usage round has no common monitoring timestamp", cdn.ErrNoSafePoint)
	}
	domains := cdnUsageRoundDomains(round)
	if len(domains) == 0 {
		return fmt.Errorf("%w: CDN usage round has no successful domains", cdn.ErrNoSafePoint)
	}
	priorMonth, monthFailed := fetchCDNPriorMonthTraffic(ctx, client, domains, window, location)
	monitoringComplete := len(monitoringFailed) == 0 && slices.Equal(domains, resourceScope)
	var priorMonthBandwidth cdn.BandwidthUsageAggregate
	var bandwidthErr error
	includeMonthBandwidth := false
	if monitoringComplete {
		priorMonthBandwidth, bandwidthErr = loadCompletedMonthBandwidth(ctx, client, resourceScope, window, location, bandwidthCache)
		includeMonthBandwidth = bandwidthErr == nil
	}
	usage, err := buildCDNUsageSnapshot(
		round, priorMonth, priorMonthBandwidth, window, location, monitoringComplete,
		len(monthFailed) == 0, includeMonthBandwidth,
	)
	if err != nil {
		return err
	}
	if usageScopeMu == nil {
		return fmt.Errorf("cdn: nil usage scope mutex")
	}
	usageScopeMu.Lock()
	if !slices.Equal(catalog.Snapshot(), resourceScope) {
		usageScopeMu.Unlock()
		return fmt.Errorf("cdn: usage resource scope changed during collection")
	}
	store.Publish(usage, snapshot.Meta{
		CollectedAt: time.Now(), DataAt: round.DataEnd, StaleAfter: staleAfter,
	})
	metrics.SetDataTimestamp("cdn", "usage", round.DataEnd)
	usageScopeMu.Unlock()

	failed := make(cdnPartialErrors, len(monitoringFailed)+len(monthFailed))
	for domain, failure := range monitoringFailed {
		failed[domain] = failure
	}
	for domain, failure := range monthFailed {
		failed[domain] = failure
	}
	if bandwidthErr != nil {
		if len(failed) > 0 {
			return errors.Join(failed, bandwidthErr)
		}
		return bandwidthErr
	}
	if len(failed) > 0 {
		return failed
	}
	return nil
}

func cdnUsageRoundDomains(round cdnUsageRound) []string {
	result := make([]string, 0)
	for _, batch := range round.Traffic {
		result = append(result, batch.Domains...)
	}
	return result
}

func buildCDNUsageSnapshot(
	round cdnUsageRound,
	priorMonth []cdn.UsageBatch,
	priorMonthBandwidth cdn.BandwidthUsageAggregate,
	window cdnMonitoringWindow,
	location *time.Location,
	complete bool,
	includeMonthTraffic bool,
	includeMonthBandwidth bool,
) (collector.CDNUsageSnapshot, error) {
	var result collector.CDNUsageSnapshot
	if len(round.Bandwidth) == 0 || len(round.Traffic) == 0 {
		return result, fmt.Errorf("%w: CDN usage round has no monitoring batches", cdn.ErrInvalidInput)
	}
	if round.DataEnd.IsZero() || round.DataEnd.After(window.SafeBefore) {
		return result, fmt.Errorf("%w: invalid CDN usage data timestamp", cdn.ErrInvalidInput)
	}
	window.SafeBefore = round.DataEnd
	window.HourEnd = round.DataEnd.In(location).Truncate(time.Hour)
	window.HourStart = window.HourEnd.Add(-time.Hour)

	hourTraffic, err := cdn.AggregateTrafficUsage(round.Traffic, cdn.GranularityFiveMinutes, window.HourStart, window.HourEnd, location)
	if err != nil {
		return result, fmt.Errorf("aggregate last complete hour traffic: %w", err)
	}
	hourBandwidth, err := cdn.AggregateBandwidthUsage(round.Bandwidth, cdn.GranularityFiveMinutes, window.HourStart, window.HourEnd, location)
	if err != nil {
		return result, fmt.Errorf("aggregate last complete hour bandwidth: %w", err)
	}
	todayTraffic := zeroCDNTrafficUsage(cdnUsageRoundDomains(round), window.TodayStart)
	var todayBandwidth cdn.BandwidthUsageAggregate
	hasTodayBandwidth := false
	if window.TodayStart.Before(window.SafeBefore) {
		todayTraffic, err = cdn.AggregateTrafficUsage(round.Traffic, cdn.GranularityFiveMinutes, window.TodayStart, window.SafeBefore, location)
		if err != nil {
			return result, fmt.Errorf("aggregate today traffic: %w", err)
		}
		todayBandwidth, err = cdn.AggregateBandwidthUsage(round.Bandwidth, cdn.GranularityFiveMinutes, window.TodayStart, window.SafeBefore, location)
		if err != nil {
			return result, fmt.Errorf("aggregate today bandwidth: %w", err)
		}
		hasTodayBandwidth = true
	}

	currentMonth := todayTraffic
	currentMonthBandwidth := todayBandwidth
	monthStart := time.Date(window.TodayStart.Year(), window.TodayStart.Month(), 1, 0, 0, 0, 0, location)
	if includeMonthTraffic && monthStart.Before(window.TodayStart) {
		prior, err := cdn.AggregateTrafficUsage(priorMonth, cdn.GranularityDay, monthStart, window.TodayStart, location)
		if err != nil {
			return result, fmt.Errorf("aggregate completed current-month days: %w", err)
		}
		currentMonth, err = mergeCDNTrafficUsage(prior, todayTraffic)
		if err != nil {
			return result, err
		}
	}
	if includeMonthBandwidth {
		if hasTodayBandwidth {
			currentMonthBandwidth, err = mergeCDNBandwidthUsage(priorMonthBandwidth, todayBandwidth)
			if err != nil {
				return result, err
			}
		} else {
			currentMonthBandwidth = priorMonthBandwidth
		}
	}

	result.Periods = []collector.CDNUsagePeriodSnapshot{
		{Period: collector.CDNUsagePeriodLastCompleteHour, Traffic: hourTraffic, Bandwidth: hourBandwidth, HasBandwidth: true, Complete: complete},
		{Period: collector.CDNUsagePeriodToday, Traffic: todayTraffic, Bandwidth: todayBandwidth, HasBandwidth: hasTodayBandwidth, Complete: complete},
	}
	if includeMonthTraffic {
		result.Periods = append(result.Periods, collector.CDNUsagePeriodSnapshot{
			Period:       collector.CDNUsagePeriodCurrentMonth,
			Traffic:      currentMonth,
			Bandwidth:    currentMonthBandwidth,
			HasBandwidth: includeMonthBandwidth && currentMonthBandwidth.BucketCount > 0,
			Complete:     complete,
		})
	}
	return result, nil
}

func zeroCDNTrafficUsage(domains []string, at time.Time) cdn.TrafficUsageAggregate {
	result := cdn.TrafficUsageAggregate{PeriodStart: at, PeriodEnd: at, Domains: make([]cdn.DomainTrafficUsage, 0, len(domains))}
	for _, domain := range domains {
		result.Domains = append(result.Domains, cdn.DomainTrafficUsage{Domain: domain})
	}
	return result
}

func zeroCDNBandwidthUsage(domains []string, at time.Time) cdn.BandwidthUsageAggregate {
	result := cdn.BandwidthUsageAggregate{PeriodStart: at, PeriodEnd: at, Domains: make([]cdn.DomainBandwidthUsage, 0, len(domains))}
	for _, domain := range domains {
		result.Domains = append(result.Domains, cdn.DomainBandwidthUsage{Domain: domain})
	}
	return result
}

func mergeCDNTrafficUsage(prior, today cdn.TrafficUsageAggregate) (cdn.TrafficUsageAggregate, error) {
	var result cdn.TrafficUsageAggregate
	if len(prior.Domains) != len(today.Domains) {
		return result, fmt.Errorf("%w: prior-month and today domain counts differ", cdn.ErrSeriesMisaligned)
	}
	result.PeriodStart = prior.PeriodStart
	result.PeriodEnd = today.PeriodEnd
	result.BucketCount = prior.BucketCount + today.BucketCount
	result.Domains = make([]cdn.DomainTrafficUsage, len(today.Domains))
	for index := range today.Domains {
		if prior.Domains[index].Domain != today.Domains[index].Domain {
			return cdn.TrafficUsageAggregate{}, fmt.Errorf("%w: prior-month and today domains differ", cdn.ErrSeriesMisaligned)
		}
		china, err := addFiniteCDNUsage(prior.Domains[index].ChinaBytes, today.Domains[index].ChinaBytes)
		if err != nil {
			return cdn.TrafficUsageAggregate{}, err
		}
		oversea, err := addFiniteCDNUsage(prior.Domains[index].OverseaBytes, today.Domains[index].OverseaBytes)
		if err != nil {
			return cdn.TrafficUsageAggregate{}, err
		}
		bytes, err := addFiniteCDNUsage(china, oversea)
		if err != nil {
			return cdn.TrafficUsageAggregate{}, err
		}
		result.Domains[index] = cdn.DomainTrafficUsage{
			Domain: today.Domains[index].Domain, ChinaBytes: china, OverseaBytes: oversea, Bytes: bytes, Active: bytes > 0,
		}
		result.AccountBytes, err = addFiniteCDNUsage(result.AccountBytes, bytes)
		if err != nil {
			return cdn.TrafficUsageAggregate{}, err
		}
	}
	return result, nil
}

func mergeCDNBandwidthUsage(prior, current cdn.BandwidthUsageAggregate) (cdn.BandwidthUsageAggregate, error) {
	if prior.BucketCount == 0 {
		current.PeriodStart = prior.PeriodStart
		return current, nil
	}
	if current.BucketCount == 0 {
		return prior, nil
	}
	if len(prior.Domains) != len(current.Domains) {
		return cdn.BandwidthUsageAggregate{}, fmt.Errorf("%w: bandwidth period domain counts differ", cdn.ErrSeriesMisaligned)
	}
	result := cdn.BandwidthUsageAggregate{
		PeriodStart: prior.PeriodStart,
		PeriodEnd:   current.PeriodEnd,
		BucketCount: prior.BucketCount + current.BucketCount,
		Domains:     make([]cdn.DomainBandwidthUsage, len(prior.Domains)),
	}
	for index := range prior.Domains {
		if prior.Domains[index].Domain != current.Domains[index].Domain {
			return cdn.BandwidthUsageAggregate{}, fmt.Errorf("%w: bandwidth period domains differ", cdn.ErrSeriesMisaligned)
		}
		selected := prior.Domains[index]
		if current.Domains[index].PeakBitsPerSecond > selected.PeakBitsPerSecond {
			selected = current.Domains[index]
		}
		selected.Active = prior.Domains[index].Active || current.Domains[index].Active
		result.Domains[index] = selected
	}
	if prior.AccountPeakBitsPerSecond >= current.AccountPeakBitsPerSecond {
		result.AccountPeakBitsPerSecond = prior.AccountPeakBitsPerSecond
		result.AccountPeakAt = prior.AccountPeakAt
	} else {
		result.AccountPeakBitsPerSecond = current.AccountPeakBitsPerSecond
		result.AccountPeakAt = current.AccountPeakAt
	}
	return result, nil
}

func addFiniteCDNUsage(left, right float64) (float64, error) {
	value := left + right
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, fmt.Errorf("%w: CDN usage aggregate overflow", cdn.ErrInvalidValue)
	}
	return value, nil
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
