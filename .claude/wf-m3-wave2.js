export const meta = {
  name: 'asker-m3-wave2',
  description: 'M3 wave 2: web media UI, gateway thumbnail proxy, media e2e (spoken-phrase->video timestamp + text->image)',
  phases: [{ title: 'Build', detail: '3 parallel builders' }],
}

const RESULT = {
  type: 'object',
  required: ['files', 'summary', 'decisions', 'issues'],
  properties: {
    files: { type: 'array', items: { type: 'string' } },
    summary: { type: 'string' },
    decisions: { type: 'array', items: { type: 'string' } },
    issues: { type: 'array', items: { type: 'string' } },
  },
}

const BASE = `
# Asker M3 Wave 2 — Shared Build Contract (BINDING)

Milestone M3 (media pipeline), final wave. Repo /Users/dio/works/asker. M0-M2 + M3 wave0/1 are
COMMITTED and green: media docs (IMAGE/AUDIO/VIDEO) flow connectors/upload -> hub -> ingest
(pass-through) -> enrich (OCR/CLIP/Whisper/ffmpeg -> chunks with embeddings + start_ms/end_ms +
modality + MediaInfo) -> index-writer -> Vespa (embedding + clip_embedding + parallel chunk arrays +
media fields); the query service has a CLIP text->image arm and fills Hit.start_ms/end_ms/modality/
thumbnail_key. Read FIRST: docs/adr/ADR-013-media-pipeline.md; the query Hit proto
(platform/proto/.../asker/query/v1, Hit has start_ms/end_ms/modality/thumbnail_key); the gateway
REST search shape (web/src/api.ts + services/gateway); vespa/README.md (matched-chunk resolution).

## Hard rules
- Write ONLY within your owned paths. NEVER modify go.mod/go.sum/Makefile/platform/**/other services/
  connectors/deploy/compose (integrator wires compose) except where your task explicitly owns a path.
- Go deps FROZEN; stdlib testing. Verify your dir: gofmt; go build + go test -race; golangci-lint 0
  issues (US-locale "canceled"). Web: npm run build + npm test + npm run lint all green. Bash e2e:
  set -euo pipefail, numbered checks, PASS/FAIL table, python3 for JSON (no jq), env-overridable URLs,
  cleanup trap, non-zero exit on failure (house style: tools/e2e/smoke.sh, m1-e2e.sh).
- Never log/leak credentials. Tenancy sacred.

## Pinned thumbnail-serving contract (gateway + web agree on this)
- Gateway adds: GET /v1/media?key=<blobKey>  (OIDC bearer; tenant from the verified JWT ONLY) ->
  proxies to the hub GET {HUB_HTTP_URL}/internal/media?key=<key> with header
  x-asker-tenant: <tenant>, streaming the decrypted bytes back with the upstream Content-Type. 404
  if absent; 401 unauth. This is how the browser fetches a Hit's thumbnail_key (the hub endpoint is
  internal-only). The web client fetches it WITH the bearer token (fetch -> objectURL), since <img>
  can't send Authorization.

Your structured result lists EVERY file written and flags integrator items (compose, etc.) in issues.
`

phase('Build')

