export const meta = {
  name: 'asker-m3-review',
  description: 'Adversarial review of M3 media pipeline: dual-space query, deep-link mechanics, crypto boundary, media enrich, isolation',
  phases: [
    { title: 'Review', detail: '5 parallel dimension reviewers' },
    { title: 'Verify', detail: 'adversarial verification of each finding' },
  ],
}

const FINDINGS = {
  type: 'object',
  required: ['findings'],
  properties: {
    findings: {
      type: 'array',
      items: {
        type: 'object',
        required: ['title', 'file', 'severity', 'detail'],
        properties: {
          title: { type: 'string' },
          file: { type: 'string' },
          severity: { type: 'string', enum: ['blocker', 'major', 'minor'] },
          detail: { type: 'string' },
          fix: { type: 'string' },
        },
      },
    },
  },
}

const VERDICT = {
  type: 'object',
  required: ['isReal', 'reasoning'],
  properties: {
    isReal: { type: 'boolean' },
    reasoning: { type: 'string' },
    severityAdjustment: { type: 'string' },
  },
}

const PRE = [
  'You are reviewing milestone M3 (media pipeline) of Asker, a multi-tenant personal search engine at',
  '/Users/dio/works/asker. M3 added: two embedding spaces (bge-m3 text via TEI, CLIP image/text via a',
  'new services/clip), the media enrich path in services/enrich (Tesseract OCR, faster-whisper ASR,',
  'ffmpeg keyframes, CLIP via the clip service), timestamp-anchored chunks (Chunk.start_ms/end_ms/',
  'modality), Document.media (MediaInfo/Keyframe), a Vespa clip_embedding field + parallel chunk',
  'arrays + a clip rank profile, the index-writer routing vectors to embedding vs clip_embedding by',
  'length, ingest media pass-through routing, the connector-hub /internal/media decrypt/encrypt',
  'endpoint, the query service CLIP text->image arm + media Hit fields, the gateway /v1/media proxy +',
  'search media fields, the upload connector DocType classification, the web media UI, and the',
  'tools/e2e/m3-media.sh exit suite. Read docs/adr/ADR-013-media-pipeline.md (THE design) and the',
  'relevant code. The exit criterion is "search a spoken phrase -> get the video at the right',
  'timestamp" (ASR/bge-m3 arm) plus CLIP text->image. Do NOT flag intentionally-deferred items:',
  'recurrence expansion, a real media player in the UI, mTLS on the internal media endpoint (M4),',
  'SSRF on connector URLs (M4/M6), per-document ACL enforcement (ADR-012). The dev VM is memory-tight:',
  'do NOT run dev-up or start/stop compose; you MAY run go test / pytest on individual packages, grep,',
  'read files. Report only findings with concrete file+line evidence.',
].join(' ')

