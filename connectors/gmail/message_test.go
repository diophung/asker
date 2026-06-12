package gmail

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"

	gmailapi "google.golang.org/api/gmail/v1"

	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

func b64url(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func plainMessage(id string, headers map[string]string, body string) *gmailapi.Message {
	payload := &gmailapi.MessagePart{
		MimeType: "text/plain",
		Body:     &gmailapi.MessagePartBody{Data: b64url(body)},
	}
	for name, value := range headers {
		payload.Headers = append(payload.Headers, &gmailapi.MessagePartHeader{Name: name, Value: value})
	}
	return &gmailapi.Message{
		Id:           id,
		ThreadId:     "thread-" + id,
		HistoryId:    7,
		InternalDate: 1718000000000,
		Payload:      payload,
	}
}

func TestMessageDocumentMultipartPrefersPlainText(t *testing.T) {
	t.Parallel()
	msg := &gmailapi.Message{
		Id:        "m1",
		ThreadId:  "t1",
		HistoryId: 3,
		Payload: &gmailapi.MessagePart{
			MimeType: "multipart/alternative",
			Headers: []*gmailapi.MessagePartHeader{
				{Name: "Subject", Value: "hello"},
				{Name: "From", Value: "a@example.com"},
			},
			Parts: []*gmailapi.MessagePart{
				{
					MimeType: "multipart/related",
					Parts: []*gmailapi.MessagePart{
						{MimeType: "text/plain; charset=UTF-8", Body: &gmailapi.MessagePartBody{Data: b64url("the plain body")}},
					},
				},
				{MimeType: "text/html", Body: &gmailapi.MessagePartBody{Data: b64url("<p>the html body</p>")}},
				{MimeType: "application/pdf", Body: &gmailapi.MessagePartBody{AttachmentId: "att-1"}},
			},
		},
	}
	doc, err := messageDocument(testTenant, msg)
	if err != nil {
		t.Fatalf("messageDocument: %v", err)
	}
	if doc.GetBodyText() != "the plain body" {
		t.Errorf("body = %q, want the nested text/plain part", doc.GetBodyText())
	}
	if doc.GetVersionEtag() != "3" {
		t.Errorf("version_etag = %q, want %q", doc.GetVersionEtag(), "3")
	}
}

func TestMessageDocumentHTMLFallback(t *testing.T) {
	t.Parallel()
	html := `<html><head><style>p { color: red }</style></head>` +
		`<body><script>alert("x")</script><h1>Update</h1>` +
		`<p>First &amp; second.</p><div>Third line</div></body></html>`
	msg := &gmailapi.Message{
		Id:        "m2",
		HistoryId: 4,
		Payload: &gmailapi.MessagePart{
			MimeType: "text/html",
			Headers:  []*gmailapi.MessagePartHeader{{Name: "Subject", Value: "s"}},
			Body:     &gmailapi.MessagePartBody{Data: b64url(html)},
		},
	}
	doc, err := messageDocument(testTenant, msg)
	if err != nil {
		t.Fatalf("messageDocument: %v", err)
	}
	body := doc.GetBodyText()
	for _, want := range []string{"Update", "First & second.", "Third line"} {
		if !strings.Contains(body, want) {
			t.Errorf("body %q missing %q", body, want)
		}
	}
	for _, banned := range []string{"<", ">", "alert", "color: red"} {
		if strings.Contains(body, banned) {
			t.Errorf("body %q still contains %q", body, banned)
		}
	}
	if !strings.Contains(body, "Update\n") {
		t.Errorf("body %q lost the block boundary after the heading", body)
	}
}

func TestMessageDocumentNoSubject(t *testing.T) {
	t.Parallel()
	doc, err := messageDocument(testTenant, plainMessage("m3", map[string]string{"From": "a@b.c"}, "x"))
	if err != nil {
		t.Fatalf("messageDocument: %v", err)
	}
	if doc.GetTitle() != noSubjectTitle {
		t.Errorf("title = %q, want %q", doc.GetTitle(), noSubjectTitle)
	}
}

func TestMessageDocumentParticipants(t *testing.T) {
	t.Parallel()
	msg := plainMessage("m4", map[string]string{
		"Subject": "s",
		"From":    `"Ava Alvarez" <ava@example.com>`,
		"To":      "Ben <ben@example.com>, carol@example.com",
		"Cc":      "Dario Rossi <dario@example.com>",
	}, "x")
	doc, err := messageDocument(testTenant, msg)
	if err != nil {
		t.Fatalf("messageDocument: %v", err)
	}
	want := []*askerv1.Participant{
		{Name: "Ava Alvarez", Email: "ava@example.com", Role: "from"},
		{Name: "Ben", Email: "ben@example.com", Role: "to"},
		{Email: "carol@example.com", Role: "to"},
		{Name: "Dario Rossi", Email: "dario@example.com", Role: "cc"},
	}
	got := doc.GetParticipants()
	if len(got) != len(want) {
		t.Fatalf("participants = %v, want %v", got, want)
	}
	for i := range want {
		if got[i].GetName() != want[i].GetName() || got[i].GetEmail() != want[i].GetEmail() || got[i].GetRole() != want[i].GetRole() {
			t.Errorf("participant[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestMessageDocumentUnparseableAddressFallsBack(t *testing.T) {
	t.Parallel()
	raw := "totally <<< broken"
	doc, err := messageDocument(testTenant, plainMessage("m5", map[string]string{"Subject": "s", "From": raw}, "x"))
	if err != nil {
		t.Fatalf("messageDocument: %v", err)
	}
	got := doc.GetParticipants()
	if len(got) != 1 || got[0].GetName() != raw || got[0].GetRole() != "from" || got[0].GetEmail() != "" {
		t.Errorf("participants = %v, want one raw-name fallback for %q", got, raw)
	}
}

func TestMessageDocumentEtagFallsBackToBodyHash(t *testing.T) {
	t.Parallel()
	msg := plainMessage("m6", map[string]string{"Subject": "s"}, "stable body")
	msg.HistoryId = 0
	doc, err := messageDocument(testTenant, msg)
	if err != nil {
		t.Fatalf("messageDocument: %v", err)
	}
	sum := sha256.Sum256([]byte("stable body"))
	if want := hex.EncodeToString(sum[:]); doc.GetVersionEtag() != want {
		t.Errorf("version_etag = %q, want sha256 of body %q", doc.GetVersionEtag(), want)
	}
}

func TestMessageDocumentRejectsMissingID(t *testing.T) {
	t.Parallel()
	if _, err := messageDocument(testTenant, &gmailapi.Message{}); err == nil {
		t.Error("messageDocument accepted a message without an id")
	}
	if _, err := messageDocument(testTenant, nil); err == nil {
		t.Error("messageDocument accepted a nil message")
	}
}

func TestTombstoneDocument(t *testing.T) {
	t.Parallel()
	c := newTestConnector()
	doc := c.tombstoneDocument(testTenant, "gone-1", 17)
	if !doc.GetTombstone().GetDeleted() {
		t.Error("tombstone.deleted = false")
	}
	if doc.GetTombstone().GetDeletedAt() == nil {
		t.Error("tombstone.deleted_at unset")
	}
	if doc.GetVersionEtag() != "17" {
		t.Errorf("version_etag = %q, want %q", doc.GetVersionEtag(), "17")
	}
	if doc.GetBodyText() != "" || len(doc.GetChunks()) != 0 || doc.GetTitle() != "" {
		t.Error("tombstone carries content")
	}
}

func TestDecodeBase64Variants(t *testing.T) {
	t.Parallel()
	const text = "ünïcode body?>" // produces +/= in std encoding
	for name, encoded := range map[string]string{
		"raw url": base64.RawURLEncoding.EncodeToString([]byte(text)),
		"url":     base64.URLEncoding.EncodeToString([]byte(text)),
		"std":     base64.StdEncoding.EncodeToString([]byte(text)),
		"raw std": base64.RawStdEncoding.EncodeToString([]byte(text)),
	} {
		got, ok := decodeBase64(encoded)
		if !ok || string(got) != text {
			t.Errorf("%s: decodeBase64(%q) = (%q, %v), want %q", name, encoded, got, ok, text)
		}
	}
	if _, ok := decodeBase64("!!!"); ok {
		t.Error("decodeBase64 accepted junk")
	}
}
