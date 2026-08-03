package telemetry

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestResourceCollectorLastSuccessUsesOldestResourceSuccess(t *testing.T) {
	metrics := New(prometheus.NewRegistry(), "test", "test")
	metrics.InitResources("kodo", "activity", []string{"bucket-a", "bucket-b"}, time.Hour)

	metrics.observeResourceResults("kodo", "activity", map[string]error{"bucket-a": nil})
	oldest := time.Unix(1_700_000_000, 0)
	metrics.resourceMu.Lock()
	metrics.resourceLastSuccessTimes["kodo/activity"]["bucket-a"] = oldest
	metrics.resourceMu.Unlock()

	metrics.observeResourceResults("kodo", "activity", map[string]error{"bucket-b": nil})

	got := gaugeValue(t, metrics.collectorLastSuccess.WithLabelValues("kodo", "activity"))
	if want := float64(oldest.Unix()); got != want {
		t.Fatalf("dataset last success=%v, want oldest resource success %v", got, want)
	}
}

func TestCollectorStaleAfterMetricUsesConfiguredDuration(t *testing.T) {
	metrics := New(prometheus.NewRegistry(), "test", "test")
	metrics.InitResources("cdn", "analytics", []string{"cdn.example.com"}, 45*time.Minute)
	metrics.InitCollector("billing", "balance")
	metrics.SetCollectorStaleAfter("billing", "balance", 3*time.Hour)

	if got, want := gaugeValue(t, metrics.collectorStaleAfter.WithLabelValues("cdn", "analytics")), (45 * time.Minute).Seconds(); got != want {
		t.Fatalf("cdn stale-after=%v, want %v", got, want)
	}
	if got, want := gaugeValue(t, metrics.collectorStaleAfter.WithLabelValues("billing", "balance")), (3 * time.Hour).Seconds(); got != want {
		t.Fatalf("billing stale-after=%v, want %v", got, want)
	}
}

func gaugeValue(t *testing.T, metric prometheus.Metric) float64 {
	t.Helper()
	value := &dto.Metric{}
	if err := metric.Write(value); err != nil {
		t.Fatal(err)
	}
	return value.GetGauge().GetValue()
}
