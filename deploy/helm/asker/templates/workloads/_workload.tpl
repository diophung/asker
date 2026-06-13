{{/*
================================================================================
Asker — shared workload template library (stateless-workloads wave, M4 wave 0).

Renders the four objects every internal stateless service needs from ONE
per-service values block (.Values.services.<svc>), keeping the 7 service files
(query, ingest, enrich, index-writer, connector-hub, control-plane, clip) tiny
and uniform, and CONSISTENT with the edge wave's gateway/web templates:

  - asker.workload.deployment  -> apps/v1 Deployment
  - asker.workload.service     -> v1 Service (ClusterIP, name == compose DNS)
  - asker.workload.hpa         -> autoscaling/v2 HorizontalPodAutoscaler (toggle)
  - asker.workload.pdb         -> policy/v1 PodDisruptionBudget (toggle)
  - asker.workload.all         -> all four, joined with "---"

Each is invoked as `(list $root $svcKey)` where $svcKey indexes
.Values.services.<svcKey> (and equals the compose service name / Service DNS).
All four reuse the chart-skeleton + config helpers:
  asker.serviceName / asker.serviceFullname (helpers)
  asker.serviceLabels / asker.serviceSelectorLabels (helpers)
  asker.serviceAccountNameFor / asker.image / asker.imagePullPolicy (helpers)
  asker.configMapName (config wave) / asker.secretName (helpers)

Topology / contracts honored:
  - Service metadata.name == bare service key so peers dial query:9200,
    connector-hub:9300, clip:9800, control-plane:9100, etc. unchanged (compose).
  - Cross-service env (KAFKA_BROKERS, *_URL, *_ADDR deps, EMBEDDING_DIM/CLIP_DIM,
    MINIO_*, CONTROL_PLANE_GRPC_ADDR, HUB_*_URL, OTEL) comes from the shared
    ConfigMap via envFrom (ADR-005/013 dims are deploy-time config). Credentials
    (MINIO_ACCESS_KEY/SECRET_KEY, DATABASE_URL, ...) come from the shared Secret,
    referenced only by services that need them (services.<svc>.useSecret). The
    per-service listen addresses (QUERY_ADDR, *_HEALTH_ADDR, HUB_ADDR, ...) are
    rendered as explicit env from services.<svc>.config.* (they differ per svc).
  - Probes are httpGet on each service's documented health port/path, taken from
    services.<svc>.health.{liveness,readiness}. Probe timing is tunable per
    service (services.<svc>.probeTiming.*) with secure, generous defaults; an
    optional startupProbe (services.<svc>.startupProbe) covers model-load boot
    (enrich, clip) so liveness does not kill a still-loading pod.
  - connector-hub / control-plane mount the shared KEK at /keys (KEK_FILE) via
    services.<svc>.kek (a Secret in dev / Vault CSI in wave 1; ADR-013, ADR-009).
================================================================================
*/}}

{{/*
--------------------------------------------------------------------------------
asker.workload.probe — render one container probe from a health descriptor
{ type: httpGet|exec|grpc, port: <name>, path? } plus a timing dict
{ initialDelaySeconds, periodSeconds, timeoutSeconds, failureThreshold,
  successThreshold? }. Defaults are filled when timing is absent.
Usage: include "asker.workload.probe" (dict "h" $h "t" $timing)
--------------------------------------------------------------------------------
*/}}
{{- define "asker.workload.probe" -}}
{{- $h := .h -}}
{{- $t := .t | default dict -}}
{{- $type := $h.type | default "httpGet" -}}
{{- if eq $type "httpGet" }}
httpGet:
  path: {{ $h.path | default "/healthz" }}
  port: {{ $h.port }}
  {{- with $h.scheme }}
  scheme: {{ . }}
  {{- end }}
{{- else if eq $type "tcpSocket" }}
tcpSocket:
  port: {{ $h.port }}
{{- else if eq $type "grpc" }}
grpc:
  port: {{ $h.port }}
{{- else if eq $type "exec" }}
exec:
  command:
    {{- toYaml $h.command | nindent 4 }}
{{- end }}
initialDelaySeconds: {{ $t.initialDelaySeconds | default 5 }}
periodSeconds: {{ $t.periodSeconds | default 10 }}
timeoutSeconds: {{ $t.timeoutSeconds | default 3 }}
failureThreshold: {{ $t.failureThreshold | default 3 }}
{{- with $t.successThreshold }}
successThreshold: {{ . }}
{{- end }}
{{- end -}}

