export const meta = {
  name: 'asker-m3-wave0',
  description: 'M3 wave 0: Vespa schema v2 (CLIP + media), ingest media routing, index-writer media, clip service, hub media endpoint, query CLIP arm',
  phases: [{ title: 'Build', detail: '6 parallel foundation builders' }],
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
# Asker M3 Wave 0 — Shared Build Contract (BINDING)

Milestone M3 (media pipeline) of Asker, a multi-tenant personal search engine. Repo
/Users/dio/works/asker (Go module github.com/asker/asker, Go 1.26; Python 3.12 for ML workers).
M0+M1+M2 are COMMITTED and green: the full pipeline (connectors -> hub -> Kafka -> ingest -> enrich
[Python, bge-m3 via TEI] -> index-writer -> Vespa streaming -> query [hybrid + degradation] ->
gateway -> web) runs; 14 connectors; the M3 PROTO foundation is committed. Read FIRST:
- docs/adr/ADR-013-media-pipeline.md (THE M3 design — two embedding spaces, the clip service, the
  hub /internal/media endpoint, per-type pipeline, chunk time anchoring). Everything below conforms.
- docs/adr/ADR-005 (EMBEDDING_DIM), ADR-006 (hybrid ranking + degradation), ADR-007 (Python enrich),
  ADR-009 (internal trust boundary).
- platform/proto/.../asker/v1 document.proto (Chunk.start_ms/end_ms/modality, Document.media =
  MediaInfo{duration_ms,width,height,thumbnail BlobRef, repeated Keyframe, transcript_lang}) and
  asker/query/v1 query.proto (Hit.start_ms/end_ms/modality/thumbnail_key) — JUST regenerated.
- vespa/README.md + vespa/app/schemas/doc.sd (current schema, hybrid/keyword profiles, the feed JSON
  shape + the YQL the query service issues) — your changes extend these.
- The service you own (read its current code before changing it).

## Hard rules
- Write ONLY within your owned paths (named in your task). NEVER modify go.mod/go.sum/Makefile/
  .github/deploy/compose/** (the integrator wires compose), platform/proto/** (generated; done),
  other services/connectors. Needs outside scope -> issues.
- Go deps FROZEN (NO go get/tidy/edit). Python: pin deps in requirements.txt. stdlib testing for Go.
- Verify: gofmt -w; go build + go test -race on your Go dir; ./bin/golangci-lint run on it (0 issues,
  US-locale "canceled"). Python: ruff clean + pytest. slog/structured logs; never log creds/bodies.
- Tenancy sacred: tenant only from cfg/ctx tenancy.Context, never request/source data.

## Pinned cross-component contracts (other builders + wave 1 code against these EXACTLY)

### Two embedding spaces / dims
- bge-m3 text space: env EMBEDDING_DIM (dev 384). Vespa field 'embedding'
  tensor<bfloat16>(chunk{},x[@EMBEDDING_DIM@]) — EXISTS.
- CLIP space: env CLIP_DIM (dev 512, ViT-B/32). NEW Vespa field 'clip_embedding'
  tensor<bfloat16>(chunk{},x[@CLIP_DIM@]). The schema template token is @CLIP_DIM@ (mirrors how
  @EMBEDDING_DIM@ works); vespa/deploy.sh substitutes \${CLIP_DIM:-512}.

### clip model service (services/clip, Python, HTTP) — analogous to TEI
- POST /embed/text  body {"inputs":["q1","q2"]}      -> {"embeddings":[[float x CLIP_DIM], ...]}
- POST /embed/image body {"images_b64":["<base64>"]} -> {"embeddings":[[float x CLIP_DIM], ...]}
- GET  /health -> 200 when the model is loaded.
- Both encoders come from ONE loaded open_clip model so text and image vectors share the space;
  embeddings are L2-normalized (so Vespa 'angular'/dotproduct closeness is cosine). Env: CLIP_MODEL
  (default ViT-B-32), CLIP_PORT (default :9800), CLIP_PRETRAINED (default openai). Listens :9800.

### Vespa doc feed additions (index-writer PUTs these; vespa schema declares them)
- clip_embedding: mixed tensor<bfloat16>(chunk{},x[CLIP_DIM]) in Vespa blocks form
  {"blocks":{"<chunkIndex>":[...]}} — present ONLY for chunks that carry a CLIP vector (image /
  keyframe chunks); omit the field entirely when no chunk has one.
- chunk_starts_ms, chunk_ends_ms: array<long> parallel to the 'chunks' array (same index order;
  0 for text chunks).
- chunk_modalities: array<string> parallel to 'chunks' ("text"|"ocr"|"asr"|"caption").
- media_duration_ms (long), media_width (int -> long in Vespa), media_height (long),
  thumbnail_key (string), transcript_lang (string) — from Document.media. Omit/zero for text docs.
  Keyframes: encode as part of chunks (each keyframe is a chunk with a clip_embedding + start_ms +
  modality "caption"); also store thumbnail_key for the poster.
- doc path unchanged: /document/v1/asker/doc/group/<tenant>/<doc_id>, POST full put.

### Query result contract (query service fills Hit; gateway/web already expect these proto fields)
- For a hit, return Hit.start_ms/end_ms = the MATCHED chunk's time offset (0 for whole-doc/text),
  modality = the matched chunk's modality, thumbnail_key = the doc's thumbnail.
- text->image (CLIP arm): the query service POSTs the query text to clip /embed/text and runs a
  Vespa nearestNeighbor over clip_embedding; merge with the existing bge-m3 text/hybrid arm,
  dedupe by doc_id, blend scores. Degradation (ADR-006): clip service down -> drop the CLIP arm
  (log once), never fail closed.

Your structured result lists EVERY file you wrote and flags anything for the integrator (compose
wiring, Makefile CLIP_DIM export) in issues.
`

phase('Build')

const tasks = [
  {
    label: 'vespa-schema',
    prompt: `${BASE}

## Your task: Vespa schema v2 + deploy templating + README. Owned paths: vespa/**.
1. doc.sd: add the fields in the "Vespa doc feed additions" contract — clip_embedding
   tensor<bfloat16>(chunk{},x[@CLIP_DIM@]) attribute (distance-metric for CLIP cosine on normalized
   vectors — match how 'embedding' is configured), chunk_starts_ms/chunk_ends_ms (array<long>,
   attribute|summary), chunk_modalities (array<string>, attribute|summary), media_duration_ms/
   media_width/media_height (long, attribute|summary), thumbnail_key (string, attribute|summary),
   transcript_lang (string, attribute|summary). KEEP all existing fields/profiles working (M0 smoke +
   M1/M2 must still pass).
2. Rank profiles: add a 'clip' profile whose first-phase is closeness(field, clip_embedding) (inputs
   query(qclip) tensor<float>(x[@CLIP_DIM@])), for the text->image arm. Keep 'keyword'/'hybrid'.
   Verify against Vespa 8 streaming docs that nearestNeighbor(clip_embedding,qclip) works the same
   way as the existing embedding field; cite what you verified.
3. THE KEY MECHANIC — returning the matched chunk: the query path must learn WHICH chunk element
   matched so it can return that chunk's start_ms/modality. Research and implement the Vespa-native
   way: for the CLIP nearestNeighbor arm use the closest(clip_embedding) rank feature (returns the
   label/index of the nearest cell) surfaced via summary-features; for the text/ASR arm, the
   array<string> dynamic chunk_snippets already marks matched elements. Expose enough in the
   document-summary 'search' (summary-features and/or the parallel arrays) for the query service to
   resolve the matched chunk index -> chunk_starts_ms[index] + chunk_modalities[index]. WebFetch
   https://docs.vespa.ai/en/reference/rank-features.html (closest/elementwise) and
   https://docs.vespa.ai/en/streaming-search.html to confirm; document exactly how the query service
   should read the matched-chunk index from the response (this is the contract the query builder
   relies on — be concrete with the JSON path).
4. deploy.sh: template @CLIP_DIM@ -> \${CLIP_DIM:-512} alongside the existing @EMBEDDING_DIM@
   substitution (same staging-copy+sed mechanism). Update the header comment + the env doc.
5. vespa/README.md: document the new fields, the clip profile, @CLIP_DIM@ templating, the feed JSON
   for clip_embedding blocks + parallel arrays + media fields (a concrete example), and the
   matched-chunk-index read path for the query service.
Validation without docker: xmllint/review + WebFetch the Vespa docs; note residual risk (first real
validation is the integrator's make dev-up). If a field/profile would be rejected, say so.`,
  },
  {
    label: 'clip-service',
    prompt: `${BASE}

## Your task: services/clip — the CLIP model service (Python). Owned paths: services/clip/**.
Build a small HTTP service exposing the pinned clip contract: POST /embed/text {"inputs":[...]} and
POST /embed/image {"images_b64":[...]} -> {"embeddings":[[float x CLIP_DIM]]}, GET /health.
- Use open_clip_torch (open_clip) + torch (CPU) + pillow. ONE loaded model (CLIP_MODEL env, default
  ViT-B-32; CLIP_PRETRAINED default openai) provides BOTH encoders so text & image vectors share the
  space; L2-normalize every embedding (so Vespa cosine/closeness is correct). Decode images_b64
  (base64) with Pillow, preprocess with the model's transform, encode; tokenize text with open_clip
  tokenizer, encode. Batch sensibly; cap batch size; bound request size.
- requirements.txt PINNED (open_clip_torch, torch CPU wheel — pick a CPU-friendly version, pillow,
  the HTTP layer: use stdlib http.server or a tiny framework already implying NO new heavy dep —
  prefer stdlib http.server in a threadpool to avoid extra deps; pytest, ruff). pyproject.toml for
  ruff (line-length 100) + pytest. Dockerfile (python:3.12-slim, non-root, model can be pre-pulled
  or lazy-loaded on first request with a warmup; document the first-call latency). Listens :9800,
  health on the same port.
- Structure for testability: factor the encoder behind a small interface so tests can inject a fake
  that returns deterministic CLIP_DIM-length unit vectors WITHOUT downloading torch/CLIP (the real
  model is too heavy for unit tests). Unit-test: the HTTP layer (routes, JSON shapes, base64 decode,
  batch handling, error cases, health), embedding length == CLIP_DIM, L2-normalization, against the
  fake encoder. A real-model smoke is integration (document running it; do NOT download models in
  unit tests). ruff clean; pytest green.
- services/clip/README.md: the API, the model/dim config, first-load latency, the dev/CI small model.
Note for the integrator (issues): compose must run services/clip (:9800, host 127.0.0.1:9800
optional), CLIP_MODEL/CLIP_DIM env, a model-cache volume, generous health start_period (model load).`,
  },
  {
    label: 'ingest-media',
    prompt: `${BASE}

## Your task: services/ingest media-type routing. Owned paths: services/ingest/**.
Read services/ingest/handler.go + chunker. Today ingest normalizes + dedupes + text-chunks docs.
For MEDIA documents (Document.type IMAGE/AUDIO/VIDEO, or original.content_type image//audio//video/*)
there is no body_text to chunk — the enrich worker produces their chunks from the media. FIX:
- Detect media docs (by Type in {IMAGE,VIDEO,AUDIO}); for them, SKIP text chunking entirely and pass
  the document through to docs.chunked unchanged EXCEPT for normalization that still applies
  (ensure version_etag set). Do NOT emit zero-chunk text behavior or run the email/paragraph chunker
  on them. Keep dedupe (record-after-produce) working for media docs too (same (doc_id,version_etag)
  key) so re-delivery is idempotent.
- Non-media docs: unchanged (existing behavior, all current tests pass).
- Tombstones: unchanged pass-through.
Add table tests: an IMAGE/AUDIO/VIDEO doc (no body, has original BlobRef) passes through to
docs.chunked with NO chunks added and version_etag preserved/derived; a normal text doc still gets
chunked; dedupe still suppresses a replay of a media doc. Keep coverage >=80%. Document the routing
in a comment. (The actual OCR/ASR/CLIP chunking happens in the Python enrich worker, wave 1 — out of
your scope.)`,
  },
  {
    label: 'index-writer-media',
    prompt: `${BASE}

## Your task: services/index-writer media fields. Owned paths: services/index-writer/**.
Read services/index-writer/writer.go (the feed JSON builder). Extend it to write the M3 media fields
per the "Vespa doc feed additions" contract, WITHOUT breaking the existing text feed:
- clip_embedding: build the mixed-tensor blocks {"blocks":{"<i>":[...]}} from chunks that have a
  CLIP vector. NOTE the proto Chunk has ONE 'embedding' field (bge-m3). For M3, a chunk's CLIP
  vector also arrives in 'embedding' for CLIP-only chunks? NO — resolve this cleanly: a chunk is
  EITHER a text/bge-m3 chunk (embedding len==EMBEDDING_DIM, modality text/ocr/asr) OR a CLIP chunk
  (embedding len==CLIP_DIM, modality caption/keyframe). Decide which Vespa field a chunk's vector
  goes to BY ITS LENGTH: len==EMBEDDING_DIM -> 'embedding' block; len==CLIP_DIM -> 'clip_embedding'
  block. (Document this contract clearly — the wave-1 enrich worker fills Chunk.embedding with the
  CLIP vector for keyframe/image chunks and the bge-m3 vector for text chunks, distinguished by
  modality + length.) Validate each vector's length is EXACTLY EMBEDDING_DIM or CLIP_DIM (both env,
  required); anything else -> error so it dead-letters (a dim mismatch must never silently index).
- chunk_starts_ms/chunk_ends_ms (array<long>) and chunk_modalities (array<string>): parallel to the
  chunks array, same order.
- media_duration_ms/media_width/media_height/thumbnail_key/transcript_lang from Document.media
  (MediaInfo); omit/zero when media is unset.
- Keep tombstone DELETE + all existing text-doc feed behavior identical.
Tests: golden feed JSON for (a) a text doc (unchanged), (b) an audio doc with asr chunks carrying
start_ms/modality + bge-m3 embeddings, (c) an image/video doc with a CLIP-embedding chunk ->
clip_embedding block + thumbnail_key + media dims, (d) a chunk whose embedding length is neither dim
-> rejected. Update the httptest Vespa stub assertions. >=80% coverage. Read CLIP_DIM/EMBEDDING_DIM
from env (required).`,
  },
  {
    label: 'hub-media-endpoint',
    prompt: `${BASE}

## Your task: connector-hub /internal/media decrypt/encrypt endpoint. Owned paths:
services/connector-hub/** (ADD an internal media endpoint; do not disturb existing hub behavior).
Per ADR-013, the Python enrich worker cannot decrypt blobs (crypto is Go). Add an INTERNAL-ONLY HTTP
endpoint on the hub (it already has platform/blob + the tenant crypto.TenantCipher via buildDeps):
- GET  /internal/media?key=<blobkey>  with header x-asker-tenant: <tenant> -> 200 with the DECRYPTED
  object bytes (Content-Type from the BlobRef/stored metadata if available, else
  application/octet-stream). 401 if x-asker-tenant missing/invalid (tenancy.FromHeaderValue, fail
  closed). 404 if absent. The blob store's Get already enforces the tenant key prefix (fail closed) —
  rely on it; the key must be within the tenant's prefix.
- PUT  /internal/media?key=<blobkey>  x-asker-tenant + body bytes (+ optional ?content_type=) ->
  stores the bytes ENCRYPTED via the tenant cipher (blob.Put) and returns the resulting BlobRef as
  JSON. This is how the enrich worker stores thumbnails/keyframes.
- These are reachable ONLY on the internal compose/cluster network (the hub publishes no host port);
  document the trust model (ADR-009) and that M4 adds mTLS/NetworkPolicy. Wire them into the hub's
  existing HTTP mux (alongside /webhooks, /upload, /v1/sync-status) — read services/connector-hub/
  internal/hub/http.go for the pattern. The blob store + cipher are already constructed in buildDeps;
  thread them to the http layer if not already (read hub.Deps).
- Size caps + ctx timeouts; stream the GET body (don't buffer huge videos fully if avoidable, though
  blob.Get may return []byte — if so, document the M3 in-memory cap, e.g. 200MB, and note streaming
  as a follow-up).
Tests: GET returns decrypted bytes for the calling tenant; a DIFFERENT tenant's x-asker-tenant gets
404/empty (no cross-tenant read — the sacred property); missing tenant -> 401; PUT round-trips
(PUT then GET returns the same bytes, and the stored object is ciphertext != plaintext via a fake
blob store); 404 on absent key. Use the existing hub test fakes/patterns. Keep all existing hub tests
green; >=75% coverage on the new code.`,
  },
  {
    label: 'query-clip-arm',
    prompt: `${BASE}

## Your task: services/query — the CLIP text->image arm + media Hit fields + degradation. Owned
paths: services/query/**. Read ALL of services/query (vespa.go buildYQL/Search, server.go pipeline,
degrade.go, embed.go, understand.go, cache.go).
Add a second retrieval arm for text->image, merged with the existing bge-m3 text/hybrid arm:
1. Config: CLIP_URL (default http://clip:9800), CLIP_DIM (env, required like EMBEDDING_DIM),
   QUERY_CLIP_TIMEOUT (default 2s).
2. embed: add a clip-text embed call — POST CLIP_URL/embed/text {"inputs":[residualText]} ->
   first vector (len must == CLIP_DIM).
3. vespa: add a CLIP retrieval — YQL where ({targetHits:100}nearestNeighbor(clip_embedding,qclip))
   with ranking.profile 'clip' and input.query(qclip)=<vector>, scoped to the SAME tenant
   streaming.groupname. Run it ALONGSIDE the existing text/hybrid query (two Vespa calls, or document
   why one), and MERGE: union hits by doc_id (a doc matched by both keeps the higher/blended score),
   so an image with matching OCR text AND visual similarity ranks well, and a purely-visual match
   still appears. Respect limit/offset on the merged set. Keep keyword/hybrid/vector text modes
   working exactly as today for non-image queries.
4. Matched chunk -> Hit media fields: per the vespa builder's documented matched-chunk-index read
   path (read vespa/README.md after the vespa builder updates it; if not yet available when you
   build, code against the contract: chunk_starts_ms/chunk_ends_ms/chunk_modalities parallel arrays +
   a summary-feature giving the matched chunk index for the CLIP arm), populate Hit.start_ms/end_ms/
   modality from the matched chunk and Hit.thumbnail_key from the doc. For the text/ASR arm, pick the
   matched chunk element (from chunk_snippets/matched elements) and read its start_ms/modality.
5. Degradation (ADR-006): clip service error/timeout -> drop the CLIP arm, set degraded to include
   "clip-unavailable" (compose with existing degraded reasons), never fail closed; vector text arm
   degradation unchanged.
6. Cache key must incorporate that the CLIP arm ran (so a cached text-only result isn't served when
   clip is back).
Tests: extend the httptest TEI+Vespa stubs with a clip stub; assert: a text query issues a clip
/embed/text + a clip nearestNeighbor with input.query(qclip), the SAME streaming.groupname tenant
(isolation), merge/dedupe by doc_id, Hit.start_ms/modality/thumbnail_key populated from the matched
chunk, clip-down -> degraded "clip-unavailable" + text results still returned, YQL injection safety
preserved. >=80% coverage. Confirm existing query tests still pass (the M2 hybrid rank() form is
unchanged).`,
  },
]

const results = await parallel(
  tasks.map((t) => () => agent(t.prompt, { label: t.label, phase: 'Build', schema: RESULT }))
)
const out = {}
tasks.forEach((t, i) => { out[t.label] = results[i] || { summary: 'AGENT FAILED', files: [], decisions: [], issues: ['no return'] } })
return out
