// Command synthgen generates a realistic, deterministic, mixed-type synthetic
// corpus across many tenants and loads it into Asker for M5 scale/load/soak
// testing. Same (--seed, params) => byte-identical corpus, like
// tools/fake-gmail/server/seed.go.
//
// It is a dev/CI tool only; it never runs in production. The generation layer
// (corpus.go) is pure and unit-tested; the feed layer (feed.go) sits behind the
// Feeder interface so tests need no live stack.
//
// Two targets:
//   - vespa-direct: POST each doc to Vespa's document/v1 API into the tenant's
//     streaming group (raw index-fill at multi-tenant scale; bypasses Kafka).
//   - gateway-upload: POST each doc to the gateway's authed /v1/upload endpoint
//     (full pipeline; single-tenant — the tenant derives from the OIDC token).
//
// --dry-run prints the plan + estimated bytes and generates NOTHING, so a
// "100K tenants, TB-scale" plan is demonstrable on the 8GB dev VM.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

// config is the resolved CLI configuration. Every field is flag- and
// env-overridable (env prefix SYNTHGEN_); flags win over env, env wins over the
// default. See main() for the precedence wiring.
type config struct {
	tenants       int
	tenantID      string // explicit single-tenant id override (forces tenants=1); empty => synthgen-NNNNNNNN
	docsPerTenant int    // sets both min and max to this when >0 (uniform); 0 => use min/max
	minDocs       int
	maxDocs       int
	skewLarge     float64 // fraction of tenants drawn from the upper docs-per-tenant half
	docTypes      string  // "EMAIL:5,FILE:3,IMAGE:1,..."
	seed          int64
	rareRate      float64
	target        string // "vespa-direct" | "gateway-upload"
	vespaURL      string
	gatewayURL    string
	token         string
	concurrency   int
	dryRun        bool
	checkpoint    string
	reqTimeout    time.Duration
	progressEvery time.Duration
}

func defaultConfig() config {
	return config{
		tenants:       100,
		docsPerTenant: 0,
		minDocs:       5,
		maxDocs:       500,
		skewLarge:     0.1,
		docTypes:      "EMAIL:5,FILE:3,CHAT_MESSAGE:2,IMAGE:1,WIKI_PAGE:1,CALENDAR_EVENT:1",
		seed:          1,
		rareRate:      0.01,
		target:        "vespa-direct",
		vespaURL:      "http://localhost:8082",
		gatewayURL:    "http://localhost:8080",
		token:         "",
		concurrency:   8,
		dryRun:        true, // SAFE DEFAULT: never feed unless explicitly told to
		checkpoint:    "",
		reqTimeout:    30 * time.Second,
		progressEvery: 5 * time.Second,
	}
}

func main() {
	cfg := loadConfig()
	if err := run(context.Background(), cfg, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "synthgen:", err)
		os.Exit(1)
	}
}

// loadConfig resolves config from env (SYNTHGEN_*) then flags (flags win).
func loadConfig() config {
	cfg := defaultConfig()
	applyEnv(&cfg)

	fs := flag.CommandLine
	fs.IntVar(&cfg.tenants, "tenants", cfg.tenants, "number of tenants to generate")
	fs.StringVar(&cfg.tenantID, "tenant-id", cfg.tenantID, "explicit single-tenant id to seed into (forces --tenants 1); use to seed the EXACT tenant the load suite authenticates as so queries actually hit. Empty => synthgen-NNNNNNNN multi-tenant.")
	fs.IntVar(&cfg.docsPerTenant, "docs-per-tenant", cfg.docsPerTenant, "uniform docs per tenant (0 => use --min-docs/--max-docs skew distribution)")
	fs.IntVar(&cfg.minDocs, "min-docs", cfg.minDocs, "minimum docs per tenant (skew distribution)")
	fs.IntVar(&cfg.maxDocs, "max-docs", cfg.maxDocs, "maximum docs per tenant (skew distribution)")
	fs.Float64Var(&cfg.skewLarge, "skew-large-fraction", cfg.skewLarge, "fraction of tenants that are large (upper half of the docs range)")
	fs.StringVar(&cfg.docTypes, "doc-types", cfg.docTypes, "weighted doc-type mix, e.g. EMAIL:5,FILE:3,IMAGE:1")
	fs.Int64Var(&cfg.seed, "seed", cfg.seed, "deterministic seed (same seed+params => identical corpus)")
	fs.Float64Var(&cfg.rareRate, "rare-token-rate", cfg.rareRate, "fraction [0,1] of docs carrying a unique qzx-style rare token")
	fs.StringVar(&cfg.target, "target", cfg.target, "feed target: vespa-direct | gateway-upload")
	fs.StringVar(&cfg.vespaURL, "vespa-url", cfg.vespaURL, "Vespa document/v1 base URL (vespa-direct target)")
	fs.StringVar(&cfg.gatewayURL, "gateway-url", cfg.gatewayURL, "gateway base URL (gateway-upload target)")
	fs.StringVar(&cfg.token, "token", cfg.token, "OIDC bearer token (gateway-upload target)")
	fs.IntVar(&cfg.concurrency, "concurrency", cfg.concurrency, "concurrent feed workers")
	fs.BoolVar(&cfg.dryRun, "dry-run", cfg.dryRun, "print the plan + estimated bytes and generate NOTHING")
	fs.StringVar(&cfg.checkpoint, "checkpoint", cfg.checkpoint, "checkpoint file for --resume (saved at each tenant boundary)")
	fs.DurationVar(&cfg.reqTimeout, "request-timeout", cfg.reqTimeout, "per-request HTTP timeout for feeders")
	fs.DurationVar(&cfg.progressEvery, "progress-every", cfg.progressEvery, "progress reporting interval (0 disables)")
	flag.Parse()
	return cfg
}

