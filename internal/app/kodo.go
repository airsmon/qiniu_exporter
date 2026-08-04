package app

import (
	"context"
	"fmt"
	"time"

	"qiniu-exporter/internal/config"
	"qiniu-exporter/internal/poller"
	"qiniu-exporter/internal/qiniu/kodo"
	"qiniu-exporter/internal/snapshot"
	"qiniu-exporter/internal/telemetry"
)

type KodoDiscoverer interface {
	ListBuckets(context.Context) ([]kodo.Bucket, error)
}

type kodoPartialErrors map[string]error

func (e kodoPartialErrors) Error() string {
	return fmt.Sprintf("kodo: collection failed for %d discovered resources", len(e))
}

func (e kodoPartialErrors) ErrorFor(resource string) error { return e[resource] }

func RegisterKodo(
	scheduler *poller.Scheduler,
	client *kodo.Client,
	discoverer KodoDiscoverer,
	cfg *config.Config,
	store *snapshot.ResourceStore[[]kodo.GaugeSample],
	metrics *telemetry.Metrics,
) error {
	if discoverer == nil {
		return fmt.Errorf("kodo discoverer is required")
	}
	if !cfg.Kodo.StatisticsTimezoneVerified {
		metrics.ObserveSkipped("kodo/capacity", "timezone_unverified")
		metrics.ObserveSkipped("kodo/activity", "timezone_unverified")
	}
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return fmt.Errorf("load Kodo statistics timezone: %w", err)
	}
	classes := make([]kodo.StorageClass, len(cfg.Kodo.StorageClasses))
	for index, class := range cfg.Kodo.StorageClasses {
		classes[index] = kodo.StorageClass(class)
	}
	resourceKey := func(bucket kodo.Bucket) string { return bucket.Name + "/" + bucket.Region }
	catalog := newResourceCatalog[kodo.Bucket](nil)
	reconcile := func(ctx context.Context) error {
		buckets, err := discoverer.ListBuckets(ctx)
		if err != nil {
			return err
		}
		if err := cfg.ValidateKodoResourceCount(len(buckets)); err != nil {
			return err
		}
		resources := make([]string, len(buckets))
		for index, bucket := range buckets {
			resources[index] = resourceKey(bucket)
		}
		retained := make([]string, 0, 2*len(resources))
		for _, resource := range resources {
			retained = append(retained, "capacity/"+resource, "activity/"+resource)
		}
		store.Retain(retained)
		if cfg.Kodo.StatisticsTimezoneVerified {
			metrics.ReplaceResources("kodo", "capacity", resources, cfg.Collection.StaleAfter.Realtime.Value())
			metrics.ReplaceResources("kodo", "activity", resources, cfg.Collection.StaleAfter.Realtime.Value())
		}
		catalog.Replace(buckets)
		return nil
	}

	metrics.InitCollector("kodo", "discovery")
	if err := scheduler.Add(poller.Job{
		Name: "kodo/discovery", PhaseKey: "kodo/discovery", Interval: cfg.Collection.Intervals.Discovery.Value(),
		Timeout: discoveryTimeout(cfg.Collection.Intervals.Discovery.Value()), RunOnStart: true, Run: reconcile,
	}); err != nil {
		return err
	}
	if !cfg.Kodo.StatisticsTimezoneVerified {
		return nil
	}

	if err := scheduler.Add(poller.Job{
		Name: "kodo/capacity", PhaseKey: "kodo/capacity", Interval: cfg.Collection.Intervals.KodoCapacity.Value(),
		Timeout: collectionTimeout(cfg.Collection.Intervals.KodoCapacity.Value()),
		RunResources: func(ctx context.Context) ([]string, error) {
			buckets := catalog.Snapshot()
			if len(buckets) == 0 {
				return nil, poller.Skip("no_resources")
			}
			resources := make([]string, len(buckets))
			failed := make(kodoPartialErrors)
			for index, bucket := range buckets {
				resource := resourceKey(bucket)
				resources[index] = resource
				if err := collectKodoCapacity(ctx, client, bucket, classes, cfg.Collection.SourceLag.Value(), cfg.Collection.StaleAfter.Realtime.Value(), location, store, metrics, resource); err != nil {
					failed[resource] = err
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
	if err := scheduler.Add(poller.Job{
		Name: "kodo/activity", PhaseKey: "kodo/activity", Interval: cfg.Collection.Intervals.KodoActivity.Value(),
		Timeout: collectionTimeout(cfg.Collection.Intervals.KodoActivity.Value()),
		RunResources: func(ctx context.Context) ([]string, error) {
			buckets := catalog.Snapshot()
			if len(buckets) == 0 {
				return nil, poller.Skip("no_resources")
			}
			resources := make([]string, len(buckets))
			failed := make(kodoPartialErrors)
			for index, bucket := range buckets {
				resource := resourceKey(bucket)
				resources[index] = resource
				if err := collectKodoActivity(ctx, client, bucket, cfg.Collection.SourceLag.Value(), cfg.Collection.StaleAfter.Realtime.Value(), location, store, metrics, resource); err != nil {
					failed[resource] = err
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

func collectKodoCapacity(
	ctx context.Context,
	client *kodo.Client,
	bucket kodo.Bucket,
	classes []kodo.StorageClass,
	sourceLag, staleAfter time.Duration,
	location *time.Location,
	store *snapshot.ResourceStore[[]kodo.GaugeSample],
	metrics *telemetry.Metrics,
	resource string,
) error {
	query := kodoQuery(time.Now(), sourceLag, bucket, location)
	samples := make([]kodo.GaugeSample, 0, 2*len(classes))
	for _, class := range classes {
		storage, err := client.Storage(ctx, query, class)
		if err != nil {
			return fmt.Errorf("bucket %s storage %s: %w", bucket.Name, class, err)
		}
		objects, err := client.Objects(ctx, query, class)
		if err != nil {
			return fmt.Errorf("bucket %s objects %s: %w", bucket.Name, class, err)
		}
		samples = append(samples, storage, objects)
	}
	dataAt, err := publishKodoSnapshot(store, "capacity/"+resource, samples, staleAfter)
	if err != nil {
		return fmt.Errorf("bucket %s capacity: %w", bucket.Name, err)
	}
	metrics.SetResourceDataTimestamp("kodo", "capacity", resource, dataAt)
	return nil
}

func collectKodoActivity(
	ctx context.Context,
	client *kodo.Client,
	bucket kodo.Bucket,
	sourceLag, staleAfter time.Duration,
	location *time.Location,
	store *snapshot.ResourceStore[[]kodo.GaugeSample],
	metrics *telemetry.Metrics,
	resource string,
) error {
	query := kodoQuery(time.Now(), sourceLag, bucket, location)
	get, err := client.GETRequests(ctx, query)
	if err != nil {
		return fmt.Errorf("bucket %s GET requests: %w", bucket.Name, err)
	}
	put, err := client.PUTRequests(ctx, query)
	if err != nil {
		return fmt.Errorf("bucket %s PUT requests: %w", bucket.Name, err)
	}
	direct, err := client.DirectEgress(ctx, query)
	if err != nil {
		return fmt.Errorf("bucket %s direct egress: %w", bucket.Name, err)
	}
	cdnOrigin, err := client.CDNOriginEgress(ctx, query)
	if err != nil {
		return fmt.Errorf("bucket %s CDN origin egress: %w", bucket.Name, err)
	}
	samples := []kodo.GaugeSample{get, put, direct, cdnOrigin}
	dataAt, err := publishKodoSnapshot(store, "activity/"+resource, samples, staleAfter)
	if err != nil {
		return fmt.Errorf("bucket %s activity: %w", bucket.Name, err)
	}
	metrics.SetResourceDataTimestamp("kodo", "activity", resource, dataAt)
	return nil
}

func kodoQuery(now time.Time, sourceLag time.Duration, bucket kodo.Bucket, location *time.Location) kodo.Query {
	end := now.In(location).Add(-sourceLag).Truncate(kodo.BucketWidth)
	return kodo.Query{
		Bucket:     bucket.Name,
		Region:     bucket.Region,
		Begin:      end.Add(-3 * kodo.BucketWidth),
		End:        end,
		SafeBefore: end,
	}
}

func publishKodoSnapshot(
	store *snapshot.ResourceStore[[]kodo.GaugeSample],
	resource string,
	samples []kodo.GaugeSample,
	staleAfter time.Duration,
) (time.Time, error) {
	if len(samples) == 0 {
		return time.Time{}, fmt.Errorf("kodo snapshot has no samples")
	}
	bucketEnd := samples[0].DataAt.Add(kodo.BucketWidth)
	for index := 1; index < len(samples); index++ {
		currentEnd := samples[index].DataAt.Add(kodo.BucketWidth)
		if !currentEnd.Equal(bucketEnd) {
			return time.Time{}, fmt.Errorf("kodo snapshot bucket end mismatch at sample %d: %s != %s", index, currentEnd, bucketEnd)
		}
	}
	store.Publish(resource, samples, snapshot.Meta{
		CollectedAt: time.Now(), DataAt: bucketEnd, StaleAfter: staleAfter,
	})
	return bucketEnd, nil
}
