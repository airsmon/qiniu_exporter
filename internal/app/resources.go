package app

import (
	"sync"
	"time"
)

// resourceCatalog holds the last completely discovered resource set. Failed
// discovery rounds never replace it.
type resourceCatalog[T any] struct {
	mu     sync.RWMutex
	values []T
}

func newResourceCatalog[T any](values []T) *resourceCatalog[T] {
	catalog := &resourceCatalog[T]{}
	catalog.values = append([]T(nil), values...)
	return catalog
}

func (c *resourceCatalog[T]) Snapshot() []T {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]T(nil), c.values...)
}

func (c *resourceCatalog[T]) Replace(values []T) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.values = append(c.values[:0], values...)
}

func discoveryTimeout(interval time.Duration) time.Duration {
	return min(5*time.Minute, interval/2)
}

func collectionTimeout(interval time.Duration) time.Duration {
	return interval * 4 / 5
}
