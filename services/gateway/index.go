package main

import (
	"net/http"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	controlplanev1 "github.com/asker/asker/platform/proto/gen/go/asker/controlplane/v1"
	queryv1 "github.com/asker/asker/platform/proto/gen/go/asker/query/v1"
)

// Indexing progress + re-index (Settings "Indexing" card).
//
// "indexed" is the live count of searchable documents (from the query service /
// Vespa); "emitted" is how many the connectors have pulled from the sources
// (control-plane sync state). The gap is the pipeline backlog still flowing
// through ingest -> enrich -> index-writer. Re-index resets a connector's sync
// cursor so the scheduler re-runs a FULL sync (re-pull + re-index, idempotent).

// indexConnectorJSON is one connector's sync progress in the status response.
type indexConnectorJSON struct {
	ID                string `json:"id"`
	ConnectorID       string `json:"connector_id"`
	DisplayName       string `json:"display_name"`
	Phase             string `json:"phase"`
	DocsEmitted       int64  `json:"docs_emitted"`
	LastSyncCompleted string `json:"last_sync_completed"`
	LastError         string `json:"last_error"`
}

// indexStatusJSON is the GET /v1/index/status response.
type indexStatusJSON struct {
	Indexed    int64                `json:"indexed"`
	Emitted    int64                `json:"emitted"`
	Backlog    int64                `json:"backlog"`
	Syncing    bool                 `json:"syncing"`
	Connectors []indexConnectorJSON `json:"connectors"`
}

// handleIndexStatus serves GET /v1/index/status: the live indexing progress for
// the caller's tenant. The tenant rides the request context into both gRPC
// calls; neither reads it from the request.
func (d *deps) handleIndexStatus(w http.ResponseWriter, r *http.Request) {
	// Indexed (searchable) document count. A query-service blip degrades to -1
	// ("unknown") rather than failing the whole status — the connector progress
	// is still useful on its own.
	indexed := int64(-1)
	if cnt, err := d.query.Count(r.Context(), &queryv1.CountRequest{}); err != nil {
		d.logger.Debug("index status: count failed", "error", err)
	} else {
		indexed = cnt.GetIndexed()
	}

	instResp, err := d.control.ListConnectorInstances(r.Context(), &controlplanev1.ListConnectorInstancesRequest{})
	if err != nil {
		d.upstreamError(w, r, "ControlPlane.ListConnectorInstances", err)
		return
	}

	out := indexStatusJSON{Indexed: indexed, Connectors: []indexConnectorJSON{}}
	for _, inst := range instResp.GetInstances() {
		c := indexConnectorJSON{
			ID:          inst.GetId(),
			ConnectorID: inst.GetConnectorId(),
			DisplayName: inst.GetDisplayName(),
			Phase:       controlplanev1.SyncPhase_SYNC_PHASE_UNSPECIFIED.String(),
		}
		st, sErr := d.control.GetSyncState(r.Context(), &controlplanev1.GetSyncStateRequest{
			ConnectorInstanceId: inst.GetId(),
		})
		switch {
		case sErr == nil:
			s := st.GetState()
			c.Phase = s.GetPhase().String()
			c.DocsEmitted = s.GetDocsEmitted()
			c.LastSyncCompleted = rfc3339OrEmpty(s.GetLastSyncCompleted())
			c.LastError = s.GetLastError()
			out.Emitted += s.GetDocsEmitted()
			// PENDING / FULL_SYNC means a backfill is in progress (incl. a fresh
			// re-index); INCREMENTAL is steady state.
			if s.GetPhase() == controlplanev1.SyncPhase_PENDING ||
				s.GetPhase() == controlplanev1.SyncPhase_FULL_SYNC {
				out.Syncing = true
			}
		case status.Code(sErr) == codes.NotFound:
			// instance vanished between List and Get; report it as PENDING.
		default:
			d.upstreamError(w, r, "ControlPlane.GetSyncState", sErr)
			return
		}
		out.Connectors = append(out.Connectors, c)
	}

	if indexed >= 0 && out.Emitted > indexed {
		out.Backlog = out.Emitted - indexed
	}
	if out.Backlog > 0 {
		out.Syncing = true
	}
	writeJSON(w, http.StatusOK, out)
}

// handleReindexConnector serves POST /v1/connectors/{id}/reindex: reset the
// connector's sync cursor to "" and phase to PENDING (and the emitted counter /
// last error) so the connector-hub scheduler re-runs a FULL sync — re-pulling
// the source and re-indexing it (idempotent upserts). The reset is tenant-
// scoped: SetSyncState writes only an instance owned by the caller's tenant, so
// a foreign or unknown id is a 404, never a cross-tenant reset.
func (d *deps) handleReindexConnector(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_, err := d.control.SetSyncState(r.Context(), &controlplanev1.SetSyncStateRequest{
		State: &controlplanev1.SyncState{
			ConnectorInstanceId: id,
			Cursor:              "",
			Phase:               controlplanev1.SyncPhase_PENDING,
			DocsEmitted:         0,
			LastError:           "",
		},
	})
	if err != nil {
		d.upstreamError(w, r, "ControlPlane.SetSyncState", err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "reindexing"})
}
