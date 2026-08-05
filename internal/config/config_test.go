package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLoadRejectsUnknownResourceConfiguration(t *testing.T) {
	path := writeConfig(t, `
server:
  listen: ":9106"
credentials:
  main:
    access_key_env: TEST_AK
    secret_key_env: TEST_SK
kodo:
  enabled: true
  credential: main
  unexpected: true
`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "field unexpected") {
		t.Fatalf("expected strict YAML error, got %v", err)
	}

	path = writeConfig(t, `
credentials:
  main:
    access_key_env: TEST_AK
    secret_key_env: TEST_SK
kodo:
  enabled: true
  credential: main
  storage_classes: [standard]
  buckets: []
`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "field buckets") {
		t.Fatalf("expected obsolete buckets field to be rejected, got %v", err)
	}
}

func TestLoadRejectsMultipleDocuments(t *testing.T) {
	path := writeConfig(t, `
credentials:
  main:
    access_key_env: TEST_AK
    secret_key_env: TEST_SK
cdn:
  enabled: true
  credential: main
---
server:
  listen: ":9999"
`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "multiple YAML documents") {
		t.Fatalf("expected multiple-document error, got %v", err)
	}
}

func TestLoadRejectsOversizedConfigInsteadOfParsingTruncatedPrefix(t *testing.T) {
	path := writeConfig(t, `
credentials:
  main:
    access_key_env: TEST_AK
    secret_key_env: TEST_SK
cdn:
  enabled: true
  credential: main
`+"#"+strings.Repeat("x", maxConfigBytes))
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected oversized config error, got %v", err)
	}
}