const tasks = [
  {
    label: 'gateway-media',
    prompt: `${BASE}

## Your task: gateway thumbnail/media proxy. Owned paths: services/gateway/** (extend; keep ALL
existing routes/tests green). Read all of services/gateway first (auth middleware, deps/clients,
routes.go, the HUB_HTTP_URL config used by /v1/upload).
Add an authed media proxy per the pinned contract: GET /v1/media?key=<blobKey>.
- Behind the existing OIDC auth + rate-limit middleware. Tenant from the verified JWT
  (tenancy.FromContext) — NEVER from the query/header. Reject empty/malformed key (400).
- Proxy to {HUB_HTTP_URL}/internal/media?key=<urlencoded key> with header
  x-asker-tenant: tenancy.HeaderValue(tc); stream the response body back (io.Copy with a size cap,
  e.g. MAX_MEDIA_MB env default 25 for thumbnails), pass through Content-Type and the upstream
  status (200/404). Use a dedicated http.Client with a timeout. The hub already fail-closes on
  tenant prefix, but ALSO ensure the key the user passes is forwarded as-is (the hub enforces the
  tenant prefix) — document that the gateway relies on the hub's tenant-scoping (defense in depth:
  the x-asker-tenant we send is the JWT tenant, so a user can only ever read their own tenant's
  blobs).
- CORS: the existing CORS middleware must allow GET /v1/media for the web origin (it likely already
  wraps all routes — confirm).
Tests (extend the suite): /v1/media without token -> 401; with token -> proxies with
x-asker-tenant == the JWT tenant (assert against an httptest fake hub, THE isolation property);
streams bytes + content-type; 404 passthrough; oversize cap; bad key -> 400. Keep every existing
gateway test passing. Note for integrator: no compose change needed (HUB_HTTP_URL already set).`,
  },
  {
    label: 'web-media-ui',
    prompt: `${BASE}

## Your task: media results in the web UI. Owned paths: web/** (the search + connectors UI exists;
ADD media rendering without breaking them). Read web/src (api.ts types incl. the Hit fields, the
search results components, styles).
Render media hits richly:
1. api.ts: extend the Hit type with startMs/endMs/modality/thumbnailKey (match the gateway JSON
   field names — read what the gateway emits; the proto json is start_ms etc., the gateway may
   camelCase — confirm against services/gateway and use the ACTUAL wire names). Add
   fetchThumbnail(key): fetches GET {API}/v1/media?key=... WITH the bearer token and returns an
   object URL (revoke on unmount). Reuse the existing auth/token plumbing.
2. Result cards by type:
   - IMAGE: show the thumbnail (via fetchThumbnail(hit.thumbnailKey)); modality badge if OCR matched.
   - VIDEO/AUDIO: show thumbnail/poster if present, and when start_ms>0 show a "jump to MM:SS" deep-
     link/label formatted from start_ms, with a modality badge ("transcript"/"caption"). The deep-
     link is a visible, accessible control showing the timestamp (a real player is out of scope;
     render the timestamp + a link/button that would seek — document that wiring a player is future).
   - Text docs: unchanged.
   Loading/placeholder for thumbnails (don't block the list on image fetches); never
   dangerouslySetInnerHTML; safe rendering.
3. Tests (vitest + testing-library): an IMAGE hit renders an <img> sourced via fetchThumbnail (mock
   the api); a VIDEO hit with start_ms renders the formatted timestamp deep-link + modality badge; a
   text hit is unchanged; timestamp formatting (ms -> M:SS / H:MM:SS) is unit-tested; fetchThumbnail
   sends the token and revokes object URLs. npm run build + npm test + npm run lint all green.
Update web/README.md briefly. Match the existing visual style.`,
  },
  {
    label: 'media-e2e',
    prompt: `${BASE}

## Your task: the M3 media e2e (THE exit criterion) + committed media fixtures. Owned paths:
tools/e2e/m3-media.sh, tools/e2e/fixtures/media/** (commit tiny generated media), and a small
generator script tools/e2e/gen-media-fixtures.sh.
The full stack is brought up by the integrator (make dev-up). Your suite runs against it (host
endpoints: gateway http://localhost:8080, keycloak 8081 alice/password123, fake-gmail 9400 unused
here). Media reaches Asker via the gateway upload: POST /v1/upload (multipart file=@<media>,
title=...) -> 202 {doc_id} (the upload connector stores the blob + emits an IMAGE/VIDEO/AUDIO doc;
the enrich worker OCR/ASR/CLIP-enriches it). Then GET /v1/search?q=... returns hits with start_ms/
modality/thumbnail_key.

1. tools/e2e/gen-media-fixtures.sh (run once, locally, by you to PRODUCE the committed fixtures —
   macOS host tools: \`say\`, \`ffmpeg\`, \`sips\`/\`ffmpeg\` for images):
   - A short SPEECH VIDEO with a KNOWN phrase: \`say -o /tmp/p.aiff "the quarterly roadmap review
     happens on tuesday afternoon"\` then ffmpeg to mux that audio with a simple colored/keyframed
     video track into a small .mp4 (a few seconds; keep it tiny). Put the known phrase + the
     expected searchable rare word(s) in a sidecar .txt/JSON so the test asserts deterministically.
   - A couple of distinct IMAGES with recognizable visual content for text->image (e.g. ffmpeg
     lavfi testsrc/color + drawtext, or solid-color images: one clearly "blue" one clearly "red",
     sized small). Commit them.
   Commit the generated media under tools/e2e/fixtures/media/ (tiny — tens of KB). The generator is
   committed too (documents how they were made; CI does NOT run it — CI has no \`say\` — it uses the
   committed fixtures). Print sizes; keep total fixtures well under ~1MB.
2. tools/e2e/m3-media.sh — the suite (env-overridable URLs; default the same as smoke.sh):
   - Preflight: gateway healthz; token for alice; assert clip + enrich are reachable indirectly
     (a search works).
   - SPOKEN-PHRASE -> VIDEO@TIMESTAMP (the exit criterion): upload the committed speech .mp4 as
     alice; poll /v1/search for a rare word from the known phrase (e.g. "roadmap") until a hit
     appears (generous timeout — first media doc triggers whisper-tiny model download; poll up to
     ~600s with progress). Assert: the hit is the uploaded VIDEO doc, modality is "asr", and
     start_ms corresponds to where the phrase occurs (assert start_ms is within the clip's duration
     and > 0 OR == the segment start; since the whole phrase is the clip, assert start_ms is a
     plausible segment offset and end_ms>start_ms). Report the returned timestamp.
   - TEXT -> IMAGE: upload the two distinct images as alice; search a text query describing one
     (e.g. "a blue image"/"the color blue") and assert the matching image doc ranks above the other
     (CLIP arm), and that an IMAGE hit carries a thumbnail_key; fetch GET /v1/media?key=<thumbnail_key>
     with alice's token -> 200 image bytes (the thumbnail pipeline end to end).
   - OCR: (if the image fixtures contain drawn text) search the drawn word and assert the image hit
     with modality "ocr". (Optional but nice; include if your fixtures have text.)
   - TENANT ISOLATION: bob searching alice's phrase/image rare tokens -> 0 hits (media must respect
     the same per-tenant isolation).
   - Cleanup trap; summary table; non-zero exit on any FAIL. Scale knobs via env.
3. Because the e2e needs the live heavy stack (clip + enrich models), you CANNOT run it to green
   yourself unless the stack is up. Do this: VERIFY the script with bash -n + shellcheck-style
   review, GENERATE + COMMIT the fixtures (you have say/ffmpeg), and run as far as the stack allows
   if it happens to be up (try \`curl -fsS http://localhost:8080/healthz\`); if up, run it and paste
   the result; if not, say so clearly and leave the suite + fixtures ready for the integrator to run
   via \`make dev-up && bash tools/e2e/m3-media.sh\`. Add a Makefile-style note in issues (integrator
   adds an e2e-m3-media target + CI step). Report the exact known phrase + expected rare words.`,
  },
]

const results = await parallel(
  tasks.map((t) => () => agent(t.prompt, { label: t.label, phase: 'Build', schema: RESULT }))
)
const out = {}
tasks.forEach((t, i) => { out[t.label] = results[i] || { summary: 'AGENT FAILED', files: [], decisions: [], issues: ['no return'] } })
return out
