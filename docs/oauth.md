# Connector OAuth — operator guide

How Asker connects to a user's cloud sources (Gmail, Google Drive/Calendar, Outlook/Teams,
Slack, Jira/Confluence) over OAuth 2.0, what an operator must register and configure for each
provider, and the security properties the flow guarantees. The OAuth machinery is the shared
`platform/oauth` package; the gateway drives the browser flow and the connector hub uses the
stored tokens. Nothing here weakens the sacred invariant — **`tenant_id` comes ONLY from the
verified OIDC token; no operation crosses tenants** (see [`security.md`](security.md)).

In **dev/CI you need nothing real**: a fake OAuth provider stands in for Google/Microsoft/Slack/
Atlassian, and `make dev-up` wires it. Real client IDs/secrets are operator-supplied for a
production deployment, as documented below.

## 1. The model

Asker uses the **authorization-code grant with PKCE (S256)** and refresh tokens:

1. **Start (authed).** The user, signed in to the web UI, clicks "Connect" on a connector
   instance. The web calls `GET /v1/connectors/{id}/oauth/start` with the user's bearer token.
   The gateway derives `tenant_id` from that verified token (never from the request), looks up
   the connector's provider and default read scopes via `oauth.ConnectorOAuth`, generates a PKCE
   verifier + challenge and an unguessable random `state`, **stores the verifier + tenant +
   connector instance server-side keyed by `state`** (Redis, short TTL, single-use), and returns
   `{"authorize_url": "<provider authorize URL>"}`. The web navigates the browser there.

2. **Consent at the provider.** The user authenticates and consents at the provider; the
   provider redirects the browser back to Asker's **fixed, registered callback**:
   `<GATEWAY_PUBLIC_URL>/v1/oauth/callback?code=...&state=...`.

3. **Callback (public, no bearer).** The gateway looks up the server-side state by `state`
   (deleting it — **single use**), rebuilds the trusted `tenant_id` and connector instance from
   it, and calls `oauth.Service.Exchange` (sending the PKCE verifier) to swap the `code` for a
   token. It stores `oauth.Marshal(Token{...})` in the **same envelope-encrypted control-plane
   vault** that holds manually-pasted tokens (via control-plane `PutToken`, with `x-asker-tenant`
   set from the trusted state). On success it `302`s the browser to
   `<WEB_APP_URL>/connectors?oauth=connected`; on any error to
   `<WEB_APP_URL>/connectors?oauth=error` — never leaking a code, token, or internal detail.

4. **Use + refresh.** When the connector hub runs the instance it reads the stored token
   (`GetToken` → `oauth.Parse`), and if `Token.NeedsRefresh(now, skew)` is true it calls
   `oauth.Service.Refresh` (using the refresh token) and writes the renewed token back. It then
   hands the **access token** to the connector as `sdk.Config.Token` — the connector only ever
   sees a live, scoped bearer, never the refresh token, client secret, or vault
   (see [`connectors/building-a-connector.md`](connectors/building-a-connector.md)).

Tokens are stored as a versioned JSON blob (`asker_oauth: "v1"`) so the vault keeps treating
them as opaque `[]byte`; a legacy hand-pasted token (e.g. `fake-gmail-token:<email>`) lacks the
marker, so `oauth.Parse` returns `ok=false` and the hub passes it through unchanged. This is why
the manual-token path and the OAuth path coexist on the same connector instance.

## 2. Connector → provider → scopes

The mapping and default **read** scopes are defined in `platform/oauth` (`connectorMapping`) and
returned by `oauth.ConnectorOAuth(connectorID)`. A connector not in this table is not an OAuth
connector (`ok=false`), and its `/oauth/start` returns a 4xx.

| Connector ID | Provider | Default scopes (read) |
| :--- | :--- | :--- |
| `gmail` | Google | `https://www.googleapis.com/auth/gmail.readonly` |
| `gcal` | Google | `https://www.googleapis.com/auth/calendar.readonly` |
| `gdrive` | Google | `https://www.googleapis.com/auth/drive.readonly` |
| `outlook-mail` | Microsoft | `Mail.Read`, `offline_access` |
| `outlook-cal` | Microsoft | `Calendars.Read`, `offline_access` |
| `msteams` | Microsoft | `Chat.Read`, `ChannelMessage.Read.All`, `offline_access` |
| `slack` | Slack | `channels:history`, `channels:read`, `users:read` |
| `jira` | Atlassian | `read:jira-work`, `offline_access` |
| `confluence` | Atlassian | `read:confluence-content.all`, `offline_access` |

Provider-specific details `platform/oauth` handles for you (you do not configure these):

- **Google** requests offline access via auth-URL params (`access_type=offline&prompt=consent`),
  not a scope, so its default scope list has no `offline_access`.
- **Microsoft** expresses refresh as the `offline_access` scope; its endpoints embed the tenant
  segment (`ASKER_OAUTH_MICROSOFT_TENANT`, default `common`).
