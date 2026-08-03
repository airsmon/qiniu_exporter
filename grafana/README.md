# qiniu_exporter Grafana Dashboard

`qiniu_exporter.json` 是可直接导入或由 Grafana provisioning 加载的 Dashboard JSON，面板覆盖 Exporter 健康与数据新鲜度、API 限流、Kodo、CDN 和 Billing。

## 导入

1. 在 Grafana 中打开 **Dashboards → New → Import**。
2. 上传 `qiniu_exporter.json`。
3. 为 `DS_PROMETHEUS` 选择保存 `qiniu_exporter` 指标的 Prometheus 数据源。
4. 使用顶部的 `job`、`instance`、`bucket`、`domain`、`region`、`storage_class` 和 `currency` 变量筛选数据。

Dashboard 不假设存在账号标签；账号如需区分，应继续由 Prometheus scrape target 注入，并按环境自行扩展变量。资源包数量带有 `unit` 标签，不能跨单位求和。

使用 provisioning 时，将 JSON 放入 Grafana dashboard provider 指向的目录即可。Dashboard 使用 UID `qiniu-exporter-overview`，数据源由 `${DS_PROMETHEUS}` 变量选择，没有硬编码 Prometheus UID。
