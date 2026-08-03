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

func RegisterKodo(
	scheduler *poller.Scheduler,
	client *kodo.Client,
	cfg config.KodoConfig,
	sourceLag time.Duration,
	staleAfter time.Duration,
	store *snapshot.ResourceStore[[]kodo.GaugeSample],
	metrics *telemetry.Metrics,
) error {
	if !cfg.StatisticsTimezoneVerified {
		metrics.ObserveSkipped("kodo/capacity", "timezone_unverified")
		metrics.ObserveSkipped("kodo/activity", "timezone_unverified")
		return nil
	}
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return fmt.Errorf("load Kodo statistics timezone: %w", err)
	}
	classes := make([]kodo.StorageClass, len(cfg.StorageClasses))
	for index, class := range cfg.StorageClasses {
		classes[index] = kodo.StorageClass(class)
	}
	resources := make([]string, len(cfg.Buckets))
	for index, bucket := range cfg.Buckets {
		resources[index] = bucket.Name
	}
	metrics.InitResources("kodo", "capacity", resources, staleAfter)
	metrics.InitResources("kodo", "activity", resources, staleAfter)

	for _, configuredBucket := range cfg.Buckets {
		bucket := configuredBucket
		if err := scheduler.Add(poller.Job{
			Name:     "kodo/capacity",
			Resource: bucket.Name,
			PhaseKey: "kodo/capacity/" + bucket.Name,
			Interval: 15 * time.Minute,
			Timeout:  2 * time.Minute,
			Run: func(ctx context.Context) error {
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
				dataAt, err := publishKodoSnapshot(store, "capacity/"+bucket.Name, samples, staleAfter)
				if err != nil {
					return fmt.Errorf("bucket %s capacity: %w", bucket.Name, err)
				}
				metrics.SetResourceDataTimestamp("kodo", "capacity", bucket.Name, dataAt)
				return nil
			},
		}); err != nil {
			return err
		}

		if err := scheduler.Add(poller.Job{
			Name:     "kodo/activity",
			Resource: bucket.Name,
			PhaseKey: "kodo/activity/" + bucket.Name,
			Interval: 5 * time.Minute,
			Timeout:  time.Minute,
			Run: func(ctx context.Context) error {
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
				dataAt, err := publishKodoSnapshot(store, "activity/"+bucket.Name, samples, staleAfter)
				if err != nil {
					return fmt.Errorf("bucket %s activity: %w", bucket.Name, err)
				}
				metrics.SetResourceDataTimestamp("kodo", "activity", bucket.Name, dataAt)
				return nil
			},
		}); err != nil {
			return err
		}
	}
	return nil
}

func kodoQuery(now time.Time, sourceLag time.Duration, bucket config.Bucket, location *time.Location) kodo.Query {
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
