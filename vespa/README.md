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

## Schema (M1)

| Field           | Type                                          | Indexing               | Notes |
|-----------------|-----------------------------------------------|------------------------|-------|
| `doc_id`        | string                                        | summary \| attribute   | `sdk.DocID(connectorID, sourceNativeID)` |
| `connector_id`  | string                                        | summary \| attribute   | |
| `type`          | string                                        | summary \| attribute   | DocType name, e.g. `EMAIL` |
| `title`         | string                                        | summary \| index       | |
| `body`          | string                                        | summary \| index       | |
| `created_at`    | long                                          | summary \| attribute   | epoch seconds |
| `chunks`        | array&lt;string&gt;                           | summary \| index       | ~512-token chunks, 64 overlap; array index = embedding key |
| `embedding`     | tensor&lt;bfloat16&gt;(chunk{},x[`@EMBEDDING_DIM@`]) | attribute (no summary) | `distance-metric: angular`; one vector per chunk |
| `participants`  | array&lt;string&gt;                           | summary \| index       | `index` so streaming supports query-time `{substring:true}` |
| `metadata_json` | string                                        | summary                | opaque JSON object for result cards |
| `modified_at`   | long                                          | summary \| attribute   | epoch seconds |
| `version_etag`  | string                                        | summary \| attribute   | idempotent upserts / freshness checks |
| `acl`           | array&lt;string&gt;                           | attribute              | exact word match; in no summary class |

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
   `@EMBEDDING_DIM@` token with `${EMBEDDING_DIM:-1024}` (`vespa/app` itself is
   never modified).
3. Zips the staged copy from inside the directory, so `services.xml` is at the
   archive root.
4. POSTs the zip with `Content-Type: application/zip` to
   `http://localhost:19071/application/v2/tenant/default/prepareandactivate`,
   printing the server's error body and exiting non-zero on failure.
5. Waits for `http://localhost:8082/state/v1/health` to report `"up"`.

Override endpoints with `VESPA_CFG_URL` / `VESPA_QUERY_URL`, the per-step
timeout with `WAIT_TIMEOUT_SECS`, and the embedding dimensionality with
`EMBEDDING_DIM` (default **1024** = bge-m3; the local dev `.env` uses **384**
to match the small TEI model). `EMBEDDING_DIM` must equal the TEI model's
output dimension and the services' `EMBEDDING_DIM` env — redeploying with a
different value changes the tensor type, and already-fed embeddings of the old
dimensionality will be rejected/invalid (dev: re-feed; there is no migration).

## Tenant-group model

One streaming group per `tenant_id`. The group is part of the document id
(`id:asker:doc:g=<tenant_id>:<doc_id>`), and every query must name the group.
`tenant_id` derives only from the verified JWT (`platform/tenancy`); it is
never taken from a request body, query string, or header.

## Feeding (index-writer contract)

Document v1 API; the path segment `group/<tenant_id>` sets `g=`. PUT (or POST)
with the full field set. The example below uses `EMBEDDING_DIM=4` — i.e. field
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

## Ranking profiles

| Profile   | Expression | Use |
|-----------|------------|-----|
| `default` | `nativeRank(title, body)` | M0 compatibility; used when `ranking=` is absent |
| `keyword` | `nativeRank(title, body, chunks)` | `mode=keyword` |
| `hybrid`  | `query(keywordWeight)*nativeRank(title,body,chunks) + query(vectorWeight)*closeness(field,embedding)` | `mode=hybrid` |

`nativeRank` needs no corpus statistics, so it is meaningful in streaming
mode. `bm25` is intentionally not used: streaming collects no term/field-length
statistics, so bm25 requires significance-model and `averageFieldLength`
configuration to produce useful scores (revisit at M5).
`closeness(field, embedding)` is `1/(1+distance)` in `[0,1]`; for the mixed
chunk tensor the closest chunk vector per document wins. Keyword-only matches
score `closeness = 0` — they rank on the keyword term alone, never fail.

## Snippets & highlighting

The `search` document-summary returns the lean result-card fields (`doc_id`,
`connector_id`, `type`, `title`, `created_at`, `modified_at`, `version_etag`,
`participants`, `metadata_json`) plus two **dynamic summary** fields:

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
