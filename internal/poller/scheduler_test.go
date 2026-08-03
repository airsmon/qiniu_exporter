package poller

import (
	"context"
	"testing"
	"time"
)

func TestStablePhase(t *testing.T) {
	interval := 5 * time.Minute
	first := stablePhase("bucket-a", interval)
	if second := stablePhase("bucket-a", interval); first != second {
		t.Fatalf("phase changed: %v != %v", first, second)
	}
	if first < 0 || first >= interval {
		t.Fatalf("phase %v outside interval", first)
	}
}

func TestJitterRemainsWithinTenPercent(t *testing.T) {
	interval := 100 * time.Second
	for range 100 {
		got := jitteredInterval(interval)
		if got < 90*time.Second || got > 110*time.Second {
			t.Fatalf("jittered interval %s outside ±10%%", got)
		}
	}
}

func TestRunOnStartPredicateIsEvaluatedWhenSchedulerStarts(t *testing.T) {
	runs := make(chan struct{}, 1)
	scheduler := New(nil)
	if err := scheduler.Add(Job{
		Name: "daily", Next: func(time.Time) time.Duration { return time.Hour }, Timeout: time.Second,
		RunOnStartWhen: func(time.Time) bool { return true },
		Run:            func(context.Context) error { runs <- struct{}{}; return nil },
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	scheduler.Run(ctx)
	select {
	case <-runs:
	case <-time.After(time.Second):
		t.Fatal("conditional startup run did not execute")
	}
	cancel()
	scheduler.Wait()
}
