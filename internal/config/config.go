package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"go.yaml.in/yaml/v3"
)

const (
	maxConfigBytes                  = 1 << 20
	maxBillingResourcePackAllowlist = 200
	maxDiscoveredKodoResources      = 200
	maxDiscoveredCDNDomains         = 2000
	MaxFirstRequestUtilization      = 0.8
)

type Duration time.Duration

func (d *Duration) UnmarshalText(text []byte) error {
	valueText := string(text)
	if strings.HasSuffix(valueText, "d") && !strings.Contains(valueText[:len(valueText)-1], ".") {
		days, err := strconv.Atoi(strings.TrimSuffix(valueText, "d"))
		if err != nil {
			return err
		}
		*d = Duration(time.Duration(days) * 24 * time.Hour)
		return nil
	}
	value, err := time.ParseDuration(valueText)
	if err != nil {
		return err
	}
	*d = Duration(value)
	return nil
}

func (d Duration) Value() time.Duration { return time.Duration(d) }

type Config struct {
	Server      ServerConfig                `yaml:"server"`
	Credentials map[string]CredentialConfig `yaml:"credentials"`
	Kodo        KodoConfig                  `yaml:"kodo"`
	CDN         CDNConfig                   `yaml:"cdn"`
	Billing     BillingConfig               `yaml:"billing"`
	Collection  CollectionConfig            `yaml:"collection"`
}

type ServerConfig struct {
	Listen string `yaml:"listen"`
}

type CredentialConfig struct {
	AccessKeyEnv  string `yaml:"access_key_env"`
	AccessKeyFile string `yaml:"access_key_file"`
	SecretKeyEnv  string `yaml:"secret_key_env"`
	SecretKeyFile string `yaml:"secret_key_file"`
}

type Credentials struct {
	AccessKey string
	SecretKey string
}

type KodoConfig struct {
	Enabled                    bool     `yaml:"enabled"`
	Credential                 string   `yaml:"credential"`
	StatisticsTimezoneVerified bool     `yaml:"statistics_timezone_verified"`
	StorageClasses             []string `yaml:"storage_classes"`
}

type CDNConfig struct {
	Enabled                    bool   `yaml:"enabled"`
	Credential                 string `yaml:"credential"`
	StatisticsTimezoneVerified bool   `yaml:"statistics_timezone_verified"`
	MonitoringUnitsVerified    bool   `yaml:"monitoring_units_verified"`
}

type BillingConfig struct {
	Enabled               bool                    `yaml:"enabled"`
	Credential            string                  `yaml:"credential"`
	Timezone              string                  `yaml:"timezone"`
	ResourcePackAllowlist []ResourcePackAllowlist `yaml:"resource_pack_allowlist"`
}

// ResourcePackAllowlist is an exact, account-specific label tuple. Qiniu's
// resource-pack API returns display strings, so only configured tuples may
// become Prometheus labels.
type ResourcePackAllowlist struct {
	Item          string `yaml:"item"`
	Zone          string `yaml:"zone"`
	AvailableTime string `yaml:"available_time"`
	Unit          string `yaml:"unit"`
}

type CollectionConfig struct {
	SourceLag               Duration         `yaml:"source_lag"`
	Intervals               IntervalConfig   `yaml:"intervals"`
	StaleAfter              StaleAfterConfig `yaml:"stale_after"`
	KodoMaxQPS              float64          `yaml:"kodo_max_qps"`
	CDNFusionMaxQPS         float64          `yaml:"cdn_fusion_max_qps"`
	QiniuAPIHostMaxQPS      float64          `yaml:"qiniu_api_host_max_qps"`
	BillingMaxQPS           float64          `yaml:"billing_max_qps"`
	LimiterBurst            int              `yaml:"limiter_burst"`
	FirstRequestUtilization float64          `yaml:"first_request_utilization"`
	KodoMaxConcurrency      int              `yaml:"kodo_max_concurrency"`
	CDNMaxConcurrency       int              `yaml:"cdn_max_concurrency"`
	BillingMaxConcurrency   int              `yaml:"billing_max_concurrency"`
}

type IntervalConfig struct {
	Discovery     Duration `yaml:"discovery"`
	KodoCapacity  Duration `yaml:"kodo_capacity"`
	KodoActivity  Duration `yaml:"kodo_activity"`
	CDNMonitoring Duration `yaml:"cdn_monitoring"`
	CDNAnalytics  Duration `yaml:"cdn_analytics"`
}

