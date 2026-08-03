# Grafana dashboard

[`qiniu_exporter.json`](./qiniu_exporter.json) is a ready-to-import Grafana
dashboard for `qiniu_exporter`. It covers exporter health and data freshness,
API rate limiting, Kodo, CDN, and Billing metrics.

## Import

1. In Grafana, open **Dashboards → New → Import**.
2. Upload `qiniu_exporter.json`.
3. Map `DS_PROMETHEUS` to the Prometheus data source that scrapes
   `qiniu_exporter`.
4. Use the `job`, `instance`, `bucket`, `domain`, `region`, `storage_class`, and
   `currency` variables to filter the dashboard.

The dashboard does not assume an account label. Add an account name as a
Prometheus scrape target label and extend the dashboard variables when multiple
accounts share the same Prometheus server.

Resource-pack quantities include a `unit` label and must not be aggregated
across different units.

## Provisioning

Place `qiniu_exporter.json` in a directory watched by a Grafana dashboard
provider. The dashboard uses UID `qiniu-exporter-overview` and selects its data
source through `${DS_PROMETHEUS}`; it does not contain a hard-coded Prometheus
data-source UID.
