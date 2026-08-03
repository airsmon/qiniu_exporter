package limiter

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"
)

var ErrInvalidConfig = errors.New("limiter qps must be positive and concurrency, when used, must be positive")

// Limiter combines a burst-one rate limiter, a concurrency gate, and host-wide
// adaptive slowdown. It is intentionally process-local; multiple exporters for
// one Qiniu account must still share a lower aggregate budget externally.
type Limiter struct {
	mu sync.Mutex

	baseQPS       float64
	currentQPS    float64
	next          time.Time
	cooldownUntil time.Time
	recoverAfter  time.Time

	semaphore chan struct{}
	now       func() time.Time
}

func New(qps float64, maxConcurrency int) (*Limiter, error) {
	if qps <= 0 || math.IsNaN(qps) || math.IsInf(qps, 0) || maxConcurrency <= 0 {
		return nil, ErrInvalidConfig
	}
	return newLimiter(qps, make(chan struct{}, maxConcurrency)), nil
}

// NewRate creates a burst-one pacing limiter without a concurrency gate. It
// is used for the first-attempt budget layered under the hard host limiter.
func NewRate(qps float64) (*Limiter, error) {
	if qps <= 0 || math.IsNaN(qps) || math.IsInf(qps, 0) {
		return nil, ErrInvalidConfig
	}
	return newLimiter(qps, nil), nil
}

func newLimiter(qps float64, semaphore chan struct{}) *Limiter {
	return &Limiter{
		baseQPS:    qps,
		currentQPS: qps,
		semaphore:  semaphore,
		now:        time.Now,
	}
}

// Acquire waits for both the concurrency slot and the next rate slot. Every
// attempt, including pagination and retries, must call Acquire.
func (l *Limiter) Acquire(ctx context.Context) (release func(), waited time.Duration, err error) {
	started := l.now()
	if err := ctx.Err(); err != nil {
		return nil, l.now().Sub(started), err
	}
	release = func() {}
	if l.semaphore != nil {
		select {
		case l.semaphore <- struct{}{}:
		case <-ctx.Done():
			return nil, l.now().Sub(started), ctx.Err()
		}
		release = func() { <-l.semaphore }
	}
	for {
		if err := ctx.Err(); err != nil {
			release()
			return nil, l.now().Sub(started), err
		}
		delay, acquired := l.takeOrDelay()
		if acquired {
			return release, l.now().Sub(started), nil
		}
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			release()
			return nil, l.now().Sub(started), ctx.Err()
		}
	}
}

// takeOrDelay advances the pacing clock only when the caller can actually
// take a token. A canceled waiter therefore cannot leave phantom reservations
// that push future requests farther into the future.
func (l *Limiter) takeOrDelay() (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	if !l.recoverAfter.IsZero() && !now.Before(l.recoverAfter) {
		l.currentQPS *= 2
		if l.currentQPS >= l.baseQPS {
			l.currentQPS = l.baseQPS
			l.recoverAfter = time.Time{}
		} else {
			l.recoverAfter = now.Add(5 * time.Minute)
		}
	}

	allowedAt := now
	if l.next.After(allowedAt) {
		allowedAt = l.next
	}
	if l.cooldownUntil.After(allowedAt) {
		allowedAt = l.cooldownUntil
	}
	if allowedAt.After(now) {
		return allowedAt.Sub(now), false
	}
	l.next = now.Add(pacingInterval(l.currentQPS))
	return 0, true
}

// pacingInterval keeps every positive finite QPS value safe when converted to
// time.Duration. In particular, an extremely small configured QPS must become
// a very long wait instead of overflowing to a negative duration and silently
// disabling rate limiting.
func pacingInterval(qps float64) time.Duration {
	nanoseconds := float64(time.Second) / qps
	if math.IsInf(nanoseconds, 0) || nanoseconds >= float64(math.MaxInt64) {
		return time.Duration(math.MaxInt64)
	}
	if nanoseconds <= 1 {
		return time.Nanosecond
	}
	return time.Duration(math.Ceil(nanoseconds))
}

// OnRateLimited applies a host-wide cooldown and halves the effective rate.
// Recovery is gradual after five quiet minutes.
func (l *Limiter) OnRateLimited(retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	if retryAfter < 5*time.Second {
		retryAfter = 5 * time.Second
	}
	if until := now.Add(retryAfter); until.After(l.cooldownUntil) {
		l.cooldownUntil = until
	}
	l.currentQPS /= 2
	if floor := l.baseQPS / 16; l.currentQPS < floor {
		l.currentQPS = floor
	}
	l.recoverAfter = now.Add(5 * time.Minute)
}

func (l *Limiter) CurrentQPS() float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.currentQPS
}
