# Asker web UI

React 18 + TypeScript + Vite single-page app, authenticated against the dev
Keycloak. Two views, switched by top-bar tabs (a `view` state toggle in
`App.tsx` — no router):

- **Search** — the M1 search page (query box, filters, result cards with safe
  snippet highlighting, paging). M3 adds rich rendering for media hits (image
  thumbnails, video/audio timestamp deep-links and modality badges).
- **Connectors** — M2 connector management: connect a source from the catalog
  and watch existing instances sync.

No UI framework — one plain stylesheet (`src/styles.css`).

## Connectors view

`src/connectors/ConnectorsPage.tsx` drives the management UI against the
gateway's connector endpoints (all behind the OIDC bearer):

```
GET    /v1/connectors            -> [{instance, sync}]
POST   /v1/connectors            {connector_id, display_name, config} -> instance
DELETE /v1/connectors/{id}
PUT    /v1/connectors/{id}/token {token}
```

- **Catalog** (`src/connectors/catalog.ts`): the known connector types grouped
  by category, each with config-field hints mirroring the Go connector schemas
  (e.g. gmail needs `user_email`, s3 needs `endpoint`/`bucket`, ical needs
  `feed_url`). Token/OAuth2 connectors are flagged `needsToken`.
- **Connect form** (`ConnectForm.tsx`): a display name + the connector's config
  fields + (for token connectors) a dev "paste a token" field. Submitting calls
  `createConnector` then `putConnectorToken` on the new instance. Real OAuth
  redirect is hub work — the form shows a clear dev affordance.
- **Instance list** (`InstanceList.tsx`): each instance's status badge, sync
  phase, `docs_emitted`, last-sync relative time, and `last_error` (surfaced as
  an alert). Delete is two-step (confirm). The page polls `listConnectors`
  every ~15s so sync progress updates live.

`ConnectorClient` lives in `src/api.ts` alongside `SearchClient` (same bearer
auth + `AbortController` plumbing). The Connectors view never uses
`dangerouslySetInnerHTML`; forms are label-associated and accessible.

## Media results (M3)

Media hits (`IMAGE` / `VIDEO` / `AUDIO`) carry four extra fields on the pinned
`/v1/search` Hit (snake_case, matching the gateway's hand-written JSON for
every other field): `start_ms`, `end_ms`, `modality`, `thumbnail_key`. They are
optional in `src/api.ts` because text-document hits omit them.

`ResultCard` (`src/components/ResultCard.tsx`) branches by `hit.type`:

- **IMAGE** — shows the thumbnail; a modality badge (e.g. "OCR") when an OCR
  chunk matched. Text snippet renders as before.
- **VIDEO / AUDIO** — shows the poster/thumbnail if present, and when
  `start_ms > 0` a visible, accessible "Jump to M:SS" deep-link plus a modality
  badge ("transcript" / "caption"). The link targets a media fragment
  (`#t=<seconds>`); wiring an actual seeking player is future work.
- **Text docs** — unchanged.

Thumbnails are bearer-authenticated, so `<img src>` cannot fetch them directly.
`MediaClient.fetchThumbnail(key)` (in `src/api.ts`) calls
`GET {API}/v1/media?key=...` with the JWT and returns an object URL; the
gateway proxies the internal-only hub endpoint and derives the tenant from the
verified token only. The `Thumbnail` component (`src/components/Thumbnail.tsx`)
runs that fetch in an effect (so the list never blocks on image bytes), shows a
shimmer placeholder until it resolves, falls back gracefully on error, and
revokes the object URL on unmount. As everywhere in this UI, nothing uses
`dangerouslySetInnerHTML`; timestamps and modality labels are plain text nodes.

## Prerequisites

- Node 26+ (matches the Docker build image `node:26-alpine`).
- The dev stack running for live use: `make dev-up` at the repo root brings up
  Keycloak (`http://localhost:8081`) and the gateway (`http://localhost:8080`).

