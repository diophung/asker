{{/*
================================================================================
Asker — NetworkPolicy helper library (M4 wave 1: default-deny + least-privilege).

ADR-016 turns the internal trust boundary (ADR-009 §4) from topological
("everything on one compose network") into ENFORCED: a default-deny-all-ingress
policy for the release namespace (templates/netpol/default-deny.yaml), plus one
explicit INGRESS allow policy per protected pod that names exactly which peers
may connect to it on which port. The call graph in the M4 brief is phrased
caller -> callee:port; that maps to an ingress policy ON the callee allowing
FROM the caller — the canonical least-privilege NetworkPolicy shape. The source
of truth for who-dials-whom is deploy/compose/docker-compose.yml.

Model:
  - APP services are targeted/selected by their wave-0 component label via
    asker.serviceSelectorLabels (the same labels the Deployment/Service use), so
    a policy's podSelector matches exactly the wave-0 pods.
  - DEPENDENCY pods (postgres/redis/minio/vespa/tei/kafka/vault/keycloak) are
    targeted by a configurable selector (.Values.networkPolicy.deps.<dep>) so
    this wave stays decoupled from the sibling stateful-layer builder's exact
    labels. Defaults assume those StatefulSets carry the chart-wide
    app.kubernetes.io/part-of: asker plus app.kubernetes.io/component: <dep>.
  - The INGRESS CONTROLLER peer (for the public edge -> gateway/web) is
    environment-specific, so it is a configurable namespace/pod selector
    (.Values.networkPolicy.ingressController.*) with a permissive,
    documented default (namespaceSelector only).

Egress is intentionally NOT default-denied (that would also block DNS and every
dependency dial whose pod labels the sibling builder owns). Targeted egress
(DNS for all, connector-hub external egress) is added separately and the
dep-egress least-privilege policies are gated behind networkPolicy.restrictEgress
(default false). See ADR-016 "what is enforced now vs prod-config".

All helpers take (list $root $svc) like the wave-0 workload helpers.
================================================================================
*/}}

{{/*
--------------------------------------------------------------------------------
asker.netpol.enabled — is the NetworkPolicy feature on? Gate every object on it.
Returns a non-empty string when on, empty when off (Helm truthiness). Default on.
Usage: {{- if (include "asker.netpol.enabled" .) }}
--------------------------------------------------------------------------------
*/}}
{{- define "asker.netpol.enabled" -}}
{{- $np := .Values.networkPolicy | default dict -}}
{{- if hasKey $np "enabled" -}}
{{- if $np.enabled }}true{{ end -}}
{{- else -}}
true
{{- end -}}
{{- end -}}

{{/*
--------------------------------------------------------------------------------
asker.netpol.restrictEgress — is the optional least-privilege egress posture on?
Default false: wave 1 ships ingress-default-deny; the app->dep egress allow
policies render only when this is true (so dep dials are not prematurely cut off
before the sibling stateful builder's pod labels are pinned). Returns truthy str.
--------------------------------------------------------------------------------
*/}}
{{- define "asker.netpol.restrictEgress" -}}
{{- $np := .Values.networkPolicy | default dict -}}
{{- if $np.restrictEgress }}true{{ end -}}
{{- end -}}

{{/*
--------------------------------------------------------------------------------
asker.netpol.allowName — release-scoped NetworkPolicy name for a target's allow
policy. e.g. release "asker" + "query" -> "asker-query-allow".
--------------------------------------------------------------------------------
*/}}
{{- define "asker.netpol.allowName" -}}
{{- $root := index . 0 -}}
{{- $svc := index . 1 -}}
{{- printf "%s-%s-allow" (include "asker.fullname" $root) $svc | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
--------------------------------------------------------------------------------
asker.netpol.appPeer — a `- podSelector:` ingress/egress peer matching an APP
service's pods (its wave-0 component label) within this release. Emitted as one
list item under `from:` / `to:`. Usage:
  {{- include "asker.netpol.appPeer" (list $root "gateway") | nindent 8 }}
--------------------------------------------------------------------------------
*/}}
{{- define "asker.netpol.appPeer" -}}
{{- $root := index . 0 -}}
{{- $svc := index . 1 -}}
- podSelector:
    matchLabels:
      {{- include "asker.serviceSelectorLabels" (list $root $svc) | nindent 6 }}
{{- end -}}

