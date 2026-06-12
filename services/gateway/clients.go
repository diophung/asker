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
	// hubURL is the connector-hub base URL (no trailing slash).
	hubURL    string
	hubClient *http.Client
	// counter backs the per-tenant rate limiter (Redis in production).
	counter        rateCounter
	maxUploadBytes int64
	logger         *slog.Logger
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
	cleanup := func() {
		_ = queryConn.Close()
		_ = controlConn.Close()
	}
	return &deps{
		query:   queryv1.NewQueryServiceClient(queryConn),
		control: controlplanev1.NewControlPlaneServiceClient(controlConn),
		hubURL:  strings.TrimRight(cfg.HubHTTPURL, "/"),
		// Generous timeout: uploads stream through this client.
		hubClient:      &http.Client{Timeout: 2 * time.Minute},
		counter:        newRedisCounter(cfg.RedisAddr),
		maxUploadBytes: cfg.MaxUploadMB << 20,
		logger:         logger,
	}, cleanup, nil
}
