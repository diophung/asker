package gdrive

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/asker/asker/connectors/sdk"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

// googleDocMimePrefix marks Google-native documents (Docs, Sheets, Slides,
// Drawings...), whose bytes are not directly downloadable and must be exported.
const googleDocMimePrefix = "application/vnd.google-apps."

// googleFolderMime is the Google Drive folder pseudo-type. Folders carry no
// body and are skipped during export.
const googleFolderMime = "application/vnd.google-apps.folder"

// textMimePrefix and a small allow-list identify files whose bytes are plain
// text and can be pulled via alt=media within the size cap. Everything else
// (PDFs, images, office binaries) is left body-empty for M3 extraction.
const textMimePrefix = "text/"

// untitledFile is the display title for a file with no name.
const untitledFile = "(untitled)"

// fileDocument maps one Drive file to the canonical Document: type FILE,
// doc_id sdk.DocID("gdrive", file.ID), title from name, body from the exported
// text (Google docs) / downloaded bytes (text files) / empty (other binaries),
// participants from owners, metadata (file_id, mime_type, web_view_link,
// parents, size), timestamps from createdTime/modifiedTime, version_etag from
// the file version (or modifiedTime+md5), and acl from the permissions feed.
//
// body is the already-extracted text (may be empty); acl is the populated
// AclInfo (never nil for a Drive file — Drive is a shared source). The caller
// fetches both before mapping so this function stays pure and easy to test.
func fileDocument(tenant string, f *driveFile, body string, acl *askerv1.AclInfo) *askerv1.Document {
	title := strings.TrimSpace(f.Name)
	if title == "" {
		title = untitledFile
	}

	doc := &askerv1.Document{
		TenantId:       tenant,
		DocId:          sdk.DocID(connectorID, f.ID),
		ConnectorId:    connectorID,
		SourceNativeId: f.ID,
		Type:           askerv1.DocType_FILE,
		Title:          title,
		BodyText:       body,
		Participants:   owners(f),
		Metadata:       fileMetadata(f),
		VersionEtag:    versionEtag(f),
		Acl:            acl,
	}
	if ts := timestamps(f); ts != nil {
		doc.Ts = ts
	}
	return doc
}

// tombstoneDocument builds the deletion Document for one file: identity fields
// plus tombstone.deleted and deleted_at — no body, no chunks. etag is a value
// newer than any prior upsert etag for the same doc_id (the change time, or a
// monotonic fallback) so the delete wins the idempotent merge.
func (c *Connector) tombstoneDocument(tenant, fileID, etag string) *askerv1.Document {
	if etag == "" {
		etag = strconv.FormatInt(c.now().UTC().UnixNano(), 10)
	}
	return &askerv1.Document{
		TenantId:       tenant,
		DocId:          sdk.DocID(connectorID, fileID),
		ConnectorId:    connectorID,
		SourceNativeId: fileID,
		Type:           askerv1.DocType_FILE,
		VersionEtag:    etag,
		Tombstone: &askerv1.Tombstone{
			Deleted:   true,
			DeletedAt: timestamppb.New(c.now().UTC()),
		},
	}
}

// versionEtag derives a content-changing etag. Drive's monotonic file
// "version" is ideal; when absent (some responses omit it) fall back to
// sha256(modifiedTime + md5Checksum), both of which change when the bytes
// change. A last-resort sha256 of the id keeps the field non-empty.
func versionEtag(f *driveFile) string {
	if f.Version != "" {
		return f.Version
	}
	seed := f.ModifiedTime + "|" + f.Md5Checksum
	if strings.Trim(seed, "|") == "" {
		seed = f.ID
	}
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])
}

// fileMetadata builds the flat metadata map. Empty values are omitted.
func fileMetadata(f *driveFile) map[string]string {
	md := make(map[string]string, 5)
	put := func(k, v string) {
		if v != "" {
			md[k] = v
		}
	}
	put("file_id", f.ID)
	put("mime_type", f.MimeType)
	put("web_view_link", f.WebViewLink)
	put("size", f.Size)
	if len(f.Parents) > 0 {
		md["parents"] = strings.Join(f.Parents, ",")
	}
	return md
}

