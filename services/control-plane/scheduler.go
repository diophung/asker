package main

import (
	"context"
	"errors"
	"log/slog"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	controlplanev1 "github.com/asker/asker/platform/proto/gen/go/asker/controlplane/v1"
	"github.com/asker/asker/platform/tenancy/tenancygrpc"
)

// schedulerServer implements controlplanev1.SchedulerServiceServer, the
// INTERNAL-ONLY cross-tenant enumeration surface for the connector-hub
// scheduler, which must discover every tenant's connector instances to
// schedule syncs.
//
// Trust model: this is the one RPC exempt from the x-asker-tenant metadata
// requirement (see tenantInterceptorSkipping in this file — the exemption is
// wired by exact full-method name, never by prefix). It is safe only because
// the gRPC port is never exposed outside the compose/cluster-internal network
// (ADR-009); M4's NetworkPolicies + mTLS turn that topological assumption
// into an enforced one.
type schedulerServer struct {
	controlplanev1.UnimplementedSchedulerServiceServer

	store  Store
	logger *slog.Logger
}

var _ controlplanev1.SchedulerServiceServer = (*schedulerServer)(nil)

func newSchedulerServer(store Store, logger *slog.Logger) *schedulerServer {
	return &schedulerServer{store: store, logger: logger}
}

// ListAllInstances returns every tenant's connector instances as
// (tenant_id, instance) pairs — the only payload anywhere that carries a
// tenant ID, because the caller has no single-tenant scope to inherit.
func (s *schedulerServer) ListAllInstances(ctx context.Context, _ *controlplanev1.ListAllInstancesRequest) (*controlplanev1.ListAllInstancesResponse, error) {
	instances, err := s.store.ListAll(ctx)
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled):
			return nil, status.Error(codes.Canceled, "request canceled")
		case errors.Is(err, context.DeadlineExceeded):
			return nil, status.Error(codes.DeadlineExceeded, "deadline exceeded")
		}
		s.logger.ErrorContext(ctx, "store operation failed",
			"rpc", "ListAllInstances", "error", err)
		return nil, status.Error(codes.Internal, "ListAllInstances failed")
	}
	out := make([]*controlplanev1.TenantInstance, 0, len(instances))
	for _, inst := range instances {
		out = append(out, &controlplanev1.TenantInstance{
			TenantId: string(inst.TenantID),
			Instance: instanceToProto(inst),
		})
	}
	return &controlplanev1.ListAllInstancesResponse{Instances: out}, nil
}

// tenantInterceptorSkipping wraps tenancygrpc.UnaryServerInterceptor so that
// the methods named in exempt (exact full-method strings such as
// "/asker.controlplane.v1.SchedulerService/ListAllInstances") bypass the
// tenant-metadata requirement. Every other method keeps the wrapped
// interceptor's fail-closed behavior unchanged: missing/invalid/ambiguous
// x-asker-tenant metadata is rejected with codes.Unauthenticated before any
// handler runs, and valid metadata is installed as a tenancy.Context.
func tenantInterceptorSkipping(exempt ...string) grpc.UnaryServerInterceptor {
	exemptSet := make(map[string]struct{}, len(exempt))
	for _, m := range exempt {
		exemptSet[m] = struct{}{}
	}
	requireTenant := tenancygrpc.UnaryServerInterceptor()
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if _, ok := exemptSet[info.FullMethod]; ok {
			return handler(ctx, req)
		}
		return requireTenant(ctx, req, info, handler)
	}
}
