{{/*
================================================================================
Asker — internal TLS scaffolding helpers (M4 wave 1; ADR-016).

HONEST SCOPE. Default-deny NetworkPolicies (templates/netpol/**) make the
internal network least-privilege. TRANSPORT encryption + per-service identity
(mTLS) between in-cluster services is a separate concern that needs either:
  (a) a service mesh (Istio/Linkerd) that injects sidecars and does mTLS
      transparently — NO chart-rendered certs needed, just mesh enrolment; or
  (b) cert-manager issuing a per-service keypair that each service's TLS server/
      client loads.
This chart does NOT run a CA, does NOT terminate TLS in the Go/Python servers
today (the M0-M3 binaries speak plaintext gRPC/HTTP on the internal network), and
canNOT by itself perform an mTLS handshake. What it DOES ship, gated and OFF by
default, is the cert-manager Certificate CRs (one per service) following a fixed
Secret-naming convention, so when a mesh/cert-manager is present the certificate
material exists and the wiring is declarative rather than ad hoc. See ADR-016
"what is enforced now vs prod-config".

tls.internal.enabled (default false) gates the Certificate CRs. tls.issuer names
the cert-manager Issuer/ClusterIssuer. The CRs are cert-manager.io/v1 (a CRD), so
kubeconform skips them via -ignore-missing-schemas, exactly like the Strimzi /
Vespa-operator kinds in this chart.
================================================================================
*/}}

{{/*
--------------------------------------------------------------------------------
asker.tls.enabled — is internal-TLS cert issuance on? Default OFF (false): mTLS
is mesh/cert-manager-provided in prod; this only emits Certificate CRs when an
operator opts in. Returns truthy string when on.
--------------------------------------------------------------------------------
*/}}
{{- define "asker.tls.enabled" -}}
{{- $tls := .Values.tls | default dict -}}
{{- $internal := $tls.internal | default dict -}}
{{- if $internal.enabled }}true{{ end -}}
{{- end -}}

{{/*
--------------------------------------------------------------------------------
asker.tls.secretName — the per-service TLS Secret name convention. One keypair
Secret per service so a peer can be authenticated by its SAN. Convention:
  <fullname>-<svc>-tls   (overridable via tls.internal.secretNameTemplate)
Usage: {{ include "asker.tls.secretName" (list $root "query") }}
--------------------------------------------------------------------------------
*/}}
{{- define "asker.tls.secretName" -}}
{{- $root := index . 0 -}}
{{- $svc := index . 1 -}}
{{- printf "%s-%s-tls" (include "asker.fullname" $root) $svc | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
--------------------------------------------------------------------------------
asker.tls.dnsNames — the SANs a service's cert must carry so peers dialling it by
its in-cluster Service DNS (the bare compose name, e.g. "query", "query.<ns>",
"query.<ns>.svc", "query.<ns>.svc.<clusterDomain>") validate. Mirrors the
asker.serviceName naming contract (bare service key == Service metadata.name).
Returns a YAML list (no key). Usage:
  dnsNames:
    {{- include "asker.tls.dnsNames" (list $root "query") | nindent 4 }}
--------------------------------------------------------------------------------
*/}}
{{- define "asker.tls.dnsNames" -}}
{{- $root := index . 0 -}}
{{- $svc := index . 1 -}}
{{- $ns := $root.Release.Namespace -}}
{{- $domain := $root.Values.global.clusterDomain | default "cluster.local" -}}
- {{ $svc | quote }}
- {{ printf "%s.%s" $svc $ns | quote }}
- {{ printf "%s.%s.svc" $svc $ns | quote }}
- {{ printf "%s.%s.svc.%s" $svc $ns $domain | quote }}
{{- end -}}

{{/*
--------------------------------------------------------------------------------
asker.tls.certificate — a cert-manager.io/v1 Certificate for one service. The
issuerRef points at tls.issuer (kind tls.issuerKind, default ClusterIssuer). The
SANs cover the service's in-cluster DNS so the cert authenticates the service to
its peers (and, with the cert presented client-side too, enables mTLS). Rendered
only inside the tls.enabled gate by the caller.
Usage: {{ include "asker.tls.certificate" (list $root "query") }}
--------------------------------------------------------------------------------
*/}}
{{- define "asker.tls.certificate" -}}
{{- $root := index . 0 -}}
{{- $svc := index . 1 -}}
{{- $tls := $root.Values.tls | default dict -}}
{{- $internal := $tls.internal | default dict -}}
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: {{ include "asker.tls.secretName" (list $root $svc) }}
  labels:
    {{- include "asker.serviceLabels" (list $root $svc) | nindent 4 }}
spec:
  secretName: {{ include "asker.tls.secretName" (list $root $svc) }}
  duration: {{ $internal.duration | default "2160h" | quote }}       # 90d
  renewBefore: {{ $internal.renewBefore | default "360h" | quote }}  # 15d
  isCA: false
  privateKey:
    algorithm: {{ $internal.keyAlgorithm | default "ECDSA" }}
    size: {{ $internal.keySize | default 256 }}
  usages:
    - server auth
    - client auth
  dnsNames:
    {{- include "asker.tls.dnsNames" (list $root $svc) | nindent 4 }}
  issuerRef:
    name: {{ $tls.issuer | default "asker-internal-ca" | quote }}
    kind: {{ $tls.issuerKind | default "ClusterIssuer" | quote }}
    group: cert-manager.io
{{- end -}}