type StaleAfterConfig struct {
	Realtime         Duration `yaml:"realtime"`
	BillingBalance   Duration `yaml:"billing_balance"`
	BillingDaily     Duration `yaml:"billing_daily"`
	BillingFinalized Duration `yaml:"billing_finalized"`
}

func Load(path string) (*Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, maxConfigBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	if len(content) > maxConfigBytes {
		return nil, fmt.Errorf("config exceeds %d bytes", maxConfigBytes)
	}

	cfg := defaults()
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, errors.New("decode config: multiple YAML documents are not allowed")
		}
		return nil, fmt.Errorf("decode config trailing document: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func defaults() Config {
	return Config{
		Server:  ServerConfig{Listen: ":9106"},
		Billing: BillingConfig{Timezone: "Asia/Shanghai"},
		Collection: CollectionConfig{
			SourceLag:               Duration(10 * time.Minute),
			Intervals:               IntervalConfig{Duration(time.Hour), Duration(30 * time.Minute), Duration(30 * time.Minute), Duration(30 * time.Minute), Duration(30 * time.Minute)},
			StaleAfter:              StaleAfterConfig{Duration(time.Hour), Duration(3 * time.Hour), Duration(36 * time.Hour), Duration(40 * 24 * time.Hour)},
			KodoMaxQPS:              1,
			CDNFusionMaxQPS:         5,
			QiniuAPIHostMaxQPS:      10,
			BillingMaxQPS:           1,
			LimiterBurst:            1,
			FirstRequestUtilization: 0.8,
			KodoMaxConcurrency:      1,
			CDNMaxConcurrency:       4,
			BillingMaxConcurrency:   1,
		},
	}
}

