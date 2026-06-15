package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	controlplanev1 "github.com/asker/asker/platform/proto/gen/go/asker/controlplane/v1"
)

// createTestConnector creates one connector for the default tenant and returns
// its id (via the real POST /v1/connectors handler).
func createTestConnector(t *testing.T, env *testEnv) string {
	t.Helper()
	rec := env.do(http.MethodPost, "/v1/connectors",
		strings.NewReader(`{"connector_id":"gmail","display_name":"Mail"}`), nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create connector: status %d, body %s", rec.Code, rec.Body.String())
	}
	var inst struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &inst); err != nil || inst.ID == "" {
		t.Fatalf("create connector: bad body %s (err %v)", rec.Body.String(), err)
	}
	return inst.ID
}

func TestIndexStatus(t *testing.T) {
	env := newTestEnv(t)
	id := createTestConnector(t, env)
	// Steady state: everything emitted is indexed (no backlog), INCREMENTAL.
	env.query.indexed = 100
	env.control.seedSyncState(testSubject, id, &controlplanev1.SyncState{
		ConnectorInstanceId: id,
		Phase:               controlplanev1.SyncPhase_INCREMENTAL,
		DocsEmitted:         100,
		LastSyncCompleted:   timestamppb.New(fakeNow),
	})

	rec := env.do(http.MethodGet, "/v1/index/status", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	var got indexStatusJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Indexed != 100 || got.Emitted != 100 || got.Backlog != 0 {
		t.Errorf("indexed/emitted/backlog = %d/%d/%d, want 100/100/0", got.Indexed, got.Emitted, got.Backlog)
	}
	if got.Syncing {
		t.Error("syncing = true, want false (caught up + INCREMENTAL)")
	}
	if len(got.Connectors) != 1 {
		t.Fatalf("connectors = %d, want 1", len(got.Connectors))
	}
	c := got.Connectors[0]
	if c.ID != id || c.ConnectorID != "gmail" || c.DocsEmitted != 100 || c.Phase != "INCREMENTAL" {
		t.Errorf("connector = %+v, want id=%s gmail/100/INCREMENTAL", c, id)
	}
}

func TestIndexStatusBacklogImpliesSyncing(t *testing.T) {
	env := newTestEnv(t)
	id := createTestConnector(t, env)
	env.query.indexed = 10
	env.control.seedSyncState(testSubject, id, &controlplanev1.SyncState{
		ConnectorInstanceId: id,
		Phase:               controlplanev1.SyncPhase_FULL_SYNC,
		DocsEmitted:         100,
	})
	rec := env.do(http.MethodGet, "/v1/index/status", nil, nil)
	var got indexStatusJSON
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Backlog != 90 || !got.Syncing {
		t.Errorf("backlog/syncing = %d/%v, want 90/true (FULL_SYNC + backlog)", got.Backlog, got.Syncing)
	}
}

func TestIndexStatusCountUnavailableDegrades(t *testing.T) {
	env := newTestEnv(t)
	_ = createTestConnector(t, env)
	env.query.countErr = status.Error(codes.Unavailable, "vespa down")

	rec := env.do(http.MethodGet, "/v1/index/status", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	var got indexStatusJSON
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Indexed != -1 {
		t.Errorf("indexed = %d, want -1 (unknown when count is unavailable)", got.Indexed)
	}
	if got.Backlog != 0 {
		t.Errorf("backlog = %d, want 0 when indexed is unknown", got.Backlog)
	}
}

func TestReindexConnectorResetsSyncState(t *testing.T) {
	env := newTestEnv(t)
	id := createTestConnector(t, env)
	env.control.seedSyncState(testSubject, id, &controlplanev1.SyncState{
		ConnectorInstanceId: id,
		Cursor:              "cursor-abc",
		Phase:               controlplanev1.SyncPhase_INCREMENTAL,
		DocsEmitted:         500,
	})

	rec := env.do(http.MethodPost, "/v1/connectors/"+id+"/reindex", nil, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("reindex status %d, body %s", rec.Code, rec.Body.String())
	}
	st := env.control.storedSyncState(testSubject, id)
	if st == nil {
		t.Fatal("no sync state stored after reindex")
	}
	if st.GetCursor() != "" || st.GetPhase() != controlplanev1.SyncPhase_PENDING || st.GetDocsEmitted() != 0 {
		t.Errorf("reset state = cursor:%q phase:%v emitted:%d, want \"\"/PENDING/0",
			st.GetCursor(), st.GetPhase(), st.GetDocsEmitted())
	}
}

func TestReindexUnknownConnectorIs404(t *testing.T) {
	env := newTestEnv(t)
	rec := env.do(http.MethodPost, "/v1/connectors/00000000-0000-0000-0000-000000000000/reindex", nil, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("reindex unknown = %d, want 404", rec.Code)
	}
}

func TestReindexMethodNotAllowed(t *testing.T) {
	env := newTestEnv(t)
	id := createTestConnector(t, env)
	rec := env.do(http.MethodGet, "/v1/connectors/"+id+"/reindex", nil, nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET reindex = %d, want 405", rec.Code)
	}
}

// TestReindexTenantIsolation: one tenant cannot re-index another tenant's
// connector — the reset is scoped to the verified-token tenant.
func TestReindexTenantIsolation(t *testing.T) {
	env := newTestEnv(t)
	id := createTestConnector(t, env) // owned by the default tenant (testSubject)

	// A different tenant tries to re-index it.
	req := httptest.NewRequest(http.MethodPost, "/v1/connectors/"+id+"/reindex", nil)
	req.Header.Set("Authorization", env.bearerFor("other-tenant"))
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("cross-tenant reindex = %d, want 404", rec.Code)
	}
	if st := env.control.storedSyncState(testSubject, id); st != nil {
		t.Error("cross-tenant reindex mutated the owner's sync state")
	}
}
