{{/*
================================================================================
Asker — self-hosted STATEFUL DEPENDENCY template library (M4 wave 1).

Renders the cloud-agnostic, self-hosted defaults for the six stateful deps the
compose stack runs (deploy/compose/docker-compose.yml is the source of truth for
image / env / ports / healthchecks):

  postgres  StatefulSet  postgres:17                pg_isready exec probes
  redis     StatefulSet  redis:7                    redis-cli ping exec probes
  minio     StatefulSet  minio/minio:RELEASE...     GET /minio/health/live
  keycloak  Deployment   quay.io/keycloak/keycloak  GET /health/ready on mgmt :9000
  tei       Deployment   text-embeddings-inference  GET /health  (+ model-cache PVC)

(Redpanda/Kafka is Strimzi-operator managed per ADR-003 §1 / ADR-014 §4 and is
NOT rendered here; Vespa is the Vespa K8s operator's concern. config.kafkaBrokers
/ config.vespaUrl point at whatever the operator exposes.)

DESIGN (matches the wave-0 contract):
  - The Service metadata.name == the bare compose DNS name (postgres, redis,
    minio, keycloak, tei) so the shared ConfigMap addresses (postgresHost
    "postgres", redisAddr "redis:6379", minioEndpoint "minio:9000",
    teiUrl "http://tei:80", the gateway OIDC JWKS "http://keycloak:8080/...")
    resolve UNCHANGED. We deliberately do NOT release-prefix the Service name —
    same rule as asker.serviceName for the app tier (ADR-014 §3).
  - StatefulSet/Deployment/headless-Service objects ARE release-prefixed via
    asker.fullname (e.g. "asker-postgres") so workload objects are release-scoped.
  - Each dep is gated behind a values toggle <dep>.deploy (DEFAULT true). Flip it
    false to use a managed/external instance and point the matching config.* /
    deps address value at the external host (ADR-014 §4 / §5).
  - PVCs use .Values.persistence.storageClass (chart-wide, default "" => cluster
    default StorageClass). No provider class name anywhere (ADR-014 §5).
  - Credentials come from the EXISTING dev Secret (asker.secretName): Postgres
    POSTGRES_USER/PASSWORD and MinIO MINIO_ACCESS_KEY/SECRET_KEY (mapped to the
    server's MINIO_ROOT_USER/PASSWORD). Dev defaults match compose (asker/asker,
    asker-minio/asker-minio-secret).

VALUES INTERFACE (NEW keys — see the wave-1 issues; all read DEFENSIVELY via
`default` so the chart renders before the integrator merges them into values.yaml):

  persistence.storageClass            (EXISTING; "" => cluster default)
  stateful:
    postgres:  { deploy, image, storage, resources, podSecurityContext,
                 securityContext, nodeSelector, tolerations, affinity }
    redis:     { deploy, image, storage, resources, ... }
    minio:     { deploy, image, storage, console{enabled}, resources, ... }
    keycloak:  { deploy, image, javaOptsHeap, resources, ... } (Deployment, no PVC)
    tei:       { deploy, image, modelId, args[], storage, resources, ... }
                 (Deployment + model-cache PVC; dev profile sets the small modelId)
================================================================================
*/}}

{{/*
--------------------------------------------------------------------------------
asker.stateful.cfg — the per-dep values block, defaulted to an empty dict so
every field can be `dig`/`default`-ed even before values.yaml carries the block.
Usage: $c := include ... ; better: use `asker.stateful.get` which returns the dict.
--------------------------------------------------------------------------------
*/}}
{{- define "asker.stateful.get" -}}
{{- $root := index . 0 -}}
{{- $dep := index . 1 -}}
{{- $sf := $root.Values.stateful | default dict -}}
{{- index $sf $dep | default dict | toYaml -}}
{{- end -}}

{{/*
asker.stateful.enabled — is this dep self-hosted? <dep>.deploy, DEFAULT true.
Returns "true"/"false" (string) for use with `eq`.
*/}}
{{- define "asker.stateful.enabled" -}}
{{- $root := index . 0 -}}
{{- $dep := index . 1 -}}
{{- $sf := $root.Values.stateful | default dict -}}
{{- $c := index $sf $dep | default dict -}}
{{- if hasKey $c "deploy" -}}
{{- $c.deploy | toString -}}
{{- else -}}
true
{{- end -}}
{{- end -}}

{{/*
asker.stateful.fullname — release-prefixed name for the StatefulSet/Deployment.
e.g. release "asker" + dep "postgres" -> "asker-postgres".
*/}}
{{- define "asker.stateful.fullname" -}}
{{- $root := index . 0 -}}
{{- $dep := index . 1 -}}
{{- printf "%s-%s" (include "asker.fullname" $root) $dep | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
asker.stateful.labels / asker.stateful.selectorLabels — reuse the chart-wide
helpers, adding component=<dep> and tier=stateful. Mirrors asker.serviceLabels.
*/}}
{{- define "asker.stateful.labels" -}}
{{- $root := index . 0 -}}
{{- $dep := index . 1 -}}
{{ include "asker.labels" $root }}
app.kubernetes.io/component: {{ $dep }}
asker.dev/tier: stateful
{{- end -}}

