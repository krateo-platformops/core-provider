{{/*
Expand the name of the chart.
*/}}
{{- define "core-provider.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "core-provider.fullname" -}}
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

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "core-provider.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "core-provider.labels" -}}
helm.sh/chart: {{ include "core-provider.chart" . }}
{{ include "core-provider.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: krateo
{{- end }}

{{/*
Selector labels
*/}}
{{- define "core-provider.selectorLabels" -}}
app.kubernetes.io/name: {{ include "core-provider.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "core-provider.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "core-provider.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}


{{/*
Common labels
*/}}
{{- define "chart-inspector.labels" -}}
helm.sh/chart: {{ include "core-provider.chart" . }}
{{ include "chart-inspector.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: krateo
{{- end }}

{{/*
Selector labels
*/}}
{{- define "chart-inspector.selectorLabels" -}}
app.kubernetes.io/name: {{ include "core-provider.name" . }}-chart-inspector
app.kubernetes.io/instance: {{ .Release.Name }}-chart-inspector
{{- end }}

{{- define "chart-inspector.fullname" -}}
{{ include "core-provider.fullname" . }}-chart-inspector
{{- end }}

{{- define "chart-inspector.name" -}}
{{ include "core-provider.name" . }}-chart-inspector
{{- end }}

{{- define "chart-inspector.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "chart-inspector.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Fully-qualified image reference, honoring an optional global.imageRegistry override.
The registry host (.img.registry) is replaced by .Values.global.imageRegistry when that is set, so
a single value relocates EVERY image for a mirror / air-gapped install. Repository paths are
preserved, matching how registry replication mirrors by default. Canonical helper — identical
across all Krateo charts (shipped from the shared chart scaffold).
Usage:
  image: {{ include "krateo.image" (dict "img" .Values.image "global" .Values.global "defaultTag" .Chart.AppVersion) | quote }}
*/}}
{{- define "krateo.image" -}}
{{- $g := .global | default dict -}}
{{- $registry := $g.imageRegistry | default .img.registry -}}
{{- $tag := .img.tag | default .defaultTag -}}
{{- if $registry -}}
{{- printf "%s/%s:%s" $registry .img.repository $tag -}}
{{- else -}}
{{- printf "%s:%s" .img.repository $tag -}}
{{- end -}}
{{- end -}}

{{/*
core-provider.otlpNodeLocal is "true" when tracing/metrics export is on and no explicit otel.endpoint is
set: the Deployment then sends to the OpenTelemetry daemonset agent on ITS OWN NODE, through the
node IP and the daemonset's hostPort (Diego, 2026-11-09; the same standard for every provider).
Node-local matters: the agent's k8sattributes is node-filtered, so a span sent to another node's
agent loses its pod metadata, k8s.deployment.name included, which the audit join keys on. The
hostPort exists on every daemonset version (0.1.19 and later), unlike the <release>-local Service
(0.1.21+, named from the release). An explicit otel.endpoint wins: then this is "".
*/}}
{{- define "core-provider.otlpNodeLocal" -}}
{{- if and .Values.otel.enabled (not .Values.otel.endpoint) -}}true{{- end -}}
{{- end -}}

{{/*
core-provider.otlpPort is the daemonset's OTLP hostPort for the configured protocol: 4317 for gRPC,
4318 for http/protobuf (the default).
*/}}
{{- define "core-provider.otlpPort" -}}
{{- if eq (.Values.otel.protocol | default "http/protobuf") "grpc" -}}4317{{- else -}}4318{{- end -}}
{{- end -}}
