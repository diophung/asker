package server

// Always-on compatibility tests for the generated Gmail client
// (google.golang.org/api@v0.284.0/gmail/v1), replicating its exact wire
// behavior as read from the module-cache source:
//
//   - NewService with option.WithEndpoint sets Service.BasePath to the
//     endpoint (gmail-gen.go:178-182), and every call resolves
//     "gmail/v1/users/{userId}/..." against it via googleapi.ResolveRelative
//     (gmail-gen.go:5195 list, :4634 get, :3586 history, :2525 watch), so
//     all routes live under /gmail/v1/ on this server.
//   - {userId} is substituted with URL path escaping (googleapi.Expand), so
//     an email userId arrives percent-encoded.
//   - Query params: alt=json & prettyPrint=false always; maxResults /
//     pageToken / startHistoryId / format as strings; historyTypes repeated
//     via URLParams.SetMulti (gmail-gen.go:3506).
//   - Decode targets declare historyId/internalDate/expiration with the
//     `,string` JSON option (gmail-gen.go:949 History, :1635 Message, :2259
//     WatchResponse), so those numbers MUST arrive as JSON strings.
//   - Non-2xx responses are parsed by googleapi.CheckResponse as
//     {"error":{code,message,errors[]}} (googleapi.go:66,136).
//
// The genXxx mirror types below copy the generated client's JSON tags
// verbatim; decoding through them proves the real client would decode the
// same bytes. client_compat_tagged_test.go drives the actual generated
// client and is enabled (build tag gmail_compat) once go.mod carries the
// google.golang.org/api transitive requirements.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"
)

// --- decode-target mirrors (tags copied from gmail-gen.go v0.284.0) -----

type genMessage struct {
	HistoryId    uint64          `json:"historyId,omitempty,string"`
	Id           string          `json:"id,omitempty"`
	InternalDate int64           `json:"internalDate,omitempty,string"`
	LabelIds     []string        `json:"labelIds,omitempty"`
	Payload      *genMessagePart `json:"payload,omitempty"`
	SizeEstimate int64           `json:"sizeEstimate,omitempty"`
	Snippet      string          `json:"snippet,omitempty"`
	ThreadId     string          `json:"threadId,omitempty"`
}

type genMessagePart struct {
	Body     *genMessagePartBody     `json:"body,omitempty"`
	Filename string                  `json:"filename,omitempty"`
	Headers  []*genMessagePartHeader `json:"headers,omitempty"`
	MimeType string                  `json:"mimeType,omitempty"`
	PartId   string                  `json:"partId,omitempty"`
	Parts    []*genMessagePart       `json:"parts,omitempty"`
}

type genMessagePartBody struct {
	AttachmentId string `json:"attachmentId,omitempty"`
	Data         string `json:"data,omitempty"`
	Size         int64  `json:"size,omitempty"`
}

type genMessagePartHeader struct {
	Name  string `json:"name,omitempty"`
	Value string `json:"value,omitempty"`
}

type genListMessagesResponse struct {
	Messages           []*genMessage `json:"messages,omitempty"`
	NextPageToken      string        `json:"nextPageToken,omitempty"`
	ResultSizeEstimate int64         `json:"resultSizeEstimate,omitempty"`
}

type genHistory struct {
	Id              uint64                      `json:"id,omitempty,string"`
	Messages        []*genMessage               `json:"messages,omitempty"`
	MessagesAdded   []*genHistoryMessageAdded   `json:"messagesAdded,omitempty"`
	MessagesDeleted []*genHistoryMessageDeleted `json:"messagesDeleted,omitempty"`
}

type genHistoryMessageAdded struct {
	Message *genMessage `json:"message,omitempty"`
}

type genHistoryMessageDeleted struct {
	Message *genMessage `json:"message,omitempty"`
}

type genListHistoryResponse struct {
	History       []*genHistory `json:"history,omitempty"`
	HistoryId     uint64        `json:"historyId,omitempty,string"`
	NextPageToken string        `json:"nextPageToken,omitempty"`
}

type genWatchResponse struct {
	Expiration int64  `json:"expiration,omitempty,string"`
	HistoryId  uint64 `json:"historyId,omitempty,string"`
}