{{- define "asker.stateful.selectorLabels" -}}
{{- $root := index . 0 -}}
{{- $dep := index . 1 -}}
{{ include "asker.selectorLabels" $root }}
app.kubernetes.io/component: {{ $dep }}
{{- end -}}

{{/*
asker.stateful.storageClass — resolve the PVC storageClass. Per-dep
<dep>.storage.className wins, else the chart-wide persistence.storageClass,
default "" (cluster default). Emits a `storageClassName:` line ONLY when a
non-empty class is set, so "" correctly means "cluster default" (omitting the
field) rather than binding to a class literally named "".
Usage: include "asker.stateful.storageClass" (list $root $dep) | nindent N
*/}}
{{- define "asker.stateful.storageClass" -}}
{{- $root := index . 0 -}}
{{- $dep := index . 1 -}}
{{- $sf := $root.Values.stateful | default dict -}}
{{- $c := index $sf $dep | default dict -}}
{{- $st := $c.storage | default dict -}}
{{- $cls := $st.className | default $root.Values.persistence.storageClass -}}
{{- if $cls -}}
storageClassName: {{ $cls | quote }}
{{- end -}}
{{- end -}}

{{/*
asker.stateful.headlessService — the headless (clusterIP None) Service a
StatefulSet requires for stable pod DNS. Named "<dep>-headless". Selects the dep
pods and publishes the same ports. NOT the dialable name — that's the ClusterIP
Service below, named for compose DNS.
Usage: include "asker.stateful.headlessService" (list $root $dep $portsList)
where $portsList is a list of {name, port, [targetPort], [appProtocol]}.
*/}}
{{- define "asker.stateful.headlessService" -}}
{{- $root := index . 0 -}}
{{- $dep := index . 1 -}}
{{- $ports := index . 2 -}}
apiVersion: v1
kind: Service
metadata:
  name: {{ printf "%s-headless" $dep }}
  labels:
    {{- include "asker.stateful.labels" (list $root $dep) | nindent 4 }}
spec:
  clusterIP: None
  publishNotReadyAddresses: true
  selector:
    {{- include "asker.stateful.selectorLabels" (list $root $dep) | nindent 4 }}
  ports:
    {{- range $ports }}
    - name: {{ .name }}
      port: {{ .port }}
      targetPort: {{ .targetPort | default .name }}
      protocol: {{ .protocol | default "TCP" }}
      {{- with .appProtocol }}
      appProtocol: {{ . }}
      {{- end }}
    {{- end }}
{{- end -}}

{{/*
asker.stateful.clusterIPService — the dialable ClusterIP Service. metadata.name
== the bare compose DNS name ($dep) so config.* addresses resolve unchanged.
Usage: include "asker.stateful.clusterIPService" (list $root $dep $portsList)
*/}}
{{- define "asker.stateful.clusterIPService" -}}
{{- $root := index . 0 -}}
{{- $dep := index . 1 -}}
{{- $ports := index . 2 -}}
apiVersion: v1
kind: Service
metadata:
  name: {{ $dep }}
  labels:
    {{- include "asker.stateful.labels" (list $root $dep) | nindent 4 }}
spec:
  type: ClusterIP
  selector:
    {{- include "asker.stateful.selectorLabels" (list $root $dep) | nindent 4 }}
  ports:
    {{- range $ports }}
    - name: {{ .name }}
      port: {{ .port }}
      targetPort: {{ .targetPort | default .name }}
      protocol: {{ .protocol | default "TCP" }}
      {{- with .appProtocol }}
      appProtocol: {{ . }}
      {{- end }}
    {{- end }}
{{- end -}}

{{/*
asker.stateful.podSecurityContext / asker.stateful.containerSecurityContext —
secure defaults overridable per dep. We DO set fsGroup so the mounted PVC is
writable by the (often non-root, image-fixed) server uid. Postgres(999)/Redis(999)
/MinIO/Keycloak/TEI run as their image's own uid; we provide an fsGroup default
and let each dep override podSecurityContext entirely if needed.
*/}}
{{- define "asker.stateful.containerSecurityContext" -}}
{{- $root := index . 0 -}}
{{- $dep := index . 1 -}}
{{- $sf := $root.Values.stateful | default dict -}}
{{- $c := index $sf $dep | default dict -}}
{{- $default := dict "allowPrivilegeEscalation" false "capabilities" (dict "drop" (list "ALL")) -}}
{{- $c.securityContext | default $default | toYaml -}}
{{- end -}}
