package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLoadRejectsUnknownAndMissingStaticResources(t *testing.T) {
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
`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "static allowlist") {
		t.Fatalf("expected missing allowlist error, got %v", err)
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
  domains: [cdn.example.com]
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
  domains: [cdn.example.com]
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
  domains: [cdn.example.com]
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

func TestAdmissionRejectsOversizedCDNAllowlist(t *testing.T) {
	cfg := defaults()
	cfg.Credentials = map[string]CredentialConfig{"main": {AccessKeyEnv: "AK", SecretKeyEnv: "SK"}}
	cfg.CDN.Enabled = true
	cfg.CDN.Credential = "main"
	cfg.CDN.StatisticsTimezoneVerified = true
	for i := 0; i < 500; i++ {
		cfg.CDN.Domains = append(cfg.CDN.Domains, "d"+strings.Repeat("x", i%30)+string(rune('a'+i%26))+".example.com")
	}
	// Make names unique without weakening validation.
	for i := range cfg.CDN.Domains {
		cfg.CDN.Domains[i] = strings.Replace(cfg.CDN.Domains[i], ".example", "."+strings.Repeat("n", i/26+1)+"example", 1)
	}
	cfg.Collection.CDNFusionMaxQPS = 0.1
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "call budget") {
		t.Fatalf("expected admission error, got %v", err)
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
	cfg.CDN.Domains = []string{"cdn.example.com"}
	cfg.Collection.SourceLag = Duration(30 * time.Minute)
	cfg.Collection.StaleAfter.Realtime = Duration(30 * time.Minute)
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "source_lag + 5m") {
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
