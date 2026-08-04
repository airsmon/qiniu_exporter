package poller

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type recordingObserver struct {
	mu       sync.Mutex
	jobs     int
	batches  []recordedBatch
	skips    []recordedSkip
	observed chan struct{}
}

type recordedBatch struct {
	name      string
	resources []string
	err       error
}

type recordedSkip struct {
	name   string
	reason string
}

func (o *recordingObserver) ObserveJob(string, time.Duration, error) {
	o.mu.Lock()
	o.jobs++
	o.mu.Unlock()
	o.signal()
}

func (o *recordingObserver) ObserveSkipped(name, reason string) {
	o.mu.Lock()
	o.skips = append(o.skips, recordedSkip{name: name, reason: reason})
	o.mu.Unlock()
	o.signal()
}

func (o *recordingObserver) ObserveResourceBatchJob(name string, resources []string, _ time.Duration, err error) {
	o.mu.Lock()
	o.batches = append(o.batches, recordedBatch{name: name, resources: append([]string(nil), resources...), err: err})
	o.mu.Unlock()
	o.signal()
}

func (o *recordingObserver) signal() {
	select {
	case o.observed <- struct{}{}:
	default:
	}
}

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

func TestRunResourcesReportsTheResourcesReturnedByThatAttempt(t *testing.T) {
	observer := &recordingObserver{observed: make(chan struct{}, 1)}
	scheduler := New(observer)
	wantErr := errors.New("partial failure")
	if err := scheduler.Add(Job{
		Name: "cdn/analytics", Interval: time.Hour, Timeout: time.Second, RunOnStart: true,
		RunResources: func(context.Context) ([]string, error) {
			return []string{"a.example.com", "b.example.com"}, wantErr
		},
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	scheduler.Run(ctx)
	waitForObservation(t, observer.observed)
	cancel()
	scheduler.Wait()

	observer.mu.Lock()
	defer observer.mu.Unlock()
	if observer.jobs != 0 || len(observer.skips) != 0 || len(observer.batches) != 1 {
		t.Fatalf("observations: jobs=%d skips=%v batches=%v", observer.jobs, observer.skips, observer.batches)
	}
	got := observer.batches[0]
	if got.name != "cdn/analytics" || !errors.Is(got.err, wantErr) || len(got.resources) != 2 || got.resources[0] != "a.example.com" || got.resources[1] != "b.example.com" {
		t.Fatalf("unexpected dynamic batch observation: %#v", got)
	}
}

func TestSkipOnlyIncrementsSkipObservation(t *testing.T) {
	observer := &recordingObserver{observed: make(chan struct{}, 1)}
	scheduler := New(observer)
	if err := scheduler.Add(Job{
		Name: "kodo/capacity", Interval: time.Hour, Timeout: time.Second, RunOnStart: true,
		RunResources: func(context.Context) ([]string, error) {
			return []string{"bucket/z0"}, Skip("no_resources")
		},
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	scheduler.Run(ctx)
	waitForObservation(t, observer.observed)
	cancel()
	scheduler.Wait()

	observer.mu.Lock()
	defer observer.mu.Unlock()
	if observer.jobs != 0 || len(observer.batches) != 0 {
		t.Fatalf("skip changed normal collector state: jobs=%d batches=%v", observer.jobs, observer.batches)
	}
	if len(observer.skips) != 1 || observer.skips[0] != (recordedSkip{name: "kodo/capacity", reason: "no_resources"}) {
		t.Fatalf("unexpected skip observations: %#v", observer.skips)
	}
}

func waitForObservation(t *testing.T, observed <-chan struct{}) {
	t.Helper()
	select {
	case <-observed:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for scheduler observation")
	}
}