{{/*
================================================================================
Deployment
================================================================================
*/}}
{{- define "asker.workload.deployment" -}}
{{- $root := index . 0 -}}
{{- $svc := index . 1 -}}
{{- $cfg := index $root.Values.services $svc -}}
{{- if not $cfg -}}
{{- fail (printf "asker chart: .Values.services.%s is not defined" $svc) -}}
{{- end -}}
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ include "asker.serviceFullname" (list $root $svc) }}
  labels:
    {{- include "asker.serviceLabels" (list $root $svc) | nindent 4 }}
spec:
  {{- if not $cfg.autoscaling.enabled }}
  replicas: {{ $cfg.replicaCount }}
  {{- end }}
  revisionHistoryLimit: {{ $cfg.revisionHistoryLimit | default 3 }}
  selector:
    matchLabels:
      {{- include "asker.serviceSelectorLabels" (list $root $svc) | nindent 6 }}
  template:
    metadata:
      labels:
        {{- include "asker.serviceLabels" (list $root $svc) | nindent 8 }}
      annotations:
        # Roll pods when the shared config or dev secret changes (matches the
        # edge wave's gateway/web behavior).
        checksum/config: {{ include (print $root.Template.BasePath "/config/configmap.yaml") $root | sha256sum }}
        checksum/secret: {{ include (print $root.Template.BasePath "/config/secret.yaml") $root | sha256sum }}
        {{- with $cfg.podAnnotations }}
        {{- toYaml . | nindent 8 }}
        {{- end }}
    spec:
      serviceAccountName: {{ include "asker.serviceAccountNameFor" (list $root $svc) }}
      {{- with ($cfg.imagePullSecrets | default $root.Values.global.imagePullSecrets) }}
      imagePullSecrets:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      terminationGracePeriodSeconds: {{ $cfg.terminationGracePeriodSeconds | default 30 }}
      {{- with $cfg.podSecurityContext }}
      securityContext:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      containers:
        - name: {{ $svc }}
          image: {{ include "asker.image" (list $root $svc) }}
          imagePullPolicy: {{ include "asker.imagePullPolicy" (list $root $svc) }}
          {{- with $cfg.securityContext }}
          securityContext:
            {{- toYaml . | nindent 12 }}
          {{- end }}
          {{- with $cfg.command }}
          command:
            {{- toYaml . | nindent 12 }}
          {{- end }}
          {{- with $cfg.args }}
          args:
            {{- toYaml . | nindent 12 }}
          {{- end }}
          ports:
            {{- range $cfg.ports }}
            - name: {{ .name }}
              containerPort: {{ .port }}
              protocol: {{ .protocol | default "TCP" }}
            {{- end }}
          envFrom:
            # Shared, non-secret env (deps Service DNS, EMBEDDING_DIM/CLIP_DIM,
            # MINIO_ENDPOINT, OTEL, ...). Same ConfigMap the edge wave envFroms.
            - configMapRef:
                name: {{ include "asker.configMapName" $root }}
            {{- if and $cfg.useSecret (not $cfg.secretKeys) }}
            # Whole-Secret fallback (only when secretKeys is NOT set). Prefer the
            # scoped secretKeyRef path below: it pulls ONLY the credential keys a
            # service actually reads, keeping the blast radius minimal (ADR-014 §7)
            # — e.g. connector-hub gets MINIO_* only, not the Postgres/Keycloak
            # admin creds it never uses.
            - secretRef:
                name: {{ include "asker.secretName" $root }}
            {{- end }}
          {{- $extraEnv := include "asker.workload.serviceEnv" (list $root $svc) | trim }}
          {{- if $extraEnv }}
          env:
            {{- $extraEnv | nindent 12 }}
          {{- end }}
          {{- with $cfg.health.liveness }}
          livenessProbe:
            {{- include "asker.workload.probe" (dict "h" . "t" ($cfg.probeTiming).liveness) | trim | nindent 12 }}
          {{- end }}
          {{- with $cfg.health.readiness }}
          readinessProbe:
            {{- include "asker.workload.probe" (dict "h" . "t" ($cfg.probeTiming).readiness) | trim | nindent 12 }}
          {{- end }}
          {{- with $cfg.startupProbe }}
          startupProbe:
            {{- include "asker.workload.probe" (dict "h" .health "t" .timing) | trim | nindent 12 }}
          {{- end }}
          {{- with $cfg.resources }}
          resources:
            {{- toYaml . | nindent 12 }}
          {{- end }}
          {{- $mounts := include "asker.workload.volumeMounts" (list $root $svc) | trim }}
          {{- if $mounts }}
          volumeMounts:
            {{- $mounts | nindent 12 }}
          {{- end }}
      {{- $volumes := include "asker.workload.volumes" (list $root $svc) | trim }}
      {{- if $volumes }}
      volumes:
        {{- $volumes | nindent 8 }}
      {{- end }}
      {{- with $cfg.nodeSelector }}
      nodeSelector:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with $cfg.affinity }}
      affinity:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with $cfg.tolerations }}
      tolerations:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with $cfg.topologySpreadConstraints }}
      topologySpreadConstraints:
        {{- toYaml . | nindent 8 }}
      {{- end }}
{{- end -}}

