{{/*
THE CONTROLLER TEMPLATES core-provider renders, mounted into its pod at /tmp/assets (#651).

Each of these ConfigMaps is IMMUTABLE and named by the digest of its own data, and the core-provider
Deployment's volumes name it. So a pod reads exactly the templates its own spec names, for as long as
it lives. Before, the names were fixed and the kubelet refreshed the files in the RUNNING pod: between
helm's write and the new pod taking over, the OLD core-provider rendered the NEW chart's templates
("<no value>" for a key it does not know, a template file it does not know never applied, the new
controller image rolled out under the old binary's decisions), for as long as the new pod could not
start.

helm deletes the previous names last. A running pod keeps its files when a ConfigMap its volume names
is gone, because the volume is not `optional`: the kubelet fails the refresh before touching them.
NEVER mark these volumes optional: the kubelet would then replace the files with nothing.

core-provider.assetData is the ONE definition of each ConfigMap's data, and core-provider.assetName is
derived from it, so data and name cannot diverge. If they did, a changed data under an unchanged name
would be refused by the apiserver ("field is immutable when `immutable` is set").

Usage: include "core-provider.assetData" (dict "root" . "asset" "cdc-deployment")
*/}}
{{- define "core-provider.assetData" -}}
{{- include (printf "core-provider.assetData.%s" .asset) .root -}}
{{- end -}}

{{/*
core-provider.assetName is the ConfigMap's name: <fullname>-<asset>-<digest of its data>.
*/}}
{{- define "core-provider.assetName" -}}
{{- printf "%s-%s-%s" (include "core-provider.fullname" .root) .asset (include "core-provider.assetData" . | sha256sum | trunc 10) -}}
{{- end -}}

{{/*
core-provider.assetConfigMap is the whole ConfigMap. The labels are metadata, which an immutable
ConfigMap may still change, and are not part of the digest.
*/}}
{{- define "core-provider.assetConfigMap" -}}
apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ include "core-provider.assetName" . }}
  labels:
    {{- include "core-provider.labels" .root | nindent 4 }}
immutable: true
data:
{{ include "core-provider.assetData" . | indent 2 }}
{{- end -}}

{{- define "core-provider.assetData.cdc-deployment" -}}
deployment.yaml: |
{{ tpl (.Files.Get "assets/cdc/deployment.yaml") . | indent 2 }}
# docs/design/cdc-pooling.md §5c: a pool's Deployment, rendered by core-provider only for a pool
# (read from the mounted cdc-deployment ConfigMap as pool-deployment.yaml).
pool-deployment.yaml: |
{{ tpl (.Files.Get "assets/cdc/pool-deployment.yaml") . | indent 2 }}
# The chart's cdc.pool block (values.yaml), read by core-provider to size a pool: it reads no chart
# values at runtime, so the block travels beside the asset it sizes. overrides.<pool> is deep-merged
# over default in core-provider (deploy.LoadPoolSizing).
pool-sizing.json: {{ .Values.cdc.pool | toJson | quote }}
{{- end -}}

{{- define "core-provider.assetData.cdc-configmap" -}}
configmap.yaml: |
{{ tpl (.Files.Get "assets/cdc/configmap.yaml") . | indent 2 }}
# docs/design/cdc-pooling.md §5c: a pool's process environment, rendered by core-provider only
# for a pool (read from /tmp/assets/cdc-configmap/pool-configmap.yaml).
pool-configmap.yaml: |
{{ tpl (.Files.Get "assets/cdc/pool-configmap.yaml") . | indent 2 }}
{{- end -}}

{{- define "core-provider.assetData.rbac-configmap" -}}
{{- $root := . -}}
{{- range $i, $f := list "serviceaccount" "clusterrole" "clusterrolebinding" "compositiondefinition-role" "compositiondefinition-rolebinding" "secret-role" "secret-rolebinding" "lease-role" "lease-rolebinding" "authn-serviceaccount" -}}
{{- if $i }}
{{ end -}}
{{ $f }}.yaml: |
{{ tpl ($root.Files.Get (printf "assets/cdc/rbac/%s.yaml" $f)) $root | indent 2 }}
{{- end -}}
{{- end -}}

{{- define "core-provider.assetData.jsonschema-configmap" -}}
configmap.yaml: |-
{{ tpl (.Files.Get "assets/jsonSchemaConfigmap.yaml") . | indent 2 }}
{{- end -}}

{{- define "core-provider.assetData.service-configmap" -}}
service.yaml: |
{{ tpl (.Files.Get "assets/cdc/service.yaml") . | indent 2 }}
{{- end -}}
