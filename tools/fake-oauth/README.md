# fake-oauth

A standards-correct **fake OAuth 2.0 authorization server** for dev and CI. It
lets the whole Asker connector OAuth flow run end to end without real
Google/Microsoft/Slack/Atlassian credentials.

> **Dev/CI only — never deploy to production.** It *auto-consents*: there is no
> login UI, so any caller obtains a token for the configured subject. It must
> only ever be reachable on the compose network.

It speaks the same endpoints `platform/oauth`'s `oauth.Service` drives, and a
single instance backs all four providers (distinguished by URL path).

## Endpoints

| Method & path | Purpose |
| --- | --- |
| `GET /{provider}/authorize` | Auto-consent. Validates `redirect_uri` + PKCE (`code_challenge`, `code_challenge_method=S256`), mints a one-time `code` bound to the challenge/redirect/subject, then `302` → `redirect_uri?code=<code>&state=<state>`. |
| `POST /{provider}/token` | `grant_type=authorization_code`: validates the code (single-use, unexpired), the PKCE `code_verifier` (S256), and `redirect_uri`, then issues `access_token` + `refresh_token` + `expires_in`. `grant_type=refresh_token`: rotates the access token. |
| `GET /healthz` | Liveness (`200 ok`). |

`{provider}` is one of `google`, `microsoft`, `slack`, `atlassian`.

### Subject (the resource owner)

The subject defaults to `FAKE_OAUTH_SUBJECT` (default `alice@example.com`) and is
overridden per-request by a `login_hint` query param on `/authorize`.

### Issued tokens

- **Google** → `fake-gmail-token:<subject>` so the Gmail connector's call to the
  dev **fake-gmail** server succeeds end to end.
- **Microsoft / Atlassian** → opaque `fakeoauth:<provider>:<subject>` (standard
  RFC 6749 JSON response).
- **Slack** → opaque `fakeoauth:slack:<subject>` returned in Slack's
  **non-standard** envelope: `{"ok":true,"authed_user":{"access_token":...,"token_type":"bearer",...}}`.

### Errors

- Standard providers: `400 {"error":"invalid_grant"}` (bad/used/expired code,
  PKCE mismatch, `redirect_uri` mismatch); `400 {"error":"unsupported_grant_type"}`.
- Slack: `200 {"ok":false,"error":"invalid_code"}` (Slack signals failure with
  HTTP 200, not a 4xx).

## Configuration (env, `FAKE_OAUTH_` prefix)

| Var | Default | Meaning |
| --- | --- | --- |
| `FAKE_OAUTH_ADDR` | `:9500` | Listen address. |
| `FAKE_OAUTH_SUBJECT` | `alice@example.com` | Subject used when no `login_hint`. |
| `FAKE_OAUTH_CODE_TTL` | `5m` | Authorization-code lifetime. |

## Run

```sh
go run ./tools/fake-oauth                 # listens on :9500
FAKE_OAUTH_SUBJECT=bob@example.com go run ./tools/fake-oauth
```

The container has no shell; its healthcheck runs the binary itself:
`["CMD", "/fake-oauth", "-healthcheck"]`.

## Wiring it into the dev stack

Point each provider's `*_AUTH_URL` / `*_TOKEN_URL` at this server and supply dev
client creds (any non-empty value — the fake does not verify them):

```sh
ASKER_OAUTH_GOOGLE_AUTH_URL=http://fake-oauth:9500/google/authorize
ASKER_OAUTH_GOOGLE_TOKEN_URL=http://fake-oauth:9500/google/token
ASKER_OAUTH_GOOGLE_CLIENT_ID=dev-google
ASKER_OAUTH_GOOGLE_CLIENT_SECRET=dev-google-secret
# …and the same shape for MICROSOFT / SLACK / ATLASSIAN.
```

The integrator owns `deploy/compose` and the gateway/connector-hub env; see the
issues filed alongside this tool for the full env map and the compose service.

## Tests

```sh
go test -race ./tools/fake-oauth/...
```

The suite includes a cross-package interop test that drives the **real**
`platform/oauth.Service` (AuthCodeURL → Exchange → Refresh) against this fake for
both the standard and Slack shapes, proving the two stay compatible.
