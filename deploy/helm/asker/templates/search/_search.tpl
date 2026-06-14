{{/*
================================================================================
Asker umbrella chart — search tier (Vespa on Kubernetes) helpers.

ADR-014 §4 / spec §2.7: Vespa runs on K8s with a MULTI-GROUP streaming-mode
content cluster. Asker's data is ~10M tiny per-tenant corpora; each tenant's docs
live in one streaming group (id ...g=<tenant_id>) and every query scans exactly
ONE group (vespa/README "Tenant-group model"). Modelling N content GROUPS (each a
full copy of the corpus, partitioned by Vespa's distribution) means a hot tenant's
query touches a single group's nodes rather than the whole fleet — the §2.7
horizontal-scaling property.

This is the dev/compose single-node Vespa (vespa/services.xml) grown into a real
StatefulSet: `vespa.groups` content groups x `vespa.nodesPerGroup` nodes, plus
config servers. Gated behind `vespa.deploy` (default false). The application
package (vespa/app) is deployed against the config server by a follow-up Job
running the vespa/deploy.sh flow; this wave ships the StatefulSet + Services +
grouping values + the package carried in a ConfigMap (see vespa-configmap.yaml).

Service DNS contract (ADR-014 §3): the query/document Service is named "vespa" so
config.vespaUrl ("http://vespa:8080") resolves unchanged from compose. The config
server is reachable on :19071 (same Service / same pods in this simple topology).

All keys live under `.Values.vespa.*`, read via `default`, so templates render
before the integrator merges the keys into the authoritative values.yaml.
================================================================================
*/}}

{{/*
asker.vespa.name — base name for Vespa objects (StatefulSet, headless Service,
ConfigMap). Release-prefixed so multiple releases coexist; distinct from the bare
"vespa" client-facing Service name. Usage: {{ include "asker.vespa.name" . }}
*/}}
{{- define "asker.vespa.name" -}}
{{- printf "%s-vespa" (include "asker.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end }}

{{/*
asker.vespa.labels — chart labels + a "search" component label for Vespa objects.
*/}}
{{- define "asker.vespa.labels" -}}
{{ include "asker.labels" . }}
app.kubernetes.io/component: search
{{- end }}

{{/*
asker.vespa.selectorLabels — stable selector labels for the Vespa StatefulSet +
its Services. No version label (stable across upgrades).
*/}}
{{- define "asker.vespa.selectorLabels" -}}
{{ include "asker.selectorLabels" . }}
app.kubernetes.io/component: search
{{- end }}

{{/*
asker.vespa.replicas — total Vespa node count = groups * nodesPerGroup. The
content nodes are evenly split into `vespa.groups` groups of `vespa.nodesPerGroup`
each (spec §2.7). Usage: {{ include "asker.vespa.replicas" . }}
*/}}
{{- define "asker.vespa.replicas" -}}
{{- $vespa := .Values.vespa | default dict -}}
{{- $groups := int ($vespa.groups | default 1) -}}
{{- $nodesPerGroup := int ($vespa.nodesPerGroup | default 1) -}}
{{- mul $groups $nodesPerGroup -}}
{{- end }}
