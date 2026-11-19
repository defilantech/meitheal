{{/*
Expand the name of the chart.
*/}}
{{- define "meitheal.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "meitheal.fullname" -}}
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
Chart label, used by app.kubernetes.io/version etc.
*/}}
{{- define "meitheal.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Resolved namespace. Defaults to .Values.namespace; falls back to Release.Namespace.
*/}}
{{- define "meitheal.namespace" -}}
{{- default .Release.Namespace .Values.namespace }}
{{- end }}

{{/*
Common labels applied to every resource.
*/}}
{{- define "meitheal.labels" -}}
helm.sh/chart: {{ include "meitheal.chart" . }}
{{ include "meitheal.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels (shared across operator + agent so a single Service or
NetworkPolicy can target either). Components stamp an additional
app.kubernetes.io/component label downstream.
*/}}
{{- define "meitheal.selectorLabels" -}}
app.kubernetes.io/name: {{ include "meitheal.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/part-of: meitheal
{{- end }}

{{/*
Operator pod labels = base selector + component=operator.
*/}}
{{- define "meitheal.operator.labels" -}}
{{ include "meitheal.selectorLabels" . }}
app.kubernetes.io/component: operator
{{- end }}

{{/*
Agent pod labels = base selector + component=agent.

Release-scoped, NOT pool-scoped: the agent ServiceAccount/RBAC and the
gate-cache PVC are per-release objects and must keep these labels stable.
Anything that needs to tell one agent pool from another wants
meitheal.agent.poolLabels instead.
*/}}
{{- define "meitheal.agent.labels" -}}
{{ include "meitheal.selectorLabels" . }}
app.kubernetes.io/component: agent
{{- end }}

{{/*
Agent Deployment labels, selector and pod-template labels, scoped to one pool.

Takes the per-agent context ($ctx) built by agent-deployment.yaml, so it can
read .agentName. Every pool in the multi-fleet form gets an agent-pool label,
which is what keeps two Deployments from selecting each other's pods (#1523):
meitheal.agent.labels alone is identical for every pool, so without this the
controllers each count the other's replicas toward their own replicaCount and
scale down what the other scales up.

The legacy single-agent form is deliberately left alone. Its config is wrapped
under the "_implicit_" sentinel and renders exactly one Deployment, so there is
nothing to collide with, and a Deployment's selector is immutable: adding a
label there would break `helm upgrade` on every existing install. Migrating
from agent: to agents: already renames the object (meitheal-agent becomes
meitheal-<pool>-agent), so the new selector lands on a new object.
*/}}
{{- define "meitheal.agent.poolLabels" -}}
{{ include "meitheal.agent.labels" . }}
{{- if ne .agentName "_implicit_" }}
{{ printf "meitheal.dev/agent-pool: %s" .agentName }}
{{- end }}
{{- end }}

{{/*
ServiceAccount name for the meitheal-operator.
*/}}
{{- define "meitheal.operator.serviceAccountName" -}}
{{- if .Values.operator.serviceAccount.create }}
{{- default (printf "%s-operator" (include "meitheal.fullname" .)) .Values.operator.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.operator.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
ServiceAccount name for the meitheal-agent. Expects a context dict with
"agentName" (the entry from .Values.agents, or the legacy sentinel
"_implicit_" when .Values.agent: is used unchanged) and "agentConfig"
(the per-agent values dict). When the sentinel is passed, the legacy
"<fullname>-agent" name is preserved so an existing install's SA is not
renamed on upgrade; an explicit agents.<name> entry always suffixes the
agent key so multiple agents never collide.
*/}}
{{- define "meitheal.agent.serviceAccountName" -}}
{{- if .agentConfig.serviceAccount.create }}
{{- if eq .agentName "_implicit_" }}
{{- default (printf "%s-agent" (include "meitheal.fullname" .)) .agentConfig.serviceAccount.name }}
{{- else }}
{{- default (printf "%s-%s-agent" (include "meitheal.fullname" .) .agentName | trunc 63 | trimSuffix "-") .agentConfig.serviceAccount.name }}
{{- end }}
{{- else }}
{{- default "default" .agentConfig.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Default resource name for one meitheal-agent (Deployment / ClusterRole /
ClusterRoleBinding). Same shape as the SA default — "<fullname>-agent"
for the legacy sentinel and "<fullname>-<agentName>-agent" for an
explicit agents.<name> entry — but with no per-agent override since the
SA already carries that escape hatch.
*/}}
{{- define "meitheal.agent.resourceName" -}}
{{- if eq .agentName "_implicit_" }}
{{- printf "%s-agent" (include "meitheal.fullname" .) }}
{{- else }}
{{- printf "%s-%s-agent" (include "meitheal.fullname" .) .agentName | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{/*
The agents map to render. When the user opts into the multi-fleet form
(.Values.agents is set) the map is returned verbatim. Otherwise the
legacy top-level .Values.agent: block is wrapped under a sentinel key
("_implicit_") so a single template path can render both shapes and the
legacy install's resource names are preserved on upgrade (#994). Callers
must not pass "_implicit_" as an explicit agents.* key.
*/}}
{{- define "meitheal.agents" -}}
{{- if .Values.agents }}
{{- .Values.agents | toYaml }}
{{- else }}
{{- dict "_implicit_" .Values.agent | toYaml }}
{{- end }}
{{- end }}

{{/*
Coder Job pod labels = base selector + component=coder.
*/}}
{{- define "meitheal.coder.labels" -}}
{{ include "meitheal.selectorLabels" . }}
app.kubernetes.io/component: coder
{{- end }}

{{/*
ServiceAccount name for the coder Job pods. Defaults to "meitheal-coder"
so the sample Agent's spec.execution.serviceAccountName lines up out of
the box; override via coder.serviceAccount.name.
*/}}
{{- define "meitheal.coder.serviceAccountName" -}}
{{- if .Values.coder.serviceAccount.create }}
{{- default "meitheal-coder" .Values.coder.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.coder.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Meitheal operator image (registry/repo:tag or registry/repo@digest).
*/}}
{{- define "meitheal.operator.image" -}}
{{- $img := .Values.operator.image -}}
{{- $repo := $img.repository -}}
{{- if $img.registry -}}
{{- $repo = printf "%s/%s" $img.registry $img.repository -}}
{{- end -}}
{{- if $img.digest -}}
{{- printf "%s@%s" $repo $img.digest -}}
{{- else -}}
{{- $tag := $img.tag | default .Chart.AppVersion -}}
{{- printf "%s:%s" $repo $tag -}}
{{- end -}}
{{- end }}

{{/*
Meitheal agent image (registry/repo:tag or registry/repo@digest). Expects
the per-agent context dict with "agentConfig" (.Values.agents.<name> or
.Values.agent) and the chart context (Release/Values/Chart) at the root.
*/}}
{{- define "meitheal.agent.image" -}}
{{- $img := .agentConfig.image -}}
{{- $repo := $img.repository -}}
{{- if $img.registry -}}
{{- $repo = printf "%s/%s" $img.registry $img.repository -}}
{{- end -}}
{{- if $img.digest -}}
{{- printf "%s@%s" $repo $img.digest -}}
{{- else -}}
{{- $tag := $img.tag | default .Chart.AppVersion -}}
{{- printf "%s:%s" $repo $tag -}}
{{- end -}}
{{- end }}

{{/*
Gate cache PVC name for a single agent. Expects the per-agent context
dict; the legacy install (sentinel "_implicit_") keeps "<fullname>-gate-cache"
so an upgrade does not rename an in-use PVC. Explicit agents.<name>
entries get "<fullname>-<agentName>-gate-cache" so multiple agents each
own a distinct PVC. Users can still override via .agentConfig.gateCache.name.
*/}}
{{- define "meitheal.gateCache.pvcName" -}}
{{- $suffix := "" -}}
{{- if ne .agentName "_implicit_" -}}
{{- $suffix = printf "-%s" .agentName -}}
{{- end -}}
{{- default (printf "%s%s-gate-cache" (include "meitheal.fullname" .) $suffix) .agentConfig.gateCache.name }}
{{- end }}

{{/*
Webhook Service name. The validating webhook's clientConfig targets this
Service; the operator Deployment's pod labels are the Service selector.
*/}}
{{- define "meitheal.webhook.serviceName" -}}
{{- printf "%s-webhook" (include "meitheal.fullname" .) -}}
{{- end }}

{{/*
Webhook serving-cert Secret name. Holds tls.crt + tls.key (+ ca.crt for
reference). Mounted into the operator pod at the controller-runtime
default cert dir and reused across upgrades via lookup.
*/}}
{{- define "meitheal.webhook.secretName" -}}
{{- printf "%s-webhook-cert" (include "meitheal.fullname" .) -}}
{{- end }}

{{/*
ValidatingWebhookConfiguration name.
*/}}
{{- define "meitheal.webhook.configName" -}}
{{- printf "%s-validating-webhook" (include "meitheal.fullname" .) -}}
{{- end }}

{{/*
meitheal.webhook.certs resolves the serving cert + CA bundle for the
webhook, reusing the existing Secret's material when present so the cert
and the injected caBundle stay STABLE across `helm upgrade`. Returns a
dict with keys "ca", "cert", "key" (all base64-encoded PEM).

Lookup-reuse: if the serving Secret already exists AND carries tls.crt /
tls.key / ca.crt, reuse them verbatim. Otherwise generate a fresh
self-signed CA + serving cert whose SANs cover the in-cluster Service DNS
names. The caBundle in the ValidatingWebhookConfiguration is injected from
the SAME dict, so they always match.

Note: `lookup` returns an empty dict during `helm template` / dry-run, so
those always render freshly-generated material (fine: template output is
not applied). On a real install/upgrade against an API server the existing
Secret is found and reused.
*/}}
{{- define "meitheal.webhook.certs" -}}
{{- $svc := include "meitheal.webhook.serviceName" . -}}
{{- $ns := include "meitheal.namespace" . -}}
{{- $altNames := list (printf "%s.%s.svc" $svc $ns) (printf "%s.%s.svc.cluster.local" $svc $ns) -}}
{{- $secretName := include "meitheal.webhook.secretName" . -}}
{{- $existing := lookup "v1" "Secret" $ns $secretName -}}
{{- if and $existing $existing.data (index $existing.data "tls.crt") (index $existing.data "tls.key") (index $existing.data "ca.crt") -}}
{{- dict "ca" (index $existing.data "ca.crt") "cert" (index $existing.data "tls.crt") "key" (index $existing.data "tls.key") | toYaml -}}
{{- else -}}
{{- $ca := genCA (printf "%s-webhook-ca" (include "meitheal.fullname" .)) (int .Values.webhook.certValidityDays) -}}
{{- $cert := genSignedCert $svc nil $altNames (int .Values.webhook.certValidityDays) $ca -}}
{{- dict "ca" ($ca.Cert | b64enc) "cert" ($cert.Cert | b64enc) "key" ($cert.Key | b64enc) | toYaml -}}
{{- end -}}
{{- end }}
