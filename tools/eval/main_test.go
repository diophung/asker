package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestVerifyFrozenManifestRejectsEditedLabels(t *testing.T) {
	path := "fixtures/local-v1/manifest.json"
	if digest, err := verifyManifest(path); err != nil || len(digest) != 64 {
		t.Fatalf("valid frozen manifest: %q %v", digest, err)
	}
	directory := t.TempDir()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Files map[string]string `json:"files_sha256"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	for filename := range manifest.Files {
		original, err := os.ReadFile(filepath.Join(filepath.Dir(path), filename))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, filename), original, 0600); err != nil {
			t.Fatal(err)
		}
	}
	copiedManifest := filepath.Join(directory, "manifest.json")
	if err := os.WriteFile(copiedManifest, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "dev.jsonl"), []byte("edited\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyManifest(copiedManifest); err == nil {
		t.Fatal("edited labels must not retain frozen qualification")
	}
}

func TestMandatoryEligibleGroupsCannotBeSkipped(t *testing.T) {
	cfg := RunConfig{Baseline: "base", Candidate: "cand", AllowEqual: true, MinExactSuccess: .98, MinNeedCoverage: .95}
	base := Aggregate{Pipeline: "base", Supported: true, N: 1, Expected: 1, Scored: 1, NDCG: 1, TaskSuccess: 1}
	cand := base
	cand.Pipeline = "cand"
	if gate := evaluateGate([]Aggregate{base, cand}, cfg); gate.Pass {
		t.Fatal("missing exact/multi-need samples must fail")
	}
	cand.ExactN, cand.ExactSuccess, cand.NeedN, cand.NeedCoverage = 1, 1, 1, .94
	if gate := evaluateGate([]Aggregate{base, cand}, cfg); gate.Pass {
		t.Fatal("below95% all-needs coverage must fail")
	}
	cand.NeedCoverage = 1
	if gate := evaluateGate([]Aggregate{base, cand}, cfg); !gate.Pass {
		t.Fatalf("fully covered eligible groups should pass: %+v", gate)
	}
}

func TestTokenResolverRefreshesBeforeSustainedRunExpiry(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.Form.Get("username") != "alice" {
			t.Error("wrong principal")
		}
		_, _ = w.Write([]byte(`{"access_token":"private-test-token","expires_in":60}`))
	}))
	defer server.Close()
	resolver, err := newTokenResolver("", server.URL, "asker-web", "password123")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := resolver.token("alice"); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("valid token must be reused: %d grants", calls)
	}
	resolver.cacheUntil["alice"] = time.Now().Add(-time.Second)
	if _, err := resolver.token("alice"); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("expired evaluator token must be refreshed: %d grants", calls)
	}
}