// applyEnv overlays SYNTHGEN_*-prefixed environment variables onto cfg before
// flags are parsed. Kept hand-rolled (not config.Load) because flags must win
// over env, and config.Load would re-apply env on top of the flag defaults.
func applyEnv(cfg *config) {
	envInt := func(k string, dst *int) {
		if v := os.Getenv(k); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				*dst = n
			}
		}
	}
	envInt64 := func(k string, dst *int64) {
		if v := os.Getenv(k); v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				*dst = n
			}
		}
	}
	envFloat := func(k string, dst *float64) {
		if v := os.Getenv(k); v != "" {
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				*dst = f
			}
		}
	}
	envStr := func(k string, dst *string) {
		if v := os.Getenv(k); v != "" {
			*dst = v
		}
	}
	envBool := func(k string, dst *bool) {
		if v := os.Getenv(k); v != "" {
			if b, err := strconv.ParseBool(v); err == nil {
				*dst = b
			}
		}
	}
	envDur := func(k string, dst *time.Duration) {
		if v := os.Getenv(k); v != "" {
			if d, err := time.ParseDuration(v); err == nil {
				*dst = d
			}
		}
	}
	envInt("SYNTHGEN_TENANTS", &cfg.tenants)
	envStr("SYNTHGEN_TENANT_ID", &cfg.tenantID)
	envInt("SYNTHGEN_DOCS_PER_TENANT", &cfg.docsPerTenant)
	envInt("SYNTHGEN_MIN_DOCS", &cfg.minDocs)
	envInt("SYNTHGEN_MAX_DOCS", &cfg.maxDocs)
	envFloat("SYNTHGEN_SKEW_LARGE_FRACTION", &cfg.skewLarge)
	envStr("SYNTHGEN_DOC_TYPES", &cfg.docTypes)
	envInt64("SYNTHGEN_SEED", &cfg.seed)
	envFloat("SYNTHGEN_RARE_TOKEN_RATE", &cfg.rareRate)
	envStr("SYNTHGEN_TARGET", &cfg.target)
	envStr("SYNTHGEN_VESPA_URL", &cfg.vespaURL)
	envStr("SYNTHGEN_GATEWAY_URL", &cfg.gatewayURL)
	envStr("SYNTHGEN_TOKEN", &cfg.token)
	envInt("SYNTHGEN_CONCURRENCY", &cfg.concurrency)
	envBool("SYNTHGEN_DRY_RUN", &cfg.dryRun)
	envStr("SYNTHGEN_CHECKPOINT", &cfg.checkpoint)
	envDur("SYNTHGEN_REQUEST_TIMEOUT", &cfg.reqTimeout)
	envDur("SYNTHGEN_PROGRESS_EVERY", &cfg.progressEvery)
}

// run validates the config, builds the Spec, and either prints the dry-run plan
// or generates+feeds the corpus.
func run(ctx context.Context, cfg config, out *os.File) error {
	spec, err := specFromConfig(cfg)
	if err != nil {
		return err
	}

	if cfg.dryRun {
		_, _ = fmt.Fprint(out, BuildPlan(spec).String())
		return nil
	}

	feeder, err := feederFromConfig(cfg)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	runner := &Runner{
		Spec:           spec,
		Feeder:         feeder,
		Concurrency:    cfg.concurrency,
		CheckpointPath: cfg.checkpoint,
		ProgressEvery:  cfg.progressEvery,
		Out:            out,
	}
	res, runErr := runner.Run(ctx)
	PrintSummary(out, feeder, res)
	if runErr != nil {
		return runErr
	}
	return nil
}

