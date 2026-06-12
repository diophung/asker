# ADR-001: Single Go module for the monorepo

## Status

Accepted (M0).

## Context

Asker is a monorepo containing shared platform libraries (`platform/...`), multiple Go
services (`services/...`), and — from M2 — connector packages. Go offers two ways to structure
this: one module at the repo root, or multiple modules stitched together with `go.work`.

Multi-module gives independently versioned components, but at a price: per-module `go.mod`
/`go.sum` maintenance, cross-module `replace`/workspace bookkeeping, harder atomic refactors
across module boundaries, and CI that must enumerate modules instead of running
`go build ./...` and `go test ./...` once.

At the current scale there are no external consumers of any Asker package, so independent
versioning buys nothing.

## Decision

One Go module, `github.com/asker/asker`, at the repository root. All platform libraries,
services, and (later) in-tree connectors live in this module. No `go.work`.

## Consequences

- CI, tooling, and local builds are single commands over `./...`; coverage, vet, and lint see
  the whole tree at once.
- Refactors across library/service boundaries are atomic — one commit, no version dance.
- A single dependency graph: one `go.sum`, one upgrade path, no skew between services.
- All code shares one Go version and one set of dependency versions; a dependency needed by
  one service is visible to all (acceptable — builds still only link what they import).
- **Revisit trigger:** if external consumers need to depend on an Asker package with
  independent versioning — most likely the Connector SDK in M2, which third parties build
  against — that package may be split into its own module then. This ADR should be superseded,
  not silently violated.
