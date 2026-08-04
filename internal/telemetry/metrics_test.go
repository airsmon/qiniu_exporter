package telemetry

import (
	"errors"
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

func TestReplaceResourcesPreservesUnchangedStateAndRemovesDepartedSeries(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := New(registry, "test", "test")
	metrics.ReplaceResources("cdn", "analytics", []string{"removed.example.com", "retained.example.com"}, time.Hour)
	metrics.observeResourceResults("cdn", "analytics", map[string]error{
		"removed.example.com":  nil,
		"retained.example.com": nil,
	})
	removedDataAt := time.Unix(1_700_000_000, 0)
	retainedDataAt := removedDataAt.Add(time.Hour)
	metrics.SetResourceDataTimestamp("cdn", "analytics", "removed.example.com", removedDataAt)
	metrics.SetResourceDataTimestamp("cdn", "analytics", "retained.example.com", retainedDataAt)

	metrics.resourceMu.Lock()
	retainedLastSuccess := metrics.resourceLastSuccessTimes["cdn/analytics"]["retained.example.com"]
	metrics.resourceMu.Unlock()
	metrics.ReplaceResources("cdn", "analytics", []string{"retained.example.com", "new.example.com"}, 2*time.Hour)

	metrics.resourceMu.Lock()
	states := metrics.resourceStates["cdn/analytics"]
	lastSuccess := metrics.resourceLastSuccessTimes["cdn/analytics"]
	dataTimes := metrics.resourceDataTimes["cdn/analytics"]
	if len(states) != 2 || !states["retained.example.com"] || states["new.example.com"] {
		metrics.resourceMu.Unlock()
		t.Fatalf("unexpected reconciled states: %#v", states)
	}
	if got := lastSuccess["retained.example.com"]; !got.Equal(retainedLastSuccess) {
		metrics.resourceMu.Unlock()
		t.Fatalf("retained last success=%s, want %s", got, retainedLastSuccess)
	}
	if _, ok := lastSuccess["removed.example.com"]; ok {
		metrics.resourceMu.Unlock()
		t.Fatal("removed resource retained its last-success state")
	}
	if got := dataTimes["retained.example.com"]; !got.Equal(retainedDataAt) {
		metrics.resourceMu.Unlock()
		t.Fatalf("retained data timestamp=%s, want %s", got, retainedDataAt)
	}
	if _, ok := dataTimes["removed.example.com"]; ok {
		metrics.resourceMu.Unlock()
		t.Fatal("removed resource retained its data timestamp state")
	}
	metrics.resourceMu.Unlock()

	assertGaugeSeries(t, registry, "qiniu_exporter_resource_collector_success", map[string]string{
		"module": "cdn", "collector": "analytics", "resource": "retained.example.com",
	}, 1)
	assertGaugeSeries(t, registry, "qiniu_exporter_resource_collector_success", map[string]string{
		"module": "cdn", "collector": "analytics", "resource": "new.example.com",
	}, 0)
	assertNoSeries(t, registry, "qiniu_exporter_resource_collector_success", map[string]string{
		"module": "cdn", "collector": "analytics", "resource": "removed.example.com",
	})
	assertNoSeries(t, registry, "qiniu_exporter_resource_last_success_timestamp_seconds", map[string]string{
		"module": "cdn", "collector": "analytics", "resource": "removed.example.com",
	})
	assertNoSeries(t, registry, "qiniu_exporter_resource_data_timestamp_seconds", map[string]string{
		"module": "cdn", "collector": "analytics", "resource": "removed.example.com",
	})
	assertGaugeSeries(t, registry, "qiniu_exporter_collector_stale_after_seconds", map[string]string{
		"module": "cdn", "collector": "analytics",
	}, (2 * time.Hour).Seconds())
	assertNoSeries(t, registry, "qiniu_exporter_data_timestamp_seconds", map[string]string{
		"module": "cdn", "collector": "analytics",
	})
}

func TestRemovedResourceCannotBeRecreatedByLateCollectionResult(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := New(registry, "test", "test")
	metrics.ReplaceResources("kodo", "activity", []string{"removed/z0", "retained/z0"}, time.Hour)
	metrics.ReplaceResources("kodo", "activity", []string{"retained/z0"}, time.Hour)

	metrics.observeResourceResults("kodo", "activity", map[string]error{"removed/z0": nil})
	metrics.SetResourceDataTimestamp("kodo", "activity", "removed/z0", time.Unix(1_700_000_000, 0))
	metrics.observeResourceResults("kodo", "activity", map[string]error{"retained/z0": errors.New("upstream")})

	metrics.resourceMu.Lock()
	_, stateExists := metrics.resourceStates["kodo/activity"]["removed/z0"]
	_, successExists := metrics.resourceLastSuccessTimes["kodo/activity"]["removed/z0"]
	_, dataExists := metrics.resourceDataTimes["kodo/activity"]["removed/z0"]
	metrics.resourceMu.Unlock()
	if stateExists || successExists || dataExists {
		t.Fatalf("late result recreated removed resource: state=%v success=%v data=%v", stateExists, successExists, dataExists)
	}
	assertNoSeries(t, registry, "qiniu_exporter_resource_collector_success", map[string]string{
		"module": "kodo", "collector": "activity", "resource": "removed/z0",
	})
	assertNoSeries(t, registry, "qiniu_exporter_resource_last_success_timestamp_seconds", map[string]string{
		"module": "kodo", "collector": "activity", "resource": "removed/z0",
	})
	assertNoSeries(t, registry, "qiniu_exporter_resource_data_timestamp_seconds", map[string]string{
		"module": "kodo", "collector": "activity", "resource": "removed/z0",
	})
}

func gaugeValue(t *testing.T, metric prometheus.Metric) float64 {
	t.Helper()
	value := &dto.Metric{}
	if err := metric.Write(value); err != nil {
		t.Fatal(err)
	}
	return value.GetGauge().GetValue()
}

func assertGaugeSeries(t *testing.T, registry *prometheus.Registry, family string, labels map[string]string, want float64) {
	t.Helper()
	got, ok := gatherGaugeSeries(t, registry, family, labels)
	if !ok || got != want {
		t.Fatalf("gauge %s%v=(%v,%v), want (%v,true)", family, labels, got, ok, want)
	}
}

func assertNoSeries(t *testing.T, registry *prometheus.Registry, family string, labels map[string]string) {
	t.Helper()
	if got, ok := gatherGaugeSeries(t, registry, family, labels); ok {
		t.Fatalf("unexpected gauge %s%v=%v", family, labels, got)
	}
}

func gatherGaugeSeries(t *testing.T, registry *prometheus.Registry, family string, labels map[string]string) (float64, bool) {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, metricFamily := range families {
		if metricFamily.GetName() != family {
			continue
		}
		for _, metric := range metricFamily.Metric {
			matched := true
			for name, want := range labels {
				found := false
				for _, label := range metric.Label {
					if label.GetName() == name && label.GetValue() == want {
						found = true
						break
					}
				}
				if !found {
					matched = false
					break
				}
			}
			if matched {
				return metric.GetGauge().GetValue(), true
			}
		}
	}
	return 0, false
}
