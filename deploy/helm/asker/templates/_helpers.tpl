{{/*
================================================================================
Asker umbrella chart — shared template helpers.

Style follows what `helm create` generates (fullname / labels / selectorLabels /
serviceAccountName / chart), EXTENDED for an umbrella chart that renders 9
stateless services from one release. The per-service helpers below take the
service name as the second pipeline arg, e.g.:

    {{ include "asker.serviceFullname" (list . "query") }}
    {{- include "asker.serviceLabels" (list . "query") | nindent 4 }}

The sibling builders (stateless-workloads + edge) MUST use these helpers so all
Deployments/Services/HPAs/PDBs share one labelling and naming scheme.

Naming contract (load-bearing — other services dial these by DNS):
  Service object name == the bare service key ("gateway", "query", "ingest",
  "enrich", "index-writer", "connector-hub", "control-plane", "clip", "web").
  This MUST equal the compose service name so in-cluster addresses such as
  "dns:///query:9200", "http://connector-hub:9300", "http://clip:9800" resolve
  unchanged. Use `asker.serviceName` for the Service metadata.name; use
  `asker.serviceFullname` (release-prefixed) for Deployment/HPA/PDB names.
================================================================================
*/}}

{{/*
Expand the name of the chart.
*/}}
{{- define "asker.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name (release-scoped).
We truncate at 63 chars because some Kubernetes name fields are limited to this
(by the DNS naming spec). If release name contains chart name it will be used as
a full name.
*/}}
{{- define "asker.fullname" -}}
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
{{- define "asker.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels (chart-wide). Applied to every object this chart renders.
*/}}
{{- define "asker.labels" -}}
helm.sh/chart: {{ include "asker.chart" . }}
{{ include "asker.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: asker
{{- end }}

{{/*
Selector labels (chart-wide). Stable across upgrades — do NOT add version here.
*/}}
{{- define "asker.selectorLabels" -}}
app.kubernetes.io/name: {{ include "asker.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use (chart-wide default).
Per-service overrides go through `asker.serviceAccountName` below.
*/}}
{{- define "asker.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "asker.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
--------------------------------------------------------------------------------
Per-service helpers. Each takes (list $root $svcName) where $svcName is the bare
service key (matches .Values.services.<svcName> and the compose service name).
--------------------------------------------------------------------------------
*/}}

{{/*
asker.serviceName — the Kubernetes Service metadata.name for a service.
This MUST equal the bare service key so cross-service DNS (query:9200,
connector-hub:9300, clip:9800, control-plane:9100, ...) resolves unchanged from
the compose topology. We deliberately do NOT release-prefix it.
*/}}
{{- define "asker.serviceName" -}}
{{- $svc := index . 1 -}}
{{- $svc -}}
{{- end }}

{{/*
asker.serviceFullname — release-scoped name for a service's Deployment/HPA/PDB.
e.g. release "asker" + svc "query" -> "asker-query". Distinct from the Service
metadata.name (which is the bare key) so workload objects are clearly scoped to
the release while the dialable Service name stays stable.
*/}}
{{- define "asker.serviceFullname" -}}
{{- $root := index . 0 -}}
{{- $svc := index . 1 -}}
{{- printf "%s-%s" (include "asker.fullname" $root) $svc | trunc 63 | trimSuffix "-" -}}
{{- end }}

{{/*
asker.serviceLabels — common labels for a service's objects, adding the
app.kubernetes.io/component label set to the service key.
*/}}
{{- define "asker.serviceLabels" -}}
{{- $root := index . 0 -}}
{{- $svc := index . 1 -}}
{{ include "asker.labels" $root }}
app.kubernetes.io/component: {{ $svc }}
{{- end }}

{{/*
asker.serviceSelectorLabels — selector labels for a service's Deployment/Service.
Adds the component label to the stable chart-wide selector labels. These are the
labels a Service selector and a Deployment .spec.selector.matchLabels must use.
*/}}
{{- define "asker.serviceSelectorLabels" -}}
{{- $root := index . 0 -}}
{{- $svc := index . 1 -}}
{{ include "asker.selectorLabels" $root }}
app.kubernetes.io/component: {{ $svc }}
{{- end }}

{{/*
asker.serviceAccountNameFor — service account name for a given service.
Falls back to the per-service serviceAccount.name, then the chart-wide SA.
*/}}
{{- define "asker.serviceAccountNameFor" -}}
{{- $root := index . 0 -}}
{{- $svc := index . 1 -}}
{{- $cfg := index $root.Values.services $svc -}}
{{- if and $cfg $cfg.serviceAccount $cfg.serviceAccount.name -}}
{{- $cfg.serviceAccount.name -}}
{{- else -}}
{{- include "asker.serviceAccountName" $root -}}
{{- end -}}
{{- end }}

{{/*
asker.image — fully qualified image reference for a service.
Composes global.imageRegistry (optional registry prefix) + the service's
image.repository + ":" + (image.tag | default .Chart.Version). Lets one global
registry knob redirect every image while per-service repo/tag still override.

  global.imageRegistry: "ghcr.io"   (may be "" to use repository as-is)
  services.<svc>.image.repository:  "asker/query"
  services.<svc>.image.tag:         "" -> defaults to chart version

Usage: {{ include "asker.image" (list . "query") }}
*/}}
{{- define "asker.image" -}}
{{- $root := index . 0 -}}
{{- $svc := index . 1 -}}
{{- $cfg := index $root.Values.services $svc -}}
{{- $registry := $root.Values.global.imageRegistry -}}
{{- $repo := $cfg.image.repository -}}
{{- $tag := $cfg.image.tag | default $root.Chart.Version -}}
{{- if $registry -}}
{{- printf "%s/%s:%s" $registry $repo $tag -}}
{{- else -}}
{{- printf "%s:%s" $repo $tag -}}
{{- end -}}
{{- end }}

{{/*
asker.imagePullPolicy — per-service pull policy with a global fallback.
Usage: {{ include "asker.imagePullPolicy" (list . "query") }}
*/}}
{{- define "asker.imagePullPolicy" -}}
{{- $root := index . 0 -}}
{{- $svc := index . 1 -}}
{{- $cfg := index $root.Values.services $svc -}}
{{- default $root.Values.global.imagePullPolicy $cfg.image.pullPolicy -}}
{{- end }}

{{/*
asker.secretName — name of the chart-managed Secret holding dev/bootstrap
credentials when secrets.strategy == "chart". The stateless-workloads builder
references this from envFrom/secretKeyRef. With strategy "external" or "vault"
this same name is the ExternalSecret/Vault-synced target the workloads consume,
so the workload templates need not branch on strategy for the Secret NAME.
Usage: {{ include "asker.secretName" . }}
*/}}
{{- define "asker.secretName" -}}
{{- default (printf "%s-secrets" (include "asker.fullname" .)) .Values.secrets.name -}}
{{- end }}
