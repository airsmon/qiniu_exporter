package limiter

import (
	"context"
	"math"
	"testing"
	"time"
)

func TestRateLimitHalvesEffectiveQPS(t *testing.T) {
	limiter, err := New(4, 1)
	if err != nil {
		t.Fatal(err)
	}
	limiter.OnRateLimited(0)
	if got := limiter.CurrentQPS(); got != 2 {
		t.Fatalf("CurrentQPS=%v, want 2", got)
	}
}

func TestCanceledWaiterDoesNotAdvancePacingClock(t *testing.T) {
	limiter, err := New(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	limiter.mu.Lock()
	limiter.next = time.Now().Add(time.Minute)
	want := limiter.next
	limiter.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := limiter.Acquire(ctx); err == nil {
		t.Fatal("expected canceled acquire")
	}
	limiter.mu.Lock()
	got := limiter.next
	limiter.mu.Unlock()
	if !got.Equal(want) {
		t.Fatalf("canceled waiter advanced next from %s to %s", want, got)
	}
}

func TestAcquireHonorsContextWhileConcurrencyIsFull(t *testing.T) {
	limiter, err := New(100, 1)
	if err != nil {
		t.Fatal(err)
	}
	release, _, err := limiter.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, _, err := limiter.Acquire(ctx); err == nil {
		t.Fatal("expected context deadline error")
	}
}

func TestRateOnlyLimiterHasNoConcurrencyGate(t *testing.T) {
	limiter, err := NewRate(1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	release, _, err := limiter.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	secondRelease, _, err := limiter.Acquire(ctx)
	if err != nil {
		t.Fatalf("second rate-only acquire was blocked by concurrency: %v", err)
	}
	secondRelease()
}

func TestExtremelySmallQPSDoesNotOverflowPacingInterval(t *testing.T) {
	constructors := map[string]func() (*Limiter, error){
		"combined":  func() (*Limiter, error) { return New(1e-300, 1) },
		"rate-only": func() (*Limiter, error) { return NewRate(1e-300) },
	}
	for name, construct := range constructors {
		t.Run(name, func(t *testing.T) {
			limit, err := construct()
			if err != nil {
				t.Fatal(err)
			}
			now := time.Unix(0, 0)
			limit.now = func() time.Time { return now }
			release, _, err := limit.Acquire(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			release()

			limit.mu.Lock()
			got := limit.next
			limit.mu.Unlock()
			want := now.Add(time.Duration(math.MaxInt64))
			if !got.Equal(want) {
				t.Fatalf("next=%s, want saturated future time %s", got, want)
			}
			delay, acquired := limit.takeOrDelay()
			if acquired || delay <= 0 {
				t.Fatalf("second token acquired=%t delay=%s, want a positive wait", acquired, delay)
			}
		})
	}
}
