package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testEmail = "alice@example.com"
	testToken = tokenPrefix + testEmail
)

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fixture is a fake-gmail server running on an httptest listener.
type fixture struct {
	t   *testing.T
	srv *Server
	ts  *httptest.Server
}

func newFixture(t *testing.T, opts ...Option) *fixture {
	t.Helper()
	opts = append([]Option{WithLogger(testLogger(t))}, opts...)
	srv := New(opts...)
	ts := httptest.NewServer(srv)
	t.Cleanup(func() {
		ts.Close()
		srv.Close()
	})
	return &fixture{t: t, srv: srv, ts: ts}
}

// do performs an HTTP request and decodes the JSON response into out (if
// non-nil), returning the status code.
func (f *fixture) do(method, path, token string, body any, out any) int {
	f.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			f.t.Fatalf("marshal request body: %v", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, f.ts.URL+path, rdr)
	if err != nil {
		f.t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		f.t.Fatalf("read response: %v", err)
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			f.t.Fatalf("decode response %q: %v", data, err)
		}
	}
	return resp.StatusCode
}

func (f *fixture) seed(email string, count int, seed int64) seedResponse {
	f.t.Helper()
	var resp seedResponse
	code := f.do(http.MethodPost, "/admin/users/"+email+"/seed", "", seedRequest{Count: count, Seed: seed}, &resp)
	if code != http.StatusOK {
		f.t.Fatalf("seed: status %d", code)
	}
	return resp
}

func (f *fixture) addMessage(email, subject, body string) wireMessage {
	f.t.Helper()
	var msg wireMessage
	code := f.do(http.MethodPost, "/admin/users/"+email+"/messages", "",
		addMessageRequest{Subject: subject, Body: body}, &msg)
	if code != http.StatusCreated {
		f.t.Fatalf("add message: status %d", code)
	}
	return msg
}

func (f *fixture) editMessage(email, id, subject, body string) wireMessage {
	f.t.Helper()
	var msg wireMessage
	code := f.do(http.MethodPut, "/admin/users/"+email+"/messages/"+id, "",
		editMessageRequest{Subject: subject, Body: body}, &msg)
	if code != http.StatusOK {
		f.t.Fatalf("edit message: status %d", code)
	}
	return msg
}

func (f *fixture) deleteMessage(email, id string) {
	f.t.Helper()
	if code := f.do(http.MethodDelete, "/admin/users/"+email+"/messages/"+id, "", nil, nil); code != http.StatusNoContent {
		f.t.Fatalf("delete message: status %d", code)
	}
}

func TestHealthz(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	if code := f.do(http.MethodGet, "/healthz", "", nil, nil); code != http.StatusOK {
		t.Fatalf("healthz: status %d, want 200", code)
	}
}

func TestListMessagesPagination(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed(testEmail, 25, 7)

	seen := make(map[string]bool)
	token := ""
	pages := 0
	for {
		path := "/gmail/v1/users/me/messages?maxResults=10"
		if token != "" {
			path += "&pageToken=" + token
		}
		var resp wireListMessagesResponse
		if code := f.do(http.MethodGet, path, testToken, nil, &resp); code != http.StatusOK {
			t.Fatalf("list: status %d", code)
		}
		if resp.ResultSizeEstimate != 25 {
			t.Fatalf("resultSizeEstimate = %d, want 25", resp.ResultSizeEstimate)
		}
		pages++
		for _, m := range resp.Messages {
			if m.ID == "" || m.ThreadID == "" {
				t.Fatalf("ref missing id/threadId: %+v", m)
			}
			if seen[m.ID] {
				t.Fatalf("duplicate id %q across pages", m.ID)
			}
			seen[m.ID] = true
		}
		if resp.NextPageToken == "" {
			if len(resp.Messages) != 5 {
				t.Fatalf("last page size = %d, want 5", len(resp.Messages))
			}
			break
		}
		if len(resp.Messages) != 10 {
			t.Fatalf("page size = %d, want 10", len(resp.Messages))
		}
		token = resp.NextPageToken
	}
	if pages != 3 || len(seen) != 25 {
		t.Fatalf("pages = %d, unique ids = %d; want 3 pages, 25 ids", pages, len(seen))
	}
}

