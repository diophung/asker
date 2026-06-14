package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newUploaderFor builds a gatewayUploader pointed at srv with a test token.
func newUploaderFor(srv *httptest.Server, token string) *gatewayUploader {
	return &gatewayUploader{client: srv.Client(), baseURL: srv.URL, token: token}
}

func TestGatewayUploadHappyPath(t *testing.T) {
	var (
		gotPath     string
		gotAuth     string
		gotTitle    string
		gotFilename string
		gotFile     string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		gotTitle = r.FormValue("title")
		f, hdr, err := r.FormFile("file")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer func() { _ = f.Close() }()
		gotFilename = hdr.Filename
		b, _ := io.ReadAll(f)
		gotFile = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"doc_id":"d-777"}`))
	}))
	t.Cleanup(srv.Close)

	up := newUploaderFor(srv, "secret-token")
	docID, err := up.Upload(context.Background(), transcript{
		filename: "imessage-g1.txt",
		title:    "iMessage: g1",
		body:     "hello transcript",
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if docID != "d-777" {
		t.Errorf("docID = %q, want d-777", docID)
	}
	if gotPath != "/v1/upload" {
		t.Errorf("path = %q, want /v1/upload", gotPath)
	}
	if gotAuth != "Bearer secret-token" {
		t.Errorf("auth = %q, want Bearer secret-token", gotAuth)
	}
	if gotTitle != "iMessage: g1" {
		t.Errorf("title = %q", gotTitle)
	}
	if gotFilename != "imessage-g1.txt" {
		t.Errorf("filename = %q", gotFilename)
	}
	if gotFile != "hello transcript" {
		t.Errorf("file = %q", gotFile)
	}
}

func TestGatewayUploadRejectsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":"unsupported file type"}`))
	}))
	t.Cleanup(srv.Close)

	up := newUploaderFor(srv, "t")
	_, err := up.Upload(context.Background(), transcript{filename: "x.txt", body: "x"})
	if err == nil {
		t.Fatal("Upload should error on 422")
	}
	if !strings.Contains(err.Error(), "unsupported file type") {
		t.Errorf("error = %v, want it to surface the gateway message", err)
	}
	if !strings.Contains(err.Error(), "422") {
		t.Errorf("error = %v, want it to include the status", err)
	}
}

func TestGatewayUploadMissingDocID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	up := newUploaderFor(srv, "t")
	if _, err := up.Upload(context.Background(), transcript{filename: "x.txt", body: "x"}); err == nil {
		t.Error("Upload should error when the accepted response has no doc_id")
	}
}

func TestGatewayUploadNoTitleField(t *testing.T) {
	var hadTitle bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(1 << 20)
		_, hadTitle = r.MultipartForm.Value["title"]
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"doc_id":"d-1"}`))
	}))
	t.Cleanup(srv.Close)

	up := newUploaderFor(srv, "t")
	if _, err := up.Upload(context.Background(), transcript{filename: "x.txt", body: "x"}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if hadTitle {
		t.Error("empty title should be omitted from the multipart form")
	}
}

func TestGatewayUploadServerDown(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // refuse connections

	up := &gatewayUploader{client: srv.Client(), baseURL: url, token: "t"}
	if _, err := up.Upload(context.Background(), transcript{filename: "x.txt", body: "x"}); err == nil {
		t.Error("Upload should error when the gateway is unreachable")
	}
}