// genErrorReply mirrors googleapi.errorReply / googleapi.Error.
type genErrorReply struct {
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Errors  []struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"errors"`
	} `json:"error"`
}

// clientGet performs a GET exactly as the generated client would: BasePath
// (the test server URL) + "gmail/v1/" + path, with alt=json and
// prettyPrint=false always present, decoding 2xx JSON into out.
func clientGet(t *testing.T, f *fixture, path string, params url.Values, out any) (int, []byte) {
	t.Helper()
	if params == nil {
		params = url.Values{}
	}
	// The generated client always sets these (gmail-gen.go calls
	// urlParams_.Set("alt", alt) / setOptions sets prettyPrint=false).
	params.Set("alt", "json")
	params.Set("prettyPrint", "false")
	u := f.ts.URL + "/gmail/v1/" + path + "?" + params.Encode()
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", u, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}
	if out != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if err := json.Unmarshal(buf.Bytes(), out); err != nil {
			t.Fatalf("decoding %q through generated-client mirror types failed (the real client would fail identically): %v", buf.Bytes(), err)
		}
	}
	return resp.StatusCode, buf.Bytes()
}

func TestClientWireListAndGet(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed(testEmail, 12, 21)

	var ids []string
	pageToken := ""
	for {
		params := url.Values{"maxResults": {"5"}}
		if pageToken != "" {
			params.Set("pageToken", pageToken)
		}
		var resp genListMessagesResponse
		code, body := clientGet(t, f, "users/me/messages", params, &resp)
		if code != http.StatusOK {
			t.Fatalf("list: status %d body %s", code, body)
		}
		for _, m := range resp.Messages {
			ids = append(ids, m.Id)
		}
		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}
	if len(ids) != 12 {
		t.Fatalf("listed %d ids, want 12", len(ids))
	}

	var msg genMessage
	code, body := clientGet(t, f, "users/me/messages/"+ids[0], url.Values{"format": {"full"}}, &msg)
	if code != http.StatusOK {
		t.Fatalf("get: status %d body %s", code, body)
	}
	// These four prove the `,string` envelope works end to end.
	if msg.HistoryId == 0 || msg.InternalDate == 0 || msg.Id != ids[0] || msg.ThreadId == "" {
		t.Fatalf("client-mirror decode incomplete: %+v", msg)
	}
	if msg.Payload == nil || msg.Payload.MimeType != "text/plain" || msg.Payload.Body == nil {
		t.Fatalf("payload: %+v", msg.Payload)
	}
	if _, err := base64.RawURLEncoding.DecodeString(msg.Payload.Body.Data); err != nil {
		t.Fatalf("body data not base64url: %v", err)
	}
}

func TestClientWireEmailUserIDPercentEncoded(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed(testEmail, 1, 1)

	// googleapi.Expand path-escapes {userId}; an email arrives as
	// alice%40example.com. The router must decode and accept it.
	var resp genListMessagesResponse
	code, body := clientGet(t, f, "users/"+url.PathEscape(testEmail)+"/messages", nil, &resp)
	if code != http.StatusOK {
		t.Fatalf("status %d body %s", code, body)
	}
	if len(resp.Messages) != 1 {
		t.Fatalf("got %d messages, want 1", len(resp.Messages))
	}
}

func TestClientWireHistory(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	msgA := f.addMessage(testEmail, "a", "body a")
	start := msgA.HistoryID
	f.editMessage(testEmail, msgA.ID, "", "body a v2")
	msgB := f.addMessage(testEmail, "b", "body b")
	f.deleteMessage(testEmail, msgB.ID)

	params := url.Values{
		"startHistoryId": {fmt.Sprintf("%d", start)},
		// SetMulti: the client repeats the key per value.
		"historyTypes": {"messageAdded", "messageDeleted"},
	}
	var resp genListHistoryResponse
	code, body := clientGet(t, f, "users/me/history", params, &resp)
	if code != http.StatusOK {
		t.Fatalf("history: status %d body %s", code, body)
	}
	if resp.HistoryId == 0 {
		t.Fatal("response historyId did not decode through `,string`")
	}
	// Replay in id order; the last event per message id wins.
	state := map[string]string{}
	var last uint64
	for _, h := range resp.History {
		if h.Id <= last {
			t.Fatalf("history ids not increasing: %d after %d", h.Id, last)
		}
		last = h.Id
		for _, d := range h.MessagesDeleted {
			state[d.Message.Id] = "deleted"
		}
		for _, a := range h.MessagesAdded {
			state[a.Message.Id] = "added"
		}
	}
	if state[msgA.ID] != "added" {
		t.Errorf("edited message replays to %q, want added (re-ingest same doc_id)", state[msgA.ID])
	}
	if state[msgB.ID] != "deleted" {
		t.Errorf("deleted message replays to %q, want deleted (tombstone)", state[msgB.ID])
	}

	// Stale startHistoryId => 404 in googleapi error shape: the connector's
	// signal to fall back to a full sync.
	params = url.Values{"startHistoryId": {fmt.Sprintf("%d", resp.HistoryId+1000)}}
	code, body = clientGet(t, f, "users/me/history", params, nil)
	if code != http.StatusNotFound {
		t.Fatalf("stale history: status %d, want 404", code)
	}
	var gerr genErrorReply
	if err := json.Unmarshal(body, &gerr); err != nil || gerr.Error == nil {
		t.Fatalf("error body %q does not match googleapi errorReply: %v", body, err)
	}
	if gerr.Error.Code != http.StatusNotFound || len(gerr.Error.Errors) == 0 || gerr.Error.Errors[0].Reason == "" {
		t.Fatalf("error body incomplete: %+v", gerr.Error)
	}
}

func TestClientWireWatch(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	reqBody := bytes.NewReader([]byte(`{"topicName":"projects/fake/topics/asker","labelIds":["INBOX"]}`))
	req, err := http.NewRequest(http.MethodPost, f.ts.URL+"/gmail/v1/users/me/watch?alt=json&prettyPrint=false", reqBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(pushURLHeader, "http://127.0.0.1:1/unused")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("watch: status %d", resp.StatusCode)
	}
	var watch genWatchResponse
	if err := json.NewDecoder(resp.Body).Decode(&watch); err != nil {
		t.Fatalf("watch response does not decode through generated-client mirror: %v", err)
	}
	if watch.Expiration <= time.Now().UnixMilli() {
		t.Fatalf("expiration %d not in the future", watch.Expiration)
	}
}

func TestClientWireAuthError(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	req, err := http.NewRequest(http.MethodGet, f.ts.URL+"/gmail/v1/users/me/messages?alt=json", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer ya29.realgoogletoken")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", resp.StatusCode)
	}
	var gerr genErrorReply
	if err := json.NewDecoder(resp.Body).Decode(&gerr); err != nil || gerr.Error == nil {
		t.Fatalf("401 body does not match googleapi error shape: %v", err)
	}
	if gerr.Error.Code != http.StatusUnauthorized || gerr.Error.Message == "" {
		t.Fatalf("401 error body incomplete: %+v", gerr.Error)
	}
}