{{/*
--------------------------------------------------------------------------------
asker.workload.serviceEnv — explicit per-service env. Emits the service's listen
addresses + literal knobs from services.<svc>.config.* (KEY names mapped to the
exact env vars the binaries read, from compose), then KEK_FILE when KEK is
mounted, then any free-form services.<svc>.env passthrough. Returns YAML list
items (no "env:" key) so the caller controls indentation.
--------------------------------------------------------------------------------
*/}}
{{- define "asker.workload.serviceEnv" -}}
{{- $root := index . 0 -}}
{{- $svc := index . 1 -}}
{{- $cfg := index $root.Values.services $svc -}}
{{- $c := $cfg.config | default dict -}}
{{- if eq $svc "query" }}
- name: QUERY_ADDR
  value: {{ $c.queryAddr | default ":9200" | quote }}
- name: QUERY_HEALTH_ADDR
  value: {{ $c.queryHealthAddr | default ":9201" | quote }}
{{- else if eq $svc "ingest" }}
- name: INGEST_HEALTH_ADDR
  value: {{ $c.ingestHealthAddr | default ":9501" | quote }}
{{- else if eq $svc "enrich" }}
- name: ENRICH_HEALTH_ADDR
  value: {{ $c.enrichHealthAddr | default ":9601" | quote }}
{{- else if eq $svc "index-writer" }}
- name: INDEX_WRITER_HEALTH_ADDR
  value: {{ $c.indexWriterHealthAddr | default ":9701" | quote }}
{{- else if eq $svc "connector-hub" }}
- name: HUB_ADDR
  value: {{ $c.hubAddr | default ":9300" | quote }}
- name: HUB_HEALTH_ADDR
  value: {{ $c.hubHealthAddr | default ":9301" | quote }}
- name: CONNECTOR_SYNC_INTERVAL
  value: {{ $c.connectorSyncInterval | default "30s" | quote }}
- name: SCHEDULER_TICK
  value: {{ $c.schedulerTick | default "10s" | quote }}
{{- else if eq $svc "control-plane" }}
- name: CONTROL_PLANE_ADDR
  value: {{ $c.controlPlaneAddr | default ":9100" | quote }}
- name: CONTROL_PLANE_HEALTH_ADDR
  value: {{ $c.controlPlaneHealthAddr | default ":9101" | quote }}
{{- else if eq $svc "clip" }}
- name: CLIP_PORT
  value: {{ $c.clipPort | default ":9800" | quote }}
- name: CLIP_MODEL
  value: {{ $c.clipModel | default "ViT-B-32" | quote }}
