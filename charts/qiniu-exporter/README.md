# Qiniu Exporter Helm Chart

This chart deploys `qiniu_exporter` with secure pod defaults and optional
Prometheus Operator resources. It follows the exporter's read-only model:
`/metrics` reads cached snapshots, while background pollers call only the
configured Qiniu statistics and billing APIs.

## Prerequisites

- Kubernetes and Helm 3
- Egress from the exporter Pod to the Qiniu APIs
- An existing Secret containing the Qiniu AK/SK pair
- Prometheus Operator CRDs when `serviceMonitor.enabled` or
  `prometheusRule.enabled` is enabled

The chart does not create namespaces, credentials, Prometheus Operator, or its
CRDs. It never accepts AK/SK values in `values.yaml`.

## Install with generated configuration

Create a credential Secret:

```bash
kubectl -n monitoring create secret generic qiniu-exporter-credentials \
  --from-literal=access-key='<QINIU_ACCESS_KEY>' \
  --from-literal=secret-key='<QINIU_SECRET_KEY>'
```

Create a values file containing only non-sensitive configuration:

```yaml
credentials:
  - name: main
    existingSecret:
      name: qiniu-exporter-credentials
      accessKeyKey: access-key
      secretKeyKey: secret-key

config:
  generated:
    kodo:
      enabled: true
      credential: main
      # Keep false until timestamps have been checked against the console.
      statisticsTimezoneVerified: false
      buckets:
        - name: production-assets
          region: z0
    cdn:
      enabled: true
      credential: main
      statisticsTimezoneVerified: false
      monitoringUnitsVerified: false
      domains:
        - cdn.example.com
    billing:
      # Enable only when this credential has financial API access.
      enabled: false
      credential: main
```

Install the chart:

```bash
helm upgrade --install qiniu-exporter ./charts/qiniu-exporter \
  --namespace monitoring \
  --create-namespace \
  --values qiniu-values.yaml
```

The generated ConfigMap references credential files under
`/var/run/secrets/qiniu/<credential-index>/`. The bundled Kodo, CDN, and Billing
settings all select the `main` credential and therefore use the same existing
Secret.

Defaults enable only Billing, keeping the generated exporter configuration
structurally valid, but intentionally omit a Secret name. Supply the existing
credential Secret before expecting the Pod to become Ready. For a restricted
sub-account, enable Kodo or CDN and explicitly set `config.generated.billing.enabled`
to `false`.

Do not pass AK/SK values through `--set`, values files, ConfigMaps, or
`extraEnv`. Helm persists release values in the cluster.

## Install with a complete configuration Secret

Create a Secret containing a complete exporter configuration:

```bash
kubectl -n monitoring create secret generic qiniu-exporter-config \
  --from-file=config.yaml=./config.yaml
```

Reference it from values:

```yaml
config:
  existingSecret:
    name: qiniu-exporter-config
    key: config.yaml
```

The external configuration's `server.listen` must use the same port as
`exporter.port` (default `9106`). If it references files mounted from the
chart's `credentials` list, use the documented index paths. It may instead use
environment variables supplied through `extraEnv`, but only Secret-backed
`valueFrom.secretKeyRef` entries should be used for credentials.

Helm cannot checksum external Secrets. Restart the Deployment after changing a
complete configuration or credential Secret:

```bash
kubectl -n monitoring rollout restart deployment/qiniu-exporter
```

## Prometheus Operator

Enable ServiceMonitor discovery and the built-in rules:

```yaml
serviceMonitor:
  enabled: true
  labels:
    release: kube-prometheus-stack

prometheusRule:
  enabled: true
  labels:
    release: kube-prometheus-stack
  ruleLabels:
    team: platform
```

The labels must match the selectors used by the installed Prometheus resource.
The built-in PrometheusRule mirrors `rules/qiniu-exporter.rules.yml`, including
collection failure/staleness, low balance/resource-pack, CDN quality alerts,
and CDN/Kodo recording rules. Set `prometheusRule.builtinRules: false` to use
only complete groups supplied through `prometheusRule.additionalGroups`.

## Security and operational notes

- The default Pod runs as UID/GID `65532`, uses a read-only root filesystem,
  drops every Linux capability, disables privilege escalation, applies
  `RuntimeDefault` seccomp, and does not mount a ServiceAccount token.
- Credential files are mounted read-only with mode `0440` and a Pod `fsGroup`
  of `65532`, so only the non-root process group can read them. The chart never
  renders a Kubernetes Secret.
- Startup/readiness use `/readyz`; liveness uses `/healthz`. Probes never scrape
  `/metrics` and therefore do not create scrape traffic.
- `replicaCount` is restricted to one and the Deployment strategy defaults to
  `Recreate`, so a rollout cannot temporarily multiply Qiniu API traffic or
  duplicate series. This exporter has no distributed leader election.
- A checksum annotation rolls Pods when the generated ConfigMap changes.
  External Secrets require an explicit restart.
- Keep the Kodo/CDN verification gates false until their timestamps and CDN
  monitoring units have been compared with the Qiniu console.
- Enable Billing only when the selected credential has financial API access.

## Validate

```bash
helm lint charts/qiniu-exporter
helm lint charts/qiniu-exporter \
  --values charts/qiniu-exporter/ci/test-values.yaml
helm template qiniu-exporter charts/qiniu-exporter \
  --namespace monitoring \
  --values charts/qiniu-exporter/ci/test-values.yaml
helm template qiniu-exporter charts/qiniu-exporter \
  --namespace monitoring \
  --values charts/qiniu-exporter/ci/generated-config-values.yaml
helm package charts/qiniu-exporter
```

## License

This chart is licensed under the [MIT License](./LICENSE).
