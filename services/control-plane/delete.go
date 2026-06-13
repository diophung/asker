package main

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	controlplanev1 "github.com/asker/asker/platform/proto/gen/go/asker/controlplane/v1"
	"github.com/asker/asker/platform/tenancy"
)

// DeleteTenant erases the CALLER's own tenant (GDPR right to erasure, M6). The
// tenant comes ONLY from the verified caller context (the same chokepoint as
// every other RPC) — never from the request — so a user can erase only THEIR
// OWN data here. The request's confirm token must equal the caller tenant id; a
// mismatch is InvalidArgument. Cross-tenant operator erasure is the separate,
// admin-gated AdminService.AdminDeleteTenant.
func (s *server) DeleteTenant(ctx context.Context, req *controlplanev1.DeleteTenantRequest) (*controlplanev1.DeleteTenantResponse, error) {
	tc, err := callerTenant(ctx)
	if err != nil {
		return nil, err
	}
	// Defense in depth against an accidental call: the caller must echo their
	// own tenant id. This NEVER selects the tenant (that is always tc); it only
	// confirms intent for an irreversible action.
	if req.GetConfirm() != string(tc.TenantID()) {
		return nil, status.Error(codes.InvalidArgument,
			"confirm must equal your own tenant id (from GET /v1/me); this action is irreversible")
	}
	report, err := s.cascadeDelete(ctx, tc.TenantID(), "self")
	if err != nil {
		return nil, err
	}
	return &controlplanev1.DeleteTenantResponse{Report: report}, nil
}