- **Slack** issues user tokens: scopes go in `user_scope` (not `scope`), and the token is read
  from `authed_user.access_token`. Slack user tokens are typically non-expiring and
  non-refreshable — `Refresh` returns them unchanged.
- **Atlassian** adds `audience=<api.atlassian.com>&prompt=consent`
  (`ASKER_OAUTH_ATLASSIAN_AUDIENCE`, default `api.atlassian.com`).

## 3. The one redirect URI to register

Every provider must be configured with **exactly one** Asker redirect/callback URI:

```
<GATEWAY_PUBLIC_URL>/v1/oauth/callback
```

`GATEWAY_PUBLIC_URL` is the externally reachable base URL of the Asker gateway (e.g.
`https://asker.example.com`). This URI is fixed server-side — it is **not** taken from any
request — so it is also the only redirect the provider will honor, and the post-callback browser
redirect goes to a separate fixed `WEB_APP_URL` (no open redirect). For local dev the value is
`http://localhost:8080/v1/oauth/callback` (most providers require HTTPS for real apps, which is
why dev uses the fake provider instead — see §6).

## 4. Registering a real app, per provider

For each provider you register an OAuth client, copy its **client ID + secret** into the Asker
environment (§5), and register the callback URI from §3. Request the read scopes from §2.

### Google (Gmail, Calendar, Drive)

1. In the **Google Cloud Console**, create or select a project.
2. **APIs & Services → Enable APIs**: enable the **Gmail API**, **Google Calendar API**, and/or
   **Google Drive API** for the connectors you will use.
3. **OAuth consent screen**: configure it (External or Internal), and add the scopes from §2
   (e.g. `.../auth/gmail.readonly`). These are **restricted/sensitive** scopes; a public app
   needs Google verification before general users can consent.
4. **Credentials → Create credentials → OAuth client ID → Web application**. Under **Authorized
   redirect URIs** add `<GATEWAY_PUBLIC_URL>/v1/oauth/callback`.
5. Copy the **Client ID** and **Client secret** →
   `ASKER_OAUTH_GOOGLE_CLIENT_ID` / `ASKER_OAUTH_GOOGLE_CLIENT_SECRET`.

### Microsoft (Outlook mail/calendar, Teams)

1. In the **Microsoft Entra admin center** (Azure AD) → **App registrations → New registration**.
2. **Redirect URI**: platform **Web**, value `<GATEWAY_PUBLIC_URL>/v1/oauth/callback`.
3. **API permissions → Microsoft Graph → Delegated permissions**: add the scopes from §2
   (`Mail.Read`, `Calendars.Read`, `Chat.Read`, `ChannelMessage.Read.All`) plus `offline_access`.
   Some Graph permissions require admin consent.
4. **Certificates & secrets → New client secret**; copy the secret **value** (not the ID).
5. Copy **Application (client) ID** and the secret →
   `ASKER_OAUTH_MICROSOFT_CLIENT_ID` / `ASKER_OAUTH_MICROSOFT_CLIENT_SECRET`. If you registered a
   single-tenant app, set `ASKER_OAUTH_MICROSOFT_TENANT` to your tenant ID (default `common`).

### Slack

1. At **api.slack.com/apps → Create New App** (from scratch).
2. **OAuth & Permissions → Redirect URLs**: add `<GATEWAY_PUBLIC_URL>/v1/oauth/callback`.
3. Under **User Token Scopes** (Asker uses user tokens) add `channels:history`, `channels:read`,
   `users:read`.
4. From **Basic Information** copy the **Client ID** and **Client Secret** →
   `ASKER_OAUTH_SLACK_CLIENT_ID` / `ASKER_OAUTH_SLACK_CLIENT_SECRET`.

### Atlassian (Jira, Confluence)

1. At **developer.atlassian.com → My apps → Create → OAuth 2.0 integration**.
2. **Authorization → OAuth 2.0 (3LO)**: set the **Callback URL** to
   `<GATEWAY_PUBLIC_URL>/v1/oauth/callback`.
3. **Permissions**: add the Jira and/or Confluence APIs and the granular scopes from §2
   (`read:jira-work`, `read:confluence-content.all`), and enable `offline_access`.
4. From **Settings** copy the **Client ID** and **Secret** →
   `ASKER_OAUTH_ATLASSIAN_CLIENT_ID` / `ASKER_OAUTH_ATLASSIAN_CLIENT_SECRET`.

## 5. Environment

`platform/oauth.LoadConfig()` reads, for each provider `<P>` ∈ `GOOGLE|MICROSOFT|SLACK|ATLASSIAN`:

