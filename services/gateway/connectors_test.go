package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestConnectorsCRUD(t *testing.T) {
	env := newTestEnv(t)

	// Create.
	rec := env.do(http.MethodPost, "/v1/connectors",
		strings.NewReader(`{"connector_id":"gmail","display_name":"My Gmail","config":{"label":"INBOX"}}`), nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	inst := decodeObject(t, rec)
	id, _ := inst["id"].(string)
	if id == "" {
		t.Fatalf("instance JSON missing id: %v", inst)
	}
	if inst["connectorId"] != "gmail" || inst["displayName"] != "My Gmail" || inst["status"] != "ACTIVE" {
		t.Errorf("instance JSON = %v, want proto json names connectorId/displayName/status", inst)
	}
	cfgBytes, err := base64.StdEncoding.DecodeString(inst["configJson"].(string))
	if err != nil {
		t.Fatalf("configJson is not base64: %v", err)
	}
	var gotCfg map[string]any
	if err := json.Unmarshal(cfgBytes, &gotCfg); err != nil {
		t.Fatalf("configJson is not JSON: %v", err)
	}
	if !reflect.DeepEqual(gotCfg, map[string]any{"label": "INBOX"}) {
		t.Errorf("config = %v, want {label: INBOX}", gotCfg)
	}

	// EnsureTenant must precede creation, for the JWT tenant.
	env.control.mu.Lock()
	ensured := append([]string(nil), env.control.ensured...)
	env.control.mu.Unlock()
	if len(ensured) != 1 || ensured[0] != testSubject {
		t.Errorf("EnsureTenant calls = %v, want [%s]", ensured, testSubject)
	}

	// List: [{"instance":...,"sync":...}] with PENDING sync inlined.
	rec = env.do(http.MethodGet, "/v1/connectors", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", rec.Code, rec.Body.String())
	}
	entries := decodeArray(t, rec)
	if len(entries) != 1 {
		t.Fatalf("list = %v, want 1 entry", entries)
	}
	entry, ok := entries[0].(map[string]any)
	if !ok {
		t.Fatalf("entry is %T, want object", entries[0])
	}
	listedInst, _ := entry["instance"].(map[string]any)
	if listedInst["id"] != id {
		t.Errorf("instance.id = %v, want %s", listedInst["id"], id)
	}
	sync, _ := entry["sync"].(map[string]any)
	if sync["phase"] != "PENDING" || sync["connectorInstanceId"] != id {
		t.Errorf("sync = %v, want phase PENDING for %s", sync, id)
	}

	// Token upsert.
	rec = env.do(http.MethodPut, "/v1/connectors/"+id+"/token",
		strings.NewReader(`{"token":"oauth-secret"}`), nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("put token status = %d, body = %s", rec.Code, rec.Body.String())
	}
	env.control.mu.Lock()
	gotToken := string(env.control.tokens[testSubject+"/"+id])
	env.control.mu.Unlock()
	if gotToken != "oauth-secret" {
		t.Errorf("stored token = %q, want oauth-secret", gotToken)
	}

	// Delete.
	rec = env.do(http.MethodDelete, "/v1/connectors/"+id, nil, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, body = %s", rec.Code, rec.Body.String())
	}
	rec = env.do(http.MethodGet, "/v1/connectors", nil, nil)
	if got := decodeArray(t, rec); len(got) != 0 {
		t.Errorf("list after delete = %v, want []", got)
	}
	if !strings.HasPrefix(rec.Body.String(), "[]") {
		t.Errorf("empty list body = %q, want [] not null", rec.Body.String())
	}

	// Delete again -> upstream NotFound -> 404.
	rec = env.do(http.MethodDelete, "/v1/connectors/"+id, nil, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("re-delete status = %d, want 404", rec.Code)
	}
}

// TestConnectorsTenantIsolation proves the JWT tenant scopes every
// control-plane RPC: tenant B can neither see nor delete A's instance.
func TestConnectorsTenantIsolation(t *testing.T) {
	env := newTestEnv(t)

	rec := env.do(http.MethodPost, "/v1/connectors",
		strings.NewReader(`{"connector_id":"gmail"}`), nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d", rec.Code)
	}
	id := decodeObject(t, rec)["id"].(string)

	asTenantB := http.Header{"Authorization": []string{env.bearerFor("tenant-b")}}

	rec = env.do(http.MethodGet, "/v1/connectors", nil, asTenantB)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d", rec.Code)
	}
	if got := decodeArray(t, rec); len(got) != 0 {
		t.Errorf("tenant B sees tenant A's connectors: %v", got)
	}

	rec = env.do(http.MethodDelete, "/v1/connectors/"+id, nil, asTenantB)
	if rec.Code != http.StatusNotFound {
		t.Errorf("cross-tenant delete status = %d, want 404", rec.Code)
	}

	rec = env.do(http.MethodPut, "/v1/connectors/"+id+"/token",
		strings.NewReader(`{"token":"stolen"}`), asTenantB)
	if rec.Code != http.StatusNotFound {
		t.Errorf("cross-tenant put token status = %d, want 404", rec.Code)
	}
}

