package gcal

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

	"github.com/asker/asker/connectors/sdk"
	"github.com/asker/asker/platform/safehttp"
)

// maxResponseBody bounds a single events.list response read so a hostile or
// broken source cannot exhaust memory. Calendar pages are a few hundred KB at
// the 2500 cap; 32 MiB is a generous ceiling.
const maxResponseBody = 32 << 20

// client is a minimal Calendar API v3 REST client over net/http. It targets
// exactly the events.list subset the connector needs.
type client struct {
	http       *http.Client
	baseURL    string
	calendarID string
}

// newClient builds a client whose RoundTripper injects the bearer token on
// every request and whose base URL points at conf.BaseURL (the real API or, in
// dev/CI, the replay server).
func newClient(conf instanceConfig, token []byte) *client {
	return &client{
		http: &http.Client{
			// base_url is tenant-supplied; the SSRF-guarded base refuses
			// loopback/metadata/private/cluster IPs at connect time so the bearer
			// token is never sent to an internal host.
			Transport: &bearerTransport{token: string(token), base: safehttp.GuardedBase()},
			Timeout:   httpTimeout,
		},
		baseURL:    strings.TrimRight(conf.BaseURL, "/"),
		calendarID: conf.CalendarID,
	}
}

// event is the events.list item shape (Calendar API v3 Event resource). Only
// the fields the connector maps are decoded.
type event struct {
	ID          string         `json:"id"`
	Status      string         `json:"status"`
	Etag        string         `json:"etag"`
	Summary     string         `json:"summary"`
	Description string         `json:"description"`
	Location    string         `json:"location"`
	HTMLLink    string         `json:"htmlLink"`
	Created     string         `json:"created"`
	Updated     string         `json:"updated"`
	Start       *eventDateTime `json:"start"`
	End         *eventDateTime `json:"end"`
	Organizer   *eventActor    `json:"organizer"`
	Attendees   []*attendee    `json:"attendees"`
}

// eventDateTime is the Calendar start/end shape: a timed event carries
// dateTime (RFC3339), an all-day event carries date (YYYY-MM-DD).
type eventDateTime struct {
	Date     string `json:"date"`
	DateTime string `json:"dateTime"`
	TimeZone string `json:"timeZone"`
}

// value returns the dateTime when present, else the all-day date.
func (d *eventDateTime) value() string {
	if d == nil {
		return ""
	}
	if d.DateTime != "" {
		return d.DateTime
	}
	return d.Date
}

// eventActor is the organizer/creator shape.
type eventActor struct {
	DisplayName string `json:"displayName"`
	Email       string `json:"email"`
	Self        bool   `json:"self"`
}

// attendee is one EventAttendee.
type attendee struct {
	DisplayName    string `json:"displayName"`
	Email          string `json:"email"`
	ResponseStatus string `json:"responseStatus"`
	Organizer      bool   `json:"organizer"`
	Resource       bool   `json:"resource"`
}

// eventsList is the events.list response envelope.
type eventsList struct {
	Items         []*event `json:"items"`
	NextPageToken string   `json:"nextPageToken"`
	NextSyncToken string   `json:"nextSyncToken"`
}

// listParams selects one events.list call. Exactly one of pageToken / syncToken
// is set in steady use (both empty = first backfill page).
type listParams struct {
	pageToken  string
	syncToken  string
	maxResults int
}

// apiError is a non-2xx Calendar API response.
type apiError struct {
	status int
	body   string
}

func (e *apiError) Error() string {
	body := e.body
	if len(body) > 256 {
		body = body[:256]
	}
	return fmt.Sprintf("calendar API status %d: %s", e.status, body)
}

// listEvents performs one events.list call against calendarID. A 410 Gone on a
// syncToken means the source pruned that position; it is wrapped as
// sdk.ErrCursorExpired so the hub restarts a full sync. ctx is honored.
func (c *client) listEvents(ctx context.Context, p listParams) (*eventsList, error) {
	q := url.Values{}
	q.Set("singleEvents", "true")
	if p.maxResults > 0 {
		q.Set("maxResults", strconv.Itoa(p.maxResults))
	}
	switch {
	case p.syncToken != "":
		q.Set("syncToken", p.syncToken)
	case p.pageToken != "":
		q.Set("pageToken", p.pageToken)
	}

	endpoint := c.baseURL + "/calendars/" + url.PathEscape(c.calendarID) + "/events?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("gcal: build events.list request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gcal: events.list: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, fmt.Errorf("gcal: read events.list response: %w", err)
	}

	if resp.StatusCode == http.StatusGone {
		// 410 Gone: the syncToken is too old. Only a full re-sync recovers.
		return nil, fmt.Errorf("gcal: events.list syncToken expired (410 Gone): %w", sdk.ErrCursorExpired)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &apiError{status: resp.StatusCode, body: string(body)}
	}

	var out eventsList
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("gcal: decode events.list response: %w", err)
	}
	return &out, nil
}

// isCursorExpired reports whether err is (or wraps) sdk.ErrCursorExpired.
func isCursorExpired(err error) bool { return errors.Is(err, sdk.ErrCursorExpired) }
