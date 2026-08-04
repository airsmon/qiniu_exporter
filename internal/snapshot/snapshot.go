package snapshot

import (
	"sync"
	"sync/atomic"
	"time"
)

type Meta struct {
	CollectedAt time.Time
	DataAt      time.Time
	StaleAfter  time.Duration
}

type ResourceValue[T any] struct {
	Data T
	Meta Meta
}

// ResourceStore lets successful resources advance independently while failed
// resources retain their own last-good value until it becomes stale.
type ResourceStore[T any] struct {
	mu      sync.RWMutex
	values  map[string]ResourceValue[T]
	active  map[string]struct{}
	managed bool
}

func (s *ResourceStore[T]) Publish(resource string, data T, meta Meta) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.managed {
		if _, ok := s.active[resource]; !ok {
			return
		}
	}
	if s.values == nil {
		s.values = make(map[string]ResourceValue[T])
	}
	s.values[resource] = ResourceValue[T]{Data: data, Meta: meta}
}

// Retain removes snapshots for resources that are no longer present in the
// latest successful discovery result.
func (s *ResourceStore[T]) Retain(resources []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keep := make(map[string]struct{}, len(resources))
	for _, resource := range resources {
		keep[resource] = struct{}{}
	}
	for resource := range s.values {
		if _, ok := keep[resource]; !ok {
			delete(s.values, resource)
		}
	}
	s.active = keep
	s.managed = true
}

func (s *ResourceStore[T]) Load(now time.Time) map[string]ResourceValue[T] {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make(map[string]ResourceValue[T], len(s.values))
	for resource, value := range s.values {
		if stale(now, value.Meta) {
			continue
		}
		result[resource] = value
	}
	return result
}

type value[T any] struct {
	data T
	meta Meta
}

// Store publishes complete immutable snapshots. Callers must not mutate T after
// Publish; collectors can therefore load without holding locks.
type Store[T any] struct {
	value atomic.Pointer[value[T]]
}

func (s *Store[T]) Publish(data T, meta Meta) {
	s.value.Store(&value[T]{data: data, meta: meta})
}

// Clear removes the published snapshot. It is used when discovery changes the
// resource scope of a whole-account aggregate, so a stale aggregate cannot
// continue to expose departed resources or totals from the previous scope.
func (s *Store[T]) Clear() {
	s.value.Store(nil)
}

func (s *Store[T]) Load(now time.Time) (data T, meta Meta, ok bool) {
	v := s.value.Load()
	if v == nil {
		return data, meta, false
	}
	if stale(now, v.meta) {
		return data, v.meta, false
	}
	return v.data, v.meta, true
}

func (s *Store[T]) HasValue() bool {
	return s.value.Load() != nil
}

// stale uses the older of collection time and upstream data time. A healthy
// HTTP response containing a frozen historical bucket must not refresh the
// lifetime of operational data indefinitely.
func stale(now time.Time, meta Meta) bool {
	if meta.StaleAfter <= 0 {
		return false
	}
	reference := meta.CollectedAt
	if !meta.DataAt.IsZero() && (reference.IsZero() || meta.DataAt.Before(reference)) {
		reference = meta.DataAt
	}
	return now.Sub(reference) > meta.StaleAfter
}
