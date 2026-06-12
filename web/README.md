# Asker web UI

React 18 + TypeScript + Vite single-page app: the M1 search page (query box,
filters, result cards with safe snippet highlighting, paging) authenticated
against the dev Keycloak.

No UI framework — one plain stylesheet (`src/styles.css`).

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
  api.ts               typed /v1/search client (abortable, bearer auth)
  auth.ts              keycloak-js wrapper (init once, token refresh)
  search/filters.ts    filter state model -> SearchRequest mapping
  SearchPage.tsx       search state machine (debounce, paging, states)
  components/          SearchBar, FilterSidebar, ResultCard, Snippet,
                       Pagination, States (skeleton/error/empty/idle)
  mocks/searchMock.ts  test-only response fixture
public/
  silent-check-sso.html  keycloak silent SSO iframe endpoint
```

Snippets are rendered safely: the `<hi>...</hi>` highlight tokens from the
gateway are split out and emitted as React `<mark>` elements; everything else
is rendered as text nodes (auto-escaped). `dangerouslySetInnerHTML` is never
used.