func TestListMessagesOrderedNewestFirst(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed(testEmail, 10, 3)

	var resp wireListMessagesResponse
	if code := f.do(http.MethodGet, "/gmail/v1/users/me/messages?maxResults=500", testToken, nil, &resp); code != http.StatusOK {
		t.Fatalf("list: status %d", code)
	}
	var prev int64 = 1<<63 - 1
	for _, ref := range resp.Messages {
		var msg wireMessage
		if code := f.do(http.MethodGet, "/gmail/v1/users/me/messages/"+ref.ID+"?format=full", testToken, nil, &msg); code != http.StatusOK {
			t.Fatalf("get %s: status %d", ref.ID, code)
		}
		if msg.InternalDate > prev {
			t.Fatalf("messages not ordered newest-first: %d after %d", msg.InternalDate, prev)
		}
		prev = msg.InternalDate
	}
}

func TestListMessagesBadParams(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for _, path := range []string{
		"/gmail/v1/users/me/messages?maxResults=zero",
		"/gmail/v1/users/me/messages?maxResults=-3",
		"/gmail/v1/users/me/messages?pageToken=notanumber",
	} {
		var gerr googleError
		if code := f.do(http.MethodGet, path, testToken, nil, &gerr); code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", path, code)
		} else if gerr.Error.Code != http.StatusBadRequest {
			t.Errorf("%s: error body code %d, want 400", path, gerr.Error.Code)
		}
	}
}