func TestLoadAndResolveEnvironmentCredential(t *testing.T) {
	t.Setenv("TEST_AK", "access")
	t.Setenv("TEST_SK", "secret")
	path := writeConfig(t, `
credentials:
  main:
    access_key_env: TEST_AK
    secret_key_env: TEST_SK
cdn:
  enabled: true
  credential: main
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := cfg.Credentials["main"].Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if resolved.AccessKey != "access" || resolved.SecretKey != "secret" {
		t.Fatalf("unexpected resolved credential: %#v", resolved)
	}
	if cfg.Collection.StaleAfter.BillingFinalized.Value() != 40*24*time.Hour {
		t.Fatalf("default finalized staleness=%s", cfg.Collection.StaleAfter.BillingFinalized.Value())
	}
}

func TestDefaultDiscoveryCollectionIntervalsAndRealtimeStaleness(t *testing.T) {
	cfg := defaults()
	wantIntervals := map[string]struct {
		got  time.Duration
		want time.Duration
	}{
		"discovery":      {cfg.Collection.Intervals.Discovery.Value(), time.Hour},
		"kodo_capacity":  {cfg.Collection.Intervals.KodoCapacity.Value(), 30 * time.Minute},
		"kodo_activity":  {cfg.Collection.Intervals.KodoActivity.Value(), 30 * time.Minute},
		"cdn_monitoring": {cfg.Collection.Intervals.CDNMonitoring.Value(), 30 * time.Minute},
		"cdn_analytics":  {cfg.Collection.Intervals.CDNAnalytics.Value(), 30 * time.Minute},
	}
	for name, interval := range wantIntervals {
		if interval.got != interval.want {
			t.Errorf("default %s interval=%s, want %s", name, interval.got, interval.want)
		}
	}
	if got := cfg.Collection.StaleAfter.Realtime.Value(); got != time.Hour {
		t.Fatalf("default realtime staleness=%s, want 1h", got)
	}
}

func TestAdmissionRejectsOversizedDiscoveredCDNSet(t *testing.T) {
	cfg := defaults()
	cfg.Credentials = map[string]CredentialConfig{"main": {AccessKeyEnv: "AK", SecretKeyEnv: "SK"}}
	cfg.CDN.Enabled = true
	cfg.CDN.Credential = "main"
	cfg.CDN.StatisticsTimezoneVerified = true
	cfg.Collection.CDNFusionMaxQPS = 0.1
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := cfg.ValidateCDNResourceCount(500); err == nil || !strings.Contains(err.Error(), "call budget") {
		t.Fatalf("expected admission error, got %v", err)
	}
}

func TestKodoAdmissionUsesDiscoveredResourcesAndConfiguredIntervals(t *testing.T) {
	cfg := defaults()
	cfg.Kodo.Enabled = true
	cfg.Kodo.StatisticsTimezoneVerified = true
	cfg.Kodo.StorageClasses = []string{"standard", "ia"}

	if err := cfg.ValidateKodoResourceCount(50); err != nil {
		t.Fatalf("safe discovered set rejected at default intervals: %v", err)
	}
	cfg.Collection.Intervals.KodoCapacity = Duration(5 * time.Minute)
	cfg.Collection.Intervals.KodoActivity = Duration(5 * time.Minute)
	if err := cfg.ValidateKodoResourceCount(50); err == nil || !strings.Contains(err.Error(), "call budget") {
		t.Fatalf("expected shorter intervals to exceed admission budget, got %v", err)
	}
	if err := cfg.ValidateKodoResourceCount(-1); err == nil || !strings.Contains(err.Error(), "must not be negative") {
		t.Fatalf("expected negative resource count rejection, got %v", err)
	}
}

func TestKodoAdmissionAccountsForMonthToDateUsageCalls(t *testing.T) {
	cfg := defaults()
	cfg.Kodo.Enabled = true
	cfg.Kodo.StatisticsTimezoneVerified = true
	cfg.Kodo.StorageClasses = []string{"standard", "ia", "archive", "deep_archive", "archive_ir", "intelligent_tiering"}

	if err := cfg.ValidateKodoResourceCount(83); err == nil || !strings.Contains(err.Error(), "call budget") {
		t.Fatalf("expected six-class 30m collection to exceed the 1 QPS safety budget, got %v", err)
	}
	cfg.Collection.Intervals.KodoCapacity = Duration(time.Hour)
	if err := cfg.ValidateKodoResourceCount(83); err != nil {
		t.Fatalf("60m capacity and 30m activity should admit 83 buckets: %v", err)
	}
}

func TestKodoAdmissionStillAccountsForDiscoveryWhenStatisticsAreUnverified(t *testing.T) {
	cfg := defaults()
	cfg.Kodo.Enabled = true
	cfg.Kodo.StatisticsTimezoneVerified = false
	cfg.Kodo.StorageClasses = []string{"standard"}
	cfg.Collection.KodoMaxQPS = 0.000001

	if err := cfg.ValidateKodoResourceCount(1); err == nil || !strings.Contains(err.Error(), "call budget") {
		t.Fatalf("expected Kodo discovery calls to consume the shared Kodo budget, got %v", err)
	}
	if err := cfg.ValidateKodoResourceCount(maxDiscoveredKodoResources + 1); err == nil || !strings.Contains(err.Error(), "safety limit") {
		t.Fatalf("expected discovered resource safety limit, got %v", err)
	}
}

func TestCDNFusionAdmissionIsSkippedForUnverifiedStatistics(t *testing.T) {
	cfg := defaults()
	cfg.CDN.Enabled = true
	cfg.CDN.StatisticsTimezoneVerified = false
	cfg.Collection.CDNFusionMaxQPS = 0.000001

	if err := cfg.ValidateCDNResourceCount(1_000); err != nil {
		t.Fatalf("timezone-gated CDN set should not consume a statistics call budget: %v", err)
	}
	if err := cfg.ValidateCDNResourceCount(maxDiscoveredCDNDomains + 1); err == nil || !strings.Contains(err.Error(), "safety limit") {
		t.Fatalf("expected discovered CDN domain safety limit, got %v", err)
	}
}

func TestCDNAdmissionUsesOnlyActiveDomainsForStatisticsBudget(t *testing.T) {
	cfg := defaults()
	cfg.CDN.Enabled = true
	cfg.CDN.StatisticsTimezoneVerified = true
	cfg.CDN.MonitoringUnitsVerified = false
	cfg.Collection.CDNFusionMaxQPS = 0.01

	if err := cfg.ValidateCDNResourceCounts(1_000, 1); err != nil {
		t.Fatalf("inactive inventory must not consume the statistics call budget: %v", err)
	}
	if err := cfg.ValidateCDNResourceCounts(1, 2); err == nil || !strings.Contains(err.Error(), "must not exceed") {
		t.Fatalf("expected active/discovered consistency error, got %v", err)
	}
}

func TestCDNAdmissionAcceptsValidatedProductionScale(t *testing.T) {
	cfg := defaults()
	cfg.CDN.Enabled = true
	cfg.CDN.StatisticsTimezoneVerified = true
	cfg.CDN.MonitoringUnitsVerified = true

	if err := cfg.ValidateCDNResourceCounts(290, 290); err != nil {
		t.Fatalf("290 active CDN domains exceeded the bounded cold-start budget: %v", err)
	}
}

func TestDurationAcceptsDays(t *testing.T) {
	var duration Duration
	if err := duration.UnmarshalText([]byte("40d")); err != nil {
		t.Fatal(err)
	}
	if duration.Value() != 40*24*time.Hour {
		t.Fatalf("duration=%s", duration.Value())
	}
}

func TestRealtimeStalenessMustLeaveRoomForSourceLag(t *testing.T) {
	cfg := defaults()
	cfg.Credentials = map[string]CredentialConfig{"main": {AccessKeyEnv: "AK", SecretKeyEnv: "SK"}}
	cfg.CDN.Enabled = true
	cfg.CDN.Credential = "main"
	cfg.CDN.StatisticsTimezoneVerified = true
	cfg.Collection.SourceLag = Duration(30 * time.Minute)
	cfg.Collection.StaleAfter.Realtime = Duration(30 * time.Minute)
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "longest enabled realtime interval") {
		t.Fatalf("expected incompatible freshness window error, got %v", err)
	}
}

func TestExampleConfigLoads(t *testing.T) {
	cfg, err := Load("../../configs/qiniu-exporter.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Kodo.Enabled || !cfg.CDN.Enabled || !cfg.Billing.Enabled {
		t.Fatal("example must exercise all three modules")
	}
	if cfg.Kodo.Credential != "main" || cfg.CDN.Credential != "main" || cfg.Billing.Credential != "main" {
		t.Fatal("example modules must share the main credential")
	}
}

func TestBillingResourcePackAllowlistIsExactAndUnique(t *testing.T) {
	cfg := defaults()
	cfg.Credentials = map[string]CredentialConfig{"main": {AccessKeyEnv: "AK", SecretKeyEnv: "SK"}}
	cfg.Billing.Enabled = true
	cfg.Billing.Credential = "main"
	cfg.Billing.ResourcePackAllowlist = []ResourcePackAllowlist{
		{Item: "CDN", Zone: "mainland", AvailableTime: "all", Unit: "GB"},
		{Item: "CDN", Zone: "mainland", AvailableTime: "all", Unit: "GB"},
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "duplicated") {
		t.Fatalf("expected duplicate resource-pack tuple error, got %v", err)
	}
}

func TestBillingResourcePackAllowlistHasSeriesBudget(t *testing.T) {
	cfg := defaults()
	cfg.Credentials = map[string]CredentialConfig{"main": {AccessKeyEnv: "AK", SecretKeyEnv: "SK"}}
	cfg.Billing.Enabled = true
	cfg.Billing.Credential = "main"
	for index := 0; index <= maxBillingResourcePackAllowlist; index++ {
		cfg.Billing.ResourcePackAllowlist = append(cfg.Billing.ResourcePackAllowlist, ResourcePackAllowlist{
			Item: "item-" + strconv.Itoa(index), Zone: "mainland", AvailableTime: "all", Unit: "GB",
		})
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected resource-pack series budget error, got %v", err)
	}
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
