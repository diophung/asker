// Command eval scores Asker's retrieval quality (Recall@k, nDCG@k, MRR) and
// latency (p50/p95) for the lexical / dense / hybrid / hybrid+rerank pipelines
// against a labeled golden set, and enforces the quality gate. It drives the
// LIVE gateway search API, so a dev stack must be running and seeded (see
// tools/eval/README.md). Everything is engine-agnostic: it only reads the
// public REST search response, so the SAME command baselines today's pipeline
// and, later, the reranked one.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

func main() {
	var (
		goldenPath = flag.String("golden", "tools/eval/golden/golden.jsonl", "path to the JSONL golden set")
		gateway    = flag.String("gateway", "http://localhost:8080", "gateway base URL")
		oidcURL    = flag.String("oidc", "http://localhost:8081/realms/asker/protocol/openid-connect/token", "OIDC token endpoint for the password grant")
		clientID   = flag.String("client-id", "asker-web", "OIDC client id")
		password   = flag.String("password", "password123", "dev password used for every tenant's password grant")
		tokensPath = flag.String("tokens", "", "optional JSON file mapping tenant -> bearer token (overrides the password grant)")
		k          = flag.Int("k", 10, "cutoff for Recall@k and nDCG@k")
		limit      = flag.Int("limit", 20, "hits to request per query (>= k)")
		baseline   = flag.String("baseline", "hybrid", "gate baseline pipeline")
		candidate  = flag.String("candidate", "hybrid_rerank", "gate candidate pipeline (must match-or-beat the baseline)")
		outDir     = flag.String("out", "tools/eval/reports", "directory to write the committed report into")
	)
	flag.Parse()

	recs, err := LoadGolden(*goldenPath)
	if err != nil {
		fatal(err)
	}

	tok, err := newTokenResolver(*tokensPath, *oidcURL, *clientID, *password)
	if err != nil {
		fatal(err)
	}

	searcher := NewGatewaySearcher(*gateway, tok.token)
	cfg := RunConfig{
		K:         *k,
		Limit:     *limit,
		Pipelines: standardPipelines(),
		Baseline:  *baseline,
		Candidate: *candidate,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	report, _, err := Run(ctx, searcher, recs, cfg)
	if err != nil {
		fatal(err)
	}
	now := time.Now()
	report.Generated = now.UTC().Format(time.RFC3339)
	report.Golden = *goldenPath

	fmt.Print(renderText(report))

	path, err := writeReports(*outDir, report, now)
	if err != nil {
		fatal(err)
	}
	fmt.Printf("\nreport written to %s (and latest.json/latest.md)\n", path)

	if !report.Gate.Skipped && !report.Gate.Pass {
		os.Exit(1) // quality gate failed — usable as a CI signal
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "eval:", err)
	os.Exit(2)
}

// tokenResolver resolves per-tenant bearer tokens, either from a static file or
// via the Keycloak dev password grant (username == tenant). Tokens are cached
// for the run.
type tokenResolver struct {
	static   map[string]string
	oidcURL  string
	clientID string
	password string
	client   *http.Client

	mu    sync.Mutex
	cache map[string]string
}

func newTokenResolver(tokensPath, oidcURL, clientID, password string) (*tokenResolver, error) {
	tr := &tokenResolver{
		oidcURL:  oidcURL,
		clientID: clientID,
		password: password,
		client:   &http.Client{Timeout: 15 * time.Second},
		cache:    map[string]string{},
	}
	if tokensPath != "" {
		data, err := os.ReadFile(tokensPath) //nolint:gosec // operator-supplied tokens file
		if err != nil {
			return nil, fmt.Errorf("read tokens file: %w", err)
		}
		if err := json.Unmarshal(data, &tr.static); err != nil {
			return nil, fmt.Errorf("parse tokens file: %w", err)
		}
	}
	return tr, nil
}

func (t *tokenResolver) token(tenant string) (string, error) {
	if t.static != nil {
		if tok, ok := t.static[tenant]; ok {
			return tok, nil
		}
		return "", fmt.Errorf("no token for tenant %q in tokens file", tenant)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if tok, ok := t.cache[tenant]; ok {
		return tok, nil
	}
	tok, err := t.passwordGrant(tenant)
	if err != nil {
		return "", err
	}
	t.cache[tenant] = tok
	return tok, nil
}

// passwordGrant fetches a dev access token for username==tenant.
func (t *tokenResolver) passwordGrant(tenant string) (string, error) {
	form := url.Values{
		"grant_type": {"password"},
		"client_id":  {t.clientID},
		"username":   {tenant},
		"password":   {t.password},
	}
	resp, err := t.client.PostForm(t.oidcURL, form)
	if err != nil {
		return "", fmt.Errorf("password grant for %q: %w", tenant, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var parsed struct {
		AccessToken      string `json:"access_token"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", fmt.Errorf("password grant for %q: decode: %w", tenant, err)
	}
	if resp.StatusCode != http.StatusOK || parsed.AccessToken == "" {
		msg := parsed.ErrorDescription
		if msg == "" {
			msg = parsed.Error
		}
		return "", fmt.Errorf("password grant for %q failed (HTTP %d): %s", tenant, resp.StatusCode, strings.TrimSpace(msg))
	}
	return parsed.AccessToken, nil
}
