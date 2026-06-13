package main

import (
	"net/http"

	controlplanev1 "github.com/asker/asker/platform/proto/gen/go/asker/controlplane/v1"
	"github.com/asker/asker/platform/tenancy"
)

// handleDeleteMyData serves DELETE /v1/me/data: the authenticated user erases
// THEIR OWN tenant across every store (GDPR right to erasure). The tenant is
// taken ONLY from the verified token context — never from the request — so a
// user can only ever delete their own data here. This is IRREVERSIBLE:
// connector configs, sync cursors, encrypted tokens, the tenant DEK (crypto-
// shredded), all indexed documents (Vespa group), and all blobs (MinIO prefix)
// are destroyed and verified empty before the call returns.
//
// The confirm token the control plane requires is the caller's own tenant id,
// which the gateway fills in from the verified context (the user cannot pass a
// different tenant — there is no body field that selects a tenant).
func (d *deps) handleDeleteMyData(w http.ResponseWriter, r *http.Request) {
	tc, err := tenancy.FromContext(r.Context())
	if err != nil {
		// Unreachable behind the auth middleware; fail closed regardless.
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	resp, err := d.control.DeleteTenant(r.Context(), &controlplanev1.DeleteTenantRequest{
		Confirm: string(tc.TenantID()),
	})
	if err != nil {
		d.upstreamError(w, r, "ControlPlane.DeleteTenant", err)
		return
	}
	rep := resp.GetReport()
	// 200 with the erasure receipt (no secrets): the user gets a confirmation of
	// what was destroyed and that the stores verified empty.
	writeJSON(w, http.StatusOK, map[string]any{
		"tenant_id":                   rep.GetTenantId(),
		"deleted":                     true,
		"connector_instances_deleted": rep.GetConnectorInstancesDeleted(),
		"tokens_deleted":              rep.GetTokensDeleted(),
		"dek_destroyed":               rep.GetDekDestroyed(),
		"vespa_group_purged":          rep.GetVespaGroupPurged(),
		"blobs_deleted":               rep.GetBlobsDeleted(),
		"redis_purged":                rep.GetRedisPurged(),
		"verified_empty":              rep.GetVerifiedEmpty(),
	})
}
