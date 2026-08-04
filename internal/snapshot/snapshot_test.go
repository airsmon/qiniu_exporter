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

func TestStoreClearRemovesPublishedValue(t *testing.T) {
	var store Store[string]
	store.Publish("value", Meta{CollectedAt: time.Now()})
	store.Clear()
	if store.HasValue() {
		t.Fatal("cleared store retained its published value")
	}
	if _, _, ok := store.Load(time.Now()); ok {
		t.Fatal("cleared store returned a value")
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

func TestResourceStoreRetainRemovesDepartedResources(t *testing.T) {
	var store ResourceStore[int]
	now := time.Unix(1000, 0)
	meta := Meta{CollectedAt: now, StaleAfter: time.Hour}
	store.Publish("removed", 1, meta)
	store.Publish("retained", 2, meta)

	store.Retain([]string{"retained", "new-without-snapshot", "retained"})
	store.Publish("removed", 3, meta)
	store.Publish("retained", 4, meta)
	values := store.Load(now)
	if len(values) != 1 || values["retained"].Data != 4 {
		t.Fatalf("unexpected resources after retain: %#v", values)
	}

	store.Retain(nil)
	store.Publish("retained", 5, meta)
	if values := store.Load(now); len(values) != 0 {
		t.Fatalf("empty retain did not clear snapshots or reject a late publish: %#v", values)
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
