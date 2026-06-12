package main

import (
	"encoding/json"
	"errors"
	"net/http"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	controlplanev1 "github.com/asker/asker/platform/proto/gen/go/asker/controlplane/v1"
)

// maxConnectorBodyBytes caps the JSON bodies of the connector management
// endpoints (the control-plane itself enforces tighter per-field limits).
const maxConnectorBodyBytes = 1 << 20

// protoJSON renders control-plane messages with the proto JSON field names
// (camelCase) and every field present, per the pinned REST contract.
var protoJSON = protojson.MarshalOptions{EmitUnpopulated: true}

// connectorEntry is one element of the GET /v1/connectors response.
type connectorEntry struct {
	Instance json.RawMessage `json:"instance"`
	Sync     json.RawMessage `json:"sync"`
}

// createConnectorBody is the POST /v1/connectors request body. Config is
// decoded as a map so anything but a JSON object is rejected.
type createConnectorBody struct {
	ConnectorID string         `json:"connector_id"`
	DisplayName string         `json:"display_name"`
	Config      map[string]any `json:"config"`
}

func (d *deps) handleCreateConnector(w http.ResponseWriter, r *http.Request) {
	var body createConnectorBody
	if !decodeJSONBody(w, r, &body) {
		return
	}
	if body.ConnectorID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "connector_id is required"})
		return
	}
	configJSON := []byte("{}")
	if body.Config != nil {
		var err error
		if configJSON, err = json.Marshal(body.Config); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "config must be a JSON object"})
			return
		}
	}

	// First connector creation is also first contact for a fresh tenant:
	// register it idempotently before touching tenant-scoped tables.
	if _, err := d.control.EnsureTenant(r.Context(), &controlplanev1.EnsureTenantRequest{}); err != nil {
		d.upstreamError(w, r, "ControlPlane.EnsureTenant", err)
		return
	}
	resp, err := d.control.CreateConnectorInstance(r.Context(), &controlplanev1.CreateConnectorInstanceRequest{
		ConnectorId: body.ConnectorID,
		DisplayName: body.DisplayName,
		ConfigJson:  configJSON,
	})
	if err != nil {
		d.upstreamError(w, r, "ControlPlane.CreateConnectorInstance", err)
		return
	}
	d.writeProtoJSON(w, http.StatusCreated, resp.GetInstance())
}

func (d *deps) handleListConnectors(w http.ResponseWriter, r *http.Request) {
	resp, err := d.control.ListConnectorInstances(r.Context(), &controlplanev1.ListConnectorInstancesRequest{})
	if err != nil {
		d.upstreamError(w, r, "ControlPlane.ListConnectorInstances", err)
		return
	}
	entries := make([]connectorEntry, 0, len(resp.GetInstances()))
	for _, inst := range resp.GetInstances() {
		instJSON, err := protoJSON.Marshal(inst)
		if err != nil {
			d.internalError(w, r, "marshal connector instance", err)
			return
		}
		// "sync" is null only if the instance vanished between List and Get
		// (the control-plane answers PENDING for never-synced instances).
		syncJSON := json.RawMessage(nil)
		st, err := d.control.GetSyncState(r.Context(), &controlplanev1.GetSyncStateRequest{
			ConnectorInstanceId: inst.GetId(),
		})
		switch {
		case err == nil:
			if syncJSON, err = protoJSON.Marshal(st.GetState()); err != nil {
				d.internalError(w, r, "marshal sync state", err)
				return
			}
		case status.Code(err) == codes.NotFound:
			// keep null
		default:
			d.upstreamError(w, r, "ControlPlane.GetSyncState", err)
			return
		}
		entries = append(entries, connectorEntry{Instance: instJSON, Sync: syncJSON})
	}
	writeJSON(w, http.StatusOK, entries)
}

func (d *deps) handleDeleteConnector(w http.ResponseWriter, r *http.Request) {
	_, err := d.control.DeleteConnectorInstance(r.Context(), &controlplanev1.DeleteConnectorInstanceRequest{
		Id: r.PathValue("id"),
	})
	if err != nil {
		d.upstreamError(w, r, "ControlPlane.DeleteConnectorInstance", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (d *deps) handlePutToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if !decodeJSONBody(w, r, &body) {
		return
	}
	if body.Token == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "token is required"})
		return
	}
	_, err := d.control.PutToken(r.Context(), &controlplanev1.PutTokenRequest{
		ConnectorInstanceId: r.PathValue("id"),
		Token:               []byte(body.Token),
	})
	if err != nil {
		d.upstreamError(w, r, "ControlPlane.PutToken", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// decodeJSONBody decodes a size-capped JSON request body into dst, writing a
// 400/413 and returning false on failure.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxConnectorBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
			return false
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return false
	}
	return true
}

func (d *deps) writeProtoJSON(w http.ResponseWriter, code int, m proto.Message) {
	b, err := protoJSON.Marshal(m)
	if err != nil {
		d.logger.Error("marshal proto response", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(append(b, '\n'))
}

func (d *deps) internalError(w http.ResponseWriter, r *http.Request, op string, err error) {
	d.logger.Error("internal error", "op", op, "path", r.URL.Path, "error", err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
}