- name: CLIP_PRETRAINED
  value: {{ $c.clipPretrained | default "openai" | quote }}
{{- end }}
{{- if and $cfg.kek $cfg.kek.enabled }}
- name: KEK_FILE
  value: {{ printf "%s/%s" ($cfg.kek.mountPath | default "/keys") ($cfg.kek.fileKey | default "kek.bin") | quote }}
{{- end }}
{{- /* Scoped credentials: ONLY the Secret keys this service reads (ADR-014 §7),
       instead of envFrom-ing the whole Secret. */ -}}
{{- range $cfg.secretKeys }}
- name: {{ . }}
  valueFrom:
    secretKeyRef:
      name: {{ include "asker.secretName" $root }}
      key: {{ . }}
{{- end }}
{{- with $cfg.env }}
{{- toYaml . }}
{{- end }}
{{- end -}}

{{/*
--------------------------------------------------------------------------------
asker.workload.volumeMounts / asker.workload.volumes — KEK mount (connector-hub,
control-plane) at /keys plus any free-form services.<svc>.volumeMounts / volumes
(e.g. enrich HF cache, clip model cache). Returns YAML list items.
--------------------------------------------------------------------------------
*/}}
{{- define "asker.workload.volumeMounts" -}}
{{- $root := index . 0 -}}
{{- $svc := index . 1 -}}
{{- $cfg := index $root.Values.services $svc -}}
{{- if and $cfg.kek $cfg.kek.enabled }}
- name: kek
  mountPath: {{ $cfg.kek.mountPath | default "/keys" }}
  # Read-only only when the KEK comes from a Secret (kubelet Secret volumes are
  # read-only anyway). When self-created in a writable emptyDir (no secretName),
  # the dir MUST be writable so crypto.NewFileKEK can create the key under a
  # read-only root FS and connector-hub's file DEK store can persist alongside it.
  readOnly: {{ if $cfg.kek.secretName }}true{{ else }}false{{ end }}
{{- end }}
{{- with $cfg.volumeMounts }}
{{- toYaml . }}
{{- end }}
{{- end -}}

{{- define "asker.workload.volumes" -}}
{{- $root := index . 0 -}}
{{- $svc := index . 1 -}}
{{- $cfg := index $root.Values.services $svc -}}
{{- if and $cfg.kek $cfg.kek.enabled }}
- name: kek
  {{- if $cfg.kek.secretName }}
  # Operator-provided KEK Secret, mounted READ-ONLY. Use this only for a service
  # that does NOT write into the KEK dir: control-plane uses a Postgres DEK store,
  # so a read-only KEK file is fine. connector-hub persists wrapped DEKs alongside
  # the KEK (dekDir = dir(KEK_FILE)) and therefore needs a WRITABLE kek dir — leave
  # secretName empty (emptyDir/PVC) or use the Vault KEK provider (ADR-015).
  secret:
    secretName: {{ $cfg.kek.secretName }}
    optional: true
    items:
      - key: {{ $cfg.kek.fileKey | default "kek.bin" }}
        path: {{ $cfg.kek.fileKey | default "kek.bin" }}
  {{- else }}
  # No KEK Secret configured (dev default): a WRITABLE emptyDir so
  # crypto.NewFileKEK can self-create the KEK on first boot UNDER A READ-ONLY ROOT
  # FILESYSTEM, and connector-hub's file DEK store can persist wrapped DEKs in the
  # same dir. Ephemeral — the KEK + DEKs reset on pod restart; for a shared or
  # durable KEK set kek.secretName (read-only) or override volumes with a PVC, or
  # wire the Vault KEK provider (ADR-015). A kubelet Secret mount here would be
  # read-only and crash both NewFileKEK self-create and the DEK store.
  emptyDir: {}
  {{- end }}
{{- end }}
{{- with $cfg.volumes }}
{{- toYaml . }}
{{- end }}
{{- end -}}

{{/*
================================================================================
Service — ClusterIP, metadata.name == compose service DNS name. Exposes every
container port (services.<svc>.service.ports overrides). Mirrors edge naming.
================================================================================
*/}}
{{- define "asker.workload.service" -}}
{{- $root := index . 0 -}}
{{- $svc := index . 1 -}}
{{- $cfg := index $root.Values.services $svc -}}
{{- $svcCfg := $cfg.service | default dict -}}
{{- if ne ($svcCfg.enabled | toString) "false" -}}
apiVersion: v1
kind: Service
metadata:
  name: {{ include "asker.serviceName" (list $root $svc) }}
  labels:
    {{- include "asker.serviceLabels" (list $root $svc) | nindent 4 }}
  {{- with $svcCfg.annotations }}
  annotations:
    {{- toYaml . | nindent 4 }}
  {{- end }}
