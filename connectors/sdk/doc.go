// Package sdk is the Asker Connector SDK: the contract between a data-source
// connector (Gmail, Slack, file upload, ...) and the connector hub that runs
// it. M1 connectors are in-process Go implementations registered in a
// [Registry]; M2 adds an out-of-process gRPC plugin transport that speaks the
// same interface, so code written against this package carries forward.
//
// # What a connector is
//
// A connector turns one external source into a stream of canonical
// [askerv1.Document] values. It implements [Connector]:
//
//   - Spec describes the connector: stable ID, display name, auth type, and a
//     JSONSchema for its per-instance configuration.
//   - Validate checks a [Config] (parsed ConfigJSON, token present, source
//     reachable) without emitting anything.
//   - FullSync performs the initial, resumable backfill, emitting every
//     document and checkpointing progress; it returns the [Cursor] from which
//     incremental sync continues.
//   - IncrementalSync emits everything that changed since a cursor (the poll
//     path) and returns the advanced cursor.
//   - HandleWebhook is the push path; connectors without one return
//     [ErrWebhookUnsupported] and the hub falls back to polling.
//
// # The Emit contract (binding)
//
// Connectors hand documents to the hub through the [Emit] callback and NEVER
// touch Kafka, Vespa, or blob storage themselves. Every emitted document must
// satisfy:
//
//   - tenant_id equals cfg.Tenant.TenantID() — never any other value.
//   - doc_id is exactly [DocID](connector_id, source_native_id); connector_id
//     and source_native_id are both set.
//   - version_etag is set and changes whenever source content changes, so
//     downstream upserts are idempotent.
//   - ts.ingested is UNSET; the hub stamps ingestion time when it routes the
//     document to Kafka. ts.created / ts.modified come from the source when
//     known.
//   - Deletions are first-class: emit the same doc_id with
//     tombstone.deleted=true and no body (empty body_text, no chunks).
//
// sdk/connectortest provides [EmitRecorder], ValidateDocument, and
// RunSpecChecks so connector test suites can assert these invariants without
// a running hub.
//
// # Minimal connector walkthrough
//
// A connector for a toy "notes" source looks like this (a complete runnable
// version lives in example_test.go; the M2 "build a connector in under a day"
// tutorial expands on it):
//
//	type notesConnector struct{ /* api client, etc. */ }
//
//	func (notesConnector) Spec() sdk.Spec {
//		return sdk.Spec{
//			ID:           "notes",
//			DisplayName:  "Toy Notes",
//			AuthType:     sdk.AuthToken,
//			ConfigSchema: json.RawMessage(`{"type":"object"}`),
//		}
//	}
//
//	func (c notesConnector) FullSync(ctx context.Context, cfg sdk.Config, emit sdk.Emit) (sdk.Cursor, error) {
//		for _, n := range c.fetchAll(ctx) {
//			doc := &askerv1.Document{
//				TenantId:       string(cfg.Tenant.TenantID()),
//				ConnectorId:    "notes",
//				SourceNativeId: n.ID,
//				DocId:          sdk.DocID("notes", n.ID),
//				Type:           askerv1.DocType_FILE,
//				Title:          n.Title,
//				BodyText:       n.Body,
//				VersionEtag:    n.ETag,
//			}
//			if err := emit(ctx, doc); err != nil {
//				return "", err
//			}
//			if err := cfg.Checkpoint(ctx, sdk.Cursor(n.ID)); err != nil {
//				return "", err
//			}
//		}
//		return c.latestCursor(), nil
//	}
//
// # Division of labor with the hub
//
// The hub owns OAuth flows, token refresh and the encrypted token vault,
// sync scheduling (webhook-first with polling fallback), checkpoint
// persistence, per-tenant rate limits, retry/backoff, poison-document
// quarantine, and producing to Kafka. A connector only reads its source and
// emits documents; it receives credentials via [Config.Token] and reports
// resumption points via [Config.Checkpoint].
package sdk
