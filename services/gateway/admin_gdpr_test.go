package main

import (
	"net/http"
	"strings"
	"testing"
)

// adminClaims returns a base claim set carrying the realm admin role.
func adminClaims() map[string]any {
	c := baseClaims()
	c["realm_access"] = map[string]any{"roles": []any{"asker-admin"}}
	return c
}

func TestDeleteMyDataErasesCallerTenant(t *testing.T) {
	e := newTestEnv(t)
	rec := e.do(http.MethodDelete, "/v1/me/data", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE /v1/me/data: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	out := decodeObject(t, rec)
	if out["deleted"] != true {
		t.Errorf("response missing deleted=true: %v", out)
	}
	// The gateway derived the confirm token from the verified tenant (the fake
	// rejects a mismatched confirm with InvalidArgument), and the caller's own
	// tenant was the one deleted.
	e.control.mu.Lock()
	deleted := append([]string(nil), e.control.deletedTenants...)
	e.control.mu.Unlock()
	if len(deleted) != 1 || deleted[0] != testSubject {
		t.Errorf("deleted tenants = %v, want [%s] (caller's own tenant)", deleted, testSubject)
	}
}

func TestDeleteMyDataRejectsNonDelete(t *testing.T) {
	e := newTestEnv(t)
	// GET /v1/me/data is not a route method -> 405.
	rec := e.do(http.MethodGet, "/v1/me/data", nil, nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /v1/me/data: status = %d, want 405", rec.Code)
	}
}

func TestDeleteMyDataRequiresAuth(t *testing.T) {
	e := newTestEnv(t)
	req, rec := newRawRequest(http.MethodDelete, "/v1/me/data")
	e.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauth DELETE /v1/me/data: status = %d, want 401", rec.Code)
	}
}

func TestAdminRoutesRequireAdminClaim(t *testing.T) {
	e := newTestEnv(t)
	// A normal authenticated user (no admin role) is FORBIDDEN, not merely
	// authenticated-through.
	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodGet, "/v1/admin/tenants"},
		{http.MethodGet, "/v1/admin/tenants/tenant-x"},
		{http.MethodDelete, "/v1/admin/tenants/tenant-x"},
	} {
		rec := e.doWithClaims(tc.method, tc.path, nil, baseClaims())
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s as non-admin: status = %d, want 403", tc.method, tc.path, rec.Code)
		}
	}
	// The admin RPCs were never invoked by a non-admin caller.
	e.admin.mu.Lock()
	defer e.admin.mu.Unlock()
	if e.admin.listed || e.admin.deletedTenant != "" {
		t.Error("admin RPC invoked despite missing admin claim")
	}
}

func TestAdminRoutesRequireAuth(t *testing.T) {
	e := newTestEnv(t)
	req, rec := newRawRequest(http.MethodGet, "/v1/admin/tenants")
	e.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauth admin route: status = %d, want 401", rec.Code)
	}
}

func TestAdminListTenantsWithAdminClaim(t *testing.T) {
	e := newTestEnv(t)
	rec := e.doWithClaims(http.MethodGet, "/v1/admin/tenants", nil, adminClaims())
	if rec.Code != http.StatusOK {
		t.Fatalf("admin ListTenants: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	e.admin.mu.Lock()
	defer e.admin.mu.Unlock()
	if !e.admin.listed {
		t.Error("admin ListTenants not invoked")
	}
}

func TestAdminDeleteTenantWithAdminClaim(t *testing.T) {
	e := newTestEnv(t)
	rec := e.doWithClaims(http.MethodDelete, "/v1/admin/tenants/tenant-victim", nil, adminClaims())
	if rec.Code != http.StatusOK {
		t.Fatalf("admin delete: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	e.admin.mu.Lock()
	defer e.admin.mu.Unlock()
	if e.admin.deletedTenant != "tenant-victim" {
		t.Errorf("admin deleted %q, want tenant-victim", e.admin.deletedTenant)
	}
}

func TestAdminSuspendTenantWithScopeClaim(t *testing.T) {
	e := newTestEnv(t)
	// Admin via the scope claim variant (not the realm role).
	claims := baseClaims()
	claims["scope"] = "openid profile asker:admin"
	body := strings.NewReader(`{"suspended":true}`)
	rec := e.doWithClaims(http.MethodPost, "/v1/admin/tenants/tenant-y/suspend", body, claims)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin suspend: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	e.admin.mu.Lock()
	defer e.admin.mu.Unlock()
	if !e.admin.suspended["tenant-y"] {
		t.Error("tenant-y not suspended")
	}
}

func TestIsAdminClaimShapes(t *testing.T) {
	aud := "asker-web"
	cases := []struct {
		name   string
		claims map[string]any
		want   bool
	}{
		{"nil", nil, false},
		{"no roles", map[string]any{}, false},
		{"realm role", map[string]any{"realm_access": map[string]any{"roles": []any{"asker-admin"}}}, true},
		{"realm role absent", map[string]any{"realm_access": map[string]any{"roles": []any{"user"}}}, false},
		{"client role", map[string]any{"resource_access": map[string]any{aud: map[string]any{"roles": []any{"asker-admin"}}}}, true},
		{"scope", map[string]any{"scope": "openid asker:admin"}, true},
		{"scope substring not matched", map[string]any{"scope": "asker:administrator"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isAdmin(c.claims, aud); got != c.want {
				t.Errorf("isAdmin = %v, want %v", got, c.want)
			}
		})
	}
}
