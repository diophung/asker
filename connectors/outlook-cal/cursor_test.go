package outlookcal

import (
	"testing"

	"github.com/asker/asker/connectors/sdk"
)

func TestParseCursorDelta(t *testing.T) {
	t.Parallel()
	cur := deltaCursor("https://graph.microsoft.com/v1.0/me/calendarView/delta?$deltatoken=ABC")
	st, err := parseCursor(cur)
	if err != nil {
		t.Fatalf("parseCursor: %v", err)
	}
	if st.backfill {
		t.Error("delta cursor parsed as backfill")
	}
	if st.link != "https://graph.microsoft.com/v1.0/me/calendarView/delta?$deltatoken=ABC" {
		t.Errorf("link = %q", st.link)
	}
}

func TestParseCursorPage(t *testing.T) {
	t.Parallel()
	cur := pageCursor("https://graph.microsoft.com/v1.0/me/events?$skip=50")
	st, err := parseCursor(cur)
	if err != nil {
		t.Fatalf("parseCursor: %v", err)
	}
	if !st.backfill {
		t.Error("page cursor not parsed as backfill")
	}
	if st.link != "https://graph.microsoft.com/v1.0/me/events?$skip=50" {
		t.Errorf("link = %q", st.link)
	}
}

func TestParseCursorErrors(t *testing.T) {
	t.Parallel()
	for name, cur := range map[string]sdk.Cursor{
		"unknown prefix": sdk.Cursor("history:123"),
		"empty delta":    sdk.Cursor(deltaPrefix),
		"empty page":     sdk.Cursor(pagePrefix),
		"empty":          sdk.Cursor(""),
	} {
		if _, err := parseCursor(cur); err == nil {
			t.Errorf("parseCursor(%s=%q) succeeded, want error", name, cur)
		}
	}
}

func TestResolveLinkRehostsAndStripsVersion(t *testing.T) {
	t.Parallel()
	conf := instanceConfig{BaseURL: "http://127.0.0.1:9999"}
	got := resolveLink(conf, "https://graph.microsoft.com/v1.0/me/events?$skip=50&$top=50")
	want := "http://127.0.0.1:9999/me/events?$skip=50&$top=50"
	if got != want {
		t.Errorf("resolveLink = %q, want %q", got, want)
	}
}

func TestResolveLinkProductionBaseKeepsVersion(t *testing.T) {
	t.Parallel()
	// With the default Graph base (no override), a Graph link keeps /v1.0.
	conf := instanceConfig{}
	got := resolveLink(conf, "https://graph.microsoft.com/v1.0/me/calendarView/delta?$deltatoken=ABC")
	want := "https://graph.microsoft.com/v1.0/me/calendarView/delta?$deltatoken=ABC"
	if got != want {
		t.Errorf("resolveLink = %q, want %q", got, want)
	}
}

func TestResolveLinkUnparseable(t *testing.T) {
	t.Parallel()
	conf := instanceConfig{BaseURL: "http://x"}
	// A control character makes url.Parse fail; the link is returned unchanged.
	bad := "http://\x7f/bad"
	if got := resolveLink(conf, bad); got != bad {
		t.Errorf("resolveLink(bad) = %q, want unchanged", got)
	}
}
