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
{{- else }}
{{- printf "%s:%s" .Values.image.repository (include "openrails.tag" .) }}
{{- end }}
{{- end }}

{{- define "openrails.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "openrails.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/* config.yaml: values.config plus the listener the chart owns and the overlay paths it mounts. */}}
{{- define "openrails.config" -}}
{{- $cfg := deepCopy (default dict .Values.config) }}
{{- range list "host" "port" "private_port" }}
{{- if hasKey $cfg . }}
{{- fail (printf "config.%s is set by the chart: use ports.http and metrics.port" .) }}
{{- end }}
{{- end }}
{{- $_ := set $cfg "host" "0.0.0.0" }}
{{- $_ := set $cfg "port" (int .Values.ports.http) }}
{{- if .Values.metrics.enabled }}
{{- $_ := set $cfg "private_port" (int .Values.metrics.port) }}
{{- end }}
{{- $overlays := default list (get $cfg "merchant_manifest_overlays") }}
{{- range .Values.secrets.merchantOverlays }}
{{- $overlays = append $overlays (printf "/vault/overlays/%s/%s" .name .key) }}
{{- end }}
{{- if $overlays }}
{{- $_ := set $cfg "merchant_manifest_overlays" $overlays }}
{{- end }}
{{- toYaml $cfg }}
{{- end }}

{{/* The ServiceAccount token is mounted only for Vault's kubernetes auth, or when asked. */}}
{{- define "openrails.automountToken" -}}
{{- $vault := default dict (get (default dict .Values.config) "vault") }}
{{- or .Values.serviceAccount.automountToken (eq (get $vault "auth_method" | toString) "kubernetes") }}
{{- end }}