func TestGetMessageFull(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	created := f.addMessage(testEmail, "lunch plans", "tacos at noon?")

	var msg wireMessage
	if code := f.do(http.MethodGet, "/gmail/v1/users/me/messages/"+created.ID+"?format=full", testToken, nil, &msg); code != http.StatusOK {
		t.Fatalf("get: status %d", code)
	}
	if msg.ID != created.ID || msg.ThreadID == "" {
		t.Fatalf("id/threadId wrong: %+v", msg)
	}
	if msg.HistoryID == 0 || msg.InternalDate == 0 {
		t.Fatalf("historyId/internalDate must be set: %+v", msg)
	}
	if msg.Payload == nil || msg.Payload.MimeType != "text/plain" {
		t.Fatalf("payload wrong: %+v", msg.Payload)
	}
	headers := map[string]string{}
	for _, h := range msg.Payload.Headers {
		headers[h.Name] = h.Value
	}
	for _, name := range []string{"From", "To", "Subject", "Date"} {
		if headers[name] == "" {
			t.Errorf("missing %s header", name)
		}
	}
	if headers["Subject"] != "lunch plans" {
		t.Errorf("Subject = %q", headers["Subject"])
	}
	if _, err := time.Parse(time.RFC1123Z, headers["Date"]); err != nil {
		t.Errorf("Date header %q not RFC1123Z: %v", headers["Date"], err)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(msg.Payload.Body.Data)
	if err != nil {
		t.Fatalf("body data not base64url: %v", err)
	}
	if string(decoded) != "tacos at noon?" {
		t.Fatalf("body = %q", decoded)
	}
	if msg.Payload.Body.Size != int64(len(decoded)) {
		t.Errorf("body size = %d, want %d", msg.Payload.Body.Size, len(decoded))
	}

	// Raw JSON must carry historyId/internalDate as STRINGS (the generated
	// client uses the `,string` option and fails on bare numbers).
	req, _ := http.NewRequest(http.MethodGet, f.ts.URL+"/gmail/v1/users/me/messages/"+created.ID, nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	if _, ok := generic["historyId"].(string); !ok {
		t.Errorf("historyId must serialize as a JSON string, got %T", generic["historyId"])
	}
	if _, ok := generic["internalDate"].(string); !ok {
		t.Errorf("internalDate must serialize as a JSON string, got %T", generic["internalDate"])
	}
}

func TestGetMessageNotFound(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	var gerr googleError
	code := f.do(http.MethodGet, "/gmail/v1/users/me/messages/doesnotexist", testToken, nil, &gerr)
	if code != http.StatusNotFound || gerr.Error.Code != http.StatusNotFound {
		t.Fatalf("status %d, body %+v; want 404", code, gerr)
	}
}

func TestHistoryAddEditDeleteSequence(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	msgA := f.addMessage(testEmail, "first", "body a") // history 1: added A
	start := msgA.HistoryID
	msgB := f.addMessage(testEmail, "second", "body b")          // history 2: added B
	edited := f.editMessage(testEmail, msgA.ID, "", "body a v2") // history 3: deleted A, history 4: added A
	f.deleteMessage(testEmail, msgB.ID)                          // history 5: deleted B

	if edited.ID != msgA.ID {
		t.Fatalf("edit changed the message id: %q -> %q", msgA.ID, edited.ID)
	}
	if edited.HistoryID <= msgA.HistoryID {
		t.Fatalf("edit must bump the message historyId: %d -> %d", msgA.HistoryID, edited.HistoryID)
	}

	var resp wireListHistoryResponse
	path := fmt.Sprintf("/gmail/v1/users/me/history?startHistoryId=%d", start)
	if code := f.do(http.MethodGet, path, testToken, nil, &resp); code != http.StatusOK {
		t.Fatalf("history: status %d", code)
	}
	if len(resp.History) != 4 {
		t.Fatalf("got %d history entries, want 4: %+v", len(resp.History), resp.History)
	}
	type change struct {
		added, deleted string
	}
	var got []change
	for i, h := range resp.History {
		if i > 0 && h.ID <= resp.History[i-1].ID {
			t.Fatalf("history ids not increasing: %d then %d", resp.History[i-1].ID, h.ID)
		}
		var c change
		for _, a := range h.MessagesAdded {
			c.added = a.Message.ID
		}
		for _, d := range h.MessagesDeleted {
			c.deleted = d.Message.ID
		}
		got = append(got, c)
	}
	want := []change{
		{added: msgB.ID},   // add B
		{deleted: msgA.ID}, // edit A: delete ...
		{added: msgA.ID},   // ... then re-add SAME id
		{deleted: msgB.ID}, // delete B
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if resp.HistoryID != 5 {
		t.Errorf("current historyId = %d, want 5", resp.HistoryID)
	}

	// The edited message is re-fetchable under the same id with new content.
	var after wireMessage
	if code := f.do(http.MethodGet, "/gmail/v1/users/me/messages/"+msgA.ID, testToken, nil, &after); code != http.StatusOK {
		t.Fatalf("get after edit: status %d", code)
	}
	decoded, _ := base64.RawURLEncoding.DecodeString(after.Payload.Body.Data)
	if string(decoded) != "body a v2" {
		t.Errorf("body after edit = %q", decoded)
	}
	// The deleted message is gone.
	if code := f.do(http.MethodGet, "/gmail/v1/users/me/messages/"+msgB.ID, testToken, nil, nil); code != http.StatusNotFound {
		t.Errorf("deleted message GET: status %d, want 404", code)
	}
}

func TestHistoryTypesFilter(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	msg := f.addMessage(testEmail, "x", "y") // 1: added
	f.deleteMessage(testEmail, msg.ID)       // 2: deleted

	var onlyAdded wireListHistoryResponse
	if code := f.do(http.MethodGet, "/gmail/v1/users/me/history?startHistoryId=0&historyTypes=messageAdded", testToken, nil, &onlyAdded); code != http.StatusOK {
		t.Fatalf("history: status %d", code)
	}
	if len(onlyAdded.History) != 1 || len(onlyAdded.History[0].MessagesAdded) != 1 || len(onlyAdded.History[0].MessagesDeleted) != 0 {
		t.Fatalf("historyTypes=messageAdded gave %+v", onlyAdded.History)
	}

	var onlyDeleted wireListHistoryResponse
	if code := f.do(http.MethodGet, "/gmail/v1/users/me/history?startHistoryId=0&historyTypes=messageDeleted", testToken, nil, &onlyDeleted); code != http.StatusOK {
		t.Fatalf("history: status %d", code)
	}
	if len(onlyDeleted.History) != 1 || len(onlyDeleted.History[0].MessagesDeleted) != 1 || len(onlyDeleted.History[0].MessagesAdded) != 0 {
		t.Fatalf("historyTypes=messageDeleted gave %+v", onlyDeleted.History)
	}
}

func TestHistoryPagination(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	var ids []string
	for i := 0; i < 7; i++ {
		ids = append(ids, f.addMessage(testEmail, fmt.Sprintf("m%d", i), "b").ID)
	}
	var all []string
	token := ""
	for {
		path := "/gmail/v1/users/me/history?startHistoryId=0&maxResults=3"
		if token != "" {
			path += "&pageToken=" + token
		}
		var resp wireListHistoryResponse
		if code := f.do(http.MethodGet, path, testToken, nil, &resp); code != http.StatusOK {
			t.Fatalf("history: status %d", code)
		}
		for _, h := range resp.History {
			for _, a := range h.MessagesAdded {
				all = append(all, a.Message.ID)
			}
		}
		if resp.NextPageToken == "" {
			break
		}
		token = resp.NextPageToken
	}
	if len(all) != len(ids) {
		t.Fatalf("collected %d adds, want %d", len(all), len(ids))
	}
	for i := range ids {
		if all[i] != ids[i] {
			t.Errorf("history order mismatch at %d: %q != %q", i, all[i], ids[i])
		}
	}
}

func TestHistoryStaleStartHistoryID(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.addMessage(testEmail, "a", "b") // historyId 1

	// Beyond the current historyId: 404 like the real API.
	var gerr googleError
	code := f.do(http.MethodGet, "/gmail/v1/users/me/history?startHistoryId=99999", testToken, nil, &gerr)
	if code != http.StatusNotFound {
		t.Fatalf("future startHistoryId: status %d, want 404", code)
	}

	// Missing / malformed startHistoryId: 400.
	if code := f.do(http.MethodGet, "/gmail/v1/users/me/history", testToken, nil, nil); code != http.StatusBadRequest {
		t.Fatalf("missing startHistoryId: status %d, want 400", code)
	}
	if code := f.do(http.MethodGet, "/gmail/v1/users/me/history?startHistoryId=abc", testToken, nil, nil); code != http.StatusBadRequest {
		t.Fatalf("bad startHistoryId: status %d, want 400", code)
	}
}

func TestHistoryPrunedStartHistoryID(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	const email = "pruned@example.com"
	// Overflow the capped log directly through the store (HTTP seeding 10k+
	// messages works too but is needlessly slow for a unit test).
	f.srv.store.seedMessages(email, generateSeedMessages(email, 11, maxHistoryEntries+5))

	token := tokenPrefix + email
	// Entries 1..5 were pruned; startHistoryId below prunedThrough is stale.
	if code := f.do(http.MethodGet, "/gmail/v1/users/me/history?startHistoryId=4", token, nil, nil); code != http.StatusNotFound {
		t.Fatalf("pruned startHistoryId: status %d, want 404", code)
	}
	// startHistoryId == prunedThrough is the oldest replayable point.
	var resp wireListHistoryResponse
	if code := f.do(http.MethodGet, "/gmail/v1/users/me/history?startHistoryId=5&maxResults=1", token, nil, &resp); code != http.StatusOK {
		t.Fatalf("oldest replayable startHistoryId: status %d, want 200", code)
	}
	if len(resp.History) != 1 || resp.History[0].ID != 6 {
		t.Fatalf("first replayable entry = %+v, want id 6", resp.History)
	}
}

func (f *fixture) getProfile(token string) (wireProfile, int) {
	f.t.Helper()
	var prof wireProfile
	code := f.do(http.MethodGet, "/gmail/v1/users/me/profile", token, nil, &prof)
	return prof, code
}

func TestGetProfileShape(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	// Empty mailbox: zero totals, historyId 0.
	prof, code := f.getProfile(testToken)
	if code != http.StatusOK {
		t.Fatalf("profile: status %d", code)
	}
	if prof.EmailAddress != testEmail {
		t.Errorf("emailAddress = %q, want %q", prof.EmailAddress, testEmail)
	}
	if prof.MessagesTotal != 0 || prof.ThreadsTotal != 0 || prof.HistoryID != 0 {
		t.Errorf("empty mailbox profile = %+v, want zero totals and historyId", prof)
	}

	f.seed(testEmail, 4, 11)

	// Raw JSON: historyId must be a STRING (the generated client's Profile
	// declares it with the `,string` option); the totals are plain numbers.
	req, _ := http.NewRequest(http.MethodGet, f.ts.URL+"/gmail/v1/users/me/profile", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	if got, ok := generic["historyId"].(string); !ok || got != "4" {
		t.Errorf("historyId = %#v, want JSON string \"4\"", generic["historyId"])
	}
	if got, ok := generic["messagesTotal"].(float64); !ok || got != 4 {
		t.Errorf("messagesTotal = %#v, want JSON number 4", generic["messagesTotal"])
	}
	if got, ok := generic["threadsTotal"].(float64); !ok || got != 4 {
		t.Errorf("threadsTotal = %#v, want JSON number 4", generic["threadsTotal"])
	}
	if got, ok := generic["emailAddress"].(string); !ok || got != testEmail {
		t.Errorf("emailAddress = %#v, want %q", generic["emailAddress"], testEmail)
	}
}

func TestGetProfileTracksAdminMutations(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	msgA := f.addMessage(testEmail, "first", "body a") // history 1
	prof, _ := f.getProfile(testToken)
	if prof.HistoryID != msgA.HistoryID || prof.MessagesTotal != 1 || prof.ThreadsTotal != 1 {
		t.Fatalf("after add: profile = %+v, want historyId %d and totals 1/1", prof, msgA.HistoryID)
	}

	msgB := f.addMessage(testEmail, "second", "body b") // history 2
	edited := f.editMessage(testEmail, msgA.ID, "", "body a v2")
	// The edit appends delete+add entries; the profile must report the
	// CURRENT mailbox historyId (the add entry), not a stale one.
	prof, _ = f.getProfile(testToken)
	if prof.HistoryID != edited.HistoryID {
		t.Errorf("after edit: profile historyId = %d, want %d", prof.HistoryID, edited.HistoryID)
	}
	if prof.MessagesTotal != 2 || prof.ThreadsTotal != 2 {
		t.Errorf("after edit: totals = %d/%d, want 2/2", prof.MessagesTotal, prof.ThreadsTotal)
	}

	f.deleteMessage(testEmail, msgB.ID) // one more history entry
	prof, _ = f.getProfile(testToken)
	if prof.HistoryID != edited.HistoryID+1 {
		t.Errorf("after delete: profile historyId = %d, want %d", prof.HistoryID, edited.HistoryID+1)
	}
	if prof.MessagesTotal != 1 || prof.ThreadsTotal != 1 {
		t.Errorf("after delete: totals = %d/%d, want 1/1", prof.MessagesTotal, prof.ThreadsTotal)
	}

	// The profile and history.list views of "current historyId" agree.
	var hist wireListHistoryResponse
	if code := f.do(http.MethodGet, "/gmail/v1/users/me/history?startHistoryId="+fmt.Sprint(msgA.HistoryID), testToken, nil, &hist); code != http.StatusOK {
		t.Fatalf("history: status %d", code)
	}
	if hist.HistoryID != prof.HistoryID {
		t.Errorf("history.list historyId = %d, profile historyId = %d; must agree", hist.HistoryID, prof.HistoryID)
	}
}

func TestGetProfileAuthMapping(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.addMessage(testEmail, "hello", "world")

	cases := []struct {
		name   string
		token  string
		userID string
		want   int
	}{
		{"no token", "", "me", http.StatusUnauthorized},
		{"wrong prefix", "some-google-token", "me", http.StatusUnauthorized},
		{"valid me", testToken, "me", http.StatusOK},
		{"valid explicit self", testToken, testEmail, http.StatusOK},
		{"explicit other user", testToken, "bob@example.com", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gerr googleError
			code := f.do(http.MethodGet, "/gmail/v1/users/"+tc.userID+"/profile", tc.token, nil, &gerr)
			if code != tc.want {
				t.Fatalf("status %d, want %d", code, tc.want)
			}
			if tc.want != http.StatusOK && gerr.Error.Code != tc.want {
				t.Fatalf("google error body code = %d, want %d", gerr.Error.Code, tc.want)
			}
		})
	}
}

func TestAuthMapping(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.addMessage(testEmail, "hello", "world")

	cases := []struct {
		name   string
		token  string
		userID string
		want   int
	}{
		{"no token", "", "me", http.StatusUnauthorized},
		{"wrong prefix", "some-google-token", "me", http.StatusUnauthorized},
		{"empty email", tokenPrefix, "me", http.StatusUnauthorized},
		{"not an email", tokenPrefix + "nodomain", "me", http.StatusUnauthorized},
		{"valid me", testToken, "me", http.StatusOK},
		{"valid explicit self", testToken, testEmail, http.StatusOK},
		{"explicit other user", testToken, "bob@example.com", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gerr googleError
			code := f.do(http.MethodGet, "/gmail/v1/users/"+tc.userID+"/messages", tc.token, nil, &gerr)
			if code != tc.want {
				t.Fatalf("status %d, want %d", code, tc.want)
			}
			if tc.want != http.StatusOK && gerr.Error.Code != tc.want {
				t.Fatalf("google error body code = %d, want %d", gerr.Error.Code, tc.want)
			}
		})
	}
}

func TestWatchAndPushDelivery(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	type delivery struct {
		envelope pushEnvelope
		path     string
	}
	got := make(chan delivery, 10)
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var env pushEnvelope
		if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
			t.Errorf("decode push envelope: %v", err)
		}
		got <- delivery{envelope: env, path: r.URL.Path}
		w.WriteHeader(http.StatusOK)
	}))
	defer recv.Close()

	// Register the watch with the push-url shim header.
	req, _ := http.NewRequest(http.MethodPost, f.ts.URL+"/gmail/v1/users/me/watch",
		strings.NewReader(`{"topicName":"projects/fake/topics/asker"}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set(pushURLHeader, recv.URL+"/notify")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var watch wireWatchResponse
	if err := json.NewDecoder(resp.Body).Decode(&watch); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("watch: status %d", resp.StatusCode)
	}
	if watch.Expiration <= time.Now().UnixMilli() {
		t.Fatalf("watch expiration %d not in the future", watch.Expiration)
	}

	// A mailbox change triggers an async Pub/Sub-style push.
	msg := f.addMessage(testEmail, "ping", "pong")
	select {
	case d := <-got:
		if d.path != "/notify" {
			t.Errorf("push path = %q", d.path)
		}
		if d.envelope.Message.MessageID == "" {
			t.Error("push messageId empty")
		}
		raw, err := base64.StdEncoding.DecodeString(d.envelope.Message.Data)
		if err != nil {
			t.Fatalf("push data not std base64: %v", err)
		}
		var data pushData
		if err := json.Unmarshal(raw, &data); err != nil {
			t.Fatalf("push data not JSON: %v", err)
		}
		if data.EmailAddress != testEmail {
			t.Errorf("push emailAddress = %q", data.EmailAddress)
		}
		if data.HistoryID != msg.HistoryID {
			t.Errorf("push historyId = %d, want %d", data.HistoryID, msg.HistoryID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("push not delivered within 5s")
	}
}

func TestWatchValidation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	// Missing topicName.
	req, _ := http.NewRequest(http.MethodPost, f.ts.URL+"/gmail/v1/users/me/watch", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set(pushURLHeader, "http://example.com/push")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("watch without topicName: status %d, want 400", resp.StatusCode)
	}

	// Missing the X-Asker-Push-Url shim header.
	req, _ = http.NewRequest(http.MethodPost, f.ts.URL+"/gmail/v1/users/me/watch",
		strings.NewReader(`{"topicName":"projects/fake/topics/asker"}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("watch without push url header: status %d, want 400", resp.StatusCode)
	}
}

