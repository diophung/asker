package confluence

import (
	"errors"
	"strconv"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/asker/asker/connectors/sdk"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

// contentPage is one page of GET /content or /content/search results (offset
// pagination). Confluence echoes the limit/size and a _links.next relative URL
// when more results exist.
type contentPage struct {
	Results []content `json:"results"`
	Start   int       `json:"start"`
	Limit   int       `json:"limit"`
	Size    int       `json:"size"`
	Links   struct {
		Next string `json:"next"` // relative URL to the next page, present iff more
		Base string `json:"base"` // absolute UI base for resolving webui links
	} `json:"_links"`
}

// content is a single Confluence content item (a page) with the expansions the
// Document mapping requests: body.storage, version, space, history.
type content struct {
	ID      string  `json:"id"`
	Type    string  `json:"type"`
	Status  string  `json:"status"`
	Title   string  `json:"title"`
	Body    body    `json:"body"`
	Version version `json:"version"`
	Space   space   `json:"space"`
	History history `json:"history"`
	Links   links   `json:"_links"`
}

type body struct {
	Storage storage `json:"storage"`
}

// storage is the XHTML "storage format" body. Value is Confluence storage-format
// markup (XHTML-ish), stripped to plain text for body_text.
type storage struct {
	Value          string `json:"value"`
	Representation string `json:"representation"`
}

// version is the current version of the page. Number changes on every edit, so
// it is the version_etag; When is the modified timestamp; By is the editor.
type version struct {
	Number int    `json:"number"`
	When   string `json:"when"` // RFC 3339
	By     user   `json:"by"`
}

type space struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}

// history carries creation metadata. CreatedBy is the page author; CreatedDate
// is the source creation timestamp.
type history struct {
	CreatedBy   user   `json:"createdBy"`
	CreatedDate string `json:"createdDate"` // RFC 3339
}

// user is an Atlassian account reference as it appears in createdBy / version.by.
type user struct {
	AccountID   string `json:"accountId"`
	DisplayName string `json:"displayName"`
	Email       string `json:"email"`
}

// links holds the per-page _links Confluence returns. WebUI is a site-relative
// path (e.g. "/spaces/DEV/pages/123/Title") joined to the page-set base.
type links struct {
	WebUI string `json:"webui"`
}

// contentDocument maps one expanded page to the canonical Document: type
// WIKI_PAGE, doc_id sdk.DocID("confluence", page id), title from title,
// body_text from the storage XHTML stripped to text, participants from the
// author and last editor, metadata (page/space/version/url/status), timestamps
// from history.createdDate (created) and version.when (modified), and an empty
// captured AclInfo (ADR-012: capture, do not enforce; restrictions deferred).
//
// uiBase is the absolute UI base (contentPage._links.base) used to build an
// absolute web_url from the page's relative _links.webui.
func contentDocument(tenant string, p *content, uiBase string) (*askerv1.Document, error) {
	if p == nil || p.ID == "" {
		return nil, errors.New("confluence: content result has no id")
	}

	doc := &askerv1.Document{
		TenantId:       tenant,
		DocId:          sdk.DocID(connectorID, p.ID),
		ConnectorId:    connectorID,
		SourceNativeId: p.ID,
		Type:           askerv1.DocType_WIKI_PAGE,
		Title:          p.Title,
		BodyText:       stripStorageXHTML(p.Body.Storage.Value),
		Participants:   participants(p),
		Metadata:       metadata(p, uiBase),
		VersionEtag:    versionEtag(p),
		Acl:            capturedACL(),
	}

	if ts := timestamps(p); ts != nil {
		doc.Ts = ts
	}
	return doc, nil
}

// tombstoneDocument builds the deletion Document for a trashed/removed page:
// identity fields plus tombstone.deleted and deleted_at — no body, no chunks.
// version_etag is the version number observed at trash time when known, else a
// stable "deleted" marker so the field is never empty.
func (c *Connector) tombstoneDocument(tenant, pageID, etag string) *askerv1.Document {
	if etag == "" {
		etag = "deleted"
	}
	return &askerv1.Document{
		TenantId:       tenant,
		DocId:          sdk.DocID(connectorID, pageID),
		ConnectorId:    connectorID,
		SourceNativeId: pageID,
		Type:           askerv1.DocType_WIKI_PAGE,
		VersionEtag:    etag,
		Tombstone: &askerv1.Tombstone{
			Deleted:   true,
			DeletedAt: timestamppb.New(c.now().UTC()),
		},
	}
}

