package app

import (
	"testing"
	"time"

	"qiniu-exporter/internal/qiniu/kodo"
	"qiniu-exporter/internal/snapshot"
)

func TestPublishKodoSnapshotRejectsMismatchedBucketEnds(t *testing.T) {
	store := &snapshot.ResourceStore[[]kodo.GaugeSample]{}
	point := time.Now().UTC().Add(-10 * time.Minute).Truncate(kodo.BucketWidth)
	oldSamples := []kodo.GaugeSample{{Kind: kodo.GaugeRequestsPerSecond, Value: 1, DataAt: point.Add(-kodo.BucketWidth)}}
	oldDataAt := point
	store.Publish("activity/bucket", oldSamples, snapshot.Meta{
		CollectedAt: time.Now(), DataAt: oldDataAt, StaleAfter: time.Hour,
	})
	samples := []kodo.GaugeSample{
		{Kind: kodo.GaugeRequestsPerSecond, Value: 2, DataAt: point},
		{Kind: kodo.GaugeEgressBytesPerSecond, Value: 3, DataAt: point.Add(kodo.BucketWidth)},
	}

	if _, err := publishKodoSnapshot(store, "activity/bucket", samples, time.Hour); err == nil {
		t.Fatal("expected mismatched bucket ends to fail")
	}
	values := store.Load(time.Now())
	value, ok := values["activity/bucket"]
	if !ok {
		t.Fatal("last good snapshot was removed after mismatch")
	}
	if !value.Meta.DataAt.Equal(oldDataAt) || len(value.Data) != 1 || value.Data[0].Value != 1 {
		t.Fatalf("last good snapshot was overwritten after mismatch: %#v", value)
	}
}

func TestPublishKodoSnapshotPublishesConsistentBucketEnd(t *testing.T) {
	store := &snapshot.ResourceStore[[]kodo.GaugeSample]{}
	point := time.Now().UTC().Add(-10 * time.Minute).Truncate(kodo.BucketWidth)
	samples := []kodo.GaugeSample{
		{Kind: kodo.GaugeStorageBytes, DataAt: point},
		{Kind: kodo.GaugeObjects, DataAt: point},
	}

	dataAt, err := publishKodoSnapshot(store, "capacity/bucket", samples, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	want := point.Add(kodo.BucketWidth)
	if !dataAt.Equal(want) {
		t.Fatalf("data timestamp=%s, want %s", dataAt, want)
	}
	values := store.Load(time.Now())
	value, ok := values["capacity/bucket"]
	if !ok {
		t.Fatal("consistent snapshot was not published")
	}
	if !value.Meta.DataAt.Equal(want) {
		t.Fatalf("published data timestamp=%s, want %s", value.Meta.DataAt, want)
	}
}

func TestKodoMonthToDateQueryUsesSafeShanghaiWindow(t *testing.T) {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	bucket := kodo.Bucket{Name: "bucket", Region: "z0"}
	now := time.Date(2026, time.August, 5, 11, 23, 17, 0, location)

	query, ok := kodoMonthToDateQuery(now, 10*time.Minute, bucket, location)
	if !ok {
		t.Fatal("expected a safe current-month window")
	}
	wantBegin := time.Date(2026, time.August, 1, 0, 0, 0, 0, location)
	wantEnd := time.Date(2026, time.August, 5, 11, 10, 0, 0, location)
	if query.Bucket != bucket.Name || query.Region != bucket.Region || !query.Begin.Equal(wantBegin) || !query.End.Equal(wantEnd) {
		t.Fatalf("month-to-date query = %#v, want begin=%s end=%s", query, wantBegin, wantEnd)
	}
}

func TestKodoMonthToDateQueryWaitsForFirstSafeBucket(t *testing.T) {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 1, 0, 7, 0, 0, location)
	query, ok := kodoMonthToDateQuery(now, 10*time.Minute, kodo.Bucket{Name: "bucket", Region: "z0"}, location)
	if ok {
		t.Fatal("month-to-date query should wait until the month has a safe bucket")
	}
	want := time.Date(2026, time.August, 1, 0, 0, 0, 0, location)
	if !query.Begin.Equal(want) || !query.End.Equal(want) {
		t.Fatalf("empty current-month window = %#v, want boundary %s", query, want)
	}
}

func TestPublishKodoSummarySnapshotRejectsMismatchedDataTimes(t *testing.T) {
	store := &snapshot.ResourceStore[[]kodo.GaugeSample]{}
	dataAt := time.Now().UTC().Truncate(kodo.BucketWidth)
	oldSamples := []kodo.GaugeSample{{Kind: kodo.GaugeUsageEgressBytes, Value: 1, DataAt: dataAt.Add(-kodo.BucketWidth)}}
	store.Publish("summary/bucket", oldSamples, snapshot.Meta{
		CollectedAt: time.Now(), DataAt: oldSamples[0].DataAt, StaleAfter: time.Hour,
	})
	samples := []kodo.GaugeSample{
		{Kind: kodo.GaugeUsageEgressBytes, Value: 2, DataAt: dataAt},
		{Kind: kodo.GaugeUsageRequests, Value: 3, DataAt: dataAt.Add(kodo.BucketWidth)},
	}

	if _, err := publishKodoSummarySnapshot(store, "summary/bucket", samples, time.Hour); err == nil {
		t.Fatal("expected mismatched data timestamps to fail")
	}
	value := store.Load(time.Now())["summary/bucket"]
	if len(value.Data) != 1 || value.Data[0].Value != 1 {
		t.Fatalf("last good summary was overwritten: %#v", value)
	}
}

func TestPublishKodoSummarySnapshotPublishesConsistentDataTime(t *testing.T) {
	store := &snapshot.ResourceStore[[]kodo.GaugeSample]{}
	dataAt := time.Now().UTC().Truncate(kodo.BucketWidth)
	samples := []kodo.GaugeSample{
		{Kind: kodo.GaugeUsageEgressBytes, DataAt: dataAt},
		{Kind: kodo.GaugeUsageRequests, DataAt: dataAt},
	}

	got, err := publishKodoSummarySnapshot(store, "summary/bucket", samples, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(dataAt) {
		t.Fatalf("data timestamp=%s, want %s", got, dataAt)
	}
	value := store.Load(time.Now())["summary/bucket"]
	if !value.Meta.DataAt.Equal(dataAt) || len(value.Data) != 2 {
		t.Fatalf("published summary = %#v", value)
	}
}
