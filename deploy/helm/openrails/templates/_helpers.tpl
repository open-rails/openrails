{{- define "openrails.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "openrails.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else if contains (include "openrails.name" .) .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name (include "openrails.name" .) | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{- define "openrails.selectorLabels" -}}
app.kubernetes.io/name: {{ include "openrails.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "openrails.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "openrails.selectorLabels" . }}
app.kubernetes.io/version: {{ include "openrails.tag" . | trunc 63 | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "openrails.tag" -}}
{{- default .Chart.AppVersion .Values.image.tag }}
{{- end }}

{{- define "openrails.image" -}}
{{- if .Values.image.digest }}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest }}
{{- else if eq (include "openrails.tag" .) "latest" }}
{{- fail "image.tag or image.digest is required: this chart's appVersion is latest, which names no release (a published chart X.Y.Z runs vX.Y.Z)" }}
{{- else }}
{{- printf "%s:%s" .Values.image.repository (include "openrails.tag" .) }}
{{- end }}
{{- end }}

{{- define "openrails.metricsName" -}}
{{- printf "%s-metrics" (include "openrails.fullname" . | trunc 55 | trimSuffix "-") }}
{{- end }}

{{- define "openrails.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "openrails.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/* config.yaml: values.config plus the listeners the chart owns. */}}
{{- define "openrails.config" -}}
{{- $cfg := deepCopy (default dict .Values.config) }}
{{- if hasKey $cfg "host" }}
{{- fail "config.host is set by the chart: a container listens on 0.0.0.0, and the Service, Ingress and NetworkPolicy decide exposure" }}
{{- end }}
{{- range list "port" "metrics_port" }}
{{- if hasKey $cfg . }}
{{- fail (printf "config.%s is set by the chart: use ports.http and metrics.port" .) }}
{{- end }}
{{- end }}
{{- $_ := set $cfg "host" "0.0.0.0" }}
{{- $_ := set $cfg "port" (int .Values.ports.http) }}
{{- if .Values.metrics.enabled }}
{{- $_ := set $cfg "metrics_port" (int .Values.metrics.port) }}
{{- end }}
{{- toYaml $cfg }}
{{- end }}

{{/* The ServiceAccount token is mounted only for Vault's kubernetes auth, or when asked. */}}
{{- define "openrails.automountToken" -}}
{{- $vault := default dict (get (default dict .Values.config) "vault") }}
{{- or .Values.serviceAccount.automountToken (eq (get $vault "auth_method" | toString) "kubernetes") }}
{{- end }}
