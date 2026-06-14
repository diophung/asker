package jira

import (
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/asker/asker/connectors/sdk"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

// searchResponse is the POST /rest/api/3/search result envelope.
type searchResponse struct {
	StartAt    int      `json:"startAt"`
	MaxResults int      `json:"maxResults"`
	Total      int      `json:"total"`
	Issues     []*issue `json:"issues"`
}

// issue is one Jira issue as returned by the search API.
type issue struct {
	ID     string      `json:"id"`
	Key    string      `json:"key"`
	Fields issueFields `json:"fields"`
}

// issueFields holds the issue fields the mapping reads.
type issueFields struct {
	Summary     string         `json:"summary"`
	Description *adfNode       `json:"description"`
	Status      *namedRef      `json:"status"`
	IssueType   *namedRef      `json:"issuetype"`
	Priority    *namedRef      `json:"priority"`
	Project     *projectRef    `json:"project"`
	Reporter    *userRef       `json:"reporter"`
	Assignee    *userRef       `json:"assignee"`
	Creator     *userRef       `json:"creator"`
	Created     string         `json:"created"`
	Updated     string         `json:"updated"`
	Comment     *commentResult `json:"comment"`
}

// namedRef is a Jira reference carrying a display name (status, issue type,
// priority).
type namedRef struct {
	Name           string `json:"name"`
	StatusCategory *struct {
		Key string `json:"key"`
	} `json:"statusCategory,omitempty"`
}

// projectRef is the issue's project reference.
type projectRef struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}

// userRef is a Jira user (reporter/assignee/creator). emailAddress is often
// absent under GDPR-strict mode; accountId is always present.
type userRef struct {
	AccountID    string `json:"accountId"`
	DisplayName  string `json:"displayName"`
	EmailAddress string `json:"emailAddress"`
}

// commentResult is the paginated comment container returned when the comment
// field is requested.
type commentResult struct {
	Comments []*comment `json:"comments"`
}

// comment is one issue comment; its body is ADF.
type comment struct {
	ID     string   `json:"id"`
	Author *userRef `json:"author"`
	Body   *adfNode `json:"body"`
}

// issueDocument maps one Jira issue to the canonical Document: type TICKET,
// doc_id sdk.DocID("jira", iss.ID), title "<key>: <summary>", body from the ADF
// description plus comment text, participants from reporter/assignee/creator,
// and version_etag from fields.updated (changes on any update).
//
// The doc_id / source_native_id are derived from the IMMUTABLE numeric issue ID
// (iss.ID), not the issue Key: a Jira issue keeps its id forever, but its key
// changes on a project move or key rename, which would otherwise orphan the
// indexed document. The (mutable) key is preserved in the title and the
// issue_key metadata for display and search.
func issueDocument(tenant, base string, iss *issue) *askerv1.Document {
	summary := iss.Fields.Summary

	title := iss.Key
	if summary != "" {
		title = iss.Key + ": " + summary
	}

	doc := &askerv1.Document{
		TenantId:       tenant,
		DocId:          sdk.DocID(connectorID, iss.ID),
		ConnectorId:    connectorID,
		SourceNativeId: iss.ID,
		Type:           askerv1.DocType_TICKET,
		Title:          title,
		BodyText:       issueBody(iss),
		Participants:   issueParticipants(iss),
		Metadata:       issueMetadata(base, iss),
		VersionEtag:    iss.Fields.Updated,
		Ts:             issueTimestamps(iss),
	}
	return doc
}

