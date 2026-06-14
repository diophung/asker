{{/*
================================================================================
Asker umbrella chart — messaging (Apache Kafka via the Strimzi operator) helpers.

ADR-003 §1 / ADR-014 §4: the dev stack uses Redpanda (single binary, compose);
production uses Apache Kafka run by the Strimzi operator on Kubernetes. Both speak
the Kafka API, so the application code (and KAFKA_BROKERS) is identical against
either — only the deployment shape differs.

IMPORTANT — the Strimzi OPERATOR is NOT installed by this chart. This chart only
renders the Kafka / KafkaTopic CUSTOM RESOURCES (kafka.strimzi.io/v1beta2). The
cluster operator that reconciles them must already be installed (see the README /
the gating note below). With the operator absent these CRs are inert.

Naming: the Strimzi Kafka cluster is named via `.Values.kafka.strimzi.clusterName`
(default "asker-kafka"). Strimzi then creates the bootstrap Service
"<clusterName>-kafka-bootstrap" on :9092 — that is the address the integrator must
put in `config.kafkaBrokers` (rendered into KAFKA_BROKERS in the shared ConfigMap,
wave 0) so every app worker dials the Strimzi cluster instead of compose Redpanda:

    config.kafkaBrokers: "asker-kafka-kafka-bootstrap:9092"

All keys live under `.Values.kafka.*` and are read through `default` so the
templates render even before the integrator merges these keys into the
authoritative values.yaml (see the issues list in the build report). Defaults
mirror spec §2.7 (>=512 partitions in prod; a much smaller dev default belongs in
values-dev.yaml — noted in the report).
================================================================================
*/}}

{{/*
asker.kafka.clusterName — the Strimzi Kafka cluster (Kafka CR) name. Strimzi
derives the bootstrap Service "<clusterName>-kafka-bootstrap" from it.
Usage: {{ include "asker.kafka.clusterName" . }}
*/}}
{{- define "asker.kafka.clusterName" -}}
{{- $kafka := .Values.kafka | default dict -}}
{{- $strimzi := $kafka.strimzi | default dict -}}
{{- $strimzi.clusterName | default "asker-kafka" -}}
{{- end }}

{{/*
asker.kafka.bootstrap — the in-cluster bootstrap address Strimzi exposes for the
plain (:9092) listener. This is the value the integrator should mirror into
config.kafkaBrokers. Usage: {{ include "asker.kafka.bootstrap" . }}
*/}}
{{- define "asker.kafka.bootstrap" -}}
{{- printf "%s-kafka-bootstrap:9092" (include "asker.kafka.clusterName" .) -}}
{{- end }}

{{/*
asker.kafka.topicLabels — labels for KafkaTopic CRs. Strimzi selects topics by the
"strimzi.io/cluster" label, so we ALWAYS stamp it with the cluster name, then add
the chart-wide labels + a messaging component label.
*/}}
{{- define "asker.kafka.topicLabels" -}}
strimzi.io/cluster: {{ include "asker.kafka.clusterName" . }}
{{ include "asker.labels" . }}
app.kubernetes.io/component: messaging
{{- end }}