func TestPushRetriesThenSucceeds(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.srv.pusher.backoff = time.Millisecond

	var calls atomic.Int32
	delivered := make(chan struct{}, 1)
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
		delivered <- struct{}{}
	}))
	defer recv.Close()

	f.srv.store.setWatch(testEmail, "t", recv.URL, time.Now().Add(time.Hour))
	f.addMessage(testEmail, "retry me", "x")

	select {
	case <-delivered:
		if calls.Load() != 3 {
			t.Fatalf("delivered after %d calls, want 3", calls.Load())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("push not delivered after retries")
	}
}

func TestPushGivesUpAfterMaxAttempts(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.srv.pusher.backoff = time.Millisecond

	var calls atomic.Int32
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer recv.Close()

	f.srv.store.setWatch(testEmail, "t", recv.URL, time.Now().Add(time.Hour))
	f.addMessage(testEmail, "doomed", "x")
	f.srv.Close() // drain pushes
	if calls.Load() != pushAttempts {
		t.Fatalf("push attempted %d times, want %d", calls.Load(), pushAttempts)
	}
}

func TestExpiredWatchGetsNoPush(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	var calls atomic.Int32
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
	}))
	defer recv.Close()

	f.srv.store.setWatch(testEmail, "t", recv.URL, time.Now().Add(-time.Minute))
	f.addMessage(testEmail, "late", "x")
	f.srv.Close()
	if calls.Load() != 0 {
		t.Fatalf("expired watch received %d pushes, want 0", calls.Load())
	}
}

