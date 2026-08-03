package poller

import (
	"context"
	"fmt"
	"hash/fnv"
	"math/rand"
	"sync"
	"time"
)

type Job struct {
	Name       string
	Resource   string
	Resources  []string
	PhaseKey   string
	Interval   time.Duration
	Next       func(time.Time) time.Duration
	Timeout    time.Duration
	RunOnStart bool
	// RunOnStartWhen is evaluated by the scheduler immediately before it
	// computes the first delay. It avoids startup-time races around daily data
	// availability boundaries.
	RunOnStartWhen func(time.Time) bool
	Run            func(context.Context) error
}

type Observer interface {
	ObserveJob(name string, duration time.Duration, err error)
	ObserveSkipped(name, reason string)
}

type ResourceObserver interface {
	ObserveResourceJob(name, resource string, duration time.Duration, err error)
}

type ResourceBatchObserver interface {
	ObserveResourceBatchJob(name string, resources []string, duration time.Duration, err error)
}

// PartialResourceError allows a batch job to report failure for only some of
// its configured resources after bounded error isolation.
type PartialResourceError interface {
	error
	ErrorFor(resource string) error
}

type Scheduler struct {
	observer Observer
	jobs     []Job
	wg       sync.WaitGroup
}

func New(observer Observer) *Scheduler {
	return &Scheduler{observer: observer}
}

func (s *Scheduler) Add(job Job) error {
	if job.Name == "" || (job.Interval <= 0 && job.Next == nil) || job.Timeout <= 0 || job.Run == nil || (job.Resource != "" && len(job.Resources) > 0) {
		return fmt.Errorf("invalid poller job %q", job.Name)
	}
	s.jobs = append(s.jobs, job)
	return nil
}

func (s *Scheduler) Run(ctx context.Context) {
	for _, job := range s.jobs {
		job := job
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.runJob(ctx, job)
		}()
	}
}

func (s *Scheduler) Wait() { s.wg.Wait() }

func (s *Scheduler) runJob(ctx context.Context, job Job) {
	now := time.Now()
	delay := job.Interval
	if job.Next != nil {
		delay = job.Next(now)
	} else {
		delay = stablePhase(job.PhaseKey, job.Interval)
	}
	if job.RunOnStart || (job.RunOnStartWhen != nil && job.RunOnStartWhen(now)) {
		delay = 0
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		started := time.Now()
		runCtx, cancel := context.WithTimeout(ctx, job.Timeout)
		err := job.Run(runCtx)
		cancel()
		duration := time.Since(started)
		if s.observer != nil {
			if len(job.Resources) > 0 {
				if observer, ok := s.observer.(ResourceBatchObserver); ok {
					observer.ObserveResourceBatchJob(job.Name, job.Resources, duration, err)
				} else {
					s.observer.ObserveJob(job.Name, duration, err)
				}
			} else if job.Resource != "" {
				if observer, ok := s.observer.(ResourceObserver); ok {
					observer.ObserveResourceJob(job.Name, job.Resource, duration, err)
				} else {
					s.observer.ObserveJob(job.Name, duration, err)
				}
			} else {
				s.observer.ObserveJob(job.Name, duration, err)
			}
		}

		var nextDelay time.Duration
		if job.Next != nil {
			nextDelay = job.Next(time.Now())
		} else {
			nextDelay = jitteredInterval(job.Interval) - duration
		}
		if nextDelay <= 0 {
			if s.observer != nil {
				s.observer.ObserveSkipped(job.Name, "overlap")
			}
			if job.Interval > 0 {
				nextDelay = job.Interval
			} else {
				nextDelay = time.Second
			}
		}
		timer.Reset(nextDelay)
	}
}

func jitteredInterval(interval time.Duration) time.Duration {
	maximum := int64(float64(interval) * 0.1)
	if maximum <= 0 {
		return interval
	}
	return interval + time.Duration(rand.Int63n(2*maximum+1)-maximum)
}

func stablePhase(key string, interval time.Duration) time.Duration {
	if key == "" || interval <= 0 {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	return time.Duration(h.Sum64() % uint64(interval))
}
