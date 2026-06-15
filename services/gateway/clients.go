package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/asker/asker/platform/oauth"
	controlplanev1 "github.com/asker/asker/platform/proto/gen/go/asker/controlplane/v1"
	queryv1 "github.com/asker/asker/platform/proto/gen/go/asker/query/v1"
	"github.com/asker/asker/platform/safehttp"
	"github.com/asker/asker/platform/tenancy/tenancygrpc"
)

// deps bundles the gateway's downstream clients so handlers stay testable:
// tests inject in-proc fakes, main injects the real things.
type deps struct {
	query   queryv1.QueryServiceClient
	control controlplanev1.ControlPlaneServiceClient
	admin   controlplanev1.AdminServiceClient
	// hubURL is the connector-hub base URL (no trailing slash).
	hubURL    string
	hubClient *http.Client
	// mediaClient is dedicated to GET /v1/media: a shorter timeout than the
	// upload client (thumbnails/keyframes are small).
	mediaClient *http.Client
	// counter backs the per-tenant rate limiter (Redis in production).
	counter rateCounter
	// recent persists per-tenant recent-search history (Redis in production;
	// nil-safe — a nil store degrades the feature, never the search).
	recent recentSearchStore
	// prefs write-through-caches the resolved personalization profile + learned
	// model into Redis for the query hot path (v3.2; nil-safe).
	prefs          prefWriteStore
	maxUploadBytes int64
	maxMediaBytes  int64
	// oidcAudience is the token audience; admin client-role claims live under
	// resource_access.<oidcAudience>.roles.
	oidcAudience string
	// maxQueryChars caps the /v1/search q= length; <= 0 disables the cap.
	maxQueryChars int
	logger        *slog.Logger

	// OAuth connector flow (wave 1). oauth drives the authorization-code
	// exchange; oauthState persists the single-use, server-side flow state the
	// public callback consumes; oauthCfg answers "is this provider configured".
	// All three are nil when no provider creds are set, in which case the OAuth
	// routes degrade to a clear 501 rather than half-working.
	oauth      oauthService
	oauthState oauthStateStore
	oauthCfg   oauth.Config
	// gatewayPublicURL builds the redirect_uri; webAppURL is the FIXED
	// post-callback redirect target. Both are trimmed of any trailing slash.
	gatewayPublicURL string
	webAppURL        string
}

// newDeps builds the production dependency set. Both gRPC clients use lazy,
// non-blocking dials (grpc.NewClient performs no I/O), so the gateway starts
// even when query/control-plane are not up yet. The tenancy client
// interceptor injects the tenant from the request context on EVERY call and
// fails closed when none is present — the tenant is never user input.
func newDeps(cfg gatewayConfig, logger *slog.Logger) (*deps, func(), error) {
	dialOpts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(tenancygrpc.UnaryClientInterceptor()),
	}
	queryConn, err := grpc.NewClient(cfg.QueryGRPCAddr, dialOpts...)
	if err != nil {
		return nil, nil, fmt.Errorf("create query client: %w", err)
	}
	controlConn, err := grpc.NewClient(cfg.ControlPlaneGRPCAddr, dialOpts...)
	if err != nil {
		_ = queryConn.Close()
		return nil, nil, fmt.Errorf("create control-plane client: %w", err)
	}
	counter := newRedisCounter(cfg.RedisAddr)
	cleanup := func() {
		_ = queryConn.Close()
		_ = controlConn.Close()
		_ = counter.Close()
	}

	// OAuth (wave 1). The token-endpoint client is SSRF-/timeout-bounded via
	// safehttp; real provider token endpoints are public hosts safehttp allows.
	// For DEV (fake provider on a private compose host) the operator sets
	// ASKER_SAFEHTTP_ALLOW_PRIVATE so safehttp permits the loopback/cluster dial.
	oauthCfg := oauth.LoadConfig()
	oauthHTTP := safehttp.NewClientOrDefault(safehttp.WithTimeout(oauthHTTPTimeout))
	oauthSvc := oauth.New(oauthCfg, oauthHTTP)

	return &deps{
		query:   queryv1.NewQueryServiceClient(queryConn),
		control: controlplanev1.NewControlPlaneServiceClient(controlConn),
		// AdminService shares the control-plane connection (same target). Admin
		// authorization is enforced at the gateway BEFORE these RPCs are dialed.
		admin:  controlplanev1.NewAdminServiceClient(controlConn),
		hubURL: strings.TrimRight(cfg.HubHTTPURL, "/"),
		// Generous timeout: uploads stream through this client.
		hubClient: &http.Client{Timeout: 2 * time.Minute},
		// Media fetches are small (thumbnails/keyframes): a tighter timeout.
		mediaClient:      &http.Client{Timeout: 30 * time.Second},
		counter:          counter,
		recent:           counter, // the Redis counter also backs recent-search history
		prefs:            counter, // ...and the personalization write-through cache
		maxUploadBytes:   cfg.MaxUploadMB << 20,
		maxMediaBytes:    cfg.MaxMediaMB << 20,
		oidcAudience:     cfg.OIDCAudience,
		maxQueryChars:    cfg.MaxQueryChars,
		logger:           logger,
		oauth:            oauthSvc,
		oauthState:       counter,
		oauthCfg:         oauthCfg,
		gatewayPublicURL: strings.TrimRight(cfg.GatewayPublicURL, "/"),
		webAppURL:        strings.TrimRight(cfg.WebAppURL, "/"),
	}, cleanup, nil
}

// oauthHTTPTimeout bounds every OAuth token-endpoint call (exchange/refresh) so
// a slow or hostile provider cannot pin a request goroutine.
const oauthHTTPTimeout = 15 * time.Second