## Dev workflow

```sh
cd web
npm install
npm run dev          # Vite dev server on http://localhost:3000 (strict port)
```

Open <http://localhost:3000>, click **Sign in**, and log in with a dev realm
user (`alice` / `password123`). The port matters: the Keycloak `asker-web`
client only allows `http://localhost:3000/*` (and `:8080`) redirect URIs.

Note: the gateway `/v1/search` endpoint lands in M1 wave 2 — until then,
searches against a running stack return errors from the gateway. The UI is
built against the pinned REST contract (`src/api.ts`) and fully covered by
tests using `src/mocks/searchMock.ts` (test-only fixture; there is no runtime
mock switch).

## Build, test, lint

```sh
npm run build        # typecheck (app + node configs) + vite build -> dist/
npm test             # vitest run (jsdom + @testing-library/react)
npm run lint         # eslint flat config, zero warnings allowed
npm run preview      # serve the production bundle locally
```

Docker image (multi-stage, nginx serving `dist/` on :80 with SPA fallback and
a `/healthz` endpoint):

```sh
docker build -t asker-web .
docker run --rm -p 3000:80 asker-web
```

`VITE_*` values are baked into the static bundle at build time; override them
for non-default environments with `--build-arg VITE_API_URL=... --build-arg
VITE_KEYCLOAK_URL=...`.

## Environment variables

Vite env vars (compile-time; set in a `.env.local` for `npm run dev`, or as
Docker build args):

| Variable            | Default                 | Purpose                       |
| ------------------- | ----------------------- | ----------------------------- |
| `VITE_KEYCLOAK_URL` | `http://localhost:8081` | Keycloak base URL             |
| `VITE_API_URL`      | `http://localhost:8080` | Asker gateway (REST API) base |

Realm `asker` and client `asker-web` are fixed in `src/auth.ts` — they match
the realm auto-imported by the compose stack.

## Auth flow

1. On load the app calls `keycloak.init({onLoad: "check-sso", pkceMethod:
   "S256", silentCheckSsoRedirectUri: <origin>/silent-check-sso.html})`.
   keycloak-js loads `public/silent-check-sso.html` in a hidden iframe to
   detect an existing Keycloak session without a visible redirect.
2. No session: the app shows a pre-auth screen; **Sign in** starts the
   Authorization Code + PKCE (S256) redirect flow — `asker-web` is a public
   client, so PKCE replaces a client secret.
3. Signed in: keycloak-js holds the access/refresh tokens in memory. Before
   every API call the client calls `keycloak.updateToken(30)` (refresh when
   the token expires within 30s), then sends `Authorization: Bearer <token>`.
   The gateway validates the JWT (issuer/audience/signature) and derives the
   tenant from verified claims only.
4. **Sign out** ends the Keycloak session and returns to the app root.

## Layout

```
src/
  api.ts               typed /v1/search + /v1/connectors clients (bearer auth)
  auth.ts              keycloak-js wrapper (init once, token refresh)
  App.tsx              auth gate + Search/Connectors tab switch
  search/filters.ts    filter state model -> SearchRequest mapping
  search/media.ts      media helpers (timestamp + modality formatting)
  SearchPage.tsx       search state machine (debounce, paging, states)
  components/          SearchBar, FilterSidebar, ResultCard, Snippet,
                       Thumbnail (bearer-auth media preview), Pagination,
                       States (skeleton/error/empty/idle)
  connectors/          catalog, ConnectorsPage, ConnectForm, InstanceList,
                       relativeTime (the connector management view)
  mocks/               test-only response fixtures (search + connectors)
public/
  silent-check-sso.html  keycloak silent SSO iframe endpoint
```

Snippets are rendered safely: the `<hi>...</hi>` highlight tokens from the
gateway are split out and emitted as React `<mark>` elements; everything else
is rendered as text nodes (auto-escaped). `dangerouslySetInnerHTML` is never
used.
