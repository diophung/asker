{{/*
================================================================================
Asker umbrella chart — Vault (Transit KEK) template helpers. M4 wave 1.

These helpers resolve the vault.* values block WITHOUT modifying the wave-0
values.yaml: every key is read through `default` so the chart renders correctly
whether or not the integrator has merged the documented vault.* keys into
values.yaml yet (see docs/adr/ADR-015 and the issues list for the canonical
keys). When vault.deploy is true the chart renders a DEV-MODE, NON-PRODUCTION
Vault (single unsealed in-memory node) plus a Transit-enable init Job, a
ClusterIP Service "vault" (port 8200), a dev token Secret, and a ServiceAccount.

Naming: the in-cluster Service is named "vault" (bare, like the app Services in
_helpers.tpl) so control-plane / connector-hub can dial http://vault:8200 by DNS.
Workload-side objects (Deployment, Job) are release-prefixed via asker.fullname.
================================================================================
*/}}

{{/*
asker.vault.config — the resolved vault.* config dict, with defaults applied for
every key so callers never nil-deref. Returns a dict; callers index it.
NOTE TO INTEGRATOR: mirror these defaults as the values.yaml `vault:` block.
*/}}
{{- define "asker.vault.config" -}}
{{- $v := .Values.vault | default dict -}}
deploy: {{ $v.deploy | default false }}
addr: {{ $v.addr | default "http://vault:8200" | quote }}
keyName: {{ $v.keyName | default "asker-kek" | quote }}
image: {{ $v.image | default "hashicorp/vault:1.18" | quote }}
port: {{ $v.port | default 8200 }}
devRootToken: {{ $v.devRootToken | default "asker-dev-root" | quote }}
tokenSecretName: {{ $v.tokenSecretName | default (printf "%s-vault-token" (include "asker.fullname" .)) | quote }}
serviceAccountName: {{ $v.serviceAccountName | default (printf "%s-vault" (include "asker.fullname" .)) | quote }}
{{- end }}

{{/*
asker.vault.enabled — true only when vault.deploy is explicitly true. Used to
gate every object in this directory. Returns the literal string "true"/"false".
*/}}
{{- define "asker.vault.enabled" -}}
{{- $v := .Values.vault | default dict -}}
{{- $v.deploy | default false -}}
{{- end }}

{{/*
asker.vault.labels — chart-wide labels plus the vault component label, so the
dev Vault objects are clearly grouped and selectable.
*/}}
{{- define "asker.vault.labels" -}}
{{ include "asker.labels" . }}
app.kubernetes.io/component: vault
{{- end }}

{{/*
asker.vault.selectorLabels — stable selector labels for the dev Vault
Deployment/Service.
*/}}
{{- define "asker.vault.selectorLabels" -}}
{{ include "asker.selectorLabels" . }}
app.kubernetes.io/component: vault
{{- end }}
