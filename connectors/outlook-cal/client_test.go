package outlookcal

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/asker/asker/connectors/sdk"
)

// statusServer serves a fixed status+body for any request.
func statusServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestGetJSONOK(t *testing.T) {
	t.Parallel()
	ts := statusServer(t, http.StatusOK, `{"value":"hi"}`)
	cl := newClient(instanceConfig{}, []byte("tok"))
	var out struct {
		Value string `json:"value"`
	}
	if err := cl.getJSON(context.Background(), ts.URL+"/x", &out); err != nil {
		t.Fatalf("getJSON: %v", err)
	}
	if out.Value != "hi" {
		t.Errorf("value = %q", out.Value)
	}
}

func TestGetJSONGoneIsCursorExpired(t *testing.T) {
	t.Parallel()
	ts := statusServer(t, http.StatusGone, `{"error":{"code":"resyncRequired"}}`)
	cl := newClient(instanceConfig{}, []byte("tok"))
	err := cl.getJSON(context.Background(), ts.URL+"/delta", new(map[string]any))
	if !errors.Is(err, errCursorGone) {
		t.Fatalf("err = %v, want errCursorGone", err)
	}
	// asCursorExpired wraps it as the SDK sentinel the hub keys on.
	if wrapped := asCursorExpired(err); !errors.Is(wrapped, sdk.ErrCursorExpired) {
		t.Errorf("asCursorExpired did not yield sdk.ErrCursorExpired: %v", wrapped)
	}
}

func TestGetJSONResyncCodeWithout410(t *testing.T) {
	t.Parallel()
	// Graph sometimes returns 400 with a resync code instead of 410.
	ts := statusServer(t, http.StatusBadRequest, `{"error":{"code":"SyncStateNotFound"}}`)
	cl := newClient(instanceConfig{}, []byte("tok"))
	err := cl.getJSON(context.Background(), ts.URL+"/delta", new(map[string]any))
	if !errors.Is(err, errCursorGone) {
		t.Fatalf("err = %v, want errCursorGone for resync code", err)
	}
}

func TestGetJSONGenericError(t *testing.T) {
	t.Parallel()
	ts := statusServer(t, http.StatusInternalServerError, `{"error":{"code":"InternalServerError"}}`)
	cl := newClient(instanceConfig{}, []byte("tok"))
	err := cl.getJSON(context.Background(), ts.URL+"/x", new(map[string]any))
	if err == nil || errors.Is(err, errCursorGone) {
		t.Fatalf("err = %v, want a generic non-cursor error", err)
	}
}

func TestGetJSONDecodeError(t *testing.T) {
	t.Parallel()
	ts := statusServer(t, http.StatusOK, `{not json`)
	cl := newClient(instanceConfig{}, []byte("tok"))
	if err := cl.getJSON(context.Background(), ts.URL+"/x", new(map[string]any)); err == nil {
		t.Fatal("getJSON accepted malformed JSON")
	}
}

func TestBearerTransportSetsHeader(t *testing.T) {
	t.Parallel()
	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(ts.Close)
	cl := newClient(instanceConfig{}, []byte("secret-token"))
	if err := cl.getJSON(context.Background(), ts.URL+"/x", new(map[string]any)); err != nil {
		t.Fatalf("getJSON: %v", err)
	}
	if gotAuth != "Bearer secret-token" {
		t.Errorf("Authorization = %q, want Bearer secret-token", gotAuth)
	}
}

func TestAsCursorExpiredPassThrough(t *testing.T) {
	t.Parallel()
	other := errors.New("some other error")
	if got := asCursorExpired(other); got != other {
		t.Errorf("asCursorExpired wrapped a non-cursor error: %v", got)
	}
}
