package gcal

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/asker/asker/connectors/sdk"
	"github.com/asker/asker/connectors/sdk/connectortest"
)

// jsonServer serves a single fixed status + body for every request.
func jsonServer(t *testing.T, status int, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestFullSyncAPIError(t *testing.T) {
	t.Parallel()
	url := jsonServer(t, http.StatusForbidden, `{"error":{"code":403,"message":"insufficientPermissions"}}`)
	c := New(WithLogger(slog.New(slog.DiscardHandler)))
	cfg := configFor(t, url, nil)

	var rec connectortest.EmitRecorder
	_, err := c.FullSync(context.Background(), cfg, rec.Emit)
	if err == nil {
		t.Fatal("FullSync did not surface an API error")
	}
	if errors.Is(err, sdk.ErrCursorExpired) {
		t.Errorf("403 must not map to ErrCursorExpired: %v", err)
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error = %v, want it to mention the status", err)
	}
}

func TestFullSyncMissingSyncToken(t *testing.T) {
	t.Parallel()
	// A final page (no nextPageToken) that also omits nextSyncToken is a
	// protocol violation; FullSync must fail rather than return a bad cursor.
	url := jsonServer(t, http.StatusOK, `{"kind":"calendar#events","items":[]}`)
	c := New(WithLogger(slog.New(slog.DiscardHandler)))
	cfg := configFor(t, url, nil)

	var rec connectortest.EmitRecorder
	if _, err := c.FullSync(context.Background(), cfg, rec.Emit); err == nil {
		t.Fatal("FullSync accepted a final page without nextSyncToken")
	}
}

func TestFullSyncBadJSON(t *testing.T) {
	t.Parallel()
	url := jsonServer(t, http.StatusOK, `{not json`)
	c := New(WithLogger(slog.New(slog.DiscardHandler)))
	cfg := configFor(t, url, nil)

	var rec connectortest.EmitRecorder
	if _, err := c.FullSync(context.Background(), cfg, rec.Emit); err == nil {
		t.Fatal("FullSync accepted a malformed JSON response")
	}
}

func TestContextCanceled(t *testing.T) {
	t.Parallel()
	url := jsonServer(t, http.StatusOK, `{"items":[],"nextSyncToken":"x"}`)
	c := New(WithLogger(slog.New(slog.DiscardHandler)))
	cfg := configFor(t, url, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var rec connectortest.EmitRecorder
	if _, err := c.FullSync(ctx, cfg, rec.Emit); err == nil {
		t.Fatal("FullSync ignored a canceled context")
	}
}
