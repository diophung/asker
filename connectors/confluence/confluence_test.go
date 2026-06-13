package confluence

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/asker/asker/connectors/sdk"
	"github.com/asker/asker/connectors/sdk/connectortest"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
	"github.com/asker/asker/platform/tenancy"
)

const testTenant = "tenant-a"

// testConfig builds an sdk.Config pointing at baseURL with the DEV space scope.
func testConfig(t *testing.T, baseURL, token string) sdk.Config {
	t.Helper()
	tc, err := tenancy.FromClaims(map[string]any{"tenant_id": testTenant, "sub": "user-a"})
	if err != nil {
		t.Fatalf("tenancy.FromClaims: %v", err)
	}
	conf := map[string]string{"space_key": "DEV"}
	if baseURL != "" {
		conf["base_url"] = baseURL
	}
	raw, err := json.Marshal(conf)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	return sdk.Config{
		Tenant:     tc,
		InstanceID: "inst-1",
		ConfigJSON: raw,
		Token:      []byte(token),
		Checkpoint: sdk.NopCheckpoint,
	}
}

func TestSpec(t *testing.T) {
	spec := New().Spec()
	if spec.ID != "confluence" {
		t.Errorf("Spec.ID = %q, want %q", spec.ID, "confluence")
	}
	if spec.AuthType != sdk.AuthOAuth2 {
		t.Errorf("Spec.AuthType = %v, want AuthOAuth2", spec.AuthType)
	}
	if spec.SupportsWebhook {
		t.Errorf("Spec.SupportsWebhook = true, want false (Atlassian webhooks deferred)")
	}
	if !json.Valid(spec.ConfigSchema) {
		t.Errorf("Spec.ConfigSchema is not valid JSON")
	}
}

func TestParseConfig(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr bool
		base    string // expected resolvedBaseURL
	}{
		{name: "empty", raw: "", base: defaultBaseURL},
		{name: "only space", raw: `{"space_key":"DEV"}`, base: defaultBaseURL},
		{name: "explicit base trims slash", raw: `{"base_url":"https://acme.atlassian.net/wiki/"}`, base: "https://acme.atlassian.net/wiki"},
		{name: "not json", raw: `{`, wantErr: true},
		{name: "bad base scheme", raw: `{"base_url":"ftp://x"}`, wantErr: true},
		{name: "bad base no host", raw: `{"base_url":"http://"}`, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			conf, err := parseConfig([]byte(tc.raw))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseConfig(%q) = nil error, want error", tc.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseConfig(%q): unexpected error %v", tc.raw, err)
			}
			if got := conf.resolvedBaseURL(); got != tc.base {
				t.Errorf("resolvedBaseURL = %q, want %q", got, tc.base)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	c := New()

	// No token -> config-only validation succeeds.
	if err := c.Validate(context.Background(), testConfig(t, "http://example.invalid", "")); err != nil {
		t.Errorf("Validate without token: unexpected error %v", err)
	}

	// Bad config -> error regardless of token.
	bad := testConfig(t, "http://example.invalid", "tok")
	bad.ConfigJSON = json.RawMessage(`{"base_url":"ftp://nope"}`)
	if err := c.Validate(context.Background(), bad); err == nil {
		t.Errorf("Validate with bad base_url: want error, got nil")
	}

	// Token present -> one authed probe. First cassette interaction is 200
	// (success), the second is 401 (credential failure).
	cas, err := connectortest.LoadCassette("testdata/validate.json")
	if err != nil {
		t.Fatalf("LoadCassette: %v", err)
	}
	rs := connectortest.NewReplayServer(t, cas)

	if err := c.Validate(context.Background(), testConfig(t, rs.URL(), "good-token")); err != nil {
		t.Errorf("Validate success: unexpected error %v", err)
	}

	err = c.Validate(context.Background(), testConfig(t, rs.URL(), "bad-token"))
	if err == nil {
		t.Fatalf("Validate failure: want error, got nil")
	}
	if strings.Contains(err.Error(), "bad-token") {
		t.Errorf("Validate error leaked the token: %v", err)
	}
}

func TestHandleWebhookUnsupported(t *testing.T) {
	err := New().HandleWebhook(context.Background(), testConfig(t, "http://x", "tok"), nil, nil)
	if !errors.Is(err, sdk.ErrWebhookUnsupported) {
		t.Errorf("HandleWebhook err = %v, want ErrWebhookUnsupported", err)
	}
}