| Variable | Required | Notes |
| :--- | :--- | :--- |
| `ASKER_OAUTH_<P>_CLIENT_ID` | yes (prod) | OAuth client/application ID. |
| `ASKER_OAUTH_<P>_CLIENT_SECRET` | yes (prod) | Client secret. **Never logged.** |
| `ASKER_OAUTH_<P>_AUTH_URL` | dev/fake only | Authorize endpoint override; defaults to the real provider URL. |
| `ASKER_OAUTH_<P>_TOKEN_URL` | dev/fake only | Token endpoint override; defaults to the real provider URL. |
| `ASKER_OAUTH_MICROSOFT_TENANT` | optional | Azure AD tenant segment; default `common`. |
| `ASKER_OAUTH_ATLASSIAN_AUDIENCE` | optional | Atlassian resource audience; default `api.atlassian.com`. |

A provider is **configured** (its flows can run) only when both `CLIENT_ID` and `CLIENT_SECRET`
are set (`Config.IsConfigured`). In **production set only `_CLIENT_ID`/`_CLIENT_SECRET`** — the
authorize/token URLs default to the real provider endpoints, so leave `_AUTH_URL`/`_TOKEN_URL`
unset. The `_AUTH_URL`/`_TOKEN_URL` overrides exist to point dev/CI at the fake provider (§6).

Two non-`platform/oauth` URLs the gateway needs (operator-supplied, fixed, never from a request):

| Variable | Meaning |
| :--- | :--- |
| `GATEWAY_PUBLIC_URL` | Externally reachable gateway base; the registered callback is `<GATEWAY_PUBLIC_URL>/v1/oauth/callback`. |
| `WEB_APP_URL` | Web UI base the callback redirects to (`<WEB_APP_URL>/connectors?oauth=connected\|error`). |

## 6. The dev story (no real credentials)

In dev/CI a **fake OAuth provider** implements the standard authorize + token endpoints
`platform/oauth` expects. `make dev-up` points the `_AUTH_URL`/`_TOKEN_URL` overrides at it and
sets throwaway `_CLIENT_ID`/`_CLIENT_SECRET`, so the providers count as configured without any
real registration. The fake auto-consents and redirects straight to the gateway callback.

For the **Google/Gmail** flow the fake issues an access token of the form
`fake-gmail-token:<email>` (the email from a `login_hint` query param if present, else a
configured default such as `alice@example.com`). That is exactly the bearer the dev **fake-gmail**
service expects (`Authorization: Bearer fake-gmail-token:<email>`), so the gmail connector's call
after the OAuth flow succeeds end to end.

Walk the whole flow with the e2e script:

```sh
make e2e-oauth      # tools/e2e/oauth.sh — drives start -> consent -> callback -> sync -> search
```

It is env-overridable (`GATEWAY_URL`, `FAKE_OAUTH_URL`, `FAKE_GMAIL_URL`, `WEB_APP_URL`,
`OAUTH_E2E_EMAIL`, …) and prints a numbered PASS/FAIL summary, exiting non-zero on any failure.
See also the README "Try it" section for the manual-token path on the same connector.

## 7. Security properties

The flow is built so the unauthenticated callback can never be used to cross tenants or leak
secrets (this is the contract the gateway implements; the OAuth crypto lives in `platform/oauth`):

- **Tenant from the authed start, never the callback.** `/oauth/start` is authenticated, so
  `tenant_id` is derived from the verified OIDC token (ADR-002). The unauthenticated callback
  rebuilds `tenant_id` + the connector instance from the **server-side state** that `/oauth/start`
  created — never from the callback's query/body. `PutToken` runs with `x-asker-tenant` from that
  trusted state.
- **CSRF — single-use random `state`.** The `state` is an unguessable random value stored
  server-side with a short TTL and **deleted on first callback**; a replayed or forged callback
  finds no state and fails closed to `oauth=error`. (`tools/e2e/oauth.sh` asserts this.)
- **PKCE (S256).** The verifier is generated by the caller and held server-side; only the S256
  challenge is sent to the provider, and the verifier is sent on the code exchange — an
  intercepted `code` is useless without it.
- **No open redirect.** The post-callback redirect target is the fixed configured `WEB_APP_URL`,
  not request input. The provider redirect URI is the fixed registered
  `<GATEWAY_PUBLIC_URL>/v1/oauth/callback`.
- **No secret leakage.** The callback redirect carries only `oauth=connected|error` — never a
  code, access token, refresh token, or internal error. `platform/oauth` and its callers never
  log access/refresh tokens or client secrets. Stored tokens are envelope-encrypted under the
  per-tenant DEK in the control-plane vault (asset A4 in [`security.md`](security.md)).

## Related

- [`security.md`](security.md) — threat model, the sacred isolation invariant, the token vault (A4),
  SSRF egress controls (relevant to the gateway's outbound calls to provider token endpoints).
- [`connectors/building-a-connector.md`](connectors/building-a-connector.md) — how a connector
  receives the live access token in `sdk.Config.Token` (and why it never sees the refresh token).
- [`runbooks/top-10-failure-modes.md`](runbooks/top-10-failure-modes.md) — failure mode 8,
  connector OAuth token refresh failures.
- The README "Dev URLs" table and "Try it" section — the dev stack and the manual-token path.
