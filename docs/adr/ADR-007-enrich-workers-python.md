# ADR-007: Enrichment workers in Python

## Status

Accepted (M1).

## Context

The spec fixes service languages: Go for everything except ML workers, which are Python
(§2.3 — "Python only for ML workers (enrichment)"). At M1 the enrich stage is thin — consume
`docs.chunked`, call TEI over HTTP for embeddings, produce `docs.enriched` — and could
trivially be Go, which would keep the repo single-toolchain. But M3 grows this same worker
into the media pipeline: OpenCLIP image embeddings, Tesseract OCR, faster-whisper ASR, ffmpeg
keyframe orchestration — all of which live natively in the Python ML ecosystem. Writing enrich
in Go now would mean either rewriting it in M3 or bolting Python sidecars onto a Go worker.

## Decision

The enrich worker (`services/enrich`) is **Python from M1**, even though its first job is
plumbing:

- **aiokafka** for the consumer/producer, implementing the ADR-004 pipeline contract
  (tenant-keyed records, `tenant_id`/`doc_id`/`version_etag` headers, commit-after-success,
  3-attempt capped backoff then `docs.deadletter`).
- **httpx** to call TEI's HTTP API for text embeddings (dimension per ADR-005).
- **ruff** (lint) and **pytest** (tests) wired into CI alongside the Go jobs, per the spec's
  engineering standards (§4).

Python's footprint stays confined to this one service; gateway, query, ingest, connector-hub,
control-plane, and index-writer remain Go.

## Consequences

- A second language toolchain to maintain: separate dependency management, lint/test CI jobs,
  container base image, and on-call knowledge. Accepted as the cost of the M3 path — the
  alternative is an M3 rewrite of a working service.
- The ADR-004 Kafka semantics now exist in two implementations (`platform/kafkautil` in Go,
  the aiokafka worker in Python) with no shared code. The Python worker's tests must assert
  the contract behaviors (header propagation, at-least-once commit, dead-letter on poison
  documents) explicitly; parity is by test, not by construction.
- `platform/tenancy`'s compile-time guarantees do not extend to Python; the worker re-validates
  the tenant header against the same allowlist by convention. This widens the surface the
  cross-tenant leakage suite must cover.
- Python's protobuf bindings for `asker.v1.Document` must be generated and kept in drift-check
  with the proto source, mirroring the Go generation.
- M3 lands as a capability addition (new enrichers in an existing Python worker), not an
  architecture change.
