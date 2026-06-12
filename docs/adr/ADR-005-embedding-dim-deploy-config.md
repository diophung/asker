# ADR-005: Embedding dimension is deploy-time configuration

## Status

Accepted (M1).

## Context

The decided text-embedding model is `BAAI/bge-m3` (1024-dim). But the model actually serving
TEI varies by environment: CI overrides it to a small model for fast startup, and the primary
dev machine cannot fit bge-m3 in its Docker VM and pins `BAAI/bge-small-en-v1.5` (384-dim)
locally (both per ADR-003 §§2, 6). The embedding dimension is baked into three places that
must agree exactly: the Vespa schema's tensor type, the enrich worker's output, and the query
service's query-vector embedding. Vespa rejects a tensor whose dimension differs from the
deployed schema, so a hardcoded 1024 would make the dev/CI stacks unusable the moment vectors
land in the index (known issue flagged at M0 close).

## Decision

The embedding dimension is **deploy-time configuration, not code**:

- A single `EMBEDDING_DIM` environment variable, **default 1024** (bge-m3). It must always
  match the model TEI is serving: 384 wherever `TEI_MODEL_ID=BAAI/bge-small-en-v1.5` (this
  dev machine, CI).
- `vespa/deploy.sh` templates the dimension into the schema's embedding tensor type at deploy
  time, so the deployed application package always reflects the environment's setting.
- The enrich worker and the query service read the same variable; both treat a
  dimension mismatch from TEI as a hard startup/feed error rather than feeding wrong-size
  vectors.

There is no per-tenant or per-document dimension; one deployment has exactly one embedding
space.

## Consequences

- Dev, CI, and prod all run the same code; only the env var and the TEI model differ. The
  M0 known issue (384 vs 1024) is resolved structurally.
- **Switching embedding models is an expensive, explicit migration**: re-deploy the Vespa
  schema with the new dimension and re-embed and re-feed every chunk of every tenant. There
  is no incremental path — vectors from different models are not comparable, so a mixed index
  is meaningless. This is acceptable because model changes should be rare and measured.
- Indexes are not portable across environments with different dims; a dev Vespa volume is
  useless against a prod-dim schema. Re-feed, don't copy.
- Misconfiguration (EMBEDDING_DIM disagreeing with the TEI model) is an operator error the
  services must detect loudly at startup/first-embed; silent zero-result vector search would
  be far worse.
- CI never asserts embedding *quality* (ADR-003 §2 still holds); it asserts plumbing at
  whatever dimension is configured.
