# Vespa — Asker search core

Vespa is Asker's search and index engine, running in **streaming search mode**:
each tenant's documents live in their own document group, and every query scans
exactly one group. There are no per-tenant inverted indexes or ANN structures,
which makes per-tenant cost at rest ~zero and millions of small tenants
tractable (see `specs/asker-v1-personal-search-engine.md`, section 2.3).
Streaming mode never builds HNSW, so `nearestNeighbor` is always an **exact**
scan of the tenant's group — no approximate-index configuration exists or is
needed.

## Package layout

```
vespa/
├── app/                  # Vespa application package (deployed as a zip)
│   ├── services.xml      # topology: container cluster (search + document-api)
│   │                     # and content cluster id="asker", streaming mode,
│   │                     # redundancy 1, single node
│   └── schemas/
│       └── doc.sd        # document type "doc" — see "Schema" below.
│                         # Contains the @EMBEDDING_DIM@ template token;
│                         # deploy.sh substitutes it (NOT deployable verbatim).
├── deploy.sh             # stages + templates + deploys app/ (see below)
└── README.md
```

There is no `hosts.xml`: for a single-node package, host aliases resolve to
localhost (same convention as Vespa's official sample apps).

## Schema

### M1 fields

| Field           | Type                                          | Indexing               | Notes |
|-----------------|-----------------------------------------------|------------------------|-------|
| `doc_id`        | string                                        | summary \| attribute   | `sdk.DocID(connectorID, sourceNativeID)` |
| `connector_id`  | string                                        | summary \| attribute   | |
| `type`          | string                                        | summary \| attribute   | DocType name, e.g. `EMAIL` |
| `title`         | string                                        | summary \| index       | |
| `body`          | string                                        | summary \| index       | |
| `created_at`    | long                                          | summary \| attribute   | epoch seconds |
| `chunks`        | array&lt;string&gt;                           | summary \| index       | ~512-token chunks, 64 overlap; array index = embedding key |
| `embedding`     | tensor&lt;bfloat16&gt;(chunk{},x[`@EMBEDDING_DIM@`]) | attribute (no summary) | `distance-metric: angular`; one bge-m3 vector per chunk |
| `participants`  | array&lt;string&gt;                           | summary \| index       | `index` so streaming supports query-time `{substring:true}` |
| `metadata_json` | string                                        | summary                | opaque JSON object for result cards |
| `modified_at`   | long                                          | summary \| attribute   | epoch seconds |
| `version_etag`  | string                                        | summary \| attribute   | idempotent upserts / freshness checks |
| `acl`           | array&lt;string&gt;                           | attribute              | exact word match; in no summary class |

### M3 media fields (ADR-013)

| Field              | Type                                          | Indexing               | Notes |
|--------------------|-----------------------------------------------|------------------------|-------|
| `clip_embedding`   | tensor&lt;bfloat16&gt;(chunk{},x[`@CLIP_DIM@`]) | attribute (no summary) | `distance-metric: angular`; CLIP image vector per image/keyframe chunk. **Separate space** from `embedding`. Sparse: present only on chunks that carry a CLIP vector — index-writer **omits the whole field** for text-only docs |
| `chunk_starts_ms`  | array&lt;long&gt;                             | summary \| attribute   | parallel to `chunks`; chunk start offset (ms) within media, `0` for text/OCR |
| `chunk_ends_ms`    | array&lt;long&gt;                             | summary \| attribute   | parallel to `chunks`; chunk end offset (ms), `0` for text/OCR |
| `chunk_modalities` | array&lt;string&gt;                           | summary \| attribute   | parallel to `chunks`; `"text"`\|`"ocr"`\|`"asr"`\|`"caption"` |
| `media_duration_ms`| long                                          | summary \| attribute   | `MediaInfo.duration_ms`; `0` for text/images |
| `media_width`      | long                                          | summary \| attribute   | `MediaInfo.width` (proto int32 widens to long; Vespa has no 32-bit attribute) |
| `media_height`     | long                                          | summary \| attribute   | `MediaInfo.height` |
| `thumbnail_key`    | string                                        | summary \| attribute   | blob key of thumbnail / video poster; returned as `Hit.thumbnail_key` |
| `transcript_lang`  | string                                        | summary \| attribute   | detected ASR language (BCP-47, e.g. `"en"`); empty for non-ASR |

`fieldset default = title, body, chunks` — what `userQuery()` matches.

Design notes (verified against Vespa 8 docs; see `doc.sd` comments):

- **Embedding cell type**: `bfloat16` halves vector memory/disk (spec §2.7).
  Cells widen to `float` during distance computation, so the `tensor<float>`
  query tensor pairs correctly. The field is attribute-only — vectors never
  appear in summaries.
- **Distance metric**: `angular` (cosine). TEI serves bge-m3 vectors
  L2-normalized, but `angular` remains correct if normalization ever drifts,
  whereas `prenormalized-angular` would silently mis-rank. In a streaming
  exact scan the extra norm arithmetic is negligible next to document-store IO.
- **Multi-vector semantics**: for the mixed tensor (`chunk{}` mapped dimension)
  `nearestNeighbor`/`closeness` use the **closest chunk vector per document**
  (supported since Vespa 8.144.19).
- **Participants `index` vs `attribute`**: in streaming mode, `attribute` means
  exact whole-word matching of the untokenized value; `index` gives tokenized
  text matching plus query-time `{substring:true}` / `{prefix:true}` /
  `{suffix:true}` annotations — which is what `participant=` filtering needs
  against values like `"Alice Smith <alice@example.com>"`.
- **Two embedding spaces (M3, ADR-013)**: `embedding` (bge-m3, `EMBEDDING_DIM`)
  holds text/OCR/ASR/caption vectors; `clip_embedding` (CLIP ViT-B/32,
  `CLIP_DIM`) holds CLIP image vectors on image/keyframe chunks. The two spaces
  are **not comparable** — CLIP gives true text→image search (a text query
  matching an image with no shared keywords). The query service queries them as
  two arms and merges in-process (deduping by `doc_id`); they are never blended
  inside one Vespa ranking expression. `clip_embedding` mirrors `embedding`'s
  shape, `bfloat16`/attribute-only storage, and `angular` metric (the `clip`
  service L2-normalizes both encoders, so angular closeness is cosine).
- **Parallel chunk arrays (M3)**: `chunk_starts_ms` / `chunk_ends_ms` /
  `chunk_modalities` are positionally aligned with `chunks` (index *i* describes
  `chunks[i]`). They are `summary | attribute` so the query service can resolve
  the matched chunk index → its time anchor + modality for deep-linking. See
  **Matched-chunk resolution** below.

## How deployment works

The compose stack (`deploy/compose/docker-compose.yml`) starts the `vespa`
container with the config server on host port **19071** and the query/document
API on host port **8082**. The query port only comes alive after an application
has been activated, so the container healthcheck targets the config server and
the application is deployed *after* `docker compose up --wait` succeeds:

```sh
make dev-up         # compose up --wait, then runs vespa/deploy.sh
make vespa-deploy   # re-runs vespa/deploy.sh alone (e.g. after schema edits)
```

`deploy.sh` (idempotent, safe to re-run):

1. Waits for `http://localhost:19071/state/v1/health` to report `"up"`
   (timeout 180s, with progress output).
2. Stages a copy of `vespa/app` in a temp dir and substitutes every
   `@EMBEDDING_DIM@` token with `${EMBEDDING_DIM:-1024}` and every `@CLIP_DIM@`
   token with `${CLIP_DIM:-512}` (`vespa/app` itself is never modified). Both
   are validated as positive integers before substitution.
3. Zips the staged copy from inside the directory, so `services.xml` is at the
   archive root.
4. POSTs the zip with `Content-Type: application/zip` to
   `http://localhost:19071/application/v2/tenant/default/prepareandactivate`,
   printing the server's error body and exiting non-zero on failure.
5. Waits for `http://localhost:8082/state/v1/health` to report `"up"`.

Override endpoints with `VESPA_CFG_URL` / `VESPA_QUERY_URL`, the per-step
timeout with `WAIT_TIMEOUT_SECS`, the embedding dimensionality with
`EMBEDDING_DIM` (default **1024** = bge-m3; the local dev `.env` uses **384**
to match the small TEI model), and the CLIP dimensionality with `CLIP_DIM`
(default **512** = ViT-B/32). `EMBEDDING_DIM` must equal the TEI model's
output dimension and the services' `EMBEDDING_DIM` env; `CLIP_DIM` must equal
the `clip` service's model output dimension and the services' `CLIP_DIM` env.
Redeploying with a different value changes the tensor type, and already-fed
embeddings of the old dimensionality will be rejected/invalid (dev: re-feed;
there is no migration).

## Tenant-group model

One streaming group per `tenant_id`. The group is part of the document id
(`id:asker:doc:g=<tenant_id>:<doc_id>`), and every query must name the group.
`tenant_id` derives only from the verified JWT (`platform/tenancy`); it is
never taken from a request body, query string, or header.

## Feeding (index-writer contract)

Document v1 API; the path segment `group/<tenant_id>` sets `g=`. **POST** with
the full field set — document/v1 reserves PUT for *partial updates*, whose
bodies use `{"fields": {"<field>": {"assign": ...}}}` syntax; a PUT with plain
field values is not a full-document put. POST creates or fully replaces the
document, so replays are idempotent. The example below uses `EMBEDDING_DIM=4`
— i.e. field
type `tensor<bfloat16>(chunk{},x[4])` — for readability; real vectors have
`EMBEDDING_DIM` values per chunk. The mixed tensor uses the **blocks** form:
one dense array per `chunk` label, and the label is the **stringified index of
the chunk in the `chunks` array** (`"0"`, `"1"`, ...).

```sh
curl -X POST -H 'Content-Type: application/json' \
  --data '{
    "fields": {
      "doc_id": "9f86d081884c7d65...",
      "connector_id": "gmail",
      "type": "EMAIL",
      "title": "Quarterly planning",
      "body": "Full extracted body text ...",
      "chunks": [
        "First ~512-token chunk text ...",
        "Second ~512-token chunk text ..."
      ],
      "embedding": {
        "blocks": {
          "0": [0.0182, -0.0317, 0.0549, 0.0078],
          "1": [0.0021, 0.0911, -0.0114, 0.0330]
        }
      },
      "participants": [
        "Alice Smith <alice@example.com>",
        "Bob Jones <bob@example.com>"
      ],
      "metadata_json": "{\"sender\":\"alice@example.com\",\"labels\":\"INBOX\"}",
      "created_at": 1718000000,
      "modified_at": 1718000600,
      "version_etag": "etag-1",
      "acl": ["user:alice@example.com"]
    }
  }' \
  'http://localhost:8082/document/v1/asker/doc/group/<tenant_id>/9f86d081884c7d65...'
```

Notes for the index-writer:

- Values are plain JSON numbers; Vespa converts them to bfloat16 cells. The
  equally valid verbose form is
  `"cells": [{"address": {"chunk": "0", "x": "0"}, "value": 0.0182}, ...]` —
  prefer blocks.
- A document with no chunks/embedding (e.g. M0 smoke docs) may simply omit
  those fields; it still matches keyword search and gets `closeness = 0` in
  hybrid ranking.
- **Tombstones**: Document v1 DELETE to the same URL
  (`.../doc/group/<tenant_id>/<doc_id>`) removes the document from the
  tenant's group.

### Feeding media documents (M3)

A media document carries the same `chunks` + `embedding` (bge-m3 text/OCR/ASR
chunks) **plus** the M3 fields. `clip_embedding` is fed in the **blocks** form,
keyed by the same stringified chunk index as `embedding`, but is **present only
for the chunks that carry a CLIP vector** (image / keyframe chunks). Omit the
`clip_embedding` field entirely when no chunk has one (text-only docs). The
parallel arrays `chunk_starts_ms` / `chunk_ends_ms` / `chunk_modalities` have
one entry **per `chunks` element, in the same order** (`0`/`"text"` for text
chunks). The example below uses `EMBEDDING_DIM=4` and `CLIP_DIM=4` for
readability; real vectors have the configured dimensionality.

This example is a short video: chunk 0 is the ASR transcript segment
(00:01.500–00:04.200, bge-m3, no CLIP vector), chunk 1 is a scene keyframe at
00:02.000 (CLIP image vector, modality `caption`):

```sh
curl -X POST -H 'Content-Type: application/json' \
  --data '{
    "fields": {
      "doc_id": "abc123...",
      "connector_id": "gdrive",
      "type": "VIDEO",
      "title": "Standup recording",
      "body": "",
      "chunks": [
        "okay so the quarterly numbers are looking strong",
        ""
      ],
      "embedding": {
        "blocks": {
          "0": [0.0182, -0.0317, 0.0549, 0.0078]
        }
      },
      "clip_embedding": {
        "blocks": {
          "1": [0.0421, 0.0118, -0.0337, 0.0902]
        }
      },
      "chunk_starts_ms": [1500, 2000],
      "chunk_ends_ms":   [4200, 2000],
      "chunk_modalities": ["asr", "caption"],
      "media_duration_ms": 30000,
      "media_width": 1920,
      "media_height": 1080,
      "thumbnail_key": "tenant42/thumbs/abc123.jpg",
      "transcript_lang": "en",
      "participants": [],
      "metadata_json": "{\"path\":\"/Recordings/standup.mp4\"}",
      "created_at": 1718000000,
      "modified_at": 1718000600,
      "version_etag": "etag-1",
      "acl": []
    }
  }' \
  'http://localhost:8082/document/v1/asker/doc/group/<tenant_id>/abc123...'
```

Notes for the index-writer (media):

- `embedding` and `clip_embedding` are **independent sparse maps** over the same
  chunk index space. A chunk may have a bge-m3 vector, a CLIP vector, both, or
  neither — feed each block under the chunk's index in the corresponding field.
- The keyframe chunk above has empty `chunks` text and only a CLIP vector; that
  is fine — it is reachable via the CLIP arm, and its `chunk_starts_ms[1]` lets
  the query service deep-link to 00:02.000.
- `chunk_starts_ms` / `chunk_ends_ms` / `chunk_modalities` **must** be the same
  length as `chunks` (positional alignment is the contract the query service
  relies on). For a pure text/image doc, feed all-zero starts/ends and
  `"text"`/`"ocr"` modalities, or omit the arrays entirely (a missing array
  reads as empty → the query service falls back to `start_ms = 0`).
- Omit `clip_embedding` and the media fields entirely for text documents; the
  schema treats absent fields as zero/empty and `closeness(field,clip_embedding)`
  is `0`.

## Querying (query-service contract)

`streaming.groupname` scopes the scan to one tenant — **mandatory**; omitting
it is a tenant-isolation bug, not a fallback.

**Keyword mode** (`mode=keyword`, also the degradation path when TEI is slow):

```sh
curl -G 'http://localhost:8082/search/' \
  --data-urlencode 'yql=select * from sources * where userQuery()' \
  --data-urlencode 'query=quarterly planning' \
  --data-urlencode 'ranking=keyword' \
  --data-urlencode 'presentation.summary=search' \
  --data-urlencode 'streaming.groupname=<tenant_id>'
```

**Hybrid mode** (`mode=hybrid`): embed the query via TEI, then issue

```sh
curl -G 'http://localhost:8082/search/' \
  --data-urlencode 'yql=select * from sources * where ({targetHits:100}nearestNeighbor(embedding,q)) or userQuery()' \
  --data-urlencode 'query=quarterly planning' \
  --data-urlencode 'ranking=hybrid' \
  --data-urlencode 'input.query(q)=[0.0182, -0.0317, 0.0549, 0.0078]' \
  --data-urlencode 'presentation.summary=search' \
  --data-urlencode 'streaming.groupname=<tenant_id>'
```

- `targetHits` is **required** on `nearestNeighbor` (the query fails without
  it). In streaming mode the scan is exact, so `targetHits:100` is a recall
  floor for the vector arm, not an approximation knob.
- `input.query(q)` must have exactly `EMBEDDING_DIM` values (the rank profile
  declares `query(q) tensor<float>(x[EMBEDDING_DIM])`).
- Blend weights default to 0.5/0.5 and can be overridden per request with
  `input.query(keywordWeight)=...` / `input.query(vectorWeight)=...` (declared
  as `inputs` defaults in the profile — the Vespa 8 replacement for legacy
  `rank-properties query(...)` defaults).
- Filters (type, date ranges, participants) are ANDed into the YQL, e.g.
  `... where (({targetHits:100}nearestNeighbor(embedding,q)) or userQuery())
  and type contains "EMAIL" and created_at >= 1718000000 and participants
  contains ({substring:true}"alice")`.
- `userQuery()` matches the `default` fieldset (`title`, `body`, `chunks`).

**CLIP text→image arm** (M3, ADR-013): the query service encodes the query
text with the CLIP **text** encoder (`services/clip` `POST /embed/text`) and
issues a **separate** Vespa query against `clip_embedding` with `ranking=clip`:

```sh
curl -G 'http://localhost:8082/search/' \
  --data-urlencode 'yql=select * from sources * where {targetHits:100}nearestNeighbor(clip_embedding,qclip)' \
  --data-urlencode 'ranking=clip' \
  --data-urlencode 'input.query(qclip)=[0.0421, 0.0118, -0.0337, 0.0902]' \
  --data-urlencode 'presentation.summary=search' \
  --data-urlencode 'streaming.groupname=<tenant_id>'
```

- `input.query(qclip)` must have exactly `CLIP_DIM` values (the `clip` profile
  declares `query(qclip) tensor<float>(x[CLIP_DIM])`).
- `targetHits` is required, same as the bge-m3 arm; the streaming scan is exact.
- This is a **distinct query** from the text/hybrid arm (different space). The
  query service runs both, merges the hit lists, and **dedupes by `doc_id`**,
  blending scores (a doc may match in both arms). Filters (type/date/etc.) are
  ANDed into this YQL exactly as for the hybrid arm.
- **Degradation (ADR-006)**: if the `clip` service is down, the query service
  drops this arm (logs once) and answers from the text/OCR/ASR arm alone —
  never fail closed.

## Ranking profiles

| Profile   | Expression | Use |
|-----------|------------|-----|
| `default` | `nativeRank(title, body)` | M0 compatibility; used when `ranking=` is absent |
| `keyword` | `nativeRank(title, body, chunks)` | `mode=keyword` |
| `hybrid`  | `query(keywordWeight)*nativeRank(title,body,chunks) + query(vectorWeight)*closeness(field,embedding)` | `mode=hybrid` |
| `clip`    | `closeness(field, clip_embedding)` + `summary-features { closest(clip_embedding) }` | CLIP text→image arm (M3) |

`nativeRank` needs no corpus statistics, so it is meaningful in streaming
mode. `bm25` is intentionally not used: streaming collects no term/field-length
statistics, so bm25 requires significance-model and `averageFieldLength`
configuration to produce useful scores (revisit at M5).
`closeness(field, embedding)` is `1/(1+distance)` in `[0,1]`; for the mixed
chunk tensor the closest chunk vector per document wins. Keyword-only matches
score `closeness = 0` — they rank on the keyword term alone, never fail.

The `clip` profile ranks purely by `closeness(field, clip_embedding)` (same
`1/(1+distance)`, closest chunk vector per document) over the `clip_embedding`
field — verified against the Vespa 8 docs that `nearestNeighbor` and
`closeness(field, name)` behave identically for a second mixed tensor field as
for `embedding`: streaming mode never builds HNSW, so the scan is exact and
`targetHits` is required for both, and the closest sub-vector per document is
used automatically (see the [nearest-neighbor-search](https://docs.vespa.ai/en/querying/nearest-neighbor-search.html)
and [streaming-search](https://docs.vespa.ai/en/streaming-search.html) docs:
"HNSW indexes are not supported in streaming search … always exact",
"targetHits is a required parameter"). It declares
`query(qclip) tensor<float>(x[@CLIP_DIM@])` (the query tensor omits the mapped
`chunk` dimension; bfloat16 document cells widen to float).

## Snippets & highlighting

The `search` document-summary returns the lean result-card fields (`doc_id`,
`connector_id`, `type`, `title`, `created_at`, `modified_at`, `version_etag`,
`participants`, `metadata_json`, plus the M3 media fields `chunk_starts_ms`,
`chunk_ends_ms`, `chunk_modalities`, `media_duration_ms`, `media_width`,
`media_height`, `thumbnail_key`, `transcript_lang`) plus two **dynamic
summary** fields:

- **`snippet`** (source `body`) — extracted fragments around matching query
  terms, terms wrapped in `<hi>...</hi>` (Vespa's default highlight element —
  matching the gateway's REST snippet contract).
- **`chunk_snippets`** (source `chunks`) — same, one fragment per chunk
  element. Observed behavior: elements containing query terms come back as
  highlighted fragments; elements with no match come back as their full,
  unhighlighted text — the query service should prefer elements containing
  `<hi>` when assembling the REST `snippet`.

The dynamic fields carry their own names because Vespa rejects reusing a field
name with a different transform across summary classes (`body`/`chunks` are
`full` in the default class). Consequently the `search` class does **not**
contain raw `body`/`chunks` — use the default summary class if full text is
ever needed. Dynamic summaries are supported for string and
array&lt;string&gt; index fields and work in streaming mode (verified by
deploy + query against `vespaengine/vespa:8`). Request the class with
`presentation.summary=search`; the default summary class (full `body`, no
fragments) remains what M0 smoke queries get.

The query service should still keep a trivial fallback (truncate `body` or the
best chunk) for hits whose dynamic fragments come back empty (e.g. vector-only
matches where no query term occurs literally in the text).

## Matched-chunk resolution (M3 — query-service contract)

To fill `Hit.start_ms` / `Hit.end_ms` / `Hit.modality`, the query service must
learn **which chunk element matched** for each hit, then index the parallel
arrays. The matched chunk index is resolved per arm:

### CLIP arm — `closest(clip_embedding)` summary feature

The `clip` rank profile declares `summary-features { closest(clip_embedding) }`.
The Vespa `closest(name)` rank feature returns a mapped tensor with **exactly
one cell, value `1.0`, whose label is the chunk index of the document vector
nearest to the query vector** (confirmed against
[reference/rank-features](https://docs.vespa.ai/en/reference/rank-features.html):
*"a tensor with one or more mapped dimensions and one point with a value of 1.0,
where the label of that point indicates which document vector was closest to the
query vector in the nearest neighbor search"*). Summary-features are global to
the active rank profile and returned for **every** hit regardless of summary
class.

It appears in the result JSON under `fields.summaryfeatures`, keyed by the
exact feature string `"closest(clip_embedding)"`, as a tensor value. With the
default tensor rendering for a single mapped dimension:

```jsonc
{
  "root": { "children": [ {
    "fields": {
      "doc_id": "abc123...",
      "chunk_starts_ms": [1500, 2000],
      "chunk_ends_ms":   [4200, 2000],
      "chunk_modalities": ["asr", "caption"],
      "thumbnail_key": "tenant42/thumbs/abc123.jpg",
      "summaryfeatures": {
        "closest(clip_embedding)": {
          "type": "tensor(chunk{})",
          "cells": { "1": 1.0 }
        }
      }
    }
  } ] }
}
```

**Read path** (be defensive about tensor rendering — Vespa may emit either the
short `cells`-as-map form `{"1":1.0}` or the verbose
`"cells":[{"address":{"chunk":"1"},"value":1.0}]` form depending on
version/`presentation.format.tensors`; the matched index is the single cell's
`chunk` label either way):

1. `idx = label of the single cell in fields.summaryfeatures["closest(clip_embedding)"]`
   (parse `"1"` → `1`).
2. `Hit.start_ms = fields.chunk_starts_ms[idx]`,
   `Hit.end_ms = fields.chunk_ends_ms[idx]`,
   `Hit.modality = fields.chunk_modalities[idx]`.
3. `Hit.thumbnail_key = fields.thumbnail_key`.

If the feature or the arrays are absent (text-only docs reached via the bge-m3
arm, or a doc with no `clip_embedding`), default to `idx = 0` /
`start_ms = end_ms = 0` / `modality = "text"`.

### Text / ASR arm — `chunk_snippets`

The hybrid/keyword arm does not use `closest()`. Instead, the matched element(s)
are marked in the dynamic `chunk_snippets` array (parallel to `chunks`):
elements containing query terms come back wrapped in `<hi>…</hi>`; unmatched
elements come back as plain text. The query service takes the **index of the
first `chunk_snippets` element containing `<hi>`** as the matched chunk index,
then reads `chunk_starts_ms[idx]` / `chunk_ends_ms[idx]` / `chunk_modalities[idx]`
the same way (this is how an ASR/transcript hit deep-links to its timestamp —
the M3 exit criterion). If no element is highlighted (e.g. a pure vector match
in the text space), default to `idx = 0`.

### Merge

When a `doc_id` appears in both arms after dedupe, prefer the arm with the
higher (blended) score for the `start_ms`/`modality` of the surfaced hit; both
arms agree on `thumbnail_key` (it is doc-level).
