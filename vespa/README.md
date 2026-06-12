# Vespa — Asker search core

Vespa is Asker's search and index engine, running in **streaming search mode**:
each tenant's documents live in their own document group, and every query scans
exactly one group. There are no per-tenant inverted indexes or ANN structures,
which makes per-tenant cost at rest ~zero and millions of small tenants
tractable (see `specs/asker-v1-personal-search-engine.md`, section 2.3).

## Package layout

```
vespa/
├── app/                  # Vespa application package (deployed as a zip)
│   ├── services.xml      # topology: container cluster (search + document-api)
│   │                     # and content cluster id="asker", streaming mode,
│   │                     # redundancy 1, single node
│   └── schemas/
│       └── doc.sd        # document type "doc": doc_id, connector_id, type,
│                         # title, body (string), created_at (long);
│                         # fieldset default = title, body;
│                         # default rank-profile = nativeRank (streaming-safe)
├── deploy.sh             # deploys app/ to the config server (see below)
└── README.md
```

There is no `hosts.xml`: for a single-node package, host aliases resolve to
localhost (same convention as Vespa's official sample apps).

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
2. Zips `vespa/app` from inside the directory, so `services.xml` is at the
   archive root.
3. POSTs the zip with `Content-Type: application/zip` to
   `http://localhost:19071/application/v2/tenant/default/prepareandactivate`,
   printing the server's error body and exiting non-zero on failure.
4. Waits for `http://localhost:8082/state/v1/health` to report `"up"`.

Override endpoints with `VESPA_CFG_URL` / `VESPA_QUERY_URL`, and the per-step
timeout with `WAIT_TIMEOUT_SECS`.

## Tenant-group model

One streaming group per `tenant_id`. The group is part of the document id
(`id:asker:doc:g=<tenant_id>:<doc_id>`), and every query must name the group.
`tenant_id` derives only from the verified JWT (`platform/tenancy`); it is
never taken from a request body, query string, or header.

**Feed** (Document v1 API; the path segment `group/<tenant_id>` sets `g=`):

```sh
curl -X POST -H 'Content-Type: application/json' \
  --data '{
    "fields": {
      "doc_id": "doc-1",
      "connector_id": "upload",
      "type": "FILE",
      "title": "hello title",
      "body": "hello body text",
      "created_at": 1718000000
    }
  }' \
  'http://localhost:8082/document/v1/asker/doc/group/<tenant_id>/doc-1'
```

**Query** (`streaming.groupname` scopes the scan to one tenant — mandatory;
omitting it is a tenant-isolation bug, not a fallback):

```sh
curl -G 'http://localhost:8082/search/' \
  --data-urlencode 'yql=select * from sources * where userQuery()' \
  --data-urlencode 'query=hello' \
  --data-urlencode 'streaming.groupname=<tenant_id>'
```

`userQuery()` matches against the `default` fieldset (`title`, `body`).

## Ranking in M0

The `default` rank-profile uses `nativeRank(title, body)`, which is safe in
streaming mode. `bm25` is intentionally not used in M0: streaming mode has no
index-derived corpus statistics, so bm25 requires extra significance /
average-field-length configuration to produce meaningful scores.

## What changes in M1

- **Chunks**: documents gain a chunk-level representation (~512-token chunks,
  64-token overlap) per the canonical Document model — either a `chunk`
  document type or array/tensor fields on `doc`.
- **Embeddings**: a 1024-dim dense tensor field for `BAAI/bge-m3` vectors
  (bfloat16/int8 cell types, paged attributes at scale), fed by the enrich
  workers via TEI. Streaming mode scans raw vectors exactly — no HNSW needed.
- **Hybrid ranking**: bm25 (with streaming significance configuration) +
  `closeness()` over the embedding field, fused with reciprocal rank fusion or
  a learned linear blend; optional cross-encoder rerank of the top-50 in the
  query service.
- **Tombstones**: deletes propagated end-to-end (Document v1 DELETE per group)
  within the 30-minute freshness SLA.
