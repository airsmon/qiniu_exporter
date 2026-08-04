package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/prometheus/client_golang/prometheus"
	promcollectors "github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/qiniu/go-sdk/v7/auth"
	"qiniu-exporter/internal/app"
	"qiniu-exporter/internal/authhttp"
	"qiniu-exporter/internal/collector"
	"qiniu-exporter/internal/config"
	"qiniu-exporter/internal/limiter"
	"qiniu-exporter/internal/poller"
	"qiniu-exporter/internal/qiniu/billing"
	"qiniu-exporter/internal/qiniu/cdn"
	"qiniu-exporter/internal/qiniu/kodo"
	"qiniu-exporter/internal/snapshot"
	"qiniu-exporter/internal/telemetry"
)

var (
	version  = "dev"
	revision = "unknown"
)

func main() {
	configPath := flag.String("config.file", "config.yaml", "Path to the YAML configuration file")
	showVersion := flag.Bool("version", false, "Print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Printf("qiniu-exporter %s (%s)\n", version, revision)
		return
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(*configPath, logger); err != nil {
		logger.Error("qiniu_exporter stopped", "error", err)
		os.Exit(1)
	}
}

func run(configPath string, logger *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	registry := prometheus.NewRegistry()
	registry.MustRegister(promcollectors.NewGoCollector(), promcollectors.NewProcessCollector(promcollectors.ProcessCollectorOpts{}))
	metrics := telemetry.New(registry, version, revision)
	observer := &app.Observer{Metrics: metrics, Logger: logger}
	scheduler := poller.New(observer)

	resolved := make(map[string]config.Credentials)
	credential := func(name string) (config.Credentials, error) {
		if value, ok := resolved[name]; ok {
			return value, nil
		}
		value, err := cfg.Credentials[name].Resolve()
		if err != nil {
			return config.Credentials{}, fmt.Errorf("resolve credential %q: %w", name, err)
		}
		resolved[name] = value
		return value, nil
	}

	utilization := cfg.Collection.FirstRequestUtilization
	var qiniuAPIHostLimit, qiniuAPIFirstLimit, qiniuAPIRetryLimit *limiter.Limiter
	if cfg.CDN.Enabled || cfg.Billing.Enabled {
		qiniuAPIHostLimit, err = limiter.New(cfg.Collection.QiniuAPIHostMaxQPS, 4)
		if err != nil {
			return err
		}
		firstQPS, retryQPS := splitAttemptBudgets(utilization, cfg.Collection.QiniuAPIHostMaxQPS)
		qiniuAPIFirstLimit, err = limiter.NewRate(firstQPS)
		if err != nil {
			return err
		}
		qiniuAPIRetryLimit, err = limiter.NewRate(retryQPS)
		if err != nil {
			return err
		}
	}
	if cfg.Kodo.Enabled {
		secret, err := credential(cfg.Kodo.Credential)
		if err != nil {
			return err
		}
		hardLimit, err := limiter.New(cfg.Collection.KodoMaxQPS, cfg.Collection.KodoMaxConcurrency)
		if err != nil {
			return err
		}
		firstQPS, retryQPS := splitAttemptBudgets(utilization, cfg.Collection.KodoMaxQPS)
		firstLimit, err := limiter.NewRate(firstQPS)
		if err != nil {
			return err
		}
		retryLimit, err := limiter.NewRate(retryQPS)
		if err != nil {
			return err
		}
		doer := &authhttp.Client{
			Service: "kodo", Credentials: auth.New(secret.AccessKey, secret.SecretKey), TokenType: auth.TokenQiniu,
			AddQiniuDate: true, Policy: kodoPolicy(), FirstAttemptLimiter: firstLimit, RetryLimiter: retryLimit, HostLimiter: hardLimit, Observer: metrics, MaxRetries: 2,
		}
		client, err := kodo.NewClient(doer, "")
		if err != nil {
			return err
		}
		discoveryDoer := &authhttp.Client{
			Service: "kodo_discovery", Credentials: auth.New(secret.AccessKey, secret.SecretKey), TokenType: auth.TokenQiniu,
			AddQiniuDate: true, Policy: kodoDiscoveryPolicy(), FirstAttemptLimiter: firstLimit, RetryLimiter: retryLimit, HostLimiter: hardLimit, Observer: metrics, MaxRetries: 2,
		}
		discoverer, err := kodo.NewDiscoveryClient(discoveryDoer, "")
		if err != nil {
			return err
		}
		inventory := &snapshot.Store[[]kodo.Bucket]{}
		store := &snapshot.ResourceStore[[]kodo.GaugeSample]{}
		registry.MustRegister(collector.NewKodo(inventory, store))
		if !cfg.Kodo.StatisticsTimezoneVerified {
			logger.Warn("Kodo collectors disabled until statistics timezone semantics are verified against the Qiniu console")
		}
		if err := app.RegisterKodo(scheduler, client, discoverer, cfg, inventory, store, metrics); err != nil {
			return err
		}
	}

	if cfg.CDN.Enabled {
		secret, err := credential(cfg.CDN.Credential)
		if err != nil {
			return err
		}
		hardLimit, err := limiter.New(cfg.Collection.CDNFusionMaxQPS, cfg.Collection.CDNMaxConcurrency)
		if err != nil {
			return err
		}
		firstQPS, retryQPS := splitAttemptBudgets(utilization, cfg.Collection.CDNFusionMaxQPS)
		firstLimit, err := limiter.NewRate(firstQPS)
		if err != nil {
			return err
		}
		retryLimit, err := limiter.NewRate(retryQPS)
		if err != nil {
			return err
		}
		expectedCode := http.StatusOK
		doer := &authhttp.Client{
			Service: "cdn", Credentials: auth.New(secret.AccessKey, secret.SecretKey), TokenType: auth.TokenQBox,
			Policy: cdnPolicy(), FirstAttemptLimiter: firstLimit, RetryLimiter: retryLimit, HostLimiter: hardLimit, Observer: metrics, MaxRetries: 2, ExpectedBusinessCode: &expectedCode,
		}
		client, err := cdn.NewClient(doer, "")
		if err != nil {
			return err
		}
		discoveryDoer := &authhttp.Client{
			Service: "cdn_discovery", Credentials: auth.New(secret.AccessKey, secret.SecretKey), TokenType: auth.TokenQiniu,
			AddQiniuDate: true, Policy: cdnDiscoveryPolicy(), FirstAttemptLimiter: qiniuAPIFirstLimit, RetryLimiter: qiniuAPIRetryLimit, HostLimiter: qiniuAPIHostLimit, Observer: metrics, MaxRetries: 2,
		}
		discoverer, err := cdn.NewDomainDiscoveryClient(discoveryDoer, "")
		if err != nil {
			return err
		}
		stores := collector.CDNStores{
			Inventory:  &snapshot.Store[[]cdn.Domain]{},
			Monitoring: &snapshot.ResourceStore[collector.CDNMonitoringSnapshot]{},
			Analytics:  &snapshot.ResourceStore[collector.CDNAnalyticsSnapshot]{},
		}
		registry.MustRegister(collector.NewCDN(stores))
		if !cfg.CDN.StatisticsTimezoneVerified {
			logger.Warn("CDN collectors disabled until statistics timezone semantics are verified against the Qiniu console")
		}
		if !cfg.CDN.MonitoringUnitsVerified {
			logger.Warn("CDN monitoring collectors disabled until units are verified against the Qiniu console")
		}
		if err := app.RegisterCDN(scheduler, client, discoverer, cfg, stores, metrics); err != nil {
			return err
		}
	}

	if cfg.Billing.Enabled {
		secret, err := credential(cfg.Billing.Credential)
		if err != nil {
			return err
		}
		billingLimit, err := limiter.New(cfg.Collection.BillingMaxQPS, cfg.Collection.BillingMaxConcurrency)
		if err != nil {
			return err
		}
		firstQPS, retryQPS := splitAttemptBudgets(utilization, cfg.Collection.QiniuAPIHostMaxQPS, cfg.Collection.BillingMaxQPS)
		firstLimit, err := limiter.NewRate(firstQPS)
		if err != nil {
			return err
		}
		retryLimit, err := limiter.NewRate(retryQPS)
		if err != nil {
			return err
		}
		expectedCode := 0
		doer := &authhttp.Client{
			Service: "billing", Credentials: auth.New(secret.AccessKey, secret.SecretKey), TokenType: auth.TokenQiniu,
			Policy: billingPolicy(), FirstAttemptLimiter: firstLimit, RetryLimiter: retryLimit, HostLimiter: qiniuAPIHostLimit, EndpointLimiter: billingLimit, Observer: metrics, MaxRetries: 2, ExpectedBusinessCode: &expectedCode,
		}
		client, err := billing.NewClient(doer, "")
		if err != nil {
			return err
		}
		stores := collector.BillingStores{
			Balance:       &snapshot.Store[billing.BalanceOverview]{},
			Estimate:      &snapshot.Store[collector.BillingEstimate]{},
			ResourcePacks: &snapshot.Store[[]billing.ResourcePackMonthOverview]{},
			Finalized:     &snapshot.Store[collector.BillingFinalized]{},
			CurrentYear:   &snapshot.Store[collector.BillingFinalizedYear]{},
		}
		registry.MustRegister(collector.NewBilling(stores))
		if len(cfg.Billing.ResourcePackAllowlist) == 0 {
			logger.Warn("billing resource-pack collector disabled because billing.resource_pack_allowlist is empty")
		}
		if err := app.RegisterBilling(scheduler, client, cfg.Billing.ResourcePackAllowlist, stores, app.BillingStaleness{
			Balance: cfg.Collection.StaleAfter.BillingBalance.Value(), Daily: cfg.Collection.StaleAfter.BillingDaily.Value(), Finalized: cfg.Collection.StaleAfter.BillingFinalized.Value(),
		}, metrics); err != nil {
			return err
		}
	}

	scheduler.Run(ctx)

	var ready atomic.Bool
	ready.Store(true)
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{EnableOpenMetrics: true}))
	mux.HandleFunc("/healthz", func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/readyz", func(writer http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			http.Error(writer, "not ready", http.StatusServiceUnavailable)
			return
		}
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("ready\n"))
	})

	server := &http.Server{
		Addr: cfg.Server.Listen, Handler: mux,
		ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: time.Minute,
	}
	serveErrors := make(chan error, 1)
	go func() {
		logger.Info("qiniu_exporter listening", "address", cfg.Server.Listen)
		serveErrors <- server.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		ready.Store(false)
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownContext); err != nil {
			return fmt.Errorf("shutdown HTTP server: %w", err)
		}
		scheduler.Wait()
		return nil
	case err := <-serveErrors:
		stop()
		scheduler.Wait()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve HTTP: %w", err)
	}
}