func TestContentDocument(t *testing.T) {
	p := &content{
		ID:     "100",
		Type:   "page",
		Status: "current",
		Title:  "Onboarding Guide",
		Body: body{Storage: storage{
			Value: "<p>Welcome to the team.</p><p>Read the <strong>handbook</strong> first.</p>",
		}},
		Version: version{
			Number: 3,
			When:   "2026-06-01T09:15:00.000Z",
			By:     user{AccountID: "acc-bob", DisplayName: "Bob Editor", Email: "bob@example.com"},
		},
		Space: space{Key: "DEV", Name: "Development"},
		History: history{
			CreatedBy:   user{AccountID: "acc-alice", DisplayName: "Alice Author", Email: "alice@example.com"},
			CreatedDate: "2026-05-20T08:00:00.000Z",
		},
		Links: links{WebUI: "/spaces/DEV/pages/100/Onboarding+Guide"},
	}

	doc, err := contentDocument(testTenant, p, "https://acme.atlassian.net/wiki")
	if err != nil {
		t.Fatalf("contentDocument: %v", err)
	}

	cfg := testConfig(t, "http://x", "tok")
	connectortest.ValidateDocument(t, cfg, doc)

	if doc.GetType() != askerv1.DocType_WIKI_PAGE {
		t.Errorf("Type = %v, want WIKI_PAGE", doc.GetType())
	}
	if doc.GetTitle() != "Onboarding Guide" {
		t.Errorf("Title = %q", doc.GetTitle())
	}
	if doc.GetVersionEtag() != "3" {
		t.Errorf("VersionEtag = %q, want 3", doc.GetVersionEtag())
	}
	wantBody := "Welcome to the team.\nRead the handbook first."
	if doc.GetBodyText() != wantBody {
		t.Errorf("BodyText = %q, want %q", doc.GetBodyText(), wantBody)
	}

	md := doc.GetMetadata()
	if md["page_id"] != "100" || md["space_key"] != "DEV" || md["version_number"] != "3" || md["status"] != "current" {
		t.Errorf("metadata wrong: %v", md)
	}
	if got, want := md["web_url"], "https://acme.atlassian.net/wiki/spaces/DEV/pages/100/Onboarding+Guide"; got != want {
		t.Errorf("web_url = %q, want %q", got, want)
	}

	parts := doc.GetParticipants()
	if len(parts) != 2 {
		t.Fatalf("participants = %d, want 2", len(parts))
	}
	if parts[0].GetRole() != "author" || parts[0].GetHandle() != "acc-alice" {
		t.Errorf("author participant wrong: %+v", parts[0])
	}
	if parts[1].GetRole() != "editor" || parts[1].GetEmail() != "bob@example.com" {
		t.Errorf("editor participant wrong: %+v", parts[1])
	}

	if doc.GetAcl() == nil || doc.GetAcl().GetIsPrivate() {
		t.Errorf("acl should be captured non-private: %+v", doc.GetAcl())
	}
	created := doc.GetTs().GetCreated().AsTime()
	if created.Format(time.RFC3339) != "2026-05-20T08:00:00Z" {
		t.Errorf("created = %v", created)
	}
	modified := doc.GetTs().GetModified().AsTime()
	if modified.Format(time.RFC3339) != "2026-06-01T09:15:00Z" {
		t.Errorf("modified = %v", modified)
	}
}

func TestContentDocumentSameAuthorEditor(t *testing.T) {
	p := &content{
		ID:      "200",
		Title:   "Solo",
		Version: version{Number: 1, By: user{AccountID: "acc-x", DisplayName: "X"}},
		History: history{CreatedBy: user{AccountID: "acc-x", DisplayName: "X"}},
	}
	doc, err := contentDocument(testTenant, p, "")
	if err != nil {
		t.Fatalf("contentDocument: %v", err)
	}
	if n := len(doc.GetParticipants()); n != 1 {
		t.Errorf("participants = %d, want 1 (author==editor collapsed)", n)
	}
}

func TestContentDocumentNoID(t *testing.T) {
	if _, err := contentDocument(testTenant, &content{}, ""); err == nil {
		t.Errorf("contentDocument with no id: want error")
	}
}