const DIMS = [
  {
    key: 'deeplink-mechanics',
    body: 'Dimension: TIMESTAMP DEEP-LINK + MATCHED-CHUNK CORRECTNESS (the exit criterion hinges on this). '
      + 'Trace the full path: enrich produces ASR chunks with start_ms/end_ms (segment seconds*1000) and '
      + 'modality "asr"; index-writer writes parallel chunk_starts_ms/ends_ms/modalities arrays + the '
      + 'embedding/clip_embedding blocks; Vespa schema (doc.sd) declares them + the clip rank profile '
      + 'with closest(clip_embedding) summary-feature; the query service resolves WHICH chunk matched '
      + '(text/ASR arm: first <hi> element in chunk_snippets; CLIP arm: closest() summary-feature label) '
      + 'and reads chunk_starts_ms[idx] into Hit.start_ms. Hunt for OFF-BY-ONE or index-misalignment bugs: '
      + 'does the matched-chunk index actually line up with the parallel arrays (same order as chunks)? '
      + 'Are the arrays guaranteed same-length as chunks? Could a text-doc (no media) get a bogus '
      + 'start_ms? Does seconds->ms rounding lose the segment? Read services/query/*.go (vespa response '
      + 'parsing), services/index-writer, services/enrich/enrich/media.py, vespa/app/schemas/doc.sd. The '
      + 'e2e asserts start_ms>0 for the video — verify that is actually achievable given how a single '
      + 'phrase clip is segmented. Run the query + enrich + index-writer unit tests.',
  },
  {
    key: 'dual-space-query',
    body: 'Dimension: DUAL-SPACE QUERY CORRECTNESS + DEGRADATION. services/query runs a bge-m3 text/hybrid '
      + 'arm AND a CLIP text->image arm, merged/deduped by doc_id. Hunt for: vectors sent to the wrong '
      + 'space (a CLIP vector to the embedding field or vice versa), CLIP_DIM/EMBEDDING_DIM confusion, '
      + 'the two arms scoring on incomparable scales corrupting the merged ranking, dedupe keeping the '
      + 'wrong hit, the CLIP arm NOT being tenant-scoped (streaming.groupname must be the ctx tenant on '
      + 'BOTH arms — a missing groupname on the CLIP query is cross-tenant leakage), degradation (clip '
      + 'down -> drop CLIP arm, never fail closed; TEI down -> keyword), cache key not distinguishing '
      + 'clip-ran vs not. Also services/clip: are text and image embeddings actually from the same model/'
      + 'space and L2-normalized (else cosine is wrong)? Run go test ./services/query/ and the clip '
      + 'pytest. Verify the YQL for the CLIP arm carries streaming.groupname = ctx tenant.',
  },
  {
    key: 'crypto-boundary',
    body: 'Dimension: THE MEDIA CRYPTO BOUNDARY + ISOLATION. The connector-hub /internal/media endpoint '
      + 'returns DECRYPTED blob bytes (GET) and encrypts on PUT, scoped by x-asker-tenant. This is the '
      + 'most security-sensitive M3 surface. Hunt for: a tenant reading ANOTHER tenant\'s media (does GET '
      + 'enforce the tenant key-prefix via blob.Get fail-closed? can a crafted key escape the prefix, '
      + 'e.g. "../" or an absolute key, and read across tenants?), missing/invalid x-asker-tenant not '
      + 'failing closed, the endpoint being reachable from outside the internal network (it must publish '
      + 'no host port), the gateway /v1/media proxy forwarding the JWT tenant (not a client header) and '
      + 'not letting a user read another tenant\'s thumbnail by guessing a key. Also: does PUT let a '
      + 'tenant overwrite another tenant\'s blob? Read services/connector-hub/internal/hub/http.go (media '
      + 'endpoint), platform/blob, services/gateway/media.go. Run the hub + gateway + blob tests. This is '
      + 'the sacred isolation property extended to media — be adversarial.',
  },
  {
    key: 'media-enrich',
    body: 'Dimension: MEDIA ENRICH CORRECTNESS + ROBUSTNESS. services/enrich/enrich/media.py + '
      + 'media_clients.py + kafka_worker.py routing. Hunt for: a media doc that errors crashing the '
      + 'worker instead of retry/deadletter; unbounded memory (loading a huge video fully; no size cap on '
      + 'the media fetch); ffmpeg/tesseract subprocess injection (are file paths/args safe? temp file '
      + 'cleanup?); the chunk embedding-length contract (text/ocr/asr -> EMBEDDING_DIM, caption -> '
      + 'CLIP_DIM) actually honored so the index-writer routes correctly — a mismatch dead-letters the '
      + 'doc; ASR segment->chunk windowing preserving start_ms/end_ms correctly; zero-chunk media still '
      + 'producing; tenant not leaking via the hub media fetch (x-asker-tenant = doc.tenant_id); failures '
      + 'in CLIP/whisper/OCR handled (not silently dropping the doc). Run the enrich pytest (Python 3.13 '
      + 'venv: python3.13 -m venv, pip install -r requirements.txt -r requirements-dev.txt, '
      + 'PYTHONPATH=platform/proto/gen/python pytest). Flag real bugs/leaks/crashes.',
  },
  {
    key: 'pipeline-integration',
    body: 'Dimension: END-TO-END MEDIA INTEGRATION + CONTRACTS. Verify the media contract holds across '
      + 'stage boundaries: upload classifies DocType by content_type -> ingest passes media through '
      + 'UNchunked -> enrich chunks it -> index-writer feeds Vespa -> query returns media Hits -> gateway '
      + 'REST shape -> web renders. Hunt for contract drift: field-name/shape mismatches between query '
      + 'Hit proto, gateway searchHitJSON, and web Hit type (start_ms/end_ms/modality/thumbnail_key); the '
      + 'index-writer vector-length routing vs what enrich actually emits; the Vespa feed JSON the '
      + 'index-writer builds vs what doc.sd accepts (clip_embedding blocks, parallel arrays, media '
      + 'fields); the gateway /v1/media key vs the thumbnail_key the query returns vs what enrich stored '
      + 'via the hub PUT; the e2e (tools/e2e/m3-media.sh) asserting things the pipeline actually produces '
      + '(is it vacuous or real? does it assert the right doc, modality, a plausible start_ms, the '
      + 'thumbnail fetch, AND tenant isolation?). Run go build + the touched services\' tests. Flag any '
      + 'broken contract that would make media search return wrong/empty/mis-shaped results.',
  },
]

function verifyPrompt(f) {
  return PRE + '\n\nA reviewer claims this issue. Try to REFUTE it: read the actual files, run '
    + 'read-only checks (go test/pytest on the package, read the file), decide if it is a real, '
    + 'actionable M3-scope bug (not style, not deferred, not already handled). If you cannot reproduce '
    + 'the evidence or it misreads the code, it is refuted. Default isReal=false when uncertain.'
    + '\n\nCLAIM [' + f.severity + '] ' + f.title + '\nFile: ' + f.file + '\nDetail: ' + f.detail
    + '\nFix: ' + (f.fix || '(none)')
}

function reviewStage(d) {
  return agent(PRE + '\n\n' + d.body, { label: 'review:' + d.key, phase: 'Review', schema: FINDINGS })
}

function verifyStage(review, d) {
  const fs = (review && review.findings) || []
  if (!fs.length) return []
  const thunks = fs.map(function (f) {
    return function () {
      return agent(verifyPrompt(f), { label: 'verify:' + d.key, phase: 'Verify', schema: VERDICT })
        .then(function (v) { return Object.assign({}, f, { dimension: d.key, verdict: v }) })
    }
  })
  return parallel(thunks)
}

phase('Review')
const results = await pipeline(DIMS, reviewStage, verifyStage)

const all = results.filter(Boolean).flat().filter(Boolean)
const confirmed = all.filter(function (f) { return f.verdict && f.verdict.isReal })
const refuted = all.filter(function (f) { return !f.verdict || !f.verdict.isReal })
log(confirmed.length + ' confirmed, ' + refuted.length + ' refuted of ' + all.length)

return {
  confirmed: confirmed.map(function (f) {
    return { severity: (f.verdict.severityAdjustment || f.severity), dimension: f.dimension, title: f.title, file: f.file, detail: f.detail, fix: f.fix, verifierNote: f.verdict.reasoning }
  }),
  refuted: refuted.map(function (f) {
    return { severity: f.severity, dimension: f.dimension, title: f.title, file: f.file, why: f.verdict ? f.verdict.reasoning : 'verifier failed' }
  }),
}
