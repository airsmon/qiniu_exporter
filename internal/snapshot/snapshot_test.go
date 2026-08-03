package snapshot

import (
	"testing"
	"time"
)

func TestStoreExpiresWithoutDestroyingLastValue(t *testing.T) {
	var store Store[string]
	now := time.Unix(1000, 0)
	store.Publish("value", Meta{CollectedAt: now, StaleAfter: time.Minute})
	if value, _, ok := store.Load(now.Add(30 * time.Second)); !ok || value != "value" {
		t.Fatalf("fresh load=(%q,%v), want value,true", value, ok)
	}
	if _, _, ok := store.Load(now.Add(2 * time.Minute)); ok {
		t.Fatal("stale value should not be exported")
	}
	if !store.HasValue() {
		t.Fatal("expiration must not destroy the last-good snapshot")
	}
}

func TestResourceStoreExpiresIndependently(t *testing.T) {
	var store ResourceStore[int]
	now := time.Unix(1000, 0)
	store.Publish("fresh", 1, Meta{CollectedAt: now, StaleAfter: time.Minute})
	store.Publish("stale", 2, Meta{CollectedAt: now.Add(-time.Hour), StaleAfter: time.Minute})
	values := store.Load(now)
	if len(values) != 1 || values["fresh"].Data != 1 {
		t.Fatalf("unexpected resources: %#v", values)
	}
}

func TestStoresExpireFrozenUpstreamDataEvenAfterRecentCollection(t *testing.T) {
	now := time.Unix(10_000, 0)
	meta := Meta{CollectedAt: now, DataAt: now.Add(-2 * time.Hour), StaleAfter: time.Hour}

	var store Store[string]
	store.Publish("frozen", meta)
	if _, _, ok := store.Load(now); ok {
		t.Fatal("recent collection must not keep frozen upstream data fresh")
	}

	var resources ResourceStore[string]
	resources.Publish("resource", "frozen", meta)
	if values := resources.Load(now); len(values) != 0 {
		t.Fatalf("recent collection kept frozen resource data fresh: %#v", values)
	}
}