// specFromConfig validates the config and produces a Spec.
func specFromConfig(cfg config) (Spec, error) {
	tenants := cfg.tenants
	// An explicit --tenant-id seeds a SINGLE tenant with that exact id (so the
	// load suite can query the tenant its OIDC token resolves to). It overrides
	// --tenants to 1; seeding many tenants into one id would collide the groups.
	if cfg.tenantID != "" {
		tenants = 1
	}
	if tenants < 1 {
		return Spec{}, fmt.Errorf("--tenants must be >= 1, got %d", tenants)
	}
	if cfg.rareRate < 0 || cfg.rareRate > 1 {
		return Spec{}, fmt.Errorf("--rare-token-rate must be in [0,1], got %v", cfg.rareRate)
	}
	if cfg.skewLarge < 0 || cfg.skewLarge > 1 {
		return Spec{}, fmt.Errorf("--skew-large-fraction must be in [0,1], got %v", cfg.skewLarge)
	}
	minDocs, maxDocs := cfg.minDocs, cfg.maxDocs
	if cfg.docsPerTenant > 0 {
		minDocs, maxDocs = cfg.docsPerTenant, cfg.docsPerTenant
	}
	if minDocs < 0 {
		return Spec{}, fmt.Errorf("--min-docs must be >= 0, got %d", minDocs)
	}
	if maxDocs < minDocs {
		return Spec{}, fmt.Errorf("--max-docs (%d) must be >= --min-docs (%d)", maxDocs, minDocs)
	}
	mix, err := parseDocTypes(cfg.docTypes)
	if err != nil {
		return Spec{}, err
	}
	return Spec{
		Tenants:           tenants,
		TenantIDOverride:  cfg.tenantID,
		Seed:              cfg.seed,
		DocTypeMix:        mix,
		RareTokenRate:     cfg.rareRate,
		MinDocs:           minDocs,
		MaxDocs:           maxDocs,
		SkewLargeFraction: cfg.skewLarge,
	}, nil
}

// parseDocTypes parses "EMAIL:5,FILE:3,IMAGE:1" into a normalized mix. Type
// names are the proto DocType enum names (EMAIL, FILE, CHAT_MESSAGE, ...).
func parseDocTypes(s string) ([]TypeWeight, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("--doc-types must not be empty")
	}
	var mix []TypeWeight
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, weightStr, ok := strings.Cut(part, ":")
		weight := 1.0
		if ok {
			w, err := strconv.ParseFloat(strings.TrimSpace(weightStr), 64)
			if err != nil || w < 0 {
				return nil, fmt.Errorf("--doc-types: bad weight in %q", part)
			}
			weight = w
		}
		name = strings.ToUpper(strings.TrimSpace(name))
		dtVal, ok := askerv1.DocType_value[name]
		if !ok || dtVal == int32(askerv1.DocType_DOC_TYPE_UNSPECIFIED) {
			return nil, fmt.Errorf("--doc-types: unknown DocType %q (valid: EMAIL, FILE, CHAT_MESSAGE, CALENDAR_EVENT, WIKI_PAGE, TICKET, IMAGE, VIDEO, AUDIO)", name)
		}
		mix = append(mix, TypeWeight{Type: askerv1.DocType(dtVal), Weight: weight})
	}
	if len(mix) == 0 {
		return nil, fmt.Errorf("--doc-types: no valid entries in %q", s)
	}
	return NormalizeMix(mix), nil
}

// feederFromConfig builds the Feeder for the configured target.
func feederFromConfig(cfg config) (Feeder, error) {
	client := newHTTPClient(cfg.reqTimeout, cfg.concurrency)
	switch cfg.target {
	case "vespa-direct":
		if cfg.vespaURL == "" {
			return nil, fmt.Errorf("--vespa-url is required for the vespa-direct target")
		}
		return newVespaFeeder(cfg.vespaURL, client), nil
	case "gateway-upload":
		if cfg.gatewayURL == "" {
			return nil, fmt.Errorf("--gateway-url is required for the gateway-upload target")
		}
		if cfg.token == "" {
			return nil, fmt.Errorf("--token (OIDC bearer) is required for the gateway-upload target")
		}
		return newGatewayFeeder(cfg.gatewayURL, cfg.token, client), nil
	default:
		return nil, fmt.Errorf("unknown --target %q (want vespa-direct or gateway-upload)", cfg.target)
	}
}