func splitAttemptBudgets(firstUtilization float64, hardLimits ...float64) (firstQPS, retryQPS float64) {
	effectiveHardQPS := hardLimits[0]
	for _, hardLimit := range hardLimits[1:] {
		effectiveHardQPS = min(effectiveHardQPS, hardLimit)
	}
	return effectiveHardQPS * firstUtilization, effectiveHardQPS * (1 - config.MaxFirstRequestUtilization)
}

func kodoPolicy() authhttp.Policy {
	endpoints := make([]authhttp.Endpoint, 0, 14)
	classes := []kodo.StorageClass{
		kodo.StorageClassStandard, kodo.StorageClassIA, kodo.StorageClassIntelligentTiering,
		kodo.StorageClassArchiveIR, kodo.StorageClassArchive, kodo.StorageClassDeepArchive,
	}
	for _, class := range classes {
		paths, _ := kodo.EndpointsForStorageClass(class)
		endpoints = append(endpoints,
			authhttp.Endpoint{Method: http.MethodGet, Path: paths.CapacityPath, Name: "storage_" + string(class)},
			authhttp.Endpoint{Method: http.MethodGet, Path: paths.ObjectCountPath, Name: "objects_" + string(class)},
		)
	}
	endpoints = append(endpoints,
		authhttp.Endpoint{Method: http.MethodGet, Path: kodo.BlobIOPath, Name: "blob_io"},
		authhttp.Endpoint{Method: http.MethodGet, Path: kodo.RSPutPath, Name: "rs_put"},
	)
	return authhttp.Policy{Host: "api.qiniuapi.com", Endpoints: endpoints}
}

