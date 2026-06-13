package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
)

// transcript is one chat's rendered text plus the upload metadata.
type transcript struct {
	// filename is the multipart file part filename (stable per chat).
	filename string
	// title is the document title shown in search results.
	title string
	// body is the rendered transcript text.
	body string
}

// uploader sends a rendered transcript to Asker. It is an interface so the
// orchestration logic is unit-testable with a fake; the real implementation
// posts multipart/form-data to the gateway's /v1/upload endpoint.
type uploader interface {
	// Upload sends one transcript and returns the doc_id the gateway assigned.
	Upload(ctx context.Context, t transcript) (docID string, err error)
}

// gatewayUploader posts transcripts to the Asker gateway's POST /v1/upload
// endpoint as multipart/form-data with a "file" part (the transcript text) and
// a "title" field, authenticating with the user's OIDC bearer token. The
// gateway turns each upload into a searchable FILE document in the user's own
// tenant — there is no cross-tenant path, the tenant is derived from the token.
type gatewayUploader struct {
	client  *http.Client
	baseURL string // e.g. http://127.0.0.1:8080
	token   string // OIDC bearer; never logged
}

// uploadResponse is the gateway's 202 body: {"doc_id":"..."}.
type uploadResponse struct {
	DocID string `json:"doc_id"`
	Error string `json:"error"`
}

// Upload posts t to <baseURL>/v1/upload. A non-2xx response is an error
// carrying the gateway's status and any JSON error message (which never
// contains the bearer token).
func (u *gatewayUploader) Upload(ctx context.Context, t transcript) (string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", t.filename)
	if err != nil {
		return "", fmt.Errorf("build multipart file part: %w", err)
	}
	if _, err := io.WriteString(fw, t.body); err != nil {
		return "", fmt.Errorf("write transcript body: %w", err)
	}
	if t.title != "" {
		if err := mw.WriteField("title", t.title); err != nil {
			return "", fmt.Errorf("write title field: %w", err)
		}
	}
	if err := mw.Close(); err != nil {
		return "", fmt.Errorf("finalize multipart body: %w", err)
	}

	endpoint := strings.TrimRight(u.baseURL, "/") + "/v1/upload"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &buf)
	if err != nil {
		return "", fmt.Errorf("build upload request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if u.token != "" {
		req.Header.Set("Authorization", "Bearer "+u.token)
	}

	resp, err := u.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("POST %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", uploadError(resp.StatusCode, body)
	}

	var parsed uploadResponse
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.DocID == "" {
		// Accepted but unparseable body: not fatal, but report it so the run is
		// not silently treated as a no-op.
		return "", fmt.Errorf("upload accepted (HTTP %d) but response had no doc_id", resp.StatusCode)
	}
	return parsed.DocID, nil
}

// uploadError formats a non-2xx gateway response, preferring the JSON "error"
// field when present.
func uploadError(status int, body []byte) error {
	var parsed uploadResponse
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.Error != "" {
		return fmt.Errorf("gateway rejected upload (HTTP %d): %s", status, parsed.Error)
	}
	return fmt.Errorf("gateway rejected upload (HTTP %d)", status)
}
