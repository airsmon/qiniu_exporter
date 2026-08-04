# Grafana dashboard

[`qiniu_exporter.json`](./qiniu_exporter.json) is a ready-to-import Grafana
dashboard for `qiniu_exporter`. It covers exporter health and data freshness,
API rate limiting, Kodo, CDN, and Billing metrics.

## Import

1. In Grafana, open **Dashboards → New → Import**.
2. Upload `qiniu_exporter.json`.
3. Map `DS_PROMETHEUS` to the Prometheus data source that scrapes
   `qiniu_exporter`.
4. Select the single `qiniu_account` value first, then use `job`, `instance`,
   `bucket`, `domain`, `region`, `storage_class`, and `currency` to narrow the
   dashboard.

The dashboard requires a non-sensitive `qiniu_account` Prometheus target label
on every exporter target. Use a stable alias such as `production`; never use an
AK, account ID, email address, or other sensitive identifier. The account
selector is intentionally single-value so metrics from different Qiniu
accounts cannot be combined accidentally. Shared links can select an account
with `var-qiniu_account=production`.

The collection-gates table distinguishes persistent validation or allowlist
gates from scheduler skips that occurred in the selected time range. Kodo and
CDN business panels are expected to have no data while their timezone or unit
gate is active; successful read-only resource discovery remains visible in the
exporter health panels.

Resource-pack quantities include a `unit` label and must not be aggregated
across different units. The resource-pack status panel distinguishes an empty
allowlist, an enabled collector with zero records, and unavailable data.

The current-year Billing panels consume
`qiniu_billing_current_year_monthly_finalized_cost`. It is an instant Gauge
with `currency` and a zero-padded `month="01".."12"` label for finalized months
in the current `Asia/Shanghai` calendar year. The bar gauge, YTD total, monthly
average, and finalized-month count exclude the current-period estimate.

## Provisioning

Place `qiniu_exporter.json` in a directory watched by a Grafana dashboard
provider. The dashboard uses UID `qiniu-exporter-overview` and selects its data
source through `${DS_PROMETHEUS}`; it does not contain a hard-coded Prometheus
data-source UID.
