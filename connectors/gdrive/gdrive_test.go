package gdrive

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/asker/asker/connectors/sdk"
	"github.com/asker/asker/connectors/sdk/connectortest"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

// fixedClock returns a deterministic clock for tombstone timestamps.
func fixedClock() func() time.Time {
	t := time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC)
	return func() time.Time { return t }
}

func newTestConnector() *Connector {
	return New(withClock(fixedClock())).(*Connector)
}

func TestSpec(t *testing.T) {
	t.Parallel()
	c := New()
	connectortest.RunSpecChecks(t, c)

	spec := c.Spec()
	if spec.ID != "gdrive" {
		t.Errorf("spec.ID = %q, want %q", spec.ID, "gdrive")
	}
	if spec.AuthType != sdk.AuthOAuth2 {
		t.Errorf("spec.AuthType = %v, want AuthOAuth2", spec.AuthType)
	}
	if spec.SupportsWebhook {
		t.Error("spec.SupportsWebhook = true, want false (Drive push is deferred)")
	}
	if !json.Valid(spec.ConfigSchema) {
		t.Error("spec.ConfigSchema is not valid JSON")
	}
}

func TestParseConfig(t *testing.T) {
	t.Parallel()
	t.Run("empty is valid", func(t *testing.T) {
		t.Parallel()
		if _, err := parseConfig(nil); err != nil {
			t.Fatalf("parseConfig(nil) = %v, want nil", err)
		}
	})
	t.Run("valid base_url", func(t *testing.T) {
		t.Parallel()
		conf, err := parseConfig([]byte(`{"base_url":"https://drive.example.com/v3"}`))
		if err != nil {
			t.Fatalf("parseConfig: %v", err)
		}
		if conf.BaseURL != "https://drive.example.com/v3" {
			t.Errorf("BaseURL = %q", conf.BaseURL)
		}
	})
	t.Run("errors", func(t *testing.T) {
		t.Parallel()
		for name, raw := range map[string]string{
			"not json":     "{",
			"bad base_url": `{"base_url":"::not-a-url"}`,
			"ftp base_url": `{"base_url":"ftp://host"}`,
		} {
			if _, err := parseConfig([]byte(raw)); err == nil {
				t.Errorf("parseConfig accepted %s config %q", name, raw)
			}
		}
	})
}

func TestParseCursor(t *testing.T) {
	t.Parallel()
	t.Run("steady state", func(t *testing.T) {
		t.Parallel()
		st, err := parseCursor("page:abc123")
		if err != nil {
			t.Fatalf("parseCursor: %v", err)
		}
		if st.backfill || st.changesToken != "abc123" {
			t.Errorf("state = %+v", st)
		}
	})
	t.Run("backfill", func(t *testing.T) {
		t.Parallel()
		st, err := parseCursor("start:tok|files:page9")
		if err != nil {
			t.Fatalf("parseCursor: %v", err)
		}
		if !st.backfill || st.changesToken != "tok" || st.filesPageToken != "page9" {
			t.Errorf("state = %+v", st)
		}
	})
	t.Run("round-trips", func(t *testing.T) {
		t.Parallel()
		if got, _ := parseCursor(incrementalCursor("X")); got.changesToken != "X" {
			t.Errorf("incremental round-trip lost the token: %+v", got)
		}
		if got, _ := parseCursor(backfillCursor("S", "P")); !got.backfill || got.changesToken != "S" || got.filesPageToken != "P" {
			t.Errorf("backfill round-trip mismatch: %+v", got)
		}
	})
	t.Run("rejects garbage", func(t *testing.T) {
		t.Parallel()
		for _, bad := range []string{"", "history:5", "page:", "start:|files:x", "start:s|files:", "start:no-sep"} {
			if _, err := parseCursor(sdk.Cursor(bad)); err == nil {
				t.Errorf("parseCursor(%q) = nil error, want error", bad)
			}
		}
	})
}

func TestAclFromPermissions(t *testing.T) {
	t.Parallel()

	t.Run("private when only the owner has access", func(t *testing.T) {
		t.Parallel()
		f := &driveFile{Owners: []driveUser{{EmailAddress: "alice@example.com"}}}
		acl := aclFromPermissions(f, []*permission{
			{Type: "user", Role: "owner", EmailAddress: "alice@example.com"},
		})
		if !acl.GetIsPrivate() {
			t.Error("is_private = false, want true for an owner-only file")
		}
		if got := acl.GetAllowedPrincipals(); len(got) != 1 || got[0] != "alice@example.com" {
			t.Errorf("allowed_principals = %v, want [alice@example.com]", got)
		}
	})

	t.Run("shared with a user, domain, and anyone", func(t *testing.T) {
		t.Parallel()
		f := &driveFile{Owners: []driveUser{{EmailAddress: "alice@example.com"}}}
		acl := aclFromPermissions(f, []*permission{
			{Type: "user", Role: "owner", EmailAddress: "alice@example.com"},
			{Type: "user", Role: "writer", EmailAddress: "bob@example.com"},
			{Type: "group", Role: "reader", EmailAddress: "team@example.com"},
			{Type: "domain", Role: "reader", Domain: "example.com"},
			{Type: "anyone", Role: "reader"},
		})
		if acl.GetIsPrivate() {
			t.Error("is_private = true, want false for a shared file")
		}
		want := []string{"alice@example.com", "anyone", "bob@example.com", "domain:example.com", "team@example.com"}
		got := acl.GetAllowedPrincipals()
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("allowed_principals = %v, want %v (sorted, de-duped)", got, want)
		}
	})

	t.Run("ignores deleted permissions and dedups", func(t *testing.T) {
		t.Parallel()
		f := &driveFile{}
		acl := aclFromPermissions(f, []*permission{
			{Type: "user", EmailAddress: "x@example.com"},
			{Type: "user", EmailAddress: "x@example.com"},
			{Type: "group", EmailAddress: "g@example.com", Deleted: true},
		})
		if got := acl.GetAllowedPrincipals(); len(got) != 1 || got[0] != "x@example.com" {
			t.Errorf("allowed_principals = %v, want [x@example.com]", got)
		}
		// A non-owner user permission marks the file as shared.
		if acl.GetIsPrivate() {
			t.Error("is_private = true, want false (a non-owner user has access)")
		}
	})
}

