package outlookcal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/asker/asker/connectors/sdk"
	"github.com/asker/asker/platform/safehttp"
)

// maxBodyBytes bounds a single Graph JSON response read so a misbehaving or
// spoofed endpoint cannot exhaust memory. A page of 50 events with full bodies
// is comfortably under this.
const maxBodyBytes = 16 << 20 // 16 MiB

// client is a thin Microsoft Graph HTTP client: an *http.Client whose
// transport injects the bearer token on every request, plus a JSON GET helper
// that maps Graph error statuses (notably 410/400 resyncRequired) to typed
// errors the sync code acts on.
type client struct {
	http *http.Client
}

// newClient builds a Graph client that adds "Authorization: Bearer <token>" to
// every request. The hub owns refresh; the connector only ever sees a
// currently-valid token (sdk.Config.Token).
func newClient(_ instanceConfig, token []byte) *client {
	return &client{
		http: &http.Client{
			// base_url is tenant-supplied; the SSRF-guarded base refuses
			// loopback/metadata/private/cluster IPs at connect time so the bearer
			// token is never sent to an internal host.
			Transport: &bearerTransport{token: string(token), base: safehttp.GuardedBase()},
			Timeout:   clientTimeout,
		},
	}
}

// bearerTransport adds the Authorization header without mutating the caller's
// request and never logs the token.
type bearerTransport struct {
	token string
	base  http.RoundTripper
}

// RoundTrip implements http.RoundTripper.
func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	if t.token != "" {
		clone.Header.Set("Authorization", "Bearer "+t.token)
	}
	clone.Header.Set("Accept", "application/json")
	return t.base.RoundTrip(clone)
}

// errCursorGone marks a Graph response that says the delta token can no longer
// be replayed: HTTP 410 Gone, or 400 with error code "resyncRequired" /
// "SyncStateNotFound". The caller wraps it as sdk.ErrCursorExpired.
var errCursorGone = errors.New("outlook-cal: graph reports the delta token is no longer replayable")

// getJSON issues GET rawURL and decodes a 200 JSON body into out. A 410 (or a
// 400 resync code) returns errCursorGone; any other non-2xx returns an error
// carrying the status and Graph error code (never the token).
func (c *client) getJSON(ctx context.Context, rawURL string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("outlook-cal: build request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("outlook-cal: GET request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return fmt.Errorf("outlook-cal: read response body: %w", err)
	}

	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("outlook-cal: decode response body: %w", err)
		}
		return nil
	}

	code := graphErrorCode(body)
	if resp.StatusCode == http.StatusGone || isResyncCode(code) {
		return fmt.Errorf("graph status %d (code %q): %w", resp.StatusCode, code, errCursorGone)
	}
	if code != "" {
		return fmt.Errorf("outlook-cal: graph returned status %d (code %q)", resp.StatusCode, code)
	}
	return fmt.Errorf("outlook-cal: graph returned status %d", resp.StatusCode)
}

// isResyncCode reports whether a Graph error code means the delta state is gone
// and a full resync is required.
func isResyncCode(code string) bool {
	switch code {
	case "resyncRequired", "SyncStateNotFound", "syncStateNotFound":
		return true
	default:
		return false
	}
}

// graphErrorCode extracts the "error.code" field from a Graph error body, or ""
// when the body is not a recognizable Graph error envelope.
func graphErrorCode(body []byte) string {
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return ""
	}
	return env.Error.Code
}

// asCursorExpired wraps errCursorGone as sdk.ErrCursorExpired so the hub
// restarts a full sync; any other error is returned unchanged.
func asCursorExpired(err error) error {
	if errors.Is(err, errCursorGone) {
		return fmt.Errorf("%v: %w", err, sdk.ErrCursorExpired)
	}
	return err
}
