package connectortest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/asker/asker/connectors/sdk"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

// notesConnector is a tiny in-process sdk.Connector used to exercise the
// contract harness end to end. It talks to a "notes" HTTP API: GET /notes for
// the paged backfill and GET /notes/changes for incremental sync. Its base_url
// comes from ConfigJSON, so RunConnectorContract can point it at a ReplayServer.
type notesConnector struct{}

const notesConnectorID = "notes"

func (notesConnector) Spec() sdk.Spec {
	return sdk.Spec{
		ID:           notesConnectorID,
		DisplayName:  "Toy Notes",
		AuthType:     sdk.AuthToken,
		ConfigSchema: json.RawMessage(`{"type":"object"}`),
	}
}

type notesConfig struct {
	BaseURL string `json:"base_url"`
}

func (c notesConnector) baseURL(cfg sdk.Config) (string, error) {
	var nc notesConfig
	if err := json.Unmarshal(cfg.ConfigJSON, &nc); err != nil {
		return "", fmt.Errorf("notes: bad config: %w", err)
	}
	if nc.BaseURL == "" {
		return "", fmt.Errorf("notes: base_url is required")
	}
	return nc.BaseURL, nil
}

func (c notesConnector) Validate(ctx context.Context, cfg sdk.Config) error {
	_, err := c.baseURL(cfg)
	return err
}

type wireNote struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Body  string `json:"body"`
	ETag  string `json:"etag"`
}

func (n wireNote) doc(tenant string) *askerv1.Document {
	return &askerv1.Document{
		TenantId:       tenant,
		ConnectorId:    notesConnectorID,
		SourceNativeId: n.ID,
		DocId:          sdk.DocID(notesConnectorID, n.ID),
		Type:           askerv1.DocType_FILE,
		Title:          n.Title,
		BodyText:       n.Body,
		VersionEtag:    n.ETag,
	}
}

func tombstone(tenant, id string) *askerv1.Document {
	return &askerv1.Document{
		TenantId:       tenant,
		ConnectorId:    notesConnectorID,
		SourceNativeId: id,
		DocId:          sdk.DocID(notesConnectorID, id),
		Type:           askerv1.DocType_FILE,
		VersionEtag:    "deleted",
		Tombstone:      &askerv1.Tombstone{Deleted: true},
	}
}

// FullSync pages GET /notes?page=N until next_page is empty, emitting a doc per
// note and checkpointing each page.
func (c notesConnector) FullSync(ctx context.Context, cfg sdk.Config, emit sdk.Emit) (sdk.Cursor, error) {
	base, err := c.baseURL(cfg)
	if err != nil {
		return "", err
	}
	tenant := string(cfg.Tenant.TenantID())
	page := "1"
	last := ""
	for {
		var resp struct {
			Notes    []wireNote `json:"notes"`
			NextPage string     `json:"next_page"`
		}
		if err := getJSON(ctx, base+"/notes?page="+url.QueryEscape(page), &resp); err != nil {
			return "", err
		}
		for _, n := range resp.Notes {
			if err := emit(ctx, n.doc(tenant)); err != nil {
				return "", err
			}
			last = n.ID
		}
		if err := cfg.Checkpoint(ctx, sdk.Cursor("cur-"+last)); err != nil {
			return "", err
		}
		if resp.NextPage == "" {
			break
		}
		page = resp.NextPage
	}
	return sdk.Cursor("cur-" + last), nil
}

// IncrementalSync fetches GET /notes/changes?since=<cur>, emitting an upsert per
// changed note and a tombstone per deleted id.
func (c notesConnector) IncrementalSync(ctx context.Context, cfg sdk.Config, cur sdk.Cursor, emit sdk.Emit) (sdk.Cursor, error) {
	base, err := c.baseURL(cfg)
	if err != nil {
		return "", err
	}
	tenant := string(cfg.Tenant.TenantID())
	var resp struct {
		Changed []wireNote `json:"changed"`
		Deleted []string   `json:"deleted"`
		Cursor  string     `json:"cursor"`
	}
	if err := getJSON(ctx, base+"/notes/changes?since="+url.QueryEscape(string(cur)), &resp); err != nil {
		return "", err
	}
	for _, n := range resp.Changed {
		if err := emit(ctx, n.doc(tenant)); err != nil {
			return "", err
		}
	}
	for _, id := range resp.Deleted {
		if err := emit(ctx, tombstone(tenant, id)); err != nil {
			return "", err
		}
	}
	return sdk.Cursor(resp.Cursor), nil
}

// HandleWebhook is notification-only: it validates the body is non-empty JSON
// and emits nothing (the hub triggers an incremental sync afterward).
func (c notesConnector) HandleWebhook(_ context.Context, _ sdk.Config, r *http.Request, _ sdk.Emit) error {
	if r == nil || r.Body == nil {
		return fmt.Errorf("notes: webhook has no body")
	}
	var probe map[string]any
	if err := json.NewDecoder(r.Body).Decode(&probe); err != nil {
		return fmt.Errorf("notes: webhook body is not JSON: %w", err)
	}
	return nil
}

// getJSON GETs url and decodes the JSON body into out using the default client
// (whose transport the test may override via http.DefaultTransport, or via a
// base_url pointing at a ReplayServer).
func getJSON(ctx context.Context, rawURL string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("notes: GET %s: status %d", rawURL, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
