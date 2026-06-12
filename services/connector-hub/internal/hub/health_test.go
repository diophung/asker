package hub

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthEndpoints(t *testing.T) {
	ready := newHealthHandler(readiness{
		controlPlane: func(context.Context) error { return nil },
		kafkaReady:   func() bool { return true },
	})
	cpDown := newHealthHandler(readiness{
		controlPlane: func(context.Context) error { return errors.New("conn refused") },
		kafkaReady:   func() bool { return true },
	})
	kafkaDown := newHealthHandler(readiness{
		controlPlane: func(context.Context) error { return nil },
		kafkaReady:   func() bool { return false },
	})

	cases := []struct {
		name       string
		handler    http.Handler
		method     string
		path       string
		wantStatus int
	}{
		{"healthz ok", ready, http.MethodGet, "/healthz", http.StatusOK},
		{"healthz ok even when deps down", cpDown, http.MethodGet, "/healthz", http.StatusOK},
		{"readyz ready", ready, http.MethodGet, "/readyz", http.StatusOK},
		{"readyz control-plane down", cpDown, http.MethodGet, "/readyz", http.StatusServiceUnavailable},
		{"readyz kafka down", kafkaDown, http.MethodGet, "/readyz", http.StatusServiceUnavailable},
		{"healthz wrong method", ready, http.MethodPost, "/healthz", http.StatusMethodNotAllowed},
		{"unknown path", ready, http.MethodGet, "/nope", http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.handler.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
		})
	}
}

func TestRunHealthcheck(t *testing.T) {
	srv := httptest.NewServer(newHealthHandler(readiness{
		controlPlane: func(context.Context) error { return nil },
		kafkaReady:   func() bool { return true },
	}))
	defer srv.Close()
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}

	if code := RunHealthcheck(":" + port); code != 0 {
		t.Errorf("healthy probe exit = %d, want 0", code)
	}

	srv.Close()
	if code := RunHealthcheck(":" + port); code != 1 {
		t.Errorf("dead probe exit = %d, want 1", code)
	}
}

func TestRunHealthcheckNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}
	if code := RunHealthcheck(":" + port); code != 1 {
		t.Errorf("unhealthy probe exit = %d, want 1", code)
	}
}
