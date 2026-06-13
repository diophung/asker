package main

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"google.golang.org/grpc/metadata"

	controlplanev1 "github.com/asker/asker/platform/proto/gen/go/asker/controlplane/v1"
)

// adminSubjectMetadataKey carries the VERIFIED operator identity (the JWT "sub")
// from the gateway to the control plane so admin audit logs and DeleteReports
// name the actual operator, not a literal "admin" (finding M6-#6). It is set
// ONLY from verified claims, never from request input, and never carries the
// token itself. The control-plane adminServer reads the same key.
const adminSubjectMetadataKey = "x-asker-admin-subject"

// adminSubject returns the verified operator's identifier for the audit trail:
// the "sub" claim (a stable, server-issued Keycloak subject), falling back to
// the "email" claim, else "unknown". It reads only verified claims — never a
// token, never request input.
func adminSubject(claims map[string]any) string {
	if claims == nil {
		return "unknown"
	}
	for _, key := range []string{"sub", "email"} {
		if v, ok := claims[key].(string); ok {
			if v = strings.TrimSpace(v); v != "" {
				return v
			}
		}
	}
	return "unknown"
}

// withAdminSubject attaches the verified operator subject as outgoing gRPC
// metadata so the control plane can attribute the admin action. The value comes
// from the verified claims in ctx (set by the auth middleware); requireAdmin has
// already proven the caller is an admin before any handler runs.
func withAdminSubject(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(ctx, adminSubjectMetadataKey, adminSubject(claimsFromContext(ctx)))
}

// Admin authorization (M6): /v1/admin/* is gated by a DISTINCT admin claim in
// the verified token — NOT mere authentication. A request that authenticates as
// an ordinary user but lacks the admin claim is 403 (not 404: the admin surface
// itself is not a secret; only acting on a specific tenant must not leak). The
// claim is recognized in any of these standard Keycloak shapes:
//
//   - realm role: claims.realm_access.roles contains "asker-admin"
//   - client role: claims.resource_access.<aud>.roles contains "asker-admin"
//   - scope: the space-separated "scope" claim contains "asker:admin"
//
// HOW THE CLAIM IS SET (operator runbook): create a realm role "asker-admin" in
// the Keycloak `asker` realm and assign it to operator users (Realm roles ->
// asker-admin -> Users in role), OR define a client scope "asker:admin" and
// grant it to an operator service account. The role/scope flows into the access
// token automatically (realm-role and scope mappers are on by default); no
// application change is needed. Users NEVER self-assert it — it is server-issued
// identity material, the same trust basis as the tenant claim (ADR-002).
const (
	adminRoleName  = "asker-admin"
	adminScopeName = "asker:admin"
)

// isAdmin reports whether the verified claims carry the admin role/scope.
func isAdmin(claims map[string]any, audience string) bool {
	if claims == nil {
		return false
	}
	// realm_access.roles
	if ra, ok := claims["realm_access"].(map[string]any); ok {
		if hasRole(ra["roles"], adminRoleName) {
			return true
		}
	}
	// resource_access.<aud>.roles
	if rsa, ok := claims["resource_access"].(map[string]any); ok {
		if client, ok := rsa[audience].(map[string]any); ok {
			if hasRole(client["roles"], adminRoleName) {
				return true
			}
		}
	}
	// scope (space-separated)
	if scope, ok := claims["scope"].(string); ok {
		for _, s := range strings.Fields(scope) {
			if s == adminScopeName {
				return true
			}
		}
	}
	return false
}

func hasRole(roles any, want string) bool {
	list, ok := roles.([]any)
	if !ok {
		return false
	}
	for _, r := range list {
		if s, ok := r.(string); ok && s == want {
			return true
		}
	}
	return false
}

// requireAdmin wraps an admin handler with the admin-claim authorization check.
// It runs AFTER auth (so claims are present) and returns 403 when the verified
// identity is authenticated but not an admin.
func (d *deps) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isAdmin(claimsFromContext(r.Context()), d.oidcAudience) {
			d.logger.Warn("admin authorization denied", "path", r.URL.Path)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin role required"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleAdminListTenants serves GET /v1/admin/tenants?limit=&page_token=.
func (d *deps) handleAdminListTenants(w http.ResponseWriter, r *http.Request) {
	req := &controlplanev1.ListTenantsRequest{PageToken: r.URL.Query().Get("page_token")}
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid limit"})
			return
		}
		req.Limit = int32(n)
	}
	resp, err := d.admin.ListTenants(withAdminSubject(r.Context()), req)
	if err != nil {
		d.upstreamError(w, r, "Admin.ListTenants", err)
		return
	}
	d.writeProtoJSON(w, http.StatusOK, resp)
}

// handleAdminGetTenant serves GET /v1/admin/tenants/{tenant} (usage triage).
func (d *deps) handleAdminGetTenant(w http.ResponseWriter, r *http.Request) {
	resp, err := d.admin.GetTenantUsage(withAdminSubject(r.Context()), &controlplanev1.GetTenantUsageRequest{
		TenantId: r.PathValue("tenant"),
	})
	if err != nil {
		d.upstreamError(w, r, "Admin.GetTenantUsage", err)
		return
	}
	d.writeProtoJSON(w, http.StatusOK, resp.GetUsage())
}

// handleAdminSuspendTenant serves POST /v1/admin/tenants/{tenant}/suspend with
// body {"suspended": true|false} (pause/resume all the tenant's connectors).
func (d *deps) handleAdminSuspendTenant(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Suspended bool `json:"suspended"`
	}
	if !decodeJSONBody(w, r, &body) {
		return
	}
	resp, err := d.admin.SuspendTenant(withAdminSubject(r.Context()), &controlplanev1.SuspendTenantRequest{
		TenantId:  r.PathValue("tenant"),
		Suspended: body.Suspended,
	})
	if err != nil {
		d.upstreamError(w, r, "Admin.SuspendTenant", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tenant_id":         r.PathValue("tenant"),
		"suspended":         body.Suspended,
		"instances_changed": resp.GetInstancesChanged(),
	})
}

// handleAdminDeleteTenant serves DELETE /v1/admin/tenants/{tenant}: operator-
// initiated GDPR erasure of an ARBITRARY tenant (abuse takedown / right-to-
// erasure on behalf of a user). Audit-logged at the control plane.
func (d *deps) handleAdminDeleteTenant(w http.ResponseWriter, r *http.Request) {
	resp, err := d.admin.AdminDeleteTenant(withAdminSubject(r.Context()), &controlplanev1.AdminDeleteTenantRequest{
		TenantId: r.PathValue("tenant"),
	})
	if err != nil {
		d.upstreamError(w, r, "Admin.AdminDeleteTenant", err)
		return
	}
	rep := resp.GetReport()
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
		"actor":                       rep.GetActor(),
	})
}