func TestSeedDeterminism(t *testing.T) {
	t.Parallel()

	type snapshot struct {
		ids      []string
		subjects map[string]string
		bodies   map[string]string
		dates    map[string]int64
	}
	collect := func(seed int64, count int) snapshot {
		f := newFixture(t)
		f.seed(testEmail, count, seed)
		var list wireListMessagesResponse
		if code := f.do(http.MethodGet, "/gmail/v1/users/me/messages?maxResults=500", testToken, nil, &list); code != http.StatusOK {
			t.Fatalf("list: status %d", code)
		}
		snap := snapshot{
			subjects: map[string]string{},
			bodies:   map[string]string{},
			dates:    map[string]int64{},
		}
		for _, ref := range list.Messages {
			var msg wireMessage
			if code := f.do(http.MethodGet, "/gmail/v1/users/me/messages/"+ref.ID, testToken, nil, &msg); code != http.StatusOK {
				t.Fatalf("get: status %d", code)
			}
			snap.ids = append(snap.ids, msg.ID)
			for _, h := range msg.Payload.Headers {
				if h.Name == "Subject" {
					snap.subjects[msg.ID] = h.Value
				}
			}
			body, _ := base64.RawURLEncoding.DecodeString(msg.Payload.Body.Data)
			snap.bodies[msg.ID] = string(body)
			snap.dates[msg.ID] = msg.InternalDate
		}
		return snap
	}

	a := collect(42, 8)
	b := collect(42, 8)
	if len(a.ids) != 8 || len(b.ids) != 8 {
		t.Fatalf("expected 8 messages, got %d and %d", len(a.ids), len(b.ids))
	}
	for i := range a.ids {
		if a.ids[i] != b.ids[i] {
			t.Fatalf("same (seed,count) produced different ids: %q vs %q", a.ids[i], b.ids[i])
		}
	}
	for id := range a.subjects {
		if a.subjects[id] != b.subjects[id] {
			t.Errorf("subject mismatch for %s", id)
		}
		if a.bodies[id] != b.bodies[id] {
			t.Errorf("body mismatch for %s", id)
		}
		if a.dates[id] != b.dates[id] {
			t.Errorf("date mismatch for %s", id)
		}
	}

	c := collect(43, 8)
	same := 0
	for i := range a.ids {
		if i < len(c.ids) && a.ids[i] == c.ids[i] {
			same++
		}
	}
	if same == len(a.ids) {
		t.Fatal("different seeds produced identical id sequences")
	}
}