func TestFileDocumentMapping(t *testing.T) {
	t.Parallel()
	f := &driveFile{
		ID:           "abc",
		Name:         "Plan",
		MimeType:     "application/vnd.google-apps.document",
		WebViewLink:  "https://docs/abc",
		Parents:      []string{"p1", "p2"},
		Size:         "10",
		Version:      "5",
		CreatedTime:  "2026-01-01T00:00:00Z",
		ModifiedTime: "2026-01-02T00:00:00Z",
		Owners:       []driveUser{{DisplayName: "Alice", EmailAddress: "alice@example.com"}},
	}
	acl := &askerv1.AclInfo{AllowedPrincipals: []string{"alice@example.com"}, IsPrivate: true}
	doc := fileDocument("tenant-x", f, "body text", acl)

	if doc.GetType() != askerv1.DocType_FILE {
		t.Errorf("type = %v, want FILE", doc.GetType())
	}
	if doc.GetDocId() != sdk.DocID(connectorID, "abc") {
		t.Errorf("doc_id mismatch")
	}
	if doc.GetVersionEtag() != "5" {
		t.Errorf("version_etag = %q, want 5", doc.GetVersionEtag())
	}
	if doc.GetMetadata()["web_view_link"] != "https://docs/abc" {
		t.Errorf("web_view_link metadata missing: %v", doc.GetMetadata())
	}
	if doc.GetMetadata()["parents"] != "p1,p2" {
		t.Errorf("parents metadata = %q, want p1,p2", doc.GetMetadata()["parents"])
	}
	if len(doc.GetParticipants()) != 1 || doc.GetParticipants()[0].GetRole() != "owner" {
		t.Errorf("participants = %v", doc.GetParticipants())
	}
	if doc.GetTs().GetCreated() == nil || doc.GetTs().GetModified() == nil {
		t.Error("timestamps not populated")
	}
	if doc.GetAcl() == nil || !doc.GetAcl().GetIsPrivate() {
		t.Error("acl not carried through")
	}
}

func TestVersionEtagFallback(t *testing.T) {
	t.Parallel()
	// No version → sha256(modifiedTime|md5).
	a := versionEtag(&driveFile{ModifiedTime: "2026-01-01T00:00:00Z", Md5Checksum: "abc"})
	b := versionEtag(&driveFile{ModifiedTime: "2026-01-01T00:00:00Z", Md5Checksum: "abc"})
	if a == "" || a != b {
		t.Errorf("etag fallback not deterministic: %q vs %q", a, b)
	}
	c := versionEtag(&driveFile{ModifiedTime: "2026-01-02T00:00:00Z", Md5Checksum: "abc"})
	if a == c {
		t.Error("etag did not change when modifiedTime changed")
	}
	// No metadata at all still yields a non-empty etag.
	if versionEtag(&driveFile{ID: "only-id"}) == "" {
		t.Error("etag empty for an id-only file")
	}
}

func TestTombstoneDocument(t *testing.T) {
	t.Parallel()
	c := newTestConnector()
	tomb := c.tombstoneDocument("tenant-x", "gone", "2026-06-10T15:00:00Z")
	if !tomb.GetTombstone().GetDeleted() {
		t.Error("tombstone not marked deleted")
	}
	if tomb.GetBodyText() != "" || len(tomb.GetChunks()) != 0 {
		t.Error("tombstone carries a body")
	}
	if tomb.GetDocId() != sdk.DocID(connectorID, "gone") {
		t.Error("tombstone doc_id mismatch")
	}
	if tomb.GetVersionEtag() != "2026-06-10T15:00:00Z" {
		t.Errorf("tombstone etag = %q", tomb.GetVersionEtag())
	}
	if tomb.GetTombstone().GetDeletedAt() == nil {
		t.Error("tombstone deleted_at unset")
	}
	// Empty etag falls back to a monotonic value.
	if c.tombstoneDocument("t", "x", "").GetVersionEtag() == "" {
		t.Error("tombstone etag empty with no source etag")
	}
}

func TestMimeClassifiers(t *testing.T) {
	t.Parallel()
	if !isGoogleDoc("application/vnd.google-apps.document") {
		t.Error("Google doc not classified")
	}
	if !isFolder(googleFolderMime) {
		t.Error("folder not classified")
	}
	if isGoogleDoc("application/pdf") {
		t.Error("pdf misclassified as Google doc")
	}
	if !isPlainText("text/plain") || isPlainText("application/pdf") {
		t.Error("plain-text classification wrong")
	}
}

// TestHandleWebhook confirms the connector reports webhooks unsupported.
func TestHandleWebhook(t *testing.T) {
	t.Parallel()
	c := newTestConnector()
	err := c.HandleWebhook(context.Background(), sdk.Config{Checkpoint: sdk.NopCheckpoint}, nil, nil)
	if err != sdk.ErrWebhookUnsupported {
		t.Errorf("HandleWebhook = %v, want ErrWebhookUnsupported", err)
	}
}
