package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

// Feeder is the I/O boundary: GenerateTenant produces GenDocs, Feeder loads
// them into a target. The generation layer never touches the network; tests
// use fakeFeeder, real runs use the Vespa-direct or gateway-upload feeder.
type Feeder interface {
	// Feed loads one document. It must be safe for concurrent use (the runner
	// fans out across --concurrency goroutines).
	Feed(ctx context.Context, d GenDoc) error
	// Name identifies the target for the summary line.
	Name() string
}

// fakeFeeder records what it was asked to feed; used by tests (--dry-run is
// handled separately and never calls Feed at all). Concurrency-safe.
type fakeFeeder struct {
	mu   sync.Mutex
	docs []GenDoc
	fail func(GenDoc) error // optional per-doc failure injection
}

func newFakeFeeder() *fakeFeeder { return &fakeFeeder{} }

func (f *fakeFeeder) Feed(_ context.Context, d GenDoc) error {
	if f.fail != nil {
		if err := f.fail(d); err != nil {
			return err
		}
	}
	f.mu.Lock()
	f.docs = append(f.docs, d)
	f.mu.Unlock()
	return nil
}

func (f *fakeFeeder) Name() string { return "fake" }

// count returns how many docs were fed (concurrency-safe snapshot).
func (f *fakeFeeder) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.docs)
}

// fed returns a snapshot copy of the fed docs (concurrency-safe).
func (f *fakeFeeder) fed() []GenDoc {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]GenDoc, len(f.docs))
	copy(out, f.docs)
	return out
}

// --- Vespa-direct feeder -----------------------------------------------------

// vespaFeeder POSTs each document straight to Vespa's document/v1 API into the
// tenant's streaming group, byte-compatible with the index-writer's feed shape
// (services/index-writer/writer.go vespaFields). This bypasses the Kafka
// pipeline for raw index-fill at scale; the gateway-upload feeder exercises the
// full pipeline instead.
type vespaFeeder struct {
	baseURL string // no trailing slash
	client  *http.Client
}

func newVespaFeeder(baseURL string, client *http.Client) *vespaFeeder {
	return &vespaFeeder{baseURL: strings.TrimRight(baseURL, "/"), client: client}
}

func (vf *vespaFeeder) Name() string { return "vespa-direct(" + vf.baseURL + ")" }

