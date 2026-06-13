export const meta = {
  name: 'asker-m3-review-fixes',
  description: 'Fix M3 review findings: thumbnail serving + blob traversal, enrich robustness, modality contract',
  phases: [{ title: 'Fix', detail: '4 parallel fixers in disjoint paths' }],
}

const RESULT = {
  type: 'object',
  required: ['files', 'summary', 'issues'],
  properties: {
    files: { type: 'array', items: { type: 'string' } },
    summary: { type: 'string' },
    issues: { type: 'array', items: { type: 'string' } },
  },
}

const BASE = `
Repo /Users/dio/works/asker (Go module github.com/asker/asker, Go 1.26; Python 3.12 enrich). An
adversarial review of M3 (media pipeline, ADR-013) found bugs; fix the ones in YOUR area only. Rules:
touch ONLY your listed paths; NO go.mod/go.sum/Makefile/other-area changes; gofmt -w; go build +
go test -race on your Go dirs must pass; ./bin/golangci-lint run on them = 0 issues (US-locale
"canceled"); Python: ruff clean + pytest (use a python3.13 venv: python3.13 -m venv, pip install -r
requirements.txt -r requirements-dev.txt, PYTHONPATH=/Users/dio/works/asker/platform/proto/gen/python
pytest). For EVERY fix add/strengthen a test that FAILS without it. Never log/leak credentials.
Report every file changed.`

phase('Fix')

