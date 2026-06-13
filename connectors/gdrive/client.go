package gdrive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/asker/asker/platform/safehttp"
)

// Drive v3 resource shapes — only the fields this connector reads. JSON tags
// mirror the API exactly so cassette fixtures use real field names.

// driveFile is one entry from files.list / files.get.
type driveFile struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	MimeType     string       `json:"mimeType"`
	WebViewLink  string       `json:"webViewLink"`
	Parents      []string     `json:"parents"`
	Size         string       `json:"size"` // int64 serialized as a string by Drive
	Version      string       `json:"version"`
	Md5Checksum  string       `json:"md5Checksum"`
	CreatedTime  string       `json:"createdTime"`  // RFC 3339
	ModifiedTime string       `json:"modifiedTime"` // RFC 3339
	Trashed      bool         `json:"trashed"`
	Shared       bool         `json:"shared"`
	Owners       []driveUser  `json:"owners"`
	Permissions  []permission `json:"permissions"`
}

// driveUser is an owner (or other user) record.
type driveUser struct {
	DisplayName  string `json:"displayName"`
	EmailAddress string `json:"emailAddress"`
	PermissionID string `json:"permissionId"`
}

// permission is one entry from permissions.list (also embeddable in a file).
type permission struct {
	ID                 string `json:"id"`
	Type               string `json:"type"` // user | group | domain | anyone
	Role               string `json:"role"`
	EmailAddress       string `json:"emailAddress"`
	Domain             string `json:"domain"`
	AllowFileDiscovery bool   `json:"allowFileDiscovery"`
	Deleted            bool   `json:"deleted"`
}

// fileList is the files.list response envelope.
type fileList struct {
	Files            []*driveFile `json:"files"`
	NextPageToken    string       `json:"nextPageToken"`
	IncompleteSearch bool         `json:"incompleteSearch"`
}

// permissionList is the permissions.list response envelope.
type permissionList struct {
	Permissions   []*permission `json:"permissions"`
	NextPageToken string        `json:"nextPageToken"`
}

// change is one entry from changes.list.
type change struct {
	ChangeType string     `json:"changeType"` // "file" | "drive"
	FileID     string     `json:"fileId"`
	Removed    bool       `json:"removed"`
	Time       string     `json:"time"`
	File       *driveFile `json:"file"`
}

// changeList is the changes.list response envelope.
type changeList struct {
	Changes           []*change `json:"changes"`
	NewStartPageToken string    `json:"newStartPageToken"`
	NextPageToken     string    `json:"nextPageToken"`
}

// startPageToken is the changes.getStartPageToken response.
type startPageToken struct {
	StartPageToken string `json:"startPageToken"`
}

// apiError is the Drive error envelope ({"error":{"code":...,"message":...}}).
type apiError struct {
	Err struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// fileFields is the field mask requested for files.list / files.get / the
// change.file. Asking for exactly what the connector maps keeps payloads small
// and cassettes honest about the contract.
const fileFields = "id,name,mimeType,webViewLink,parents,size,version,md5Checksum,createdTime,modifiedTime,trashed,shared,owners(displayName,emailAddress,permissionId)"

// permissionFields is the field mask for permissions.list.
const permissionFields = "permissions(id,type,role,emailAddress,domain,allowFileDiscovery,deleted),nextPageToken"

// maxBodyBytes caps the body fetched for an exported Google-native doc or a
// plain-text file (the contract's ~1MB cap). Larger files are left body-empty
// for M3 extraction.
const maxBodyBytes = 1 << 20

// client is the thin Drive v3 HTTP client. baseURL has no trailing slash; the
// transport injects the bearer token on every request.
type client struct {
	http    *http.Client
	baseURL string
}

// newClient builds a Drive client whose http.Client attaches the bearer token
// from cfg.Token to every request and resolves paths against base_url (or the
// real Drive base when none is given).
func (c *Connector) newClient(conf instanceConfig, token []byte) (*client, error) {
	base := conf.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	base = strings.TrimRight(base, "/")
	return &client{
		http: &http.Client{
			// base_url is tenant-supplied; the SSRF-guarded base refuses
			// loopback/metadata/private/cluster IPs at connect time so the bearer
			// token is never sent to an internal host.
			Transport: &bearerTransport{token: string(token), base: safehttp.GuardedBase()},
			Timeout:   clientTimeout,
		},
		baseURL: base,
	}, nil
}

// bearerTransport adds "Authorization: Bearer <token>" to every request without
// mutating the caller's request. The hub owns refresh; the connector only ever
// sees a currently-valid token.
type bearerTransport struct {
	token string
	base  http.RoundTripper
}

// RoundTrip implements http.RoundTripper.
func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(clone)
}

// doJSON performs a GET against path?query and decodes a JSON response into out.
// A non-2xx status becomes a *statusError carrying the HTTP code so callers can
// distinguish 404 (vanished file) and 400/410 (expired cursor).
func (cl *client) doJSON(ctx context.Context, path string, query url.Values, out any) error {
	resp, err := cl.get(ctx, path, query)
	if err != nil {
		return err
	}
	defer drainClose(resp.Body)

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return fmt.Errorf("gdrive: read %s response: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return newStatusError(path, resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("gdrive: decode %s response: %w", path, err)
	}
	return nil
}

// get issues the raw GET and returns the live response (caller closes the body).
func (cl *client) get(ctx context.Context, path string, query url.Values) (*http.Response, error) {
	u := cl.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("gdrive: build request %s: %w", path, err)
	}
	resp, err := cl.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gdrive: GET %s: %w", path, err)
	}
	return resp, nil
}

