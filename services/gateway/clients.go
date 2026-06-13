package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	controlplanev1 "github.com/asker/asker/platform/proto/gen/go/asker/controlplane/v1"
	queryv1 "github.com/asker/asker/platform/proto/gen/go/asker/query/v1"
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
	counter        rateCounter
	maxUploadBytes int64
	maxMediaBytes  int64
	// oidcAudience is the token audience; admin client-role claims live under
	// resource_access.<oidcAudience>.roles.
	oidcAudience string
	// maxQueryChars caps the /v1/search q= length; <= 0 disables the cap.
	maxQueryChars int
	logger        *slog.Logger
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
		mediaClient:    &http.Client{Timeout: 30 * time.Second},
		counter:        counter,
		maxUploadBytes: cfg.MaxUploadMB << 20,
		maxMediaBytes:  cfg.MaxMediaMB << 20,
		oidcAudience:   cfg.OIDCAudience,
		maxQueryChars:  cfg.MaxQueryChars,
		logger:         logger,
	}, cleanup, nil
}
