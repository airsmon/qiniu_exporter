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