// listFiles pages files.list. pageToken "" requests the first page; pageSize 0
// uses the default. It excludes trashed files at the source so the backfill
// never emits a doc it would only tombstone moments later.
func (cl *client) listFiles(ctx context.Context, pageToken string, pageSize int) (*fileList, error) {
	q := url.Values{}
	q.Set("fields", "files("+fileFields+"),nextPageToken,incompleteSearch")
	q.Set("q", "trashed=false")
	if pageSize <= 0 {
		pageSize = listPageSize
	}
	q.Set("pageSize", strconv.Itoa(pageSize))
	if pageToken != "" {
		q.Set("pageToken", pageToken)
	}
	var out fileList
	if err := cl.doJSON(ctx, "/files", q, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// getFile fetches a single file's metadata (used to refresh a changed file when
// the change entry omits it).
func (cl *client) getFile(ctx context.Context, id string) (*driveFile, error) {
	q := url.Values{}
	q.Set("fields", fileFields)
	var out driveFile
	if err := cl.doJSON(ctx, "/files/"+url.PathEscape(id), q, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// startToken seeds the changes cursor via changes.getStartPageToken.
func (cl *client) startToken(ctx context.Context) (string, error) {
	var out startPageToken
	if err := cl.doJSON(ctx, "/changes/startPageToken", nil, &out); err != nil {
		return "", err
	}
	if out.StartPageToken == "" {
		return "", fmt.Errorf("gdrive: changes.getStartPageToken returned an empty token")
	}
	return out.StartPageToken, nil
}

// listChanges pages the changes feed from pageToken. An invalid token (Drive
// 400/410) is translated to errExpiredToken so IncrementalSync can map it onto
// sdk.ErrCursorExpired.
func (cl *client) listChanges(ctx context.Context, pageToken string) (*changeList, error) {
	q := url.Values{}
	q.Set("pageToken", pageToken)
	q.Set("pageSize", strconv.Itoa(listPageSize))
	q.Set("fields", "changes(changeType,fileId,removed,time,file("+fileFields+")),newStartPageToken,nextPageToken")
	var out changeList
	if err := cl.doJSON(ctx, "/changes", q, &out); err != nil {
		if se := asStatusError(err); se != nil && (se.code == http.StatusBadRequest || se.code == http.StatusGone) {
			return nil, fmt.Errorf("%w: %v", errExpiredToken, err)
		}
		return nil, err
	}
	return &out, nil
}

// listPermissions pages permissions.list for a file and returns every
// permission entry. A 404 (file vanished between list and permission fetch)
// surfaces to the caller, which treats it as "no acl available".
func (cl *client) listPermissions(ctx context.Context, fileID string) ([]*permission, error) {
	var all []*permission
	pageToken := ""
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		q := url.Values{}
		q.Set("fields", permissionFields)
		q.Set("pageSize", strconv.Itoa(listPageSize))
		if pageToken != "" {
			q.Set("pageToken", pageToken)
		}
		var out permissionList
		if err := cl.doJSON(ctx, "/files/"+url.PathEscape(fileID)+"/permissions", q, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Permissions...)
		if out.NextPageToken == "" {
			return all, nil
		}
		pageToken = out.NextPageToken
	}
}

// exportText downloads a Google-native doc as text/plain (capped at
// maxBodyBytes). Used for Google Docs/Sheets/Slides whose bytes are not
// directly downloadable.
func (cl *client) exportText(ctx context.Context, fileID string) (string, error) {
	q := url.Values{}
	q.Set("mimeType", "text/plain")
	return cl.fetchBody(ctx, "/files/"+url.PathEscape(fileID)+"/export", q)
}

// downloadText downloads a plain-text file's bytes via alt=media (capped at
// maxBodyBytes).
func (cl *client) downloadText(ctx context.Context, fileID string) (string, error) {
	q := url.Values{}
	q.Set("alt", "media")
	return cl.fetchBody(ctx, "/files/"+url.PathEscape(fileID), q)
}

// fetchBody GETs a raw (non-JSON) body, reading at most maxBodyBytes.
func (cl *client) fetchBody(ctx context.Context, path string, query url.Values) (string, error) {
	resp, err := cl.get(ctx, path, query)
	if err != nil {
		return "", err
	}
	defer drainClose(resp.Body)

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return "", fmt.Errorf("gdrive: read %s body: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", newStatusError(path, resp.StatusCode, body)
	}
	return string(body), nil
}

// drainClose drains and closes a response body so the connection can be reused.
func drainClose(rc io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(rc, 4096))
	_ = rc.Close()
}

// statusError is a non-2xx HTTP response from Drive.
type statusError struct {
	path    string
	code    int
	message string
}

// newStatusError parses the Drive error envelope (best effort) from body.
func newStatusError(path string, code int, body []byte) *statusError {
	se := &statusError{path: path, code: code}
	var ae apiError
	if json.Unmarshal(body, &ae) == nil && ae.Err.Message != "" {
		se.message = ae.Err.Message
	}
	return se
}

func (e *statusError) Error() string {
	if e.message != "" {
		return fmt.Sprintf("gdrive: %s returned HTTP %d: %s", e.path, e.code, e.message)
	}
	return fmt.Sprintf("gdrive: %s returned HTTP %d", e.path, e.code)
}

// asStatusError extracts a *statusError from err, if present.
func asStatusError(err error) *statusError {
	var se *statusError
	if errors.As(err, &se) {
		return se
	}
	return nil
}

// isNotFound reports whether err is an HTTP 404 from Drive.
func isNotFound(err error) bool {
	se := asStatusError(err)
	return se != nil && se.code == http.StatusNotFound
}
