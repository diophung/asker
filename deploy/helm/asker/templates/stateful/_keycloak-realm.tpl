{{/*
================================================================================
asker.stateful.keycloakRealm — the dev "asker" realm import JSON, embedded here
so the self-hosted Keycloak dev default is self-contained within the stateful
wave's owned path (templates/stateful/**) without reaching into deploy/compose/**
(a path this wave does NOT own) or relying on chart .Files (which cannot read the
templates/ tree). This is the SAME realm deploy/compose/keycloak/realm-asker.json
imports: realm "asker", public client "asker-web" (audience mapper), dev users
alice/bob/carol (password "password123"). DEV-ONLY — a production Keycloak is
provisioned out-of-band (stateful.keycloak.realmConfigMap points at a managed
ConfigMap, or stateful.keycloak.deploy=false for a central IdP; ADR-014 §4).

INTEGRATOR: if deploy/compose/keycloak/realm-asker.json changes, update this copy
(the two dev surfaces are kept in sync per ADR-014 "two deployment surfaces").
================================================================================
*/}}
{{- define "asker.stateful.keycloakRealm" -}}
{
  "realm": "asker",
  "enabled": true,
  "registrationAllowed": false,
  "sslRequired": "external",
  "accessTokenLifespan": 300,
  "clients": [
    {
      "clientId": "asker-web",
      "name": "Asker Web",
      "description": "Public client for the Asker web UI and dev password grants (dev-only configuration).",
      "enabled": true,
      "protocol": "openid-connect",
      "publicClient": true,
      "standardFlowEnabled": true,
      "directAccessGrantsEnabled": true,
      "implicitFlowEnabled": false,
      "serviceAccountsEnabled": false,
      "fullScopeAllowed": true,
      "redirectUris": [
        "http://localhost:8080/*",
        "http://localhost:3000/*"
      ],
      "webOrigins": [
        "+"
      ],
      "protocolMappers": [
        {
          "name": "asker-web-audience",
          "protocol": "openid-connect",
          "protocolMapper": "oidc-audience-mapper",
          "consentRequired": false,
          "config": {
            "included.client.audience": "asker-web",
            "id.token.claim": "false",
            "access.token.claim": "true",
            "introspection.token.claim": "true",
            "lightweight.claim": "false"
          }
        }
      ]
    }
  ],
  "users": [
    {
      "username": "alice",
      "enabled": true,
      "email": "alice@example.com",
      "emailVerified": true,
      "firstName": "Alice",
      "lastName": "Asker",
      "credentials": [
        {
          "type": "password",
          "value": "password123",
          "temporary": false
        }
      ]
    },
    {
      "username": "bob",
      "enabled": true,
      "email": "bob@example.com",
      "emailVerified": true,
      "firstName": "Bob",
      "lastName": "Asker",
      "credentials": [
        {
          "type": "password",
          "value": "password123",
          "temporary": false
        }
      ]
    },
    {
      "username": "carol",
      "enabled": true,
      "email": "carol@example.com",
      "emailVerified": true,
      "firstName": "Carol",
      "lastName": "Asker",
      "credentials": [
        {
          "type": "password",
          "value": "password123",
          "temporary": false
        }
      ]
    }
  ]
}
{{- end -}}