// tombstoneDocument builds the deletion Document for one issue: identity
// fields plus tombstone.deleted and deleted_at, no body. The version_etag is
// the issue's updated time, which is newer than any prior live etag because
// transitioning to the deleted-like status bumps updated. The doc_id is derived
// from the immutable iss.ID (matching issueDocument) so a delete converges on
// the same indexed document even after a key rename or project move.
func (c *Connector) tombstoneDocument(tenant string, iss *issue) *askerv1.Document {
	etag := iss.Fields.Updated
	if etag == "" {
		etag = c.now().UTC().Format(time.RFC3339)
	}
	return &askerv1.Document{
		TenantId:       tenant,
		DocId:          sdk.DocID(connectorID, iss.ID),
		ConnectorId:    connectorID,
		SourceNativeId: iss.ID,
		Type:           askerv1.DocType_TICKET,
		VersionEtag:    etag,
		Tombstone: &askerv1.Tombstone{
			Deleted:   true,
			DeletedAt: timestamppb.New(c.now().UTC()),
		},
	}
}

// issueBody assembles the searchable body: the ADF description rendered to
// plain text, then each comment's author and ADF text, separated by blank
// lines.
func issueBody(iss *issue) string {
	var parts []string
	if desc := adfText(iss.Fields.Description); desc != "" {
		parts = append(parts, desc)
	}
	if iss.Fields.Comment != nil {
		for _, cm := range iss.Fields.Comment.Comments {
			text := adfText(cm.Body)
			if text == "" {
				continue
			}
			if cm.Author != nil && cm.Author.DisplayName != "" {
				parts = append(parts, cm.Author.DisplayName+": "+text)
			} else {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

// issueMetadata builds the flat metadata map. Empty values are omitted.
func issueMetadata(base string, iss *issue) map[string]string {
	md := make(map[string]string, 8)
	put := func(k, v string) {
		if v != "" {
			md[k] = v
		}
	}
	put("issue_key", iss.Key)
	put("issue_id", iss.ID)
	if iss.Fields.Project != nil {
		put("project", iss.Fields.Project.Key)
	}
	if iss.Fields.Status != nil {
		put("status", iss.Fields.Status.Name)
	}
	if iss.Fields.IssueType != nil {
		put("issue_type", iss.Fields.IssueType.Name)
	}
	if iss.Fields.Priority != nil {
		put("priority", iss.Fields.Priority.Name)
	}
	if iss.Key != "" {
		put("web_url", base+"/browse/"+iss.Key)
	}
	return md
}

// issueParticipants builds the typed people facet from reporter, assignee, and
// creator. A nil or empty user is skipped.
func issueParticipants(iss *issue) []*askerv1.Participant {
	var out []*askerv1.Participant
	add := func(u *userRef, role string) {
		if u == nil {
			return
		}
		if u.DisplayName == "" && u.EmailAddress == "" && u.AccountID == "" {
			return
		}
		out = append(out, &askerv1.Participant{
			Name:   u.DisplayName,
			Email:  u.EmailAddress,
			Handle: u.AccountID,
			Role:   role,
		})
	}
	add(iss.Fields.Reporter, "reporter")
	add(iss.Fields.Assignee, "assignee")
	add(iss.Fields.Creator, "creator")
	return out
}

// issueTimestamps maps fields.created/updated (RFC 3339 with milliseconds and
// a numeric offset, e.g. "2026-06-10T09:15:00.000+0000") to the canonical
// Timestamps. Unparseable or absent values are left unset.
func issueTimestamps(iss *issue) *askerv1.Timestamps {
	ts := &askerv1.Timestamps{}
	if t, ok := parseJiraTime(iss.Fields.Created); ok {
		ts.Created = timestamppb.New(t)
	}
	if t, ok := parseJiraTime(iss.Fields.Updated); ok {
		ts.Modified = timestamppb.New(t)
	}
	if ts.Created == nil && ts.Modified == nil {
		return nil
	}
	return ts
}

// jiraTimeLayouts are the timestamp formats Jira's REST API emits, tried in
// order. The canonical form carries milliseconds and a numeric zone offset
// without a colon ("-0700"); RFC 3339 variants are tolerated too.
var jiraTimeLayouts = []string{
	"2006-01-02T15:04:05.000-0700",
	"2006-01-02T15:04:05-0700",
	time.RFC3339Nano,
	time.RFC3339,
}

// parseJiraTime parses a Jira timestamp string into a UTC time.
func parseJiraTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range jiraTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}