func kodoDiscoveryPolicy() authhttp.Policy {
	return authhttp.Policy{Host: "uc.qiniuapi.com", Endpoints: []authhttp.Endpoint{
		{Method: http.MethodGet, Path: kodo.BucketsPath, Name: "list_buckets"},
	}}
}

func cdnPolicy() authhttp.Policy {
	return authhttp.Policy{Host: "fusion.qiniuapi.com", Endpoints: []authhttp.Endpoint{
		{Method: http.MethodPost, Path: "/v2/tune/monitoring/bandwidth", Name: "monitoring_bandwidth"},
		{Method: http.MethodPost, Path: "/v2/tune/monitoring/flow", Name: "monitoring_flow"},
		{Method: http.MethodPost, Path: "/v2/tune/loganalyze/reqcount", Name: "request_count"},
		{Method: http.MethodPost, Path: "/v2/tune/loganalyze/statuscode", Name: "status_code"},
		{Method: http.MethodPost, Path: "/v2/tune/loganalyze/hitmiss", Name: "hit_miss"},
	}}
}

func cdnDiscoveryPolicy() authhttp.Policy {
	return authhttp.Policy{Host: "api.qiniu.com", Endpoints: []authhttp.Endpoint{
		{Method: http.MethodGet, Path: "/domain", Name: "list_domains"},
	}}
}

func billingPolicy() authhttp.Policy {
	return authhttp.Policy{Host: "api.qiniu.com", Endpoints: []authhttp.Endpoint{
		{Method: http.MethodGet, Path: "/billing-api/v1/account/balance-overview", Name: "balance_overview"},
		{Method: http.MethodGet, Path: "/billing-api/v2/bill/snapshot", Name: "bill_snapshot"},
		{Method: http.MethodGet, Path: "/billing-api/v1/respack/month-overview", Name: "resource_pack_overview"},
		{Method: http.MethodGet, Path: "/billing-api/v2/bill/detail", Name: "bill_detail"},
	}}
}