func TestCreateConnectorBadRequests(t *testing.T) {
	env := newTestEnv(t)
	for name, body := range map[string]string{
		"invalid JSON":         `{"connector_id":`,
		"missing connector_id": `{"display_name":"x"}`,
		"config not an object": `{"connector_id":"gmail","config":[1,2]}`,
		"config is a string":   `{"connector_id":"gmail","config":"nope"}`,
	} {
		t.Run(name, func(t *testing.T) {
			rec := env.do(http.MethodPost, "/v1/connectors", strings.NewReader(body), nil)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
			}
		})
	}
	// Empty/absent config defaults to {}.
	rec := env.do(http.MethodPost, "/v1/connectors", strings.NewReader(`{"connector_id":"upload"}`), nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	cfgBytes, err := base64.StdEncoding.DecodeString(decodeObject(t, rec)["configJson"].(string))
	if err != nil || string(cfgBytes) != "{}" {
		t.Errorf("default configJson = %q (err %v), want {}", cfgBytes, err)
	}
}

func TestPutTokenBadRequests(t *testing.T) {
	env := newTestEnv(t)
	rec := env.do(http.MethodPut, "/v1/connectors/inst-1/token", strings.NewReader(`{"token":""}`), nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty token status = %d, want 400", rec.Code)
	}
	rec = env.do(http.MethodPut, "/v1/connectors/inst-1/token", strings.NewReader(`not json`), nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad json status = %d, want 400", rec.Code)
	}
	rec = env.do(http.MethodPut, "/v1/connectors/no-such/token", strings.NewReader(`{"token":"x"}`), nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown instance status = %d, want 404", rec.Code)
	}
}

func TestConnectorsUpstreamFailures(t *testing.T) {
	setErr := func(env *testEnv, set func(f *fakeControlPlane)) {
		env.control.mu.Lock()
		defer env.control.mu.Unlock()
		set(env.control)
	}

	t.Run("EnsureTenant unavailable -> 502", func(t *testing.T) {
		env := newTestEnv(t)
		setErr(env, func(f *fakeControlPlane) { f.ensureErr = status.Error(codes.Unavailable, "down") })
		rec := env.do(http.MethodPost, "/v1/connectors", strings.NewReader(`{"connector_id":"gmail"}`), nil)
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502 (body: %s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("List unavailable -> 502", func(t *testing.T) {
		env := newTestEnv(t)
		setErr(env, func(f *fakeControlPlane) { f.listErr = status.Error(codes.Unavailable, "down") })
		rec := env.do(http.MethodGet, "/v1/connectors", nil, nil)
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502 (body: %s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("GetSyncState NotFound -> sync null", func(t *testing.T) {
		env := newTestEnv(t)
		rec := env.do(http.MethodPost, "/v1/connectors", strings.NewReader(`{"connector_id":"gmail"}`), nil)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create status = %d", rec.Code)
		}
		setErr(env, func(f *fakeControlPlane) { f.syncErr = status.Error(codes.NotFound, "gone") })
		rec = env.do(http.MethodGet, "/v1/connectors", nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("list status = %d", rec.Code)
		}
		entries := decodeArray(t, rec)
		if len(entries) != 1 {
			t.Fatalf("entries = %v", entries)
		}
		entry := entries[0].(map[string]any)
		if sync, present := entry["sync"]; !present || sync != nil {
			t.Errorf("sync = %v (present %v), want explicit null", sync, present)
		}
	})

	t.Run("GetSyncState unavailable -> 502", func(t *testing.T) {
		env := newTestEnv(t)
		rec := env.do(http.MethodPost, "/v1/connectors", strings.NewReader(`{"connector_id":"gmail"}`), nil)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create status = %d", rec.Code)
		}
		setErr(env, func(f *fakeControlPlane) { f.syncErr = status.Error(codes.Unavailable, "down") })
		rec = env.do(http.MethodGet, "/v1/connectors", nil, nil)
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("list status = %d, want 502 (body: %s)", rec.Code, rec.Body.String())
		}
	})
}

func TestConnectorsMethodNotAllowed(t *testing.T) {
	env := newTestEnv(t)
	rec := env.do(http.MethodPatch, "/v1/connectors", nil, nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != "GET, POST" {
		t.Errorf("Allow = %q, want GET, POST", allow)
	}
	if decodeObject(t, rec)["error"] != "method not allowed" {
		t.Errorf("body = %s", rec.Body.String())
	}

	rec = env.do(http.MethodGet, "/v1/connectors/abc", nil, nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /v1/connectors/{id} status = %d, want 405", rec.Code)
	}
}

func TestConnectorsRequireAuth(t *testing.T) {
	env := newTestEnv(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/connectors"},
		{http.MethodPost, "/v1/connectors"},
		{http.MethodDelete, "/v1/connectors/inst-1"},
		{http.MethodPut, "/v1/connectors/inst-1/token"},
		{http.MethodPost, "/v1/upload"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}"))
		rec := httptest.NewRecorder()
		env.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without token: status = %d, want 401", tc.method, tc.path, rec.Code)
		}
	}
}
