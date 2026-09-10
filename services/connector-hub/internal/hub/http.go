package hub

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/asker/asker/connectors/sdk"
	controlplanev1 "github.com/asker/asker/platform/proto/gen/go/asker/controlplane/v1"
	"github.com/asker/asker/platform/telemetry"
	"github.com/asker/asker/platform/tenancy"
)

// tenantHeader is the internal HTTP tenant hop header (ADR-009). It is
// trusted only because the sender (the gateway) derived it from a VERIFIED
// JWT and only trusted services share the internal network; the value is
// still re-validated via tenancy.FromHeaderValue, fail closed.
const tenantHeader = "x-asker-tenant"

// maxUploadBytes bounds /upload multipart bodies (32 MiB, M1 contract).
const maxUploadBytes = 32 << 20

// httpAPI serves the hub's :9300 surface: webhook receiver, direct upload,
// the per-tenant sync-status listing the gateway proxies, and the internal-
// only media decrypt/encrypt hop (ADR-013).
type httpAPI struct {
	cp         controlplanev1.ControlPlaneServiceClient
	registry   *sdk.Registry
	sched      *scheduler
	emit       *emitter
	upload     UploadFunc
	mediaBlobs MediaBlobStore
	logger     *slog.Logger
}

func (h *httpAPI) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhooks/{connectorID}/{instanceID}", h.handleWebhook)
	mux.HandleFunc("POST /upload", h.handleUpload)
	mux.HandleFunc("GET /v1/sync-status", h.handleSyncStatus)
	// Internal-only (ADR-009): no host port is published for :9300, only the
	// compose/cluster network reaches these; M4 adds mTLS/NetworkPolicy.
	mux.HandleFunc("GET /internal/media", h.handleMediaGet)
	mux.HandleFunc("PUT /internal/media", h.handleMediaPut)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	})
	// Security headers outermost, so EVERY hub response carries them —
	// including the 400/401/404/503 branches of handleMediaGet, which return
	// before it reaches its own setMediaSecurityHeaders call, and the "/" 404.
	// Handler-level headers are exactly the pattern that misses error paths.
	return securityHeaders(telemetry.HTTPMiddleware(serviceName)(mux))
}

// handleWebhook routes a source-originated push notification to the matched
// connector instance and, when the connector accepts it, triggers an
// immediate incremental sync.
//
// M2 HARDENING: this endpoint is deliberately unauthenticated in M1 — the
// dev source (fake-gmail, ADR-008) cannot sign requests. Real Gmail push
// delivers OIDC JWTs on its webhook calls; verification lands with the real
// Gmail connector in M2. Until then the endpoint must not be exposed outside
// the compose network, and the connector's own HandleWebhook validation is
// the only payload check.
func (h *httpAPI) handleWebhook(w http.ResponseWriter, r *http.Request) {
	connectorID := r.PathValue("connectorID")
	instanceID := r.PathValue("instanceID")

	snap, ok := h.sched.lookup(instanceID)
	if !ok || snap.inst.GetConnectorId() != connectorID {
		// Unknown instance and foreign-connector instance look identical.
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown connector instance"})
		return
	}
	conn, ok := h.registry.Get(connectorID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown connector"})
		return
	}

	tctx := tenancy.WithContext(r.Context(), snap.tenant)
	cfg, err := h.sched.buildConfig(tctx, snap, sdk.NopCheckpoint)
	if err != nil {
		h.logger.ErrorContext(tctx, "webhook config build failed",
			"instance_id", instanceID, "connector_id", connectorID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "webhook processing failed"})
		return
	}

	err = conn.HandleWebhook(tctx, cfg, r, sdk.Emit(h.emit.emitFor(snap.tenant, connectorID, nil)))
	switch {
	case err == nil:
		h.sched.trigger(instanceID)
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
	case errors.Is(err, sdk.ErrWebhookUnsupported):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "connector does not support webhooks"})
	default:
		h.logger.ErrorContext(tctx, "webhook handling failed",
			"instance_id", instanceID, "connector_id", connectorID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "webhook processing failed"})
	}
}