func (c *Config) Validate() error {
	if c.Server.Listen == "" {
		return errors.New("server.listen is required")
	}
	if !c.Kodo.Enabled && !c.CDN.Enabled && !c.Billing.Enabled {
		return errors.New("at least one module must be enabled")
	}
	if c.Collection.LimiterBurst != 1 {
		return errors.New("collection.limiter_burst must be 1")
	}
	if !positiveFinite(c.Collection.FirstRequestUtilization) || c.Collection.FirstRequestUtilization > MaxFirstRequestUtilization {
		return errors.New("collection.first_request_utilization must be in (0, 0.8]")
	}
	if !positiveFinite(c.Collection.KodoMaxQPS) || !positiveFinite(c.Collection.CDNFusionMaxQPS) || !positiveFinite(c.Collection.QiniuAPIHostMaxQPS) || !positiveFinite(c.Collection.BillingMaxQPS) {
		return errors.New("all collection QPS limits must be positive")
	}
	if c.Collection.KodoMaxQPS > 1 || c.Collection.CDNFusionMaxQPS > 5 || c.Collection.QiniuAPIHostMaxQPS > 10 || c.Collection.BillingMaxQPS > 1 {
		return errors.New("collection QPS limits may only be lowered from the safe defaults")
	}
	if c.Collection.KodoMaxConcurrency <= 0 || c.Collection.CDNMaxConcurrency <= 0 || c.Collection.BillingMaxConcurrency <= 0 {
		return errors.New("all collection concurrency limits must be positive")
	}
	if c.Collection.KodoMaxConcurrency > 1 || c.Collection.CDNMaxConcurrency > 4 || c.Collection.BillingMaxConcurrency > 1 {
		return errors.New("collection concurrency limits may only be lowered from the safe defaults")
	}
	if c.Collection.SourceLag.Value() < 5*time.Minute {
		return errors.New("collection.source_lag must be at least 5m")
	}
	intervals := []time.Duration{
		c.Collection.Intervals.Discovery.Value(),
		c.Collection.Intervals.KodoCapacity.Value(),
		c.Collection.Intervals.KodoActivity.Value(),
		c.Collection.Intervals.CDNMonitoring.Value(),
		c.Collection.Intervals.CDNAnalytics.Value(),
	}
	for _, interval := range intervals {
		if interval < 5*time.Minute {
			return errors.New("all collection.intervals durations must be at least 5m")
		}
	}
	if c.Collection.Intervals.Discovery.Value() < 15*time.Minute {
		return errors.New("collection.intervals.discovery must be at least 15m")
	}
	staleDurations := []time.Duration{
		c.Collection.StaleAfter.Realtime.Value(),
		c.Collection.StaleAfter.BillingBalance.Value(),
		c.Collection.StaleAfter.BillingDaily.Value(),
		c.Collection.StaleAfter.BillingFinalized.Value(),
	}
	for _, duration := range staleDurations {
		if duration <= 0 {
			return errors.New("all collection.stale_after durations must be positive")
		}
	}
	maximumRealtimeInterval := time.Duration(0)
	if c.Kodo.Enabled && c.Kodo.StatisticsTimezoneVerified {
		maximumRealtimeInterval = max(maximumRealtimeInterval, c.Collection.Intervals.KodoCapacity.Value(), c.Collection.Intervals.KodoActivity.Value())
	}
	if c.CDN.Enabled && c.CDN.StatisticsTimezoneVerified {
		maximumRealtimeInterval = max(maximumRealtimeInterval, c.Collection.Intervals.CDNAnalytics.Value())
		if c.CDN.MonitoringUnitsVerified {
			maximumRealtimeInterval = max(maximumRealtimeInterval, c.Collection.Intervals.CDNMonitoring.Value())
		}
	}
	if maximumRealtimeInterval > 0 {
		minimumStaleAfter := c.Collection.SourceLag.Value() + 5*time.Minute + maximumRealtimeInterval + maximumRealtimeInterval/10
		if c.Collection.StaleAfter.Realtime.Value() < minimumStaleAfter {
			return fmt.Errorf("collection.stale_after.realtime must be at least source_lag + 5m + 110%% of the longest enabled realtime interval (%s)", minimumStaleAfter)
		}
	}

	if c.Kodo.Enabled {
		if err := c.validateCredentialRef("kodo", c.Kodo.Credential); err != nil {
			return err
		}
		if len(c.Kodo.StorageClasses) == 0 {
			return errors.New("kodo.storage_classes must not be empty")
		}
		allowed := []string{"standard", "ia", "archive", "deep_archive", "archive_ir", "intelligent_tiering"}
		seen := map[string]bool{}
		for _, class := range c.Kodo.StorageClasses {
			if !slices.Contains(allowed, class) || seen[class] {
				return fmt.Errorf("invalid or duplicate kodo storage class %q", class)
			}
			seen[class] = true
		}
	}
	if c.CDN.Enabled {
		if err := c.validateCredentialRef("cdn", c.CDN.Credential); err != nil {
			return err
		}
	}
	if c.Billing.Enabled {
		if err := c.validateCredentialRef("billing", c.Billing.Credential); err != nil {
			return err
		}
		if c.Billing.Timezone != "Asia/Shanghai" {
			return errors.New("billing.timezone must be Asia/Shanghai")
		}
		if len(c.Billing.ResourcePackAllowlist) > maxBillingResourcePackAllowlist {
			return fmt.Errorf("billing.resource_pack_allowlist exceeds %d tuples", maxBillingResourcePackAllowlist)
		}
		seen := make(map[string]struct{}, len(c.Billing.ResourcePackAllowlist))
		for index, pack := range c.Billing.ResourcePackAllowlist {
			labels := []string{pack.Item, pack.Zone, pack.AvailableTime, pack.Unit}
			for _, label := range labels {
				if label == "" || strings.TrimSpace(label) != label || len(label) > 128 || containsControl(label) {
					return fmt.Errorf("billing.resource_pack_allowlist[%d] has an invalid label", index)
				}
			}
			key := strings.Join(labels, "\x00")
			if _, exists := seen[key]; exists {
				return fmt.Errorf("billing.resource_pack_allowlist[%d] is duplicated", index)
			}
			seen[key] = struct{}{}
		}
	}
	return nil
}

func containsControl(value string) bool {
	return strings.IndexFunc(value, unicode.IsControl) >= 0
}

