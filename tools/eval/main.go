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
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"time"
)

func main() {
	var (
		goldenPath  = flag.String("golden", "tools/eval/golden/golden.jsonl", "path to the JSONL golden set")
		gateway     = flag.String("gateway", "http://localhost:8080", "gateway base URL")
		oidcURL     = flag.String("oidc", "http://localhost:8081/realms/asker/protocol/openid-connect/token", "OIDC token endpoint for the password grant")
		clientID    = flag.String("client-id", "asker-web", "OIDC client id")
		password    = flag.String("password", "password123", "dev password used for every tenant's password grant")
		tokensPath  = flag.String("tokens", "", "optional JSON file mapping tenant -> bearer token (overrides the password grant)")
		k           = flag.Int("k", 10, "cutoff for Recall@k and nDCG@k")
		limit       = flag.Int("limit", 20, "hits to request per query (>= k)")
		baseline    = flag.String("baseline", "hybrid", "gate baseline pipeline")
		candidate   = flag.String("candidate", "hybrid_rerank", "gate candidate pipeline (must match-or-beat the baseline)")
		outDir      = flag.String("out", "tools/eval/reports", "directory to write the committed report into")
		bypassCache = flag.Bool("no-cache", true, "send Cache-Control:no-cache; cached results fail the qualification gate")
		repeats     = flag.Int("repeats", 1, "samples per query and pipeline; repetition does not add independently judged queries")
		concurrency = flag.Int("concurrency", 1, "simultaneous search requests; qualify concurrency 1, 2, 4 separately")
		maxP95      = flag.Float64("max-p95-ms", 5000, "candidate complete-response p95 latency ceiling")
		minTask     = flag.Float64("min-task-success", .90, "candidate successful-task fraction, including errors as failures")
		minExact    = flag.Float64("min-exact-success", .98, "candidate exact-item success@1 fraction")
		minNeeds    = flag.Float64("min-need-coverage", .95, "candidate all-needs coverage fraction on explicitly labeled multi-need tasks")
		allowEqual  = flag.Bool("allow-equal", false, "permit equal overall nDCG; record this changed gate explicitly")
		split       = flag.String("split", "", "optional dev, regression, or holdout split to evaluate")
		pipelines   = flag.String("pipelines", "lexical,dense,hybrid,hybrid_rerank", "comma-separated pipelines to run")
		hardware    = flag.String("hardware", "unverified", "observed hardware and RAM, e.g. M5 Pro 48GB; do not infer")
		model       = flag.String("model", "unverified", "exact embedding/reranker model IDs, revisions and runtime")
		corpus      = flag.String("corpus-manifest", "", "optional corpus/benchmark manifest to fingerprint")
		minSamples  = flag.Int("min-latency-samples", 100, "minimum candidate request samples for latency qualification")
	)
	flag.Parse()

	recs, err := LoadGolden(*goldenPath)
	if err != nil {
		fatal(err)
	}
	goldenDigest, err := fileSHA256(*goldenPath)
	if err != nil {
		fatal(err)
	}
	corpusDigest := ""
	if *corpus != "" {
		corpusDigest, err = verifyManifest(*corpus)
		if err != nil {
			fatal(err)
		}
	}
	if *repeats < 1 || *concurrency < 1 || *maxP95 <= 0 || *minTask < 0 || *minTask > 1 || *minExact < 0 || *minExact > 1 || *minNeeds < 0 || *minNeeds > 1 || *minSamples < 1 {
		fatal(fmt.Errorf("invalid repeats, concurrency, latency or success threshold"))
	}
	if *split != "" {
		if *split != "dev" && *split != "regression" && *split != "holdout" {
			fatal(fmt.Errorf("invalid split %q", *split))
		}
		var selected []GoldenRecord
		for _, rec := range recs {
			if rec.Split == *split {
				selected = append(selected, rec)
			}
		}
		recs = selected
		if len(recs) == 0 {
			fatal(fmt.Errorf("no records for split %q", *split))
		}
	}

	tok, err := newTokenResolver(*tokensPath, *oidcURL, *clientID, *password)
	if err != nil {
		fatal(err)
	}

	searcher := NewGatewaySearcher(*gateway, tok.token)
	searcher.BypassCache = *bypassCache
	available := map[string]Pipeline{}
	for _, p := range standardPipelines() {
		available[p.Name] = p
	}
	var selectedPipelines []Pipeline
	for _, name := range strings.Split(*pipelines, ",") {
		p, ok := available[strings.TrimSpace(name)]
		if !ok {
			fatal(fmt.Errorf("unknown pipeline %q", name))
		}
		selectedPipelines = append(selectedPipelines, p)
	}
	cfg := RunConfig{
		K:         *k,
		Limit:     *limit,
		Pipelines: selectedPipelines,
		Baseline:  *baseline,
		Candidate: *candidate,
		Repeats:   *repeats, Concurrency: *concurrency, MaxP95Ms: *maxP95,
		MinTaskSuccess: *minTask, MinExactSuccess: *minExact, MinNeedCoverage: *minNeeds, RequireUncached: *bypassCache,
		AllowEqual: *allowEqual, MinLatencySamples: *minSamples,
	}
	if *corpus != "" {
		if err := validateFrozenBenchmark(*corpus, recs, cfg); err != nil {
			fatal(err)
		}
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
	report.GoldenSHA256 = goldenDigest
	report.Metadata = map[string]string{
		"hardware": *hardware, "models_and_runtime": *model, "go_version": runtime.Version(),
		"os_arch": runtime.GOOS + "/" + runtime.GOARCH, "gateway": *gateway,
		"split": *split, "label_status": "engineering labels; independent adjudication unverified",
		"latency_scope":   "HTTP submission through complete response read and JSON decode; excludes browser rendering and token acquisition",
		"candidate_debug": "requested debug=1; only complete authorized pre-rerank head IDs are scored; no-match excluded; missing/truncated telemetry unmeasured",
	}
	if *corpus != "" {
		report.Metadata["corpus_manifest"] = *corpus
		report.Metadata["corpus_manifest_sha256"] = corpusDigest
	}

	fmt.Print(renderText(report))

	path, err := writeReports(*outDir, report, now)
	if err != nil {
		fatal(err)
	}
	fmt.Printf("\nreport written to %s (and latest.json/latest.md)\n", path)

	if !report.Gate.Pass {
		os.Exit(1) // quality gate failed — usable as a CI signal
	}
}

func fileSHA256(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("fingerprint %s: %w", path, err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

// The frozen local benchmark must match every authored file, not merely the
// manifest's own digest. Other corpus manifests retain fingerprint-only use.
func verifyManifest(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var manifest struct {
		BenchmarkID string            `json:"benchmark_id"`
		Files       map[string]string `json:"files_sha256"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return "", fmt.Errorf("parse corpus manifest: %w", err)
	}
	if manifest.BenchmarkID == "asker-local-v1" || manifest.BenchmarkID == "asker-local-v2" {
		files := []string{"documents.jsonl", "dev.jsonl", "regression.jsonl", "holdout.jsonl"}
		if manifest.BenchmarkID == "asker-local-v2" {
			files = append(files, "criteria.json", "evidence.jsonl")
		}
		for _, filename := range files {
			expected, ok := manifest.Files[filename]
			if !ok {
				return "", fmt.Errorf("frozen manifest missing %s fingerprint", filename)
			}
			actual, err := fileSHA256(filepath.Join(filepath.Dir(path), filename))
			if err != nil {
				return "", err
			}
			if actual != expected {
				return "", fmt.Errorf("frozen fixture fingerprint changed: %s", filename)
			}
		}
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

// V2 is a declared qualification contract, not an adjustable scorer. Resolve
// only token labels when seeding; changed tasks or thresholds require a new
// version. Validation does not emit query text or holdout judgments.
func validateFrozenBenchmark(path string, records []GoldenRecord, cfg RunConfig) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var manifest struct {
		BenchmarkID string          `json:"benchmark_id"`
		Acceptance  json.RawMessage `json:"acceptance"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return err
	}
	if manifest.BenchmarkID != "asker-local-v2" {
		return nil
	}
	criteriaData, err := os.ReadFile(filepath.Join(filepath.Dir(path), "criteria.json"))
	if err != nil {
		return err
	}
	var manifestCriteria, fileCriteria map[string]any
	if err := json.Unmarshal(manifest.Acceptance, &manifestCriteria); err != nil {
		return err
	}
	if err := json.Unmarshal(criteriaData, &fileCriteria); err != nil {
		return err
	}
	if !reflect.DeepEqual(manifestCriteria, fileCriteria) {
		return fmt.Errorf("frozen v2 criteria disagree with manifest")
	}
	var criteria struct {
		K                        int     `json:"k"`
		Limit                    int     `json:"limit"`
		MaxP95Ms                 float64 `json:"max_p95_ms"`
		MinTaskSuccess           float64 `json:"min_task_success"`
		MinExactSuccess          float64 `json:"min_exact_success"`
		MinNeedCoverage          float64 `json:"min_need_coverage"`
		MinLatencySamples        int     `json:"min_latency_samples"`
		RequireUncached          bool    `json:"require_uncached"`
		AllowEqual               bool    `json:"allow_equal"`
		QualificationConcurrency int     `json:"qualification_concurrency"`
	}
	if err := json.Unmarshal(criteriaData, &criteria); err != nil {
		return err
	}
	if cfg.K != criteria.K || cfg.Limit != criteria.Limit || cfg.MaxP95Ms != criteria.MaxP95Ms ||
		cfg.MinTaskSuccess != criteria.MinTaskSuccess || cfg.MinExactSuccess != criteria.MinExactSuccess ||
		cfg.MinNeedCoverage != criteria.MinNeedCoverage || cfg.MinLatencySamples != criteria.MinLatencySamples ||
		cfg.RequireUncached != criteria.RequireUncached || cfg.AllowEqual != criteria.AllowEqual ||
		cfg.Concurrency != criteria.QualificationConcurrency {
		return fmt.Errorf("run configuration conflicts with frozen v2 criteria; do not relax or change the qualification contract")
	}
	if len(records) == 0 {
		return fmt.Errorf("frozen v2 qualification requires a complete declared split")
	}
	split := records[0].Split
	if split != "dev" && split != "regression" && split != "holdout" {
		return fmt.Errorf("frozen v2 qualification requires a declared split")
	}
	original, err := LoadGolden(filepath.Join(filepath.Dir(path), split+".jsonl"))
	if err != nil {
		return err
	}
	if len(records) != len(original) {
		return fmt.Errorf("frozen v2 qualification requires every declared split task")
	}
	byID := map[string]GoldenRecord{}
	for _, record := range original {
		byID[record.ID] = record
	}
	for _, record := range records {
		wanted, found := byID[record.ID]
		wanted.Tenant = record.Tenant // verified token-label resolution only
		if !found || !reflect.DeepEqual(record, wanted) {
			return fmt.Errorf("resolved v2 golden tasks changed; only tenant token labels may be resolved")
		}
		delete(byID, record.ID)
	}
	return nil
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

	mu         sync.Mutex
	cache      map[string]string
	cacheUntil map[string]time.Time
}

func newTokenResolver(tokensPath, oidcURL, clientID, password string) (*tokenResolver, error) {
	tr := &tokenResolver{
		oidcURL:    oidcURL,
		clientID:   clientID,
		password:   password,
		client:     &http.Client{Timeout: 15 * time.Second},
		cache:      map[string]string{},
		cacheUntil: map[string]time.Time{},
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
	if tok, ok := t.cache[tenant]; ok && time.Now().Before(t.cacheUntil[tenant]) {
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
		ExpiresIn        int64  `json:"expires_in"`
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
	// Sustained repeated-request runs can outlive a dev access token. Refresh
	// proactively instead of misclassifying expired evaluator auth as search
	// failure; token acquisition remains outside the HTTP search timer.
	ttl := 2 * time.Minute
	if parsed.ExpiresIn > 0 {
		ttl = time.Duration(parsed.ExpiresIn) * time.Second
		if ttl > 30*time.Second {
			ttl -= 30 * time.Second
		}
	}
	t.cacheUntil[tenant] = time.Now().Add(ttl)
	return parsed.AccessToken, nil
}
