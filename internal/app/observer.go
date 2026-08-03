package app

import (
	"log/slog"
	"time"

	"qiniu-exporter/internal/telemetry"
)

type Observer struct {
	Metrics *telemetry.Metrics
	Logger  *slog.Logger
}

func (o *Observer) ObserveJob(name string, duration time.Duration, err error) {
	o.Metrics.ObserveJob(name, duration, err)
	o.logFailure(name, "", err)
}

func (o *Observer) ObserveResourceJob(name, resource string, duration time.Duration, err error) {
	o.Metrics.ObserveResourceJob(name, resource, duration, err)
	o.logFailure(name, resource, err)
}

func (o *Observer) ObserveResourceBatchJob(name string, resources []string, duration time.Duration, err error) {
	o.Metrics.ObserveResourceBatchJob(name, resources, duration, err)
	if err != nil {
		o.Logger.Warn("Qiniu collection failed", "collector", name, "resource_count", len(resources), "error", err)
	}
}

func (o *Observer) ObserveSkipped(name, reason string) {
	o.Metrics.ObserveSkipped(name, reason)
	o.Logger.Warn("Qiniu collection skipped", "collector", name, "reason", reason)
}

func (o *Observer) logFailure(name, resource string, err error) {
	if err == nil {
		return
	}
	attributes := []any{"collector", name, "error", err}
	if resource != "" {
		attributes = append(attributes, "resource", resource)
	}
	o.Logger.Warn("Qiniu collection failed", attributes...)
}