func positiveFinite(value float64) bool {
	return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func (c *Config) validateCredentialRef(module, name string) error {
	credential, ok := c.Credentials[name]
	if name == "" || !ok {
		return fmt.Errorf("%s.credential %q is not defined", module, name)
	}
	if err := credential.validate(); err != nil {
		return fmt.Errorf("credentials.%s: %w", name, err)
	}
	return nil
}

func (c CredentialConfig) validate() error {
	if (c.AccessKeyEnv == "") == (c.AccessKeyFile == "") {
		return errors.New("set exactly one of access_key_env/access_key_file")
	}
	if (c.SecretKeyEnv == "") == (c.SecretKeyFile == "") {
		return errors.New("set exactly one of secret_key_env/secret_key_file")
	}
	return nil
}

func (c *Config) ValidateKodoResourceCount(resourceCount int) error {
	if resourceCount < 0 {
		return errors.New("kodo discovered resource count must not be negative")
	}
	if resourceCount > maxDiscoveredKodoResources {
		return fmt.Errorf("kodo discovery exceeds the safety limit of %d bucket resources", maxDiscoveredKodoResources)
	}
	if !c.Kodo.Enabled {
		return nil
	}
	utilization := c.Collection.FirstRequestUtilization
	discoveryPages := max(1, (resourceCount+99)/100)
	required := float64(discoveryPages) / discoveryTimeout(c.Collection.Intervals.Discovery.Value()).Seconds()
	if c.Kodo.StatisticsTimezoneVerified && resourceCount > 0 {
		resources, classes := float64(resourceCount), float64(len(c.Kodo.StorageClasses))
		required += resources * (4.0/collectionTimeout(c.Collection.Intervals.KodoActivity.Value()).Seconds() + 2.0*classes/collectionTimeout(c.Collection.Intervals.KodoCapacity.Value()).Seconds())
	}
	if required > c.Collection.KodoMaxQPS*utilization {
		return fmt.Errorf("kodo call budget %.3f QPS exceeds first-request budget %.3f QPS", required, c.Collection.KodoMaxQPS*utilization)
	}
	return nil
}

func (c *Config) ValidateCDNResourceCount(resourceCount int) error {
	return c.ValidateCDNResourceCounts(resourceCount, resourceCount)
}

func (c *Config) ValidateCDNResourceCounts(discoveredCount, activeCount int) error {
	if discoveredCount < 0 || activeCount < 0 {
		return errors.New("cdn discovered and active resource counts must not be negative")
	}
	if activeCount > discoveredCount {
		return errors.New("cdn active resource count must not exceed discovered resource count")
	}
	if discoveredCount > maxDiscoveredCDNDomains {
		return fmt.Errorf("cdn discovery exceeds the safety limit of %d domain resources", maxDiscoveredCDNDomains)
	}
	if !c.CDN.Enabled || !c.CDN.StatisticsTimezoneVerified || activeCount == 0 {
		return nil
	}
	utilization := c.Collection.FirstRequestUtilization
	domains := float64(activeCount)
	required := 3 * domains / collectionTimeout(c.Collection.Intervals.CDNAnalytics.Value()).Seconds()
	if c.CDN.MonitoringUnitsVerified {
		batches := float64((activeCount + 49) / 50)
		required += 2 * batches / collectionTimeout(c.Collection.Intervals.CDNMonitoring.Value()).Seconds()
	}
	if required > c.Collection.CDNFusionMaxQPS*utilization {
		return fmt.Errorf("cdn call budget %.3f QPS exceeds first-request budget %.3f QPS", required, c.Collection.CDNFusionMaxQPS*utilization)
	}
	return nil
}

func discoveryTimeout(interval time.Duration) time.Duration {
	return min(5*time.Minute, interval/2)
}

func collectionTimeout(interval time.Duration) time.Duration {
	return interval * 4 / 5
}

func (c CredentialConfig) Resolve() (Credentials, error) {
	accessKey, err := resolveSecret(c.AccessKeyEnv, c.AccessKeyFile)
	if err != nil {
		return Credentials{}, fmt.Errorf("resolve access key: %w", err)
	}
	secretKey, err := resolveSecret(c.SecretKeyEnv, c.SecretKeyFile)
	if err != nil {
		return Credentials{}, fmt.Errorf("resolve secret key: %w", err)
	}
	return Credentials{AccessKey: accessKey, SecretKey: secretKey}, nil
}

func resolveSecret(environment, path string) (string, error) {
	var value string
	if environment != "" {
		value = os.Getenv(environment)
		if value == "" {
			return "", fmt.Errorf("environment variable %s is empty", environment)
		}
	} else {
		clean := filepath.Clean(path)
		content, err := os.ReadFile(clean)
		if err != nil {
			return "", err
		}
		value = strings.TrimSpace(string(content))
		if value == "" {
			return "", errors.New("secret file is empty")
		}
	}
	return value, nil
}