func TestSeedRareTokensTargetSingleMessages(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	const count = 6
	f.seed(testEmail, count, 99)

	var list wireListMessagesResponse
	if code := f.do(http.MethodGet, "/gmail/v1/users/me/messages?maxResults=500", testToken, nil, &list); code != http.StatusOK {
		t.Fatalf("list: status %d", code)
	}
	bodies := make([]string, 0, count)
	for _, ref := range list.Messages {
		var msg wireMessage
		if code := f.do(http.MethodGet, "/gmail/v1/users/me/messages/"+ref.ID, testToken, nil, &msg); code != http.StatusOK {
			t.Fatalf("get: status %d", code)
		}
		body, _ := base64.RawURLEncoding.DecodeString(msg.Payload.Body.Data)
		bodies = append(bodies, string(body))

		// Dates land within the seeded two-year window.
		dt := time.UnixMilli(msg.InternalDate)
		if dt.After(seedAnchor) || dt.Before(seedAnchor.Add(-seedSpread)) {
			t.Errorf("internalDate %v outside seed window", dt)
		}
	}
	for i := 0; i < count; i++ {
		token := RareToken(i)
		hits := 0
		for _, body := range bodies {
			if strings.Contains(body, token) {
				hits++
			}
		}
		if hits != 1 {
			t.Errorf("rare token %s found in %d messages, want exactly 1", token, hits)
		}
	}
}

