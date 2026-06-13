{{/*
================================================================================
Asker — observability helper library (M5, OPT-IN Prometheus + Grafana).

Everything here renders ONLY when observability.enabled is true (default false),
so the default chart output is unchanged. Naming mirrors the wave-0/stateful
contract: the dialable Service metadata.name is the bare name ("prometheus",
"grafana") so the Grafana datasource URL http://prometheus:9090 resolves exactly
as it does in the compose overlay; Deployment/ConfigMap names ARE release-prefixed
via asker.fullname.

These pods carry the chart-wide labels plus component=prometheus|grafana and
asker.dev/tier=observability, so the Prometheus->health-port NetworkPolicy can
select the Prometheus pod by its component label (same pattern as the app tier).
================================================================================
*/}}

{{/*
asker.obs.enabled — master gate. Returns "true"/"" (Helm truthiness). Default off.
Usage: {{- if eq (include "asker.obs.enabled" .) "true" }}
*/}}
{{- define "asker.obs.enabled" -}}
{{- $o := .Values.observability | default dict -}}
{{- if $o.enabled }}true{{ end -}}
{{- end -}}

{{/*
asker.obs.netpolEnabled — render the Prometheus->service ingress allow? Requires
BOTH observability.enabled AND networkPolicy.enabled AND observability.networkPolicy
.enabled (default true). Returns "true"/"".
*/}}
{{- define "asker.obs.netpolEnabled" -}}
{{- $o := .Values.observability | default dict -}}
{{- $onp := $o.networkPolicy | default dict -}}
{{- $onpOn := true -}}
{{- if hasKey $onp "enabled" -}}{{- $onpOn = $onp.enabled -}}{{- end -}}
{{- if and (eq (include "asker.obs.enabled" .) "true") (eq (include "asker.netpol.enabled" .) "true") $onpOn -}}true{{- end -}}
{{- end -}}

{{/*
asker.obs.fullname — release-prefixed name for an observability workload object.
e.g. release "asker" + "prometheus" -> "asker-prometheus".
Usage: {{ include "asker.obs.fullname" (list . "prometheus") }}
*/}}
{{- define "asker.obs.fullname" -}}
{{- $root := index . 0 -}}
{{- $name := index . 1 -}}
{{- printf "%s-%s" (include "asker.fullname" $root) $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
asker.obs.labels / asker.obs.selectorLabels — chart-wide labels + component + tier.
*/}}
{{- define "asker.obs.labels" -}}
{{- $root := index . 0 -}}
{{- $name := index . 1 -}}
{{ include "asker.labels" $root }}
app.kubernetes.io/component: {{ $name }}
asker.dev/tier: observability
{{- end -}}

{{- define "asker.obs.selectorLabels" -}}
{{- $root := index . 0 -}}
{{- $name := index . 1 -}}
{{ include "asker.selectorLabels" $root }}
app.kubernetes.io/component: {{ $name }}
{{- end -}}

{{/*
asker.obs.storageClass — resolve the PVC storageClass for an obs component.
Per-component persistence.storageClass wins, else chart-wide
persistence.storageClass, default "" (cluster default => omit the field).
Usage: include "asker.obs.storageClass" (list $root $persistenceDict) | nindent N
*/}}
{{- define "asker.obs.storageClass" -}}
{{- $root := index . 0 -}}
{{- $p := index . 1 | default dict -}}
{{- $cls := $p.storageClass | default $root.Values.persistence.storageClass -}}
{{- if $cls -}}
storageClassName: {{ $cls | quote }}
{{- end -}}
{{- end -}}