// owners maps the file's owners to participants with role "owner".
func owners(f *driveFile) []*askerv1.Participant {
	var out []*askerv1.Participant
	for _, o := range f.Owners {
		if o.DisplayName == "" && o.EmailAddress == "" {
			continue
		}
		out = append(out, &askerv1.Participant{
			Name:  o.DisplayName,
			Email: o.EmailAddress,
			Role:  "owner",
		})
	}
	return out
}

// timestamps maps createdTime/modifiedTime (RFC 3339) to the Timestamps
// message. Unparseable or absent values are skipped; an all-empty result is
// returned as nil so the document carries no zero timestamps.
func timestamps(f *driveFile) *askerv1.Timestamps {
	var ts askerv1.Timestamps
	any := false
	if t, ok := parseRFC3339(f.CreatedTime); ok {
		ts.Created = timestamppb.New(t)
		any = true
	}
	if t, ok := parseRFC3339(f.ModifiedTime); ok {
		ts.Modified = timestamppb.New(t)
		any = true
	}
	if !any {
		return nil
	}
	return &ts
}

// parseRFC3339 parses an RFC 3339 timestamp, returning ok=false for empty or
// malformed input.
func parseRFC3339(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// aclFromPermissions builds AclInfo from a file's permission feed (ADR-012,
// the M2 ACL reference). Each permission maps to a principal in Drive's own
// identifier scheme:
//
//   - type=user / type=group → the emailAddress;
//   - type=domain            → "domain:<domain>";
//   - type=anyone            → the literal "anyone" (link/public sharing).
//
// is_private is true when the file is shared with nobody beyond the connecting
// account — i.e. no group/domain/anyone principal and at most the owner's own
// user permission. Deleted permissions are ignored. Principals are sorted and
// de-duplicated for a stable, diff-able document.
func aclFromPermissions(f *driveFile, perms []*permission) *askerv1.AclInfo {
	seen := make(map[string]struct{})
	var principals []string
	add := func(p string) {
		if p == "" {
			return
		}
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		principals = append(principals, p)
	}

	ownerEmails := make(map[string]struct{}, len(f.Owners))
	for _, o := range f.Owners {
		if o.EmailAddress != "" {
			ownerEmails[strings.ToLower(o.EmailAddress)] = struct{}{}
		}
	}

	sharedBeyondOwner := false
	for _, p := range perms {
		if p == nil || p.Deleted {
			continue
		}
		switch p.Type {
		case "user":
			add(p.EmailAddress)
			if _, isOwner := ownerEmails[strings.ToLower(p.EmailAddress)]; !isOwner {
				sharedBeyondOwner = true
			}
		case "group":
			add(p.EmailAddress)
			sharedBeyondOwner = true
		case "domain":
			add("domain:" + p.Domain)
			sharedBeyondOwner = true
		case "anyone":
			add("anyone")
			sharedBeyondOwner = true
		default:
			// Unknown principal type: record what identifier we have so the
			// future enforcer has the raw material, and treat it as shared.
			if p.EmailAddress != "" {
				add(p.EmailAddress)
				sharedBeyondOwner = true
			}
		}
	}

	sort.Strings(principals)
	return &askerv1.AclInfo{
		AllowedPrincipals: principals,
		IsPrivate:         !sharedBeyondOwner,
	}
}

// isGoogleDoc reports whether the mime type is a Google-native, exportable doc.
func isGoogleDoc(mime string) bool {
	return strings.HasPrefix(mime, googleDocMimePrefix)
}

// isFolder reports whether the mime type is a Drive folder.
func isFolder(mime string) bool {
	return mime == googleFolderMime
}

// isPlainText reports whether the mime type names directly-downloadable text.
func isPlainText(mime string) bool {
	return strings.HasPrefix(mime, textMimePrefix)
}
