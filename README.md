# qiniu_exporter

`qiniu_exporter` 是面向 Prometheus 的七牛云只读 exporter，覆盖：

- Kodo（七牛对象存储）的容量、对象数、GET/PUT 请求速率和出网速率。
- CDN 的带宽、流量、请求量、状态码和缓存命中率。
- 账户余额、每日预估费用、资源包余量和最近已结算月费用。

它只调用固定的统计和财务查询接口。Bucket 和域名必须在配置中显式列出；代码不包含资源发现、创建、删除、更新、上下线、刷新、预取或订单操作。

完整的接口取舍、指标语义和调用预算见 [DESIGN.md](./DESIGN.md)。

## 运行

要求 Go 1.23 或更高版本。

```bash
cp configs/qiniu-exporter.example.yaml config.yaml
export QINIU_ACCESS_KEY='...'
export QINIU_SECRET_KEY='...'
go run ./cmd/qiniu-exporter --config.file=config.yaml
```

生产环境建议通过 Secret 文件或环境变量注入 AK/SK。配置文件不接受明文密钥，也不要把密钥放进命令行参数。Kodo、CDN 和 Billing 默认共用这一组凭据；启用 Billing 时必须使用具有财务权限的管理员账号凭据，普通子用户应关闭 `billing.enabled`。若需要隔离管理员凭据，建议把 Billing 单独部署为一个 exporter 实例；该实例仍使用相同的 `QINIU_ACCESS_KEY`、`QINIU_SECRET_KEY` 变量名，并通过网络策略限制出口和抓取来源。

HTTP 端点：

- `/metrics`：只读内存快照，不会在 scrape 路径调用七牛 API。
- `/healthz`：进程存活。
- `/readyz`：配置、调度器和 HTTP 服务已经就绪；不把上游短暂故障当作进程未就绪。

Prometheus 示例：

```yaml
scrape_configs:
  - job_name: qiniu
    static_configs:
      - targets: [qiniu-exporter:9106]
        labels:
          qiniu_account: production
```

一个进程只采集一个七牛账号。账号别名作为 Prometheus target label 注入，不在每条业务指标中重复添加。

最小 Docker 部署示例（先从示例复制并校验 `config.yaml`）：

```bash
docker build -t qiniu-exporter:local .
docker run --rm --read-only --cap-drop=ALL \
  -p 9106:9106 \
  --env-file ./qiniu-exporter.env \
  -v "$PWD/config.yaml:/etc/qiniu-exporter/config.yaml:ro" \
  qiniu-exporter:local
```

### Docker Compose

仓库内的 [`compose.yaml`](./compose.yaml) 默认从源码构建镜像，只把端口发布到宿主机的 `127.0.0.1:9106`，并通过 Compose secrets 向非 root、只读容器注入一组凭据。先准备配置和仅当前宿主机用户可进入的密钥目录：

```bash
cp configs/qiniu-exporter.compose.yaml config.yaml
install -d -m 0700 secrets
read -rsp 'Qiniu AK: ' QINIU_COMPOSE_AK && printf '%s' "$QINIU_COMPOSE_AK" > secrets/qiniu_access_key && unset QINIU_COMPOSE_AK && echo
read -rsp 'Qiniu SK: ' QINIU_COMPOSE_SK && printf '%s' "$QINIU_COMPOSE_SK" > secrets/qiniu_secret_key && unset QINIU_COMPOSE_SK && echo
chmod 0444 secrets/*
```

编辑 `config.yaml` 中的 bucket、region、CDN 域名和可选资源包 allowlist。时区与单位门禁只有在和七牛控制台核对后才能开启，然后启动并检查端点：

```bash
docker compose up -d --build
docker compose logs -f qiniu-exporter
curl --fail http://127.0.0.1:9106/healthz
curl --fail http://127.0.0.1:9106/readyz
curl --fail http://127.0.0.1:9106/metrics
```

停止服务使用 `docker compose down`。Compose 的文件型 secret 使用只读 bind mount：为使 UID `65532` 的容器进程可读，密钥文件是 `0444`，但父目录保持 `0700`，其他宿主机用户无法进入。更新密钥时先临时 `chmod 0600`，写入后再恢复 `0444`。`config.yaml`、`*.env` 和 `secrets/` 已加入 `.gitignore`；不要把真实 AK/SK 写入 `.env`、Compose、镜像或仓库。Billing 自动复用同一组 secret，不需要额外凭据文件。

如需使用已发布镜像，可设置 `QINIU_EXPORTER_IMAGE`；若要对外发布 exporter 端口，显式设置 `QINIU_EXPORTER_BIND_ADDRESS`，并同时限制防火墙和 Prometheus 抓取来源。

