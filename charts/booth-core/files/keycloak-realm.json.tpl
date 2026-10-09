{
  "realm": "{{ include "booth-core.keycloakRealm" . }}",
  "enabled": true,
  "sslRequired": "none",
  "registrationAllowed": false,
  "bruteForceProtected": true,
  "defaultDefaultClientScopes": ["web-origins", "acr", "profile", "roles", "email", "groups", "basic"],
  "clientScopes": [
    {
      "name": "basic",
      "protocol": "openid-connect",
      "attributes": { "include.in.token.scope": "false", "display.on.consent.screen": "false" },
      "protocolMappers": [
        { "name": "sub", "protocol": "openid-connect", "protocolMapper": "oidc-sub-mapper", "consentRequired": false, "config": { "id.token.claim": "true", "access.token.claim": "true" } }
      ]
    },
    {
      "name": "groups",
      "protocol": "openid-connect",
      "attributes": { "include.in.token.scope": "true", "display.on.consent.screen": "false" },
      "protocolMappers": [
        { "name": "groups", "protocol": "openid-connect", "protocolMapper": "oidc-group-membership-mapper", "consentRequired": false,
          "config": { "full.path": "true", "id.token.claim": "true", "access.token.claim": "true", "userinfo.token.claim": "true", "claim.name": "groups" } }
      ]
    },
    {
      "name": "profile",
      "protocol": "openid-connect",
      "attributes": { "include.in.token.scope": "true", "display.on.consent.screen": "true" },
      "protocolMappers": [
        { "name": "username", "protocol": "openid-connect", "protocolMapper": "oidc-usermodel-property-mapper", "consentRequired": false,
          "config": { "userinfo.token.claim": "true", "user.attribute": "username", "id.token.claim": "true", "access.token.claim": "true", "claim.name": "preferred_username" } }
      ]
    },
    {
      "name": "email",
      "protocol": "openid-connect",
      "attributes": { "include.in.token.scope": "true", "display.on.consent.screen": "true" },
      "protocolMappers": [
        { "name": "email", "protocol": "openid-connect", "protocolMapper": "oidc-usermodel-property-mapper", "consentRequired": false,
          "config": { "userinfo.token.claim": "true", "user.attribute": "email", "id.token.claim": "true", "access.token.claim": "true", "claim.name": "email" } }
      ]
    },
    {
      "name": "roles",
      "protocol": "openid-connect",
      "attributes": { "include.in.token.scope": "false", "display.on.consent.screen": "true" },
      "protocolMappers": [
        { "name": "audience resolve", "protocol": "openid-connect", "protocolMapper": "oidc-audience-resolve-mapper", "consentRequired": false, "config": {} }
      ]
    },
    {
      "name": "web-origins",
      "protocol": "openid-connect",
      "attributes": { "include.in.token.scope": "false", "display.on.consent.screen": "false" },
      "protocolMappers": [
        { "name": "allowed web origins", "protocol": "openid-connect", "protocolMapper": "oidc-allowed-origins-mapper", "consentRequired": false, "config": {} }
      ]
    },
    {
      "name": "acr",
      "protocol": "openid-connect",
      "attributes": { "include.in.token.scope": "false", "display.on.consent.screen": "false" },
      "protocolMappers": [
        { "name": "acr loa level", "protocol": "openid-connect", "protocolMapper": "oidc-acr-mapper", "consentRequired": false, "config": { "id.token.claim": "true", "access.token.claim": "true" } }
      ]
    }
  ],
  "clients": [
    {
      "clientId": "{{ include "booth-core.keycloakClientId" . }}",
      "enabled": true,
      "protocol": "openid-connect",
      "publicClient": true,
      "standardFlowEnabled": true,
      "implicitFlowEnabled": false,
      "directAccessGrantsEnabled": false,
      "serviceAccountsEnabled": false,
      "redirectUris": ["{{ include "booth-core.shellOrigin" . }}/*"],
      "webOrigins": ["{{ include "booth-core.shellOrigin" . }}"],
      "attributes": {
        "pkce.code.challenge.method": "S256"
      },
      "protocolMappers": [
        {
          "name": "booth-design-audience",
          "protocol": "openid-connect",
          "protocolMapper": "oidc-audience-mapper",
          "config": {
            "included.client.audience": "{{ include "booth-core.keycloakClientId" . }}",
            "id.token.claim": "false",
            "access.token.claim": "true"
          }
        }
      ]
    }
  ],
  "groups": [
    { "name": "workspaces" },
    { "name": "platform", "subGroups": [{ "name": "operator" }] }
  ]
}
