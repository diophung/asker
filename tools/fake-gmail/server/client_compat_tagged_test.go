//go:build gmail_compat

package server

// Compatibility tests: drive the fake through the REAL generated client
// (google.golang.org/api/gmail/v1) pointed at the fake via
// option.WithEndpoint, exactly how the Asker Gmail connector will use it.
//
// BUILD TAG: kept opt-in because the suite drives the full generated client
// (slower, network-stack heavy). go.mod carries all required deps; run it
// with:
//
//	go test -tags gmail_compat ./tools/fake-gmail/server/
//
// compat_test.go provides always-on coverage by replicating the
// generated client's exact wire behavior (verified against the module-cache
// source; see citations there).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	gmail "google.golang.org/api/gmail/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// bearerTransport injects the dev-shim bearer token like the connector's
// oauth2 transport would.
type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (t bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(clone)
}

func newGmailService(t *testing.T, baseURL, token string) *gmail.Service {
	t.Helper()
	client := &http.Client{Transport: bearerTransport{token: token, base: http.DefaultTransport}}
	svc, err := gmail.NewService(context.Background(),
		option.WithEndpoint(baseURL),
		option.WithHTTPClient(client),
	)
	if err != nil {
		t.Fatalf("gmail.NewService: %v", err)
	}
	return svc
}

func TestGeneratedClientListAndGet(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed(testEmail, 12, 21)
	svc := newGmailService(t, f.ts.URL, testToken)

	// Paginated list through the generated client.
	var ids []string
	pageToken := ""
	for {
		call := svc.Users.Messages.List("me").MaxResults(5)
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}
		resp, err := call.Do()
		if err != nil {
			t.Fatalf("messages.list: %v", err)
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
		t.Fatalf("listed %d messages, want 12", len(ids))
	}

	// Full fetch decodes through the client's `,string` number handling.
	msg, err := svc.Users.Messages.Get("me", ids[0]).Format("full").Do()
	if err != nil {
		t.Fatalf("messages.get: %v", err)
	}
	if msg.Id != ids[0] || msg.ThreadId == "" || msg.HistoryId == 0 || msg.InternalDate == 0 {
		t.Fatalf("message fields not decoded: %+v", msg)
	}
	if msg.Payload == nil || msg.Payload.MimeType != "text/plain" {
		t.Fatalf("payload: %+v", msg.Payload)
	}
	var subject, from, to string
	for _, h := range msg.Payload.Headers {
		switch h.Name {
		case "Subject":
			subject = h.Value
		case "From":
			from = h.Value
		case "To":
			to = h.Value
		}
	}
	if subject == "" || from == "" || to == "" {
		t.Fatalf("missing headers: subject=%q from=%q to=%q", subject, from, to)
	}
	body, err := base64.RawURLEncoding.DecodeString(msg.Payload.Body.Data)
	if err != nil {
		t.Fatalf("body not base64url: %v", err)
	}
	if len(body) == 0 {
		t.Fatal("empty body")
	}
}

func TestGeneratedClientGetProfile(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed(testEmail, 3, 7)
	svc := newGmailService(t, f.ts.URL, testToken)

	prof, err := svc.Users.GetProfile("me").Do()
	if err != nil {
		t.Fatalf("users.getProfile: %v", err)
	}
	if prof.EmailAddress != testEmail {
		t.Errorf("emailAddress = %q, want %q", prof.EmailAddress, testEmail)
	}
	if prof.HistoryId == 0 {
		t.Error("historyId not decoded (must serialize as a JSON string)")
	}
	if prof.MessagesTotal != 3 || prof.ThreadsTotal != 3 {
		t.Errorf("totals = %d/%d, want 3/3", prof.MessagesTotal, prof.ThreadsTotal)
	}

	// The profile historyId tracks admin mutations.
	msg := f.addMessage(testEmail, "bump", "history")
	prof, err = svc.Users.GetProfile("me").Do()
	if err != nil {
		t.Fatalf("users.getProfile after add: %v", err)
	}
	if prof.HistoryId != msg.HistoryID {
		t.Errorf("historyId after add = %d, want %d", prof.HistoryId, msg.HistoryID)
	}
	if prof.MessagesTotal != 4 {
		t.Errorf("messagesTotal after add = %d, want 4", prof.MessagesTotal)
	}
}