func TestTombstoneDocument(t *testing.T) {
	fixed := time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC)
	c := New(withClock(func() time.Time { return fixed }), WithLogger(slog.New(slog.DiscardHandler))).(*Connector)

	tomb := c.tombstoneDocument(testTenant, "103", "3")
	cfg := testConfig(t, "http://x", "tok")
	connectortest.ValidateDocument(t, cfg, tomb)

	if !tomb.GetTombstone().GetDeleted() {
		t.Errorf("tombstone.deleted = false, want true")
	}
	if tomb.GetBodyText() != "" {
		t.Errorf("tombstone carries body")
	}
	if !tomb.GetTombstone().GetDeletedAt().AsTime().Equal(fixed) {
		t.Errorf("deleted_at = %v, want %v", tomb.GetTombstone().GetDeletedAt().AsTime(), fixed)
	}
	if tomb.GetVersionEtag() != "3" {
		t.Errorf("etag = %q, want 3", tomb.GetVersionEtag())
	}

	// Empty etag falls back to a stable non-empty marker.
	tomb2 := c.tombstoneDocument(testTenant, "104", "")
	if tomb2.GetVersionEtag() == "" {
		t.Errorf("empty-etag tombstone must still set version_etag")
	}
}

func TestCursorRoundTrip(t *testing.T) {
	tm := time.Date(2026, 6, 3, 16, 45, 0, 0, time.UTC)
	cur := renderCursor(tm)
	if string(cur) != "2026-06-03 16:45" {
		t.Fatalf("renderCursor = %q", cur)
	}
	got, ok := parseCursor(cur)
	if !ok || !got.Equal(tm) {
		t.Errorf("parseCursor(%q) = %v,%v want %v,true", cur, got, ok, tm)
	}

	// Empty cursor -> zero time, ok.
	if z, ok := parseCursor(""); !ok || !z.IsZero() {
		t.Errorf("parseCursor(\"\") = %v,%v want zero,true", z, ok)
	}
	// Garbage cursor -> not ok (never an error; never ErrCursorExpired).
	if _, ok := parseCursor("not-a-time"); ok {
		t.Errorf("parseCursor(garbage) ok=true, want false")
	}
}

func TestBuildCQL(t *testing.T) {
	since := time.Date(2026, 6, 3, 16, 45, 0, 0, time.UTC)
	got := buildCQL("DEV", "current", since)
	want := `type=page and status=current and space="DEV" and lastModified>="2026-06-03 16:45" order by lastModified asc`
	if got != want {
		t.Errorf("buildCQL = %q\nwant %q", got, want)
	}
	// Zero time omits the lastModified clause; empty space omits the space clause.
	got = buildCQL("", "trashed", time.Time{})
	want = `type=page and status=trashed order by lastModified asc`
	if got != want {
		t.Errorf("buildCQL(zero) = %q\nwant %q", got, want)
	}
}

func TestStripStorageXHTML(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"empty", "", ""},
		{"paragraphs", "<p>One.</p><p>Two.</p>", "One.\nTwo."},
		{"entities", "<p>Terms &amp; defs &lt;here&gt;</p>", "Terms & defs <here>"},
		{
			"macro params dropped",
			`<ac:structured-macro ac:name="info"><ac:parameter ac:name="title">Note</ac:parameter><ac:rich-text-body><p>Escalate.</p></ac:rich-text-body></ac:structured-macro>`,
			"Escalate.",
		},
		{"list", "<ul><li>a</li><li>b</li></ul>", "a\nb"},
		{"unterminated tag", "good<p>", "good"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripStorageXHTML(tc.in); got != tc.want {
				t.Errorf("stripStorageXHTML(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestHasNextPage(t *testing.T) {
	full := &contentPage{Limit: 2, Results: make([]content, 2)}
	if !hasNextPage(full) {
		t.Errorf("full page (size==limit) should signal more")
	}
	partial := &contentPage{Limit: 50, Results: make([]content, 2)}
	if hasNextPage(partial) {
		t.Errorf("partial page should signal done")
	}
	withNext := &contentPage{Limit: 50, Results: make([]content, 2)}
	withNext.Links.Next = "/rest/api/content?start=50"
	if !hasNextPage(withNext) {
		t.Errorf("_links.next should signal more")
	}
	empty := &contentPage{Limit: 50}
	if hasNextPage(empty) {
		t.Errorf("empty page should signal done")
	}
}