{{/*
--------------------------------------------------------------------------------
asker.netpol.depPeer — a `- podSelector:` peer matching a DEPENDENCY's pods,
using the configurable .Values.networkPolicy.deps.<dep> selector (default:
part-of asker + component <dep>). Emitted as one list item under `from:` / `to:`.
Usage: {{- include "asker.netpol.depPeer" (list $root "postgres") | nindent 8 }}
--------------------------------------------------------------------------------
*/}}
{{- define "asker.netpol.depPeer" -}}
{{- $root := index . 0 -}}
{{- $dep := index . 1 -}}
{{- $np := $root.Values.networkPolicy | default dict -}}
{{- $deps := $np.deps | default dict -}}
{{- $depCfg := index $deps $dep | default dict -}}
{{- $sel := $depCfg.podSelector | default (dict "matchLabels" (dict "app.kubernetes.io/part-of" "asker" "app.kubernetes.io/component" $dep)) -}}
- podSelector:
    {{- toYaml $sel | nindent 4 }}
{{- end -}}

{{/*
--------------------------------------------------------------------------------
asker.netpol.ingressControllerPeer — the public ingress-controller peer for the
edge allow policy (-> gateway/web). Environment-specific, so configurable via
.Values.networkPolicy.ingressController.{namespaceSelector,podSelector}. The
default selects an entire namespace named "ingress-nginx" by the well-known
kubernetes.io/metadata.name label (no podSelector) — override per environment
(traefik, a cloud LB controller, or a stricter pod label). Emitted as one list
item under `from:`.
--------------------------------------------------------------------------------
*/}}
{{- define "asker.netpol.ingressControllerPeer" -}}
{{- $root := index . -}}
{{- $np := $root.Values.networkPolicy | default dict -}}
{{- $ic := $np.ingressController | default dict -}}
{{- $nsSel := $ic.namespaceSelector | default (dict "matchLabels" (dict "kubernetes.io/metadata.name" "ingress-nginx")) -}}
- namespaceSelector:
    {{- toYaml $nsSel | nindent 4 }}
  {{- with $ic.podSelector }}
  podSelector:
    {{- toYaml . | nindent 4 }}
  {{- end }}
{{- end -}}

{{/*
--------------------------------------------------------------------------------
asker.netpol.dnsEgress — egress rule allowing DNS (UDP/TCP 53) to kube-dns, so
Service name resolution keeps working under any egress restriction. Emitted as a
list item under `egress:`. The kube-dns peer is configurable
(.Values.networkPolicy.dns.{namespaceSelector,podSelector}) with the conventional
kube-system / k8s-app: kube-dns default.
--------------------------------------------------------------------------------
*/}}
{{- define "asker.netpol.dnsEgress" -}}
{{- $root := index . -}}
{{- $np := $root.Values.networkPolicy | default dict -}}
{{- $dns := $np.dns | default dict -}}
{{- $nsSel := $dns.namespaceSelector | default (dict "matchLabels" (dict "kubernetes.io/metadata.name" "kube-system")) -}}
{{- $podSel := $dns.podSelector | default (dict "matchLabels" (dict "k8s-app" "kube-dns")) -}}
- to:
    - namespaceSelector:
        {{- toYaml $nsSel | nindent 8 }}
      podSelector:
        {{- toYaml $podSel | nindent 8 }}
  ports:
    - protocol: UDP
      port: 53
    - protocol: TCP
      port: 53
{{- end -}}
