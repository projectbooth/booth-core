{{/*
Standard name/label helpers, the same shape `helm create` scaffolds — nothing
booth-core-specific here.
*/}}

{{- define "booth-core.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "booth-core.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "booth-core.labels" -}}
app.kubernetes.io/name: {{ include "booth-core.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "booth-core.selectorLabels" -}}
app.kubernetes.io/name: {{ include "booth-core.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "booth-core.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "booth-core.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "booth-core.secretName" -}}
{{- if .Values.existingSecret -}}
{{- .Values.existingSecret -}}
{{- else -}}
{{- include "booth-core.fullname" . -}}
{{- end -}}
{{- end -}}

{{/*
ADR 0106/0108: the bundled Keycloak's name, fixed realm, and the computed URLs that make
KC_HOSTNAME and oidc.issuerUrl/jwksUrl the *same* value, templated from one source, so they
cannot independently drift (the exact problem booth-e2e's own bring-up script had to work
around by hand). No Ingress exists yet (that's ADR 0108's own build, a separate PR) — the
in-cluster Service DNS name is the only reachable address today, so that's what these default
to. Once an Ingress/ingress.host exists, these helpers are the one place that changes.
*/}}

{{- define "booth-core.keycloakFullname" -}}
{{- printf "%s-keycloak" (include "booth-core.fullname" .) -}}
{{- end -}}

{{- define "booth-core.keycloakRealm" -}}
booth
{{- end -}}

{{- define "booth-core.keycloakClientId" -}}
booth-design
{{- end -}}

{{/* The base URL Keycloak itself is reachable at (no /realms/<realm> suffix) — this is the
exact value KC_HOSTNAME takes. */}}
{{- define "booth-core.keycloakBaseUrl" -}}
http://{{ include "booth-core.keycloakFullname" . }}.{{ .Release.Namespace }}.svc.cluster.local:8080
{{- end -}}

{{- define "booth-core.keycloakIssuerUrl" -}}
{{ include "booth-core.keycloakBaseUrl" . }}/realms/{{ include "booth-core.keycloakRealm" . }}
{{- end -}}

{{/* core's own reachable address — the shell's origin, since core serves the shell. Used to
template the starter realm's client redirect URIs/web origins so they can't drift from where
core is actually reachable either. */}}
{{- define "booth-core.shellOrigin" -}}
http://{{ include "booth-core.fullname" . }}.{{ .Release.Namespace }}.svc.cluster.local:{{ .Values.service.port }}
{{- end -}}

{{/* oidc.issuerUrl/clientId/jwksUrl: an explicit value always wins (so an operator pointing at
their own external provider, or their own self-managed Keycloak, is never overridden); only
when left empty AND keycloak.enabled do these default to the bundled Keycloak's own values. */}}
{{- define "booth-core.effectiveOidcIssuerUrl" -}}
{{- if .Values.oidc.issuerUrl -}}
{{- .Values.oidc.issuerUrl -}}
{{- else if .Values.keycloak.enabled -}}
{{- include "booth-core.keycloakIssuerUrl" . -}}
{{- end -}}
{{- end -}}

{{- define "booth-core.effectiveOidcClientId" -}}
{{- if .Values.oidc.clientId -}}
{{- .Values.oidc.clientId -}}
{{- else if .Values.keycloak.enabled -}}
{{- include "booth-core.keycloakClientId" . -}}
{{- end -}}
{{- end -}}

{{- define "booth-core.effectiveOidcJwksUrl" -}}
{{- if .Values.oidc.jwksUrl -}}
{{- .Values.oidc.jwksUrl -}}
{{- else if .Values.keycloak.enabled -}}
{{- include "booth-core.keycloakIssuerUrl" . }}/protocol/openid-connect/certs
{{- end -}}
{{- end -}}