func TestReseedSameSeedActsAsEdit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	first := f.seed(testEmail, 3, 5) // 3 adds -> historyId 3
	second := f.seed(testEmail, 3, 5)

	if second.HistoryID <= first.HistoryID {
		t.Fatalf("re-seed must advance historyId: %d -> %d", first.HistoryID, second.HistoryID)
	}
	// Still exactly 3 live messages, not 6.
	var list wireListMessagesResponse
	if code := f.do(http.MethodGet, "/gmail/v1/users/me/messages", testToken, nil, &list); code != http.StatusOK {
		t.Fatalf("list: status %d", code)
	}
	if list.ResultSizeEstimate != 3 {
		t.Fatalf("after re-seed: %d messages, want 3", list.ResultSizeEstimate)
	}
	// And the history shows delete+add pairs (edits), not plain duplicates.
	var hist wireListHistoryResponse
	path := fmt.Sprintf("/gmail/v1/users/me/history?startHistoryId=%d", first.HistoryID)
	if code := f.do(http.MethodGet, path, testToken, nil, &hist); code != http.StatusOK {
		t.Fatalf("history: status %d", code)
	}
	var adds, deletes int
	for _, h := range hist.History {
		adds += len(h.MessagesAdded)
		deletes += len(h.MessagesDeleted)
	}
	if adds != 3 || deletes != 3 {
		t.Fatalf("re-seed history: %d adds, %d deletes; want 3 and 3", adds, deletes)
	}
}