## Grafana 与 Helm

- Grafana Dashboard：导入 [`grafana/qiniu_exporter.json`](./grafana/qiniu_exporter.json)，选择 Prometheus 数据源和 `job`/`instance` 变量。
- Helm Chart：参见 [`charts/qiniu-exporter/README.md`](./charts/qiniu-exporter/README.md)。Chart 支持内联非敏感配置或现有 Secret，并可选创建 ServiceMonitor 和 PrometheusRule。

## CI/CD

GitHub Actions 会在 Pull Request 和 `main` 分支执行格式检查、Go 测试、竞态检测、Helm lint/render、Prometheus 规则校验以及 `linux/amd64`、`linux/arm64` 镜像冒烟测试。推送 `main` 或语义化 `v*` 标签会发布多架构镜像到当前仓库的 GitHub Container Registry；版本标签还会创建 GitHub Release、二进制压缩包、校验和与 Helm Chart 包。

## 限流策略

所有上游请求（包括分页和重试）都经过账号/Host 硬限流；Billing 还叠加独立的 1 QPS 子限流。正常采集与最多两次重试分别受独立的 80% attempt-0 和 20% retry 预算约束，并共同受 100% 硬上限约束；Billing 以 Host/子限流中的较小值切分预算，调低任一硬上限或首请求占比都不会反向扩大重试池。响应体完整读取期间也占用并发槽。

CDN monitoring 每批最多查询 50 个域名。`400032` 无效域名错误会触发二分隔离，每轮最多 16 个 batch attempt；确认的坏域名在进程生命周期内负缓存，未决域名留到下轮，健康域名继续更新。429 或 `403024` 会触发 Host 级 cooldown 和减半降速，五分钟无新限流后逐步恢复。

新 monitoring 文档没有明确声明两个响应数组的单位。完成控制台对照前保持 `cdn.monitoring_units_verified: false`：exporter 不会调用 bandwidth/flow 接口，也不会发布带单位的 monitoring 指标；在时区门禁已经开启时，analytics 请求量、状态码和命中率不受这个单位门禁影响。确认带宽为 bit/s、流量为五分钟桶 Byte 后再设为 `true`。

Kodo/CDN 的部分时间参数或响应点没有明确时区。示例配置中的 `statistics_timezone_verified` 默认关闭；在测试账号对照控制台确认 exporter 按 `Asia/Shanghai` 解释后，分别设为 `true`。门禁未通过的模块不会调用七牛 API，也不会暴露永久为 0 的 collector success，而会记录 `timezone_unverified` 跳过计数。

配置加载时会根据 bucket、存储类型和域名数量进行调用预算准入；预算超限时进程拒绝启动。提高代码内的安全上限不受支持，应先缩小 allowlist、降低指标范围或向七牛申请配额。

## 数据与告警语义

七牛返回的历史时间桶和账务快照都导出为 Gauge，不伪装成 exporter 生命周期内的 Counter。上游失败不会把业务值写成 0：最后一次成功快照会保留到 `stale_after`，过期后停止导出业务样本。若上游持续返回同一个冻结时间桶，新请求成功也不会刷新其数据寿命。

资源包的 `item/zone/available_time/unit` 会成为 Prometheus label，因此必须在 `billing.resource_pack_allowlist` 中逐项配置精确四元组；列表为空时 exporter 不调用资源包接口。启用前先从控制台核对账号实际值。示例规则不会把未启用的资源包 collector 当作数据缺失。

告警应同时检查：

- `qiniu_exporter_collector_success`
- `qiniu_exporter_collector_last_success_timestamp_seconds`
- `qiniu_exporter_collector_stale_after_seconds`
- `qiniu_exporter_data_timestamp_seconds`
- 对应的 `qiniu_exporter_resource_*` 指标

CDN/Kodo 公开统计 API 不提供实时请求时延。实时 DNS/TLS/HTTP 可用性应使用 Blackbox Exporter 或应用指标补充。

## 联调门槛

上线前需要使用测试账号完成一次真实 API PoC：

1. 验证 Kodo、CDN 和 Billing 的 IAM 权限与签名。
2. 对照控制台确认 CDN monitoring 的带宽/流量单位。
3. 确认 Kodo/CDN 无时区字段的时间点与 `Asia/Shanghai` 解释一致，再开启对应 `statistics_timezone_verified`。
4. 对照控制台核对一组容量、对象数、状态码、命中率、余额和月账单。

仓库测试使用脱敏 fixture 和本地 HTTP 模拟，不需要真实 AK/SK，也不会访问七牛 API。

## 验证

```bash
go test ./...
go test -race ./...
go vet ./...
```