// handleUpload is the gateway->hub internal hop for direct file uploads:
// tenant from x-asker-tenant (fail closed), multipart "file" (+ optional
// "title"), blob storage + Document construction delegated to the upload
// connector helper, then the document enters the pipeline through the same
// emit chokepoint as every other connector.
func (h *httpAPI) handleUpload(w http.ResponseWriter, r *http.Request) {
	tc, ok := h.tenantFromHeader(w, r)
	if !ok {
		return
	}
	tctx := tenancy.WithContext(r.Context(), tc)

	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
		code := http.StatusBadRequest
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			code = http.StatusRequestEntityTooLarge
		}
		writeJSON(w, code, map[string]string{"error": "invalid multipart body"})
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": `multipart field "file" is required`})
		return
	}
	defer func() { _ = file.Close() }()
	title := r.FormValue("title")

	doc, err := h.upload(tctx, tc, file, header.Filename, title, header.Header.Get("Content-Type"), header.Size)
	if err != nil {
		h.logger.ErrorContext(tctx, "upload failed", "tenant", tc.TenantID(), "filename", header.Filename, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "upload failed"})
		return
	}

	if err := h.emit.emitFor(tc, "upload", nil)(tctx, doc); err != nil {
		h.logger.ErrorContext(tctx, "upload emit failed", "tenant", tc.TenantID(), "doc_id", doc.GetDocId(), "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "upload failed"})
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]string{"doc_id": doc.GetDocId()})
}

// instanceStatus is one row of the sync-status listing: the instance plus
// its sync state, the shape the gateway inlines into GET /v1/connectors.
type instanceStatus struct {
	Instance instanceJSON `json:"instance"`
	Sync     syncJSON     `json:"sync"`
}

type instanceJSON struct {
	ID          string          `json:"id"`
	ConnectorID string          `json:"connector_id"`
	DisplayName string          `json:"display_name"`
	Config      json.RawMessage `json:"config"`
	Status      string          `json:"status"`
	Created     time.Time       `json:"created"`
	Updated     time.Time       `json:"updated"`
}

type syncJSON struct {
	Phase             string     `json:"phase"`
	LastSyncStarted   *time.Time `json:"last_sync_started,omitempty"`
	LastSyncCompleted *time.Time `json:"last_sync_completed,omitempty"`
	LastError         string     `json:"last_error,omitempty"`
	DocsEmitted       int64      `json:"docs_emitted"`
}

// handleSyncStatus lists the calling tenant's connector instances with their
// sync states (the gateway proxies this for the UI and e2e suite).
func (h *httpAPI) handleSyncStatus(w http.ResponseWriter, r *http.Request) {
	tc, ok := h.tenantFromHeader(w, r)
	if !ok {
		return
	}
	tctx := tenancy.WithContext(r.Context(), tc)

	list, err := h.cp.ListConnectorInstances(tctx, &controlplanev1.ListConnectorInstancesRequest{})
	if err != nil {
		h.logger.ErrorContext(tctx, "list instances failed", "tenant", tc.TenantID(), "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "control plane unavailable"})
		return
	}

	out := make([]instanceStatus, 0, len(list.GetInstances()))
	for _, inst := range list.GetInstances() {
		st, err := h.cp.GetSyncState(tctx, &controlplanev1.GetSyncStateRequest{ConnectorInstanceId: inst.GetId()})
		if err != nil {
			if status.Code(err) == codes.NotFound {
				continue // deleted between the two calls
			}
			h.logger.ErrorContext(tctx, "get sync state failed",
				"tenant", tc.TenantID(), "instance_id", inst.GetId(), "error", err)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "control plane unavailable"})
			return
		}
		out = append(out, instanceStatus{
			Instance: instanceToJSON(inst),
			Sync:     syncStateToJSON(st.GetState()),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"instances": out})
}

// tenantFromHeader re-validates the internal tenant hop header, writing a
// fail-closed 401 when it is absent or invalid.
func (h *httpAPI) tenantFromHeader(w http.ResponseWriter, r *http.Request) (tenancy.Context, bool) {
	tc, err := tenancy.FromHeaderValue(r.Header.Get(tenantHeader))
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing or invalid tenant"})
		return tenancy.Context{}, false
	}
	return tc, true
}

func instanceToJSON(inst *controlplanev1.ConnectorInstance) instanceJSON {
	cfg := inst.GetConfigJson()
	if len(cfg) == 0 || !json.Valid(cfg) {
		cfg = []byte("{}")
	}
	return instanceJSON{
		ID:          inst.GetId(),
		ConnectorID: inst.GetConnectorId(),
		DisplayName: inst.GetDisplayName(),
		Config:      json.RawMessage(cfg),
		Status:      inst.GetStatus().String(),
		Created:     inst.GetCreated().AsTime(),
		Updated:     inst.GetUpdated().AsTime(),
	}
}

func syncStateToJSON(st *controlplanev1.SyncState) syncJSON {
	out := syncJSON{
		Phase:       st.GetPhase().String(),
		LastError:   st.GetLastError(),
		DocsEmitted: st.GetDocsEmitted(),
	}
	if ts := st.GetLastSyncStarted(); ts != nil {
		t := ts.AsTime()
		out.LastSyncStarted = &t
	}
	if ts := st.GetLastSyncCompleted(); ts != nil {
		t := ts.AsTime()
		out.LastSyncCompleted = &t
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// A write error means the client went away; nothing useful remains.
	_ = json.NewEncoder(w).Encode(body)
}