func TestGeneratedClientHistoryFlow(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	svc := newGmailService(t, f.ts.URL, testToken)

	msgA := f.addMessage(testEmail, "a", "body a")
	start := msgA.HistoryID
	f.editMessage(testEmail, msgA.ID, "", "body a v2")
	msgB := f.addMessage(testEmail, "b", "body b")
	f.deleteMessage(testEmail, msgB.ID)

	resp, err := svc.Users.History.List("me").
		StartHistoryId(start).
		HistoryTypes("messageAdded", "messageDeleted").
		Do()
	if err != nil {
		t.Fatalf("history.list: %v", err)
	}
	// Replay in order, last event per id wins.
	state := map[string]string{}
	var lastID uint64
	for _, h := range resp.History {
		if h.Id <= lastID {
			t.Fatalf("history ids not increasing: %d after %d", h.Id, lastID)
		}
		lastID = h.Id
		for _, d := range h.MessagesDeleted {
			state[d.Message.Id] = "deleted"
		}
		for _, a := range h.MessagesAdded {
			state[a.Message.Id] = "added"
		}
	}
	if state[msgA.ID] != "added" {
		t.Errorf("edited message %s replays to %q, want added (re-ingest)", msgA.ID, state[msgA.ID])
	}
	if state[msgB.ID] != "deleted" {
		t.Errorf("deleted message %s replays to %q, want deleted (tombstone)", msgB.ID, state[msgB.ID])
	}
	if resp.HistoryId == 0 {
		t.Error("response historyId not decoded")
	}

	// Stale startHistoryId surfaces as *googleapi.Error 404: the connector's
	// trigger to fall back to a full sync.
	_, err = svc.Users.History.List("me").StartHistoryId(resp.HistoryId + 1000).Do()
	var gerr *googleapi.Error
	if !errors.As(err, &gerr) || gerr.Code != http.StatusNotFound {
		t.Fatalf("stale startHistoryId error = %v, want *googleapi.Error 404", err)
	}
}

func TestGeneratedClientWatchAndPush(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	svc := newGmailService(t, f.ts.URL, testToken)

	got := make(chan pushEnvelope, 5)
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var env pushEnvelope
		if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
			t.Errorf("decode envelope: %v", err)
		}
		got <- env
	}))
	defer recv.Close()

	call := svc.Users.Watch("me", &gmail.WatchRequest{TopicName: "projects/fake/topics/asker"})
	call.Header().Set(pushURLHeader, recv.URL)
	watch, err := call.Do()
	if err != nil {
		t.Fatalf("users.watch: %v", err)
	}
	if watch.Expiration == 0 {
		t.Fatal("watch expiration not decoded")
	}

	msg := f.addMessage(testEmail, "watched", "change")
	select {
	case env := <-got:
		raw, err := base64.StdEncoding.DecodeString(env.Message.Data)
		if err != nil {
			t.Fatalf("data not std base64: %v", err)
		}
		var data pushData
		if err := json.Unmarshal(raw, &data); err != nil {
			t.Fatalf("data not JSON: %v", err)
		}
		if data.EmailAddress != testEmail || data.HistoryID != msg.HistoryID {
			t.Fatalf("push data = %+v, want email %s history %d", data, testEmail, msg.HistoryID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("push not delivered within 5s")
	}
}

func TestGeneratedClientAuthErrors(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	// A non-shim token must produce a Google-shaped 401.
	svc := newGmailService(t, f.ts.URL, "ya29.realgoogletoken")
	_, err := svc.Users.Messages.List("me").Do()
	var gerr *googleapi.Error
	if !errors.As(err, &gerr) || gerr.Code != http.StatusUnauthorized {
		t.Fatalf("err = %v, want *googleapi.Error 401", err)
	}

	// Acting on another user's mailbox is forbidden.
	svc = newGmailService(t, f.ts.URL, testToken)
	_, err = svc.Users.Messages.List("bob@example.com").Do()
	if !errors.As(err, &gerr) || gerr.Code != http.StatusForbidden {
		t.Fatalf("err = %v, want *googleapi.Error 403", err)
	}
}