func TestAdminValidation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	cases := []struct {
		name   string
		method string
		path   string
		body   any
		want   int
	}{
		{"seed bad email", http.MethodPost, "/admin/users/notanemail/seed", seedRequest{Count: 1}, http.StatusBadRequest},
		{"seed zero count", http.MethodPost, "/admin/users/" + testEmail + "/seed", seedRequest{Count: 0}, http.StatusBadRequest},
		{"seed huge count", http.MethodPost, "/admin/users/" + testEmail + "/seed", seedRequest{Count: maxSeedCount + 1}, http.StatusBadRequest},
		{"add empty message", http.MethodPost, "/admin/users/" + testEmail + "/messages", addMessageRequest{}, http.StatusBadRequest},
		{"edit missing message", http.MethodPut, "/admin/users/" + testEmail + "/messages/nope", editMessageRequest{Subject: "s"}, http.StatusNotFound},
		{"edit empty patch", http.MethodPut, "/admin/users/" + testEmail + "/messages/nope", editMessageRequest{}, http.StatusBadRequest},
		{"delete missing message", http.MethodDelete, "/admin/users/" + testEmail + "/messages/nope", nil, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var aerr adminError
			code := f.do(tc.method, tc.path, "", tc.body, &aerr)
			if code != tc.want {
				t.Fatalf("status %d, want %d", code, tc.want)
			}
			if aerr.Error == "" {
				t.Fatal("admin error body must carry an error message")
			}
		})
	}

	// Malformed JSON bodies.
	for _, path := range []string{
		"/admin/users/" + testEmail + "/seed",
		"/admin/users/" + testEmail + "/messages",
	} {
		req, _ := http.NewRequest(http.MethodPost, f.ts.URL+path, strings.NewReader("{not json"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s with bad JSON: status %d, want 400", path, resp.StatusCode)
		}
	}
}

func TestAdminAddMessageDefaults(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	msg := f.addMessage(testEmail, "subject only", "")
	headers := map[string]string{}
	for _, h := range msg.Payload.Headers {
		headers[h.Name] = h.Value
	}
	if headers["From"] == "" {
		t.Error("From default not applied")
	}
	if headers["To"] != testEmail {
		t.Errorf("To = %q, want %q", headers["To"], testEmail)
	}
	if msg.ThreadID != msg.ID {
		t.Errorf("threadId = %q, want message id %q", msg.ThreadID, msg.ID)
	}
}
