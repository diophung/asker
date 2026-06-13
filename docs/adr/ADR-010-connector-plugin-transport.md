# ADR-010: Out-of-process connectors run as gRPC plugins

## Status

Accepted (M2).

## Context

The spec requires the Connector SDK to support both in-process Go connectors and "external gRPC
process" connectors (§2.5), and states that "third-party connectors run as out-of-process gRPC
plugins with no credentials beyond their own scoped tokens." In-process connectors implement
`sdk.Connector` directly and share the hub's address space; that is the right default for the
first-party connectors we ship (Gmail, Drive, Slack, ...). But it forces every connector to be
Go, to be compiled into the hub binary, and to be trusted with the hub's memory — unacceptable
for connectors written by third parties or in another language.

The question this ADR settles is the transport: how an out-of-process connector speaks the same
five-method `sdk.Connector` contract across a process boundary, and how the in-process Emit /
Checkpoint callback semantics survive serialization. The decision is already encoded in the
generated `asker.plugin.v1` (`pluginv1`) proto; this ADR records it.

## Decision

1. **Out-of-process connectors implement `pluginv1.ConnectorPluginService`** (gRPC). The hub
   hosts an adapter that satisfies `sdk.Connector` by calling the plugin, so the rest of the hub
   — scheduler, token vault, Kafka producer — sees no difference between an in-proc connector
   and a plugin. In-process connectors remain the default; plugins are for third parties and
   language flexibility.

2. **The service maps method-for-method to `sdk.Connector`:** `Spec`, `Validate`, `FullSync`,
   `IncrementalSync`, `HandleWebhook`. `AuthType` (`AUTH_NONE` / `AUTH_OAUTH2` / `AUTH_TOKEN`)
   maps to `sdk.AuthNone` / `sdk.AuthOAuth2` / `sdk.AuthToken`. `Config{tenant_id, instance_id,
   config_json, token}` carries exactly the fields of `sdk.Config` that cross a process boundary
   (the `Checkpoint` callback does not — see below).

3. **The three sync RPCs are server-streaming.** `FullSync`, `IncrementalSync`, and
   `HandleWebhook` each stream `SyncEvent`s back to the hub. `SyncEvent` is a oneof:
   - `document` — one canonical `asker.v1.Document`, equivalent to an in-proc `emit(ctx, doc)`;
   - `checkpoint` — an opaque cursor string, equivalent to an in-proc `cfg.Checkpoint(ctx, cur)`;
   - `done` — `SyncDone{cursor}`, the terminating event carrying the advanced `sdk.Cursor` that
     the in-proc methods return.

   So Emit and Checkpoint are not RPCs the plugin calls back into the hub — they are events the
   plugin *streams out*, and the hub applies them on its side (routing the Document to Kafka,
   persisting the cursor). The stream ends with exactly one `done`.

4. **Hub-side callback failure cancels the stream, preserving terminal-pass semantics.** In the
   in-proc contract an `Emit` or `Checkpoint` error is terminal for the current sync pass
   (`connectors/sdk/connector.go`). Across the boundary the equivalent is: if the hub fails to
   apply a streamed `document` or `checkpoint` (e.g. the Kafka producer errors), the hub cancels
   the RPC context. The plugin observes the cancellation as a `Send` error on the stream and must
   stop and return — exactly as an in-proc connector returns an `Emit`/`Checkpoint` error. The
   pass is terminal; the hub retries from the last persisted checkpoint.

5. **The plugin receives ONLY its per-call decrypted scoped token.** `Config.token` is the bytes
   for *this* instance, already decrypted by the hub's token vault (ADR: token vault is hub-owned
   per §2.5). A plugin never sees the vault, another tenant's token, another instance's token, or
   any credential beyond the one scoped to the call — this is the spec's "no credentials beyond
   their own scoped tokens" rule made structural: the only credential material on the wire is the
   single `token` field of the `Config` for the call in flight.

6. **The proto is the versioned contract.** `asker.plugin.v1` is committed, generated code is
   checked in, and CI fails on drift (per the repo proto rules). Breaking changes go to a new
   package version; the canonical `asker.v1.Document` embedded in `SyncEvent` is the same contract
   every pipeline stage already depends on, so a plugin's Document is validated identically to an
   in-proc connector's.

## Consequences

- **A process and serialization boundary appears** where in-proc connectors had none: Documents
  are marshaled to protobuf and streamed, not handed over by pointer. The `connectortest`
  invariants (tenant match, DocID rule, version_etag, unset `ts.ingested`, tombstone-has-no-body)
  apply unchanged because they are properties of the Document, not of the transport.
- **The hub must supervise plugin processes** — spawn, health-check, restart, resource-bound, and
  kill on tenant disconnect / GDPR delete — and translate stream cancellation into the
  terminal-pass retry. That supervisor is M2 hub work and is **out of scope for this ADR's owned
  paths**; it is noted here as a dependency and tracked as an issue, not implemented here.
- **Language and trust isolation:** a plugin can be written in any language with gRPC support and
  runs in its own process, so a misbehaving or malicious third-party connector cannot read the
  hub's memory or other tenants' credentials. Sandboxing the plugin process itself (seccomp,
  network egress policy to curb SSRF in fetchers) is hardening deferred to M4/M6.
- **One Document contract, two transports:** because both paths terminate in the same
  `asker.v1.Document`, the pipeline downstream of the hub (ingest → enrich → index) is identical
  regardless of how a connector ran, and contract tests (ADR-011) are written once against the
  SDK rather than per transport.
- **Server-streaming only, no client-streaming callbacks:** the deliberate choice to stream
  events out (rather than have the plugin dial back into a hub callback service) keeps the plugin
  a pure server — it needs no hub endpoint, no hub credentials, and no reverse connection — which
  is what makes the "scoped token and nothing else" guarantee hold.