func (vf *vespaFeeder) Feed(ctx context.Context, d GenDoc) error {
	fields := vespaFieldsFor(d.Doc)
	body, err := json.Marshal(map[string]any{"fields": fields})
	if err != nil {
		return fmt.Errorf("synthgen: marshal feed for %s: %w", d.Doc.GetDocId(), err)
	}
	// document/v1 path: group/<tenant_id> sets g=<tenant_id> (vespa/README.md).
	docURL := vf.baseURL + "/document/v1/asker/doc/group/" +
		url.PathEscape(d.Doc.GetTenantId()) + "/" + url.PathEscape(d.Doc.GetDocId())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, docURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("synthgen: build feed request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := vf.client.Do(req)
	if err != nil {
		return fmt.Errorf("synthgen: feed %s: %w", d.Doc.GetDocId(), err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("synthgen: feed %s: vespa status %d: %s",
			d.Doc.GetDocId(), resp.StatusCode, snippet)
	}
	return nil
}

// vespaFields is the document/v1 feed shape, mirroring the index-writer's
// vespaFields exactly (the M1 text fields the load suite queries). Media-only
// fields are left out: synthgen feeds text-searchable content, and absent
// fields read as zero/empty in the schema (vespa/README.md).
type vespaFieldsJSON struct {
	DocID        string   `json:"doc_id"`
	ConnectorID  string   `json:"connector_id"`
	Type         string   `json:"type"`
	Title        string   `json:"title"`
	Body         string   `json:"body"`
	Participants []string `json:"participants,omitempty"`
	MetadataJSON string   `json:"metadata_json"`
	CreatedAt    int64    `json:"created_at"`
	ModifiedAt   int64    `json:"modified_at"`
	VersionEtag  string   `json:"version_etag"`
}

// vespaFieldsFor builds the feed fields for a generated doc. metadata_json is
// the sorted-key JSON of Document.metadata (the index-writer does the same), so
// result cards and metadata filters see identical content to the real pipeline.
func vespaFieldsFor(doc *askerv1.Document) vespaFieldsJSON {
	meta := doc.GetMetadata()
	if meta == nil {
		meta = map[string]string{}
	}
	metaJSON, _ := json.Marshal(meta) // map keys marshal sorted => deterministic
	created := doc.GetTs().GetCreated().GetSeconds()
	modified := created
	if m := doc.GetTs().GetModified(); m != nil && m.GetSeconds() != 0 {
		modified = m.GetSeconds()
	}
	return vespaFieldsJSON{
		DocID:        doc.GetDocId(),
		ConnectorID:  doc.GetConnectorId(),
		Type:         doc.GetType().String(),
		Title:        doc.GetTitle(),
		Body:         doc.GetBodyText(),
		Participants: participantStrings(doc.GetParticipants()),
		MetadataJSON: string(metaJSON),
		CreatedAt:    created,
		ModifiedAt:   modified,
		VersionEtag:  doc.GetVersionEtag(),
	}
}

// participantStrings renders participants as "Name <email>" (or bare email),
// matching the index-writer's rendering so participant= filters behave the same.
func participantStrings(ps []*askerv1.Participant) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		name, email := p.GetName(), p.GetEmail()
		switch {
		case name != "" && email != "":
			out = append(out, name+" <"+email+">")
		case email != "":
			out = append(out, email)
		case name != "":
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// --- Gateway-upload feeder ---------------------------------------------------

// gatewayFeeder posts each (textual) document to the gateway's authed
// /v1/upload endpoint as a multipart file, exercising the FULL async pipeline
// (connector-hub -> Kafka -> ingest -> enrich -> index). The bearer token is a
// real OIDC access token for ONE user, so every uploaded doc lands in that
// user's tenant (tenant_id derives from the token, ADR-002 — never from the
// body). This means the gateway-upload target is single-tenant by construction;
// multi-tenant fan-out uses vespa-direct. Bodies become the file content; the
// generated tenant id / participants are advisory only here.
type gatewayFeeder struct {
	baseURL string
	token   string
	client  *http.Client
	// fed counts accepted uploads, for diagnostics.
	fed atomic.Int64
}

func newGatewayFeeder(baseURL, token string, client *http.Client) *gatewayFeeder {
	return &gatewayFeeder{baseURL: strings.TrimRight(baseURL, "/"), token: token, client: client}
}

func (gf *gatewayFeeder) Name() string { return "gateway-upload(" + gf.baseURL + ")" }

func (gf *gatewayFeeder) Feed(ctx context.Context, d GenDoc) error {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	// The file content carries title + body so the rare token / isolation
	// marker remain searchable after the pipeline indexes it.
	content := d.Doc.GetTitle() + "\n\n" + d.Doc.GetBodyText() + "\n"
	fw, err := mw.CreateFormFile("file", d.Doc.GetDocId()+".txt")
	if err != nil {
		return fmt.Errorf("synthgen: multipart file: %w", err)
	}
	if _, err := io.WriteString(fw, content); err != nil {
		return fmt.Errorf("synthgen: write content: %w", err)
	}
	if err := mw.WriteField("title", d.Doc.GetTitle()); err != nil {
		return fmt.Errorf("synthgen: title field: %w", err)
	}
	if err := mw.Close(); err != nil {
		return fmt.Errorf("synthgen: close multipart: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gf.baseURL+"/v1/upload", &buf)
	if err != nil {
		return fmt.Errorf("synthgen: build upload request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+gf.token)
	resp, err := gf.client.Do(req)
	if err != nil {
		return fmt.Errorf("synthgen: upload %s: %w", d.Doc.GetDocId(), err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("synthgen: upload %s: gateway status %d: %s",
			d.Doc.GetDocId(), resp.StatusCode, snippet)
	}
	gf.fed.Add(1)
	return nil
}

// newHTTPClient returns the shared HTTP client used by the network feeders, with
// a per-request timeout. Pooled connections matter at high --concurrency.
func newHTTPClient(timeout time.Duration, maxConns int) *http.Client {
	tr := &http.Transport{
		MaxIdleConns:        maxConns * 2,
		MaxIdleConnsPerHost: maxConns * 2,
		MaxConnsPerHost:     maxConns * 2,
		IdleConnTimeout:     90 * time.Second,
	}
	return &http.Client{Timeout: timeout, Transport: tr}
}

// approxFeedBytes estimates the on-the-wire JSON size of a Vespa feed for one
// doc; used both for the dry-run byte estimate and the live byte counter.
func approxFeedBytes(doc *askerv1.Document) int {
	fields := vespaFieldsFor(doc)
	body, err := json.Marshal(map[string]any{"fields": fields})
	if err != nil {
		return len(doc.GetTitle()) + len(doc.GetBodyText())
	}
	return len(body)
}