spec:
  type: {{ $svcCfg.type | default "ClusterIP" }}
  selector:
    {{- include "asker.serviceSelectorLabels" (list $root $svc) | nindent 4 }}
  ports:
    {{- range ($svcCfg.ports | default $cfg.ports) }}
    - name: {{ .name }}
      port: {{ .port }}
      targetPort: {{ .targetPort | default .name }}
      protocol: {{ .protocol | default "TCP" }}
      {{- with .appProtocol }}
      appProtocol: {{ . }}
      {{- end }}
    {{- end }}
{{- end -}}
{{- end -}}

{{/*
================================================================================
HorizontalPodAutoscaler — autoscaling/v2, behind services.<svc>.autoscaling.enabled.
================================================================================
*/}}
{{- define "asker.workload.hpa" -}}
{{- $root := index . 0 -}}
{{- $svc := index . 1 -}}
{{- $cfg := index $root.Values.services $svc -}}
{{- if $cfg.autoscaling.enabled -}}
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: {{ include "asker.serviceFullname" (list $root $svc) }}
  labels:
    {{- include "asker.serviceLabels" (list $root $svc) | nindent 4 }}
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: {{ include "asker.serviceFullname" (list $root $svc) }}
  minReplicas: {{ $cfg.autoscaling.minReplicas }}
  maxReplicas: {{ $cfg.autoscaling.maxReplicas }}
  metrics:
    {{- with $cfg.autoscaling.targetCPUUtilizationPercentage }}
    - type: Resource
      resource:
        name: cpu
        target:
          type: Utilization
          averageUtilization: {{ . }}
    {{- end }}
    {{- with $cfg.autoscaling.targetMemoryUtilizationPercentage }}
    - type: Resource
      resource:
        name: memory
        target:
          type: Utilization
          averageUtilization: {{ . }}
    {{- end }}
  {{- with $cfg.autoscaling.behavior }}
  behavior:
    {{- toYaml . | nindent 4 }}
  {{- end }}
{{- end -}}
{{- end -}}

{{/*
================================================================================
PodDisruptionBudget — policy/v1, behind services.<svc>.pdb.enabled.
================================================================================
*/}}
{{- define "asker.workload.pdb" -}}
{{- $root := index . 0 -}}
{{- $svc := index . 1 -}}
{{- $cfg := index $root.Values.services $svc -}}
{{- if $cfg.pdb.enabled -}}
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: {{ include "asker.serviceFullname" (list $root $svc) }}
  labels:
    {{- include "asker.serviceLabels" (list $root $svc) | nindent 4 }}
spec:
  {{- if hasKey $cfg.pdb "minAvailable" }}
  minAvailable: {{ $cfg.pdb.minAvailable }}
  {{- else if hasKey $cfg.pdb "maxUnavailable" }}
  maxUnavailable: {{ $cfg.pdb.maxUnavailable }}
  {{- end }}
  selector:
    matchLabels:
      {{- include "asker.serviceSelectorLabels" (list $root $svc) | nindent 6 }}
{{- end -}}
{{- end -}}

{{/*
================================================================================
asker.workload.all — render Deployment + Service + HPA + PDB for a service,
joined with document markers. Per-service files just call this. Skips everything
when services.<svc>.enabled is false.
================================================================================
*/}}
{{- define "asker.workload.all" -}}
{{- $root := index . 0 -}}
{{- $svc := index . 1 -}}
{{- $cfg := index $root.Values.services $svc -}}
{{- if and $cfg (ne ($cfg.enabled | toString) "false") -}}
{{- include "asker.workload.deployment" (list $root $svc) }}
{{- with (include "asker.workload.service" (list $root $svc)) }}
---
{{ . }}
{{- end }}
{{- with (include "asker.workload.hpa" (list $root $svc)) }}
---
{{ . }}
{{- end }}
{{- with (include "asker.workload.pdb" (list $root $svc)) }}
---
{{ . }}
{{- end }}
{{- end -}}
{{- end -}}
