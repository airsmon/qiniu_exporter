# qiniu_exporter

[![CI](https://github.com/airsmon/qiniu_exporter/actions/workflows/ci.yml/badge.svg)](https://github.com/airsmon/qiniu_exporter/actions/workflows/ci.yml)
[![govulncheck](https://github.com/airsmon/qiniu_exporter/actions/workflows/govulncheck.yml/badge.svg)](https://github.com/airsmon/qiniu_exporter/actions/workflows/govulncheck.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](./LICENSE)

Prometheus exporter for read-only operational and billing metrics from Qiniu
Cloud.

`qiniu_exporter` collects metrics from three Qiniu services:

- **Kodo** (object storage): capacity, object count, request rate, and egress
  rate.
- **CDN**: bandwidth, traffic, request rate, HTTP response rate, and cache hit
  metrics.
- **Billing**: account balance, estimated cost, resource-pack usage, and the
  most recent finalized monthly cost.

The exporter calls only a fixed allowlist of statistics and billing endpoints.
It does not discover, create, update, delete, publish, refresh, prefetch, or
otherwise manage Qiniu resources.

## Quick start

Go 1.23 or later is required to run from source.

```bash
cp configs/qiniu-exporter.example.yaml config.yaml
# Edit config.yaml: select modules and replace all example resources.
# Set billing.enabled to false unless the credential has financial API access.

export QINIU_ACCESS_KEY='...'
export QINIU_SECRET_KEY='...'

go run ./cmd/qiniu-exporter --config.file=config.yaml
```

In another terminal, verify the process and inspect the exported metrics:

```bash
curl --fail http://127.0.0.1:9106/healthz
curl --fail http://127.0.0.1:9106/readyz
curl --fail http://127.0.0.1:9106/metrics
```

The example enables all three modules to demonstrate the complete configuration.
Set `billing.enabled` to `false` before starting unless the supplied credential
has financial API access. Keep the Kodo/CDN verification gates disabled until
their timestamps and units have been checked against the Qiniu console.

## Collectors and metrics

Collectors are enabled and scoped through the YAML configuration file. Kodo
buckets, CDN domains, storage classes, and billing resource-pack tuples must be
listed explicitly; the exporter performs no resource discovery.

| Collector | Metric families | Labels | Enablement and behavior |
| --- | --- | --- | --- |
| Kodo storage | `qiniu_kodo_storage_bytes`, `qiniu_kodo_objects` | `bucket`, `region`, `storage_class` | Requires the Kodo timezone gate. |
| Kodo activity | `qiniu_kodo_requests_per_second`, `qiniu_kodo_egress_bytes_per_second` | `bucket`, `region`, plus `operation` or `route` | Latest complete five-minute bucket; requires the Kodo timezone gate. |
| CDN monitoring | `qiniu_cdn_monitoring_bandwidth_bits_per_second`, `qiniu_cdn_monitoring_traffic_bytes_per_second` | `domain`, `region` | Requires the CDN timezone and monitoring-unit gates. |
| CDN requests and responses | `qiniu_cdn_requests_per_second`, `qiniu_cdn_http_responses_per_second` | `domain,region`; responses also use `code` | Requires the CDN timezone gate; status-code labels are validated. |
| CDN cache rates | `qiniu_cdn_cache_requests_per_second`, `qiniu_cdn_cache_traffic_bytes_per_second` | `domain,result` | `result` is `hit` or `miss`. |
| CDN cache ratios | `qiniu_cdn_cache_request_hit_ratio`, `qiniu_cdn_cache_traffic_hit_ratio` | `domain` | Omitted when the corresponding denominator is zero. |
| Billing balance | `qiniu_billing_available_balance`, `qiniu_billing_unpaid_amount` | `currency` | Requires an account with financial API access. |
| Billing estimate | `qiniu_billing_estimated_cost`, `qiniu_billing_estimate_period_start_timestamp_seconds`, `qiniu_billing_estimate_period_end_timestamp_seconds` | `currency` on cost | Current estimated billing period. |
| Billing resource packs | `qiniu_billing_resource_pack_records`, `qiniu_billing_resource_pack_total`, `qiniu_billing_resource_pack_used`, `qiniu_billing_resource_pack_remaining`, `qiniu_billing_resource_pack_remaining_ratio` | `item`, `zone`, `available_time`, `unit` on quantities | Disabled when `resource_pack_allowlist` is empty. Never aggregate across `unit`. |
| Billing finalized cost | `qiniu_billing_last_finalized_cost`, `qiniu_billing_last_finalized_period_start_timestamp_seconds` | `currency` on cost | Most recently available finalized month. |
| Exporter health | `qiniu_exporter_*` | Varies by metric | Collector success, freshness, API activity, rate limiting, scheduler, and build information. |

The registry also exposes the standard Go runtime and process metric families,
including `go_*` and `process_*`.

Upstream time-bucket values and billing snapshots are gauges, even when they
represent requests or traffic. They may be corrected or reset by Qiniu and are
not counters accumulated over the exporter process lifetime.

## Requirements

- Go 1.23 or later when building from source
- Qiniu Access Key and Secret Key with access to the enabled read-only APIs
- Network access to the required Qiniu API hosts
- Prometheus or an OpenMetrics-compatible scraper

Billing APIs are normally available only to an administrator account. Disable
`billing.enabled` when using a restricted sub-account. To isolate privileged
billing credentials, run Billing in a separate exporter instance while keeping
the standard `QINIU_ACCESS_KEY` and `QINIU_SECRET_KEY` variable names.

## Installation

### Build from source

```bash
git clone https://github.com/airsmon/qiniu_exporter.git
cd qiniu_exporter
go build -o qiniu-exporter ./cmd/qiniu-exporter
./qiniu-exporter --version
```

### Docker

Prepare `config.yaml`, then build the image locally:

```bash
cp configs/qiniu-exporter.example.yaml config.yaml
# Edit config.yaml before starting the container.
# Disable Billing unless the credential has financial API access.
docker build -t qiniu-exporter:local .
```

Run the exporter with a configuration file and credentials inherited from the
host environment:

```bash
export QINIU_ACCESS_KEY='...'
export QINIU_SECRET_KEY='...'

docker run --rm \
  --read-only \
  --cap-drop=ALL \
  --publish 127.0.0.1:9106:9106 \
  --env QINIU_ACCESS_KEY \
  --env QINIU_SECRET_KEY \
  --volume "$PWD/config.yaml:/etc/qiniu-exporter/config.yaml:ro" \
  qiniu-exporter:local
```

CI publishes multi-architecture images to
`ghcr.io/airsmon/qiniu_exporter`. The `main` branch produces the `main` tag;
version tags produce semantic-version image tags.

### Helm

The chart supports an existing credential Secret, generated non-sensitive
configuration, an existing complete configuration Secret, ServiceMonitor, and
PrometheusRule resources.

See [charts/qiniu-exporter/README.md](./charts/qiniu-exporter/README.md) for
installation and security guidance.

## Configuration

Copy the example configuration and replace the example resources:

```bash
cp configs/qiniu-exporter.example.yaml config.yaml
```

The configuration stores only environment-variable names or Secret file paths.
It does not accept literal AK/SK values.

```yaml
server:
  listen: ":9106"

credentials:
  main:
    access_key_env: QINIU_ACCESS_KEY
    secret_key_env: QINIU_SECRET_KEY

kodo:
  enabled: true
  credential: main
  statistics_timezone_verified: false
  buckets:
    - name: example-bucket
      region: z0
  storage_classes:
    - standard

cdn:
  enabled: true
  credential: main
  statistics_timezone_verified: false
  monitoring_units_verified: false
  domains:
    - cdn.example.com

billing:
  enabled: false
  credential: main
  timezone: Asia/Shanghai
  resource_pack_allowlist: []
```

This is an intentionally safe skeleton: Billing is disabled and the Kodo/CDN
verification gates are false, so it initially exposes exporter self-metrics but
no Qiniu business samples. Enable only the modules and gates that have been
validated for the account.

Start the exporter:

```bash
export QINIU_ACCESS_KEY='...'
export QINIU_SECRET_KEY='...'
./qiniu-exporter --config.file=config.yaml
```

Available command-line flags:

| Flag | Default | Description |
| --- | --- | --- |
| `--config.file` | `config.yaml` | Path to the YAML configuration file. |
| `--version` | `false` | Print version information and exit. |

### Verification gates

Some public Qiniu statistics documentation does not define every timestamp or
unit precisely enough to publish reliable metrics without account-specific
verification:

- Keep `kodo.statistics_timezone_verified` and
  `cdn.statistics_timezone_verified` false until API timestamps match the
  Qiniu console when interpreted in `Asia/Shanghai`.
- Keep `cdn.monitoring_units_verified` false until monitoring bandwidth and
  traffic values have been compared with the console.

A collector behind a disabled verification gate does not call its upstream API
and records a corresponding `timezone_unverified` or `units_unverified`
scheduler skip. Do not enable a gate merely to make a metric appear.

### Resource-pack allowlist

Resource-pack fields become Prometheus labels, so every permitted tuple must be
configured exactly:

```yaml
billing:
  resource_pack_allowlist:
    - item: "<exact-item-name-from-Qiniu>"
      zone: "<exact-zone-name-from-Qiniu>"
      available_time: "<exact-availability-name-from-Qiniu>"
      unit: GB
```

Replace every placeholder with the exact value returned for the account. An
empty allowlist disables resource-pack API calls and metrics.

## Docker Compose

The included [compose.yaml](./compose.yaml) builds the image from source,
publishes port `9106` on loopback, runs as UID/GID `65532`, uses a read-only
root filesystem, and mounts credentials through Compose secrets.

Prepare the configuration and credential files:

```bash
cp configs/qiniu-exporter.compose.yaml config.yaml
install -d -m 0700 secrets
read -rsp 'Qiniu AK: ' QINIU_COMPOSE_AK
printf '%s' "$QINIU_COMPOSE_AK" > secrets/qiniu_access_key
unset QINIU_COMPOSE_AK
printf '\n'
read -rsp 'Qiniu SK: ' QINIU_COMPOSE_SK
printf '%s' "$QINIU_COMPOSE_SK" > secrets/qiniu_secret_key
unset QINIU_COMPOSE_SK
printf '\n'
chmod 0444 secrets/qiniu_access_key secrets/qiniu_secret_key
```

Update the bucket, region, domain, and optional resource-pack allowlist in
`config.yaml`. Disable Billing unless the credential has financial API access,
then start the service:

```bash
docker compose up -d --build
curl --fail http://127.0.0.1:9106/healthz
curl --fail http://127.0.0.1:9106/readyz
curl --fail http://127.0.0.1:9106/metrics
```

Follow logs separately when needed:

```bash
docker compose logs -f qiniu-exporter
```

To use a published release image instead of building locally, replace
`vX.Y.Z` with a release tag or pin the image by digest:

```bash
QINIU_EXPORTER_IMAGE=ghcr.io/airsmon/qiniu_exporter:vX.Y.Z \
  docker compose up -d --no-build
```

The mutable `main` image tag is intended for testing unreleased development
builds.

Stop the service with `docker compose down`.

## Prometheus configuration

Add the exporter as a normal static or discovered Prometheus target:

```yaml
scrape_configs:
  - job_name: qiniu
    static_configs:
      - targets:
          - qiniu-exporter:9106
        labels:
          qiniu_account: production
```

The exporter exposes these HTTP endpoints:

| Endpoint | Purpose |
| --- | --- |
| `/metrics` | Cached Qiniu business metrics and exporter self-metrics. Never calls an upstream API. |
| `/healthz` | Process liveness. |
| `/readyz` | Configuration, scheduler, and HTTP server readiness. Transient Qiniu failures do not make the process unready. |

Use collector health and freshness metrics in addition to Prometheus's target
`up` metric. The bundled rules demonstrate the expected checks:

- `qiniu_exporter_collector_success`
- `qiniu_exporter_collector_last_success_timestamp_seconds`
- `qiniu_exporter_collector_stale_after_seconds`
- `qiniu_exporter_data_timestamp_seconds`
- `qiniu_exporter_resource_*`

The bundled recording and alert rules preserve Prometheus target labels, so
multiple exporter targets are evaluated independently. Set a unique
`qiniu_account` target label for each account.

## Operational model

Qiniu statistics APIs have independent update intervals, source-data delays,
and account-level rate limits. The exporter therefore polls them in the
background and publishes immutable in-memory snapshots. A Prometheus scrape of
`/metrics` never makes an upstream Qiniu API request.

This model provides predictable scrapes, per-host and per-endpoint rate
limiting, bounded retries, a shared cooldown after rate-limit responses, and
explicit collector health and data freshness metrics.

One Qiniu account per exporter process is a deployment requirement. Named
credentials are supported, but the exporter cannot verify account ownership;
all credentials selected by modules in one instance must belong to the same
account. Run one instance per account and attach an account name as a Prometheus
target label instead of adding it to every exported series.

See [DESIGN.md](./DESIGN.md) for the API selection, metric semantics, polling
schedule, cardinality limits, and call-budget calculations.

## Failure and stale-data behavior

An upstream error never replaces a business value with zero. The most recent
successful snapshot remains available until its configured `stale_after`
duration expires; expired business samples are then omitted. Exporter
self-metrics remain available throughout the failure.

When Qiniu supplies a source timestamp, freshness is based on the older of the
collection time and source-data time. Repeatedly receiving the same frozen
upstream bucket therefore cannot keep stale data alive.

Configuration validation calculates the expected request rate from the number
of buckets, storage classes, and domains. The exporter refuses to start when a
collector would exceed its safe call budget. Reduce the allowlist or collected
scope instead of increasing hard-coded protection limits.

## Dashboards and alerting rules

- Import [grafana/qiniu_exporter.json](./grafana/qiniu_exporter.json) for the
  bundled Grafana dashboard.
- Load [rules/qiniu-exporter.rules.yml](./rules/qiniu-exporter.rules.yml) for
  collector health, stale data, low balance, resource-pack, CDN, and Kodo
  alerts and recording rules.
- Enable the chart's ServiceMonitor and PrometheusRule resources when using
  Prometheus Operator.

Kodo and CDN statistics APIs do not provide real-time request latency. Use the
[Blackbox Exporter](https://github.com/prometheus/blackbox_exporter) or
application instrumentation for live DNS, TLS, HTTP availability, and latency
signals.

## Security

- Use environment variables or mounted Secret files for AK/SK. Never put real
  credentials in YAML, command-line flags, container images, Git history, or
  logs.
- Restrict Qiniu IAM permissions to the configured statistics APIs and, where
  IAM supports it, the configured resources. CDN statistics actions are
  service-scoped, so the static domain allowlist provides the operational
  boundary. The exporter does not require resource-management permissions.
- Billing may require an administrator credential. Run it separately when that
  credential should not be shared with Kodo or CDN collection.
- Bucket names, CDN domains, regions, and billing labels may be visible in
  `/metrics`. Restrict network access to the exporter endpoint.
- The HTTP server does not provide built-in TLS or authentication. Bind it to
  loopback or a private network, or place it behind an authenticated reverse
  proxy or service mesh.
- The exporter signs requests only for fixed HTTPS hosts and endpoint paths;
  the HTTP client cannot be used as a generic Qiniu API proxy.

See [.github/SECURITY.md](./.github/SECURITY.md) for vulnerability reporting.

## Development

Run the local validation suite:

```bash
go mod verify
go test ./...
go test -race ./...
go vet ./...
go build ./cmd/qiniu-exporter
```

Validate the deployment artifacts when their tools are available:

```bash
helm lint charts/qiniu-exporter --strict
helm template qiniu-exporter charts/qiniu-exporter \
  --values charts/qiniu-exporter/ci/test-values.yaml
docker compose -f compose.yaml config
```

Tests use sanitized fixtures and local HTTP servers. They do not require Qiniu
credentials and do not call Qiniu APIs.

GitHub Actions validates Go code, race safety, Helm rendering, Prometheus rules,
Docker Compose, and `linux/amd64` and `linux/arm64` container images. Semantic
version tags matching `vMAJOR.MINOR.PATCH` publish release archives, checksums,
a Helm package, and a GitHub Release.

## Contributing

Keep changes within the exporter's read-only monitoring boundary. New upstream
calls must use fixed endpoint policies, the shared limiter and retry path, and
bounded labels. Include tests for metric semantics, response validation, stale
data, and failure behavior. Run the validation commands above before opening a
pull request.

## License

This project is licensed under the [MIT License](./LICENSE).