// versionEtag returns the page version number as a string. It increments on
// every edit, so a downstream upsert is a no-op unless the page changed. A page
// without a version (shouldn't happen with the version expansion) falls back to
// "0" so the field is never empty.
func versionEtag(p *content) string {
	return strconv.Itoa(p.Version.Number)
}

// metadata builds the flat metadata map. Empty values are omitted.
func metadata(p *content, uiBase string) map[string]string {
	md := make(map[string]string, 6)
	put := func(k, v string) {
		if v != "" {
			md[k] = v
		}
	}
	put("page_id", p.ID)
	put("space_key", p.Space.Key)
	put("space_name", p.Space.Name)
	put("web_url", webURL(uiBase, p.Links.WebUI))
	put("status", p.Status)
	if p.Version.Number > 0 {
		put("version_number", strconv.Itoa(p.Version.Number))
	}
	return md
}

// webURL joins the page-set UI base with the page's relative webui link. When
// the base is absent the relative link is returned as-is (still useful).
func webURL(uiBase, webui string) string {
	if webui == "" {
		return ""
	}
	if uiBase == "" {
		return webui
	}
	return joinURL(uiBase, webui)
}

// joinURL concatenates a base and a (usually leading-slash) relative path
// without doubling or dropping the separator.
func joinURL(base, rel string) string {
	switch {
	case base == "":
		return rel
	case rel == "":
		return base
	}
	baseSlash := base[len(base)-1] == '/'
	relSlash := rel[0] == '/'
	switch {
	case baseSlash && relSlash:
		return base + rel[1:]
	case !baseSlash && !relSlash:
		return base + "/" + rel
	default:
		return base + rel
	}
}

// participants returns the page author (history.createdBy, role "author") and
// the last editor (version.by, role "editor"), skipping empties and collapsing
// the case where author and editor are the same account into a single
// participant carrying both roles' intent (author wins).
func participants(p *content) []*askerv1.Participant {
	var out []*askerv1.Participant
	seen := map[string]bool{}

	add := func(u user, role string) {
		if u.AccountID == "" && u.DisplayName == "" && u.Email == "" {
			return
		}
		key := u.AccountID
		if key == "" {
			key = u.Email + "|" + u.DisplayName
		}
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, &askerv1.Participant{
			Name:   u.DisplayName,
			Email:  u.Email,
			Handle: u.AccountID,
			Role:   role,
		})
	}

	add(p.History.CreatedBy, "author")
	add(p.Version.By, "editor")
	return out
}

// capturedACL returns the captured-but-not-enforced ACL for a Confluence page.
// Per ADR-012, M2 captures who-may-see-it for shared sources but defers
// enforcement; per-page/space restrictions are not requested in the page
// expansion (they are a separate /restriction call), so the connector records
// the conservative default: not private to the connecting account, no
// principals enumerated. See README for the limitation.
func capturedACL() *askerv1.AclInfo {
	return &askerv1.AclInfo{
		IsPrivate:         false,
		AllowedPrincipals: []string{},
	}
}

// timestamps maps history.createdDate -> created and version.when -> modified.
// Unparseable or absent values are omitted; a Timestamps with neither set is
// returned as nil so the document carries no empty Ts.
func timestamps(p *content) *askerv1.Timestamps {
	var ts askerv1.Timestamps
	any := false
	if t, ok := parseTime(p.History.CreatedDate); ok {
		ts.Created = timestamppb.New(t)
		any = true
	}
	if t, ok := parseTime(p.Version.When); ok {
		ts.Modified = timestamppb.New(t)
		any = true
	}
	if !any {
		return nil
	}
	return &ts
}

// parseTime parses a Confluence timestamp. The REST API emits RFC 3339 with
// milliseconds and an offset (e.g. "2026-06-01T10:30:00.000Z"); RFC3339Nano
// handles both that and the no-fraction form.
func parseTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC(), true
	}
	return time.Time{}, false
}
