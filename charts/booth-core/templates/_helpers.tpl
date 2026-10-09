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
KC_HOSTNAME, oidc.issuerUrl, the shell origin, the client's redirect URIs and the certificate
name the *same* value, templated from one source (ingress.host), so they cannot independently
drift (the exact problem booth-e2e's own bring-up script had to work around by hand).

Two distinct "base URLs" exist on purpose, and must never be conflated:
  - the EXTERNAL one (ingress.host, https, once an Ingress exists) — what the browser and the
    realm's own client config use (KC_HOSTNAME, oidc.issuerUrl, the shell origin, redirect URIs).
  - the IN-CLUSTER one (the Service DNS name, plain http, unconditionally) — what oidc.jwksUrl
    uses, on purpose: the whole point of that mechanism (ADR 0108) is that no pod ever needs to
    trust the Ingress's certificate just to fetch signing keys. Ingress/TLS existing or not must
    never change jwksUrl's value.
With no ingress.host set (keycloak.enabled without ingress.enabled, or no Ingress built yet),
the external one falls back to the same in-cluster address the in-cluster one uses — login
simply isn't reachable from outside the cluster yet, exactly PR A's prior behavior.
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

{{/* Required when both are true, per ADR 0108 condition 5: an Ingress wired to a Keycloak with
no reachable external hostname is a broken, not a degraded, install. */}}
{{- define "booth-core.requireIngressHost" -}}
{{- if and .Values.ingress.enabled .Values.keycloak.enabled -}}
{{- required "ingress.host is required when ingress.enabled and keycloak.enabled are both true (ADR 0108) -- set it, or turn one of the two off" .Values.ingress.host -}}
{{- end -}}
{{- end -}}

{{/* The in-cluster-only base URL Keycloak is always reachable at, regardless of Ingress — used
ONLY by oidc.jwksUrl (see the note above). */}}
{{- define "booth-core.keycloakInClusterBaseUrl" -}}
http://{{ include "booth-core.keycloakFullname" . }}.{{ .Release.Namespace }}.svc.cluster.local:8080
{{- end -}}

{{- define "booth-core.keycloakInClusterIssuerUrl" -}}
{{ include "booth-core.keycloakInClusterBaseUrl" . }}/realms/{{ include "booth-core.keycloakRealm" . }}
{{- end -}}

{{/* The externally-reachable base URL -- https://ingress.host once an Ingress exists, else the
same in-cluster address as above (no external reachability yet). This is KC_HOSTNAME's value. */}}
{{- define "booth-core.keycloakExternalBaseUrl" -}}
{{- $_ := include "booth-core.requireIngressHost" . -}}
{{- if and .Values.ingress.enabled .Values.ingress.host -}}
https://{{ .Values.ingress.host }}
{{- else -}}
{{- include "booth-core.keycloakInClusterBaseUrl" . -}}
{{- end -}}
{{- end -}}

{{- define "booth-core.keycloakExternalIssuerUrl" -}}
{{ include "booth-core.keycloakExternalBaseUrl" . }}/realms/{{ include "booth-core.keycloakRealm" . }}
{{- end -}}

{{/* core's own externally-reachable address — the shell's origin, since core serves the shell.
Same ingress.host-or-fallback shape as keycloakExternalBaseUrl, and deliberately the same host
(one Ingress, one hostname, path-routed — ADR 0108) so a certificate covering ingress.host
covers both. Used to template the starter realm's client redirect URIs/web origins too. */}}
{{- define "booth-core.shellOrigin" -}}
{{- if and .Values.ingress.enabled .Values.ingress.host -}}
https://{{ .Values.ingress.host }}
{{- else -}}
http://{{ include "booth-core.fullname" . }}.{{ .Release.Namespace }}.svc.cluster.local:{{ .Values.service.port }}
{{- end -}}
{{- end -}}

{{/* oidc.issuerUrl/clientId/jwksUrl: an explicit value always wins (so an operator pointing at
their own external provider, or their own self-managed Keycloak, is never overridden); only
when left empty AND keycloak.enabled do these default to the bundled Keycloak's own values. */}}
{{- define "booth-core.effectiveOidcIssuerUrl" -}}
{{- if .Values.oidc.issuerUrl -}}
{{- .Values.oidc.issuerUrl -}}
{{- else if .Values.keycloak.enabled -}}
{{- include "booth-core.keycloakExternalIssuerUrl" . -}}
{{- end -}}
{{- end -}}

{{- define "booth-core.effectiveOidcClientId" -}}
{{- if .Values.oidc.clientId -}}
{{- .Values.oidc.clientId -}}
{{- else if .Values.keycloak.enabled -}}
{{- include "booth-core.keycloakClientId" . -}}
{{- end -}}
{{- end -}}

{{/* Always in-cluster http, Ingress or not -- see the note at the top of this section. */}}
{{- define "booth-core.effectiveOidcJwksUrl" -}}
{{- if .Values.oidc.jwksUrl -}}
{{- .Values.oidc.jwksUrl -}}
{{- else if .Values.keycloak.enabled -}}
{{- include "booth-core.keycloakInClusterIssuerUrl" . }}/protocol/openid-connect/certs
{{- end -}}
{{- end -}}

{{/* ADR 0108 item 2: the three certificate modes. secretName set by the operator (mode a) is
used verbatim, untouched by this chart. Left empty, the chart computes a fixed name; whether
core self-generates into it (mode b, the default) or cert-manager does (mode c, via the
operator's own annotations on the Ingress) is ingress.tls.selfSigned's job, not this helper's --
the Ingress just needs *a* secretName to reference either way. */}}
{{- define "booth-core.tlsSecretName" -}}
{{- if .Values.ingress.tls.secretName -}}
{{- .Values.ingress.tls.secretName -}}
{{- else -}}
{{- printf "%s-tls" (include "booth-core.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/* True only for mode (b): core generates and renews the certificate itself. False for mode
(a) (operator supplied it) or mode (c) (cert-manager will populate tlsSecretName instead). */}}
{{- define "booth-core.tlsSelfSigned" -}}
{{- if .Values.ingress.tls.secretName -}}
false
{{- else -}}
{{- .Values.ingress.tls.selfSigned -}}
{{- end -}}
{{- end -}}

{{/* Whether core's own TLS provisioning (internal/tlsprov) should run at all: mode (b), AND
there's an actual Ingress+host for the certificate to cover. */}}
{{- define "booth-core.tlsProvisioningEnabled" -}}
{{- if and .Values.ingress.enabled .Values.ingress.host (eq (include "booth-core.tlsSelfSigned" .) "true") -}}
true
{{- else -}}
false
{{- end -}}
{{- end -}}