const fixers = [
  {
    label: 'thumbnail-blob',
    prompt: `${BASE}
Area: platform/blob/** + services/connector-hub/** (coupled — the hub uses a new blob method).
Two findings:
1. (HIGH — thumbnail serving broken end-to-end) The gateway GET /v1/media proxies to the hub GET
   /internal/media?key=<key>, which reconstructs a BlobRef from ONLY the key and calls
   platform/blob Store.Get — but Store.Get hard-requires ref.Sha256 (returns "blob ref carries no
   sha256"), which the gateway/UI never have (they only have thumbnail_key). So every thumbnail/
   keyframe fetch fails. FIX: add platform/blob Store.GetByKey(ctx, tc, key string) (data []byte,
   contentType string, err error) that: validates tc has a tenant; rejects an empty/invalid key
   (reuse the existing validKey on the post-prefix remainder — see finding 2); enforces the
   "<tenant>/" key prefix (fail closed, ErrTenantMismatch); fetches + decrypts (the per-tenant AAD
   in Decrypt already authenticates the blob, so NO sha256 verification is needed for a read-by-key)
   and returns the object's stored Content-Type if the objectAPI exposes it (extend the objectAPI
   interface minimally if needed to return content-type from getObject; default
   "application/octet-stream"). Then change the connector-hub handleMediaGet to call GetByKey and set
   the response Content-Type from it (instead of building a sha256-less BlobRef + Get). Keep PUT
   (encrypt) as is.
2. (SECURITY — defense in depth) Store.Get enforces the tenant prefix with HasPrefix but does NOT
   reject "../" path traversal in the key: a key like "<tenant>/../<other>/x" passes HasPrefix yet
   escapes the prefix (today only the crypto AAD catches it at decrypt time). FIX: in Store.Get (and
   GetByKey) reject keys whose segments contain "."/".."/empty via the existing validKey helper
   (apply it to the full key, OR to the remainder after the tenant prefix — be precise so legitimate
   keys like "<tenant>/upload/<hash>" and "<tenant>/thumb/<id>.jpg" still pass). Put validation
   BEFORE any I/O.
Tests: GetByKey round-trips (Put then GetByKey returns the bytes + content-type, no sha256 needed);
GetByKey/Get reject a "../"-traversal key (ErrTenantMismatch or an invalid-key error) — proving a
crafted key cannot escape the tenant prefix; a different tenant cannot GetByKey another's key;
content-type is returned. Hub: handleMediaGet returns the decrypted thumbnail bytes + Content-Type
for the calling tenant (a key-only request now SUCCEEDS), and still 404s an absent key, 401s a
missing/invalid x-asker-tenant, and a cross-tenant key is denied. Keep ALL existing blob + hub tests
green (the existing Get-with-sha256 path stays valid).`,
  },
  {
    label: 'enrich-robustness',
    prompt: `${BASE}
Area: services/enrich/** only. Two findings (both major):
1. A silent / no-audio-track VIDEO is dead-lettered entirely (media.py ~line 209: transcription is
   attempted unconditionally; faster-whisper on a video with no audio stream errors -> the whole doc
   dead-letters, LOSING its keyframe/CLIP indexing too). FIX: detect audio-stream presence (probe()
   in media_clients.py already iterates meta['streams'] ~line 411 — add a has_audio bool to VideoInfo
   set when any stream has codec_type=="audio") and SKIP transcription when absent, still producing
   the keyframe/CLIP chunks + media metadata. A no-audio video must index its visual content, not
   dead-letter.
2. ffmpeg/ffprobe subprocesses run with NO timeout (media_clients.py ~line 546 _run): a malformed or
   adversarial video can hang the worker indefinitely. FIX: pass an explicit timeout= to
   subprocess.run in _run (a per-call bound, e.g. from config with a sane default like 120s), catch
   subprocess.TimeoutExpired alongside CalledProcessError and raise the worker's MediaError so the
   record flows to retry/dead-letter instead of hanging.
Tests: a VideoInfo with has_audio=False -> the handler produces keyframe/caption chunks + media info
and emits (no transcription attempted, NOT dead-lettered) — assert via the injected fakes; a
subprocess that exceeds the timeout -> _run raises MediaError (simulate by injecting a fake runner or
a command that sleeps, with a tiny timeout). Keep all existing enrich tests green; ruff clean.`,
  },
  {
    label: 'query-text-modality',
    prompt: `${BASE}
Area: services/query/** only. Finding (minor, contract correctness): populateMediaFields
(vespa.go ~404-406) unconditionally sets hit.Modality = ChunkModalities[idx] and the offset, so a
plain EMAIL/FILE hit (whose chunks the index-writer labels modality "text") comes back with
modality="text" and possibly a start_ms — contradicting the Hit contract (query.proto: media fields
are "set for IMAGE/AUDIO/VIDEO hits ... zero for the whole-document/non-media case"). FIX: in
populateMediaFields, when the resolved matched-chunk modality is "text" (or empty), treat it as the
whole-document/non-media case — leave Modality, StartMs, EndMs at their zero values (still set
ThumbnailKey if the doc has one, since an image with only OCR text could legitimately have a
thumbnail — but a pure text doc has none anyway). Keep media hits (asr/caption/ocr) fully populated.
Tests: fix TestPopulateMediaFieldsTextDocStaysZero to use a REALISTIC fixture (chunk_modalities=
["text","text"] as the index-writer actually emits, with a highlighted chunk) and assert the EMAIL
hit comes back with modality="" and start_ms=0; add/keep a media (asr) case asserting modality="asr"
+ start_ms>0 is still populated. Keep all existing query tests green.`,
  },
  {
    label: 'web-modality',
    prompt: `${BASE}
Area: web/** only. Finding (minor): web/src/search/media.ts modalityLabel() switches on
"transcript"/"visual" — values the pipeline NEVER emits — and is missing "asr"/"caption" (the values
enrich actually produces: "text"|"ocr"|"asr"|"caption", per query.proto and services/enrich). So a
real ASR video hit (modality "asr", the headline M3 feature) renders the raw token "asr" instead of a
friendly badge. The mock (src/mocks/searchMock.ts) and media.test.ts bake in the wrong vocabulary
("transcript"/"visual"). FIX: align modalityLabel to the real values — "asr" -> "Transcript",
"ocr" -> "OCR", "caption" -> "Image", "text"/unknown -> default (no badge / generic). Fix
searchMock.ts to use a real value ("asr" for the video mock) and update media.test.ts to assert the
real vocabulary maps to the right labels (and that "asr" no longer falls through to the raw token).
Keep npm run build + npm test + npm run lint all green; match existing style; no new deps.`,
  },
]

const results = await parallel(
  fixers.map((f) => () => agent(f.prompt, { label: f.label, phase: 'Fix', schema: RESULT }))
)
const out = {}
fixers.forEach((f, i) => { out[f.label] = results[i] || { summary: 'AGENT FAILED', files: [], issues: ['no return'] } })
return out
