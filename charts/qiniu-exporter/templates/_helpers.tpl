{{/* Expand the chart name. */}}
{{- define "qiniu-exporter.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Create a fully qualified application name. */}}
{{- define "qiniu-exporter.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/* Chart label. */}}
{{- define "qiniu-exporter.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Stable selector labels. */}}
{{- define "qiniu-exporter.selectorLabels" -}}
app.kubernetes.io/name: {{ include "qiniu-exporter.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/* Common labels. */}}
{{- define "qiniu-exporter.labels" -}}
{{- $labels := mergeOverwrite (dict) .Values.commonLabels (include "qiniu-exporter.selectorLabels" . | fromYaml) (dict
  "helm.sh/chart" (include "qiniu-exporter.chart" .)
  "app.kubernetes.io/managed-by" .Release.Service
  "app.kubernetes.io/component" "exporter"
) }}
{{- if .Chart.AppVersion }}
{{- $_ := set $labels "app.kubernetes.io/version" .Chart.AppVersion }}
{{- end }}
{{- toYaml $labels }}
{{- end }}

{{/* ServiceAccount name. */}}
{{- define "qiniu-exporter.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "qiniu-exporter.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/* Container image reference. */}}
{{- define "qiniu-exporter.image" -}}
{{- if .Values.image.digest }}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest }}
{{- else }}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) }}
{{- end }}
{{- end }}

{{/* Configuration resource name. */}}
{{- define "qiniu-exporter.configName" -}}
{{- default (include "qiniu-exporter.fullname" .) .Values.config.existingSecret.name }}
{{- end }}

{{/* Add user labels without permitting severity to be replaced. */}}
{{- define "qiniu-exporter.alertLabels" -}}
{{- $labels := mergeOverwrite (dict) .root.Values.prometheusRule.ruleLabels (dict "severity" .severity) }}
{{- toYaml $labels }}
{{- end }}