// cascadeDelete is the shared GDPR erasure cascade used by both the self-serve
// DeleteTenant and the admin AdminDeleteTenant paths. actor is "self" or
// "admin:<subject>" for the audit line. Order matters:
//
//  0. RE-INGEST FENCE (finding M6-#5): suspend the tenant's connector instances
//     FIRST (flip every instance to PAUSED). The connector-hub scheduler reads
//     instance status on each reconcile tick, so a paused fleet stops producing
//     new docs onto docs.raw. Cutting the source off BEFORE purging Vespa/blobs
//     is what keeps a sync that races the delete from re-indexing the tenant
//     after the purge completes.
//  1. Crypto-shred the DEK (DEKStore.Delete + cipher.Forget). With the wrapped
//     DEK destroyed, every blob/token ciphertext is ALREADY unrecoverable even
//     if a later store deletion partially fails — the fast, fail-safe GDPR
//     primitive.
//  2. Purge Postgres (tenant row + connector_instances + sync_states + tokens).
//     This deletes the (now-paused) instances outright, so the scheduler drops
//     their workers on its next reconcile.
//  3. Purge the Vespa streaming group, MinIO blob prefix, and Redis cache.
//  4. Verify: re-read Postgres and the DEK store; a residue fails the RPC.
//
// Every step is tenant-scoped from the verified identity and fails closed. The
// cascade is IDEMPOTENT (a retried erasure converges): suspend, shred, purge,
// and the store deletes all tolerate an already-absent tenant, so re-running
// the cascade cleans any in-flight stragglers a prior run raced.
//
// RESIDUAL RISK (documented, NOT silently claimed erased): suspending the
// connector instances stops the SCHEDULER, but a Kafka record already produced
// onto docs.raw/docs.chunked/docs.enriched around delete time can still be
// consumed by the index-writer AFTER the Vespa purge and re-create a document.
// Fully fencing that requires a per-tenant delete tombstone the index-writer
// consults before every upsert (drop-on-tombstone). That tombstone subsystem is
// a tracked M6 follow-up; the connector-suspension-first ordering plus the
// idempotent re-verify here is the M6 fix. A caller that needs hard guarantees
// against an in-flight record re-runs the cascade after the pipeline drains.
func (s *server) cascadeDelete(ctx context.Context, tenantID tenancy.TenantID, actor string) (*controlplanev1.DeleteReport, error) {
	if tenantID == "" {
		return nil, status.Error(codes.InvalidArgument, "empty tenant")
	}
	report := &controlplanev1.DeleteReport{TenantId: string(tenantID), Actor: actor}
	tc, err := tenancy.FromHeaderValue(string(tenantID))
	if err != nil {
		// The tenant id failed the syntax allowlist — should be impossible for a
		// verified caller, but fail closed rather than purge a malformed id.
		return nil, status.Error(codes.InvalidArgument, "invalid tenant")
	}

	s.logger.InfoContext(ctx, "GDPR delete: starting cascade",
		"tenant", tenantID, "actor", actor)

	// 0. Re-ingest fence: suspend the tenant's connectors BEFORE the purge so the
	// scheduler produces no new docs during/after it. PAUSE (not delete) here so
	// the step is meaningful even though step 2 deletes the rows: pausing takes
	// effect on the hub's next reconcile, decoupled from the Postgres delete.
	// Idempotent — a tenant with no/already-paused instances changes zero rows.
	if _, err := s.store.SetTenantConnectorStatus(ctx, tenantID, controlplanev1.ConnectorStatus_PAUSED.String()); err != nil {
		s.logger.ErrorContext(ctx, "GDPR delete: suspend connectors failed",
			"tenant", tenantID, "actor", actor, "error", err)
		return nil, status.Error(codes.Internal, "DeleteTenant failed")
	}

	// 1. Crypto-shred the DEK (renders all ciphertext unreadable immediately).
	if s.cfg.dek != nil {
		if err := s.cfg.dek.Delete(ctx, tenantID); err != nil {
			s.logger.ErrorContext(ctx, "GDPR delete: DEK shred failed",
				"tenant", tenantID, "actor", actor, "error", err)
			return nil, status.Error(codes.Internal, "DeleteTenant failed")
		}
		report.DekDestroyed = true
	}
	s.cipher.Forget(tenantID) // drop any cached AEAD regardless

	// 2. Purge Postgres.
	counts, err := s.store.PurgeTenant(ctx, tenantID)
	if err != nil {
		s.logger.ErrorContext(ctx, "GDPR delete: Postgres purge failed",
			"tenant", tenantID, "actor", actor, "error", err)
		return nil, status.Error(codes.Internal, "DeleteTenant failed")
	}
	report.ConnectorInstancesDeleted = counts.ConnectorInstances
	report.TokensDeleted = counts.Tokens

	// 3a. Vespa streaming group.
	if s.cfg.vespa != nil {
		if err := s.cfg.vespa.PurgeGroup(ctx, tenantID); err != nil {
			s.logger.ErrorContext(ctx, "GDPR delete: Vespa group purge failed",
				"tenant", tenantID, "actor", actor, "error", err)
			return nil, status.Error(codes.Internal, "DeleteTenant failed")
		}
		report.VespaGroupPurged = true
	}

	// 3b. MinIO blob prefix (originals + thumbnails/keyframes).
	if s.cfg.blobs != nil {
		n, err := s.cfg.blobs.DeletePrefix(ctx, tc)
		if err != nil {
			s.logger.ErrorContext(ctx, "GDPR delete: blob purge failed",
				"tenant", tenantID, "actor", actor, "error", err)
			return nil, status.Error(codes.Internal, "DeleteTenant failed")
		}
		report.BlobsDeleted = int64(n)
	}

	// 3c. Redis cache + rate-limit keys (best effort: derived, TTL'd data; a
	// failure is logged and does NOT block the erasure, but it MUST NOT be
	// reported as a complete purge. redisAttempted distinguishes "no cache
	// configured" (nothing to purge — not a failure) from "configured but the
	// purge failed" (the keys may survive until their TTL; finding M6-#7).
	redisAttempted := s.cfg.cache != nil
	redisFailed := false
	if redisAttempted {
		if err := s.cfg.cache.PurgeTenant(ctx, tenantID); err != nil {
			redisFailed = true
			s.logger.WarnContext(ctx, "GDPR delete: Redis purge failed (best effort; retry the idempotent cascade to drain)",
				"tenant", tenantID, "actor", actor, "error", err)
		} else {
			report.RedisPurged = true
		}
	}

	// 4. Verification: re-read Postgres + the DEK store. Any residue fails closed.
	empty, err := s.store.TenantResidue(ctx, tenantID)
	if err != nil {
		s.logger.ErrorContext(ctx, "GDPR delete: verification read failed",
			"tenant", tenantID, "actor", actor, "error", err)
		return nil, status.Error(codes.Internal, "DeleteTenant failed")
	}
	if !empty {
		s.logger.ErrorContext(ctx, "GDPR delete: residue remains after purge",
			"tenant", tenantID, "actor", actor)
		return nil, status.Error(codes.Internal, "DeleteTenant verification failed: residual data remains")
	}
	// VerifiedEmpty asserts COMPLETE erasure across every store. A failed Redis
	// purge means derived cache/rate-limit keys may persist until their TTL, so
	// we must not assert complete erasure: leave VerifiedEmpty false (the cascade
	// is idempotent — the caller retries). The authoritative stores (Postgres,
	// DEK, Vespa, blobs) ARE verified gone; only the best-effort cache lagged.
	// (redisFailed can only be true when redisAttempted is.)
	report.VerifiedEmpty = !redisFailed

	s.logger.InfoContext(ctx, "GDPR delete: cascade complete",
		"tenant", tenantID, "actor", actor,
		"connector_instances_deleted", report.GetConnectorInstancesDeleted(),
		"tokens_deleted", report.GetTokensDeleted(),
		"dek_destroyed", report.GetDekDestroyed(),
		"vespa_group_purged", report.GetVespaGroupPurged(),
		"blobs_deleted", report.GetBlobsDeleted(),
		"redis_purged", report.GetRedisPurged(),
		"verified_empty", report.GetVerifiedEmpty())
	return report, nil
}
