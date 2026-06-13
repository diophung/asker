package main

import (
	"context"
	"errors"
	"log/slog"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	controlplanev1 "github.com/asker/asker/platform/proto/gen/go/asker/controlplane/v1"
	"github.com/asker/asker/platform/tenancy"
)

// defaultAdminListLimit / maxAdminListLimit bound AdminService.ListTenants
// paging so an operator call cannot ask for an unbounded scan.
const (
	defaultAdminListLimit = 100
	maxAdminListLimit     = 1000
)

// adminServer implements controlplanev1.AdminServiceServer: the privileged,
// DELIBERATELY cross-tenant operator surface (M6). It acts on the tenant_id in
// the REQUEST, not the caller context — the exact inverse of every
// ControlPlaneService RPC. That is safe ONLY because:
//
//   - the GATEWAY gates /v1/admin/* on a distinct "admin" role/scope claim in
//     the verified token (authorization, not mere authentication; the claim is
//     set by a Keycloak realm/client role mapper, never self-asserted) before
//     it will dial these RPCs;
//   - like SchedulerService, AdminService is exempt from the per-tenant
//     x-asker-tenant metadata requirement by EXACT full-method name (wired in
//     main.go, never by prefix) and is never reachable outside the cluster-
//     internal network (ADR-009; M4 NetworkPolicy + mTLS enforce it);
//   - every call is audit-logged here (action + target tenant), no secrets.
//
// The cross-tenant exemption mirrors SchedulerService.ListAllInstances exactly.
type adminServer struct {
	controlplanev1.UnimplementedAdminServiceServer

	store   Store
	deleter *server // reuses the shared cascadeDelete + serverConfig purgers
	logger  *slog.Logger
}

var _ controlplanev1.AdminServiceServer = (*adminServer)(nil)

func newAdminServer(store Store, deleter *server, logger *slog.Logger) *adminServer {
	return &adminServer{store: store, deleter: deleter, logger: logger}
}

func (a *adminServer) ListTenants(ctx context.Context, req *controlplanev1.ListTenantsRequest) (*controlplanev1.ListTenantsResponse, error) {
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = defaultAdminListLimit
	}
	if limit > maxAdminListLimit {
		limit = maxAdminListLimit
	}
	a.logger.InfoContext(ctx, "admin: ListTenants", "limit", limit, "after", req.GetPageToken())
	usages, err := a.store.ListTenants(ctx, req.GetPageToken(), limit)
	if err != nil {
		return nil, a.storeErr(ctx, "ListTenants", err)
	}
	out := make([]*controlplanev1.TenantUsage, 0, len(usages))
	for _, u := range usages {
		out = append(out, usageToProto(u))
	}
	resp := &controlplanev1.ListTenantsResponse{Tenants: out}
	// A full page implies there may be more; hand back the last id as the
	// keyset cursor. A short page is the last page (empty token).
	if len(usages) == limit {
		resp.NextPageToken = string(usages[len(usages)-1].TenantID)
	}
	return resp, nil
}

func (a *adminServer) GetTenantUsage(ctx context.Context, req *controlplanev1.GetTenantUsageRequest) (*controlplanev1.GetTenantUsageResponse, error) {
	tenantID, err := adminTenant(req.GetTenantId())
	if err != nil {
		return nil, err
	}
	a.logger.InfoContext(ctx, "admin: GetTenantUsage", "target_tenant", tenantID)
	u, err := a.store.GetTenantUsage(ctx, tenantID)
	if err != nil {
		return nil, a.storeErr(ctx, "GetTenantUsage", err)
	}
	return &controlplanev1.GetTenantUsageResponse{Usage: usageToProto(u)}, nil
}

func (a *adminServer) SuspendTenant(ctx context.Context, req *controlplanev1.SuspendTenantRequest) (*controlplanev1.SuspendTenantResponse, error) {
	tenantID, err := adminTenant(req.GetTenantId())
	if err != nil {
		return nil, err
	}
	newStatus := controlplanev1.ConnectorStatus_PAUSED.String()
	if !req.GetSuspended() {
		newStatus = controlplanev1.ConnectorStatus_ACTIVE.String()
	}
	a.logger.InfoContext(ctx, "admin: SuspendTenant",
		"target_tenant", tenantID, "suspended", req.GetSuspended())
	n, err := a.store.SetTenantConnectorStatus(ctx, tenantID, newStatus)
	if err != nil {
		return nil, a.storeErr(ctx, "SuspendTenant", err)
	}
	return &controlplanev1.SuspendTenantResponse{InstancesChanged: n}, nil
}

func (a *adminServer) AdminDeleteTenant(ctx context.Context, req *controlplanev1.AdminDeleteTenantRequest) (*controlplanev1.AdminDeleteTenantResponse, error) {
	tenantID, err := adminTenant(req.GetTenantId())
	if err != nil {
		return nil, err
	}
	a.logger.WarnContext(ctx, "admin: AdminDeleteTenant (irreversible erasure)", "target_tenant", tenantID)
	report, err := a.deleter.cascadeDelete(ctx, tenantID, "admin")
	if err != nil {
		return nil, err
	}
	return &controlplanev1.AdminDeleteTenantResponse{Report: report}, nil
}

// adminTenant validates a request-supplied tenant id against the SAME syntax
// allowlist every tenant must satisfy. Admin RPCs are the only place a tenant
// arrives in a request body, so the value is validated here exactly as the
// gateway/tenancygrpc would, before it reaches a store query or storage key.
func adminTenant(raw string) (tenancy.TenantID, error) {
	if raw == "" {
		return "", status.Error(codes.InvalidArgument, "tenant_id is required")
	}
	tc, err := tenancy.FromHeaderValue(raw)
	if err != nil {
		return "", status.Error(codes.InvalidArgument, "tenant_id is not a valid tenant identifier")
	}
	return tc.TenantID(), nil
}

func (a *adminServer) storeErr(ctx context.Context, rpc string, err error) error {
	switch {
	case errors.Is(err, ErrNotFound):
		return status.Error(codes.NotFound, "tenant not found")
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "request canceled")
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "deadline exceeded")
	}
	a.logger.ErrorContext(ctx, "admin store operation failed", "rpc", rpc, "error", err)
	return status.Errorf(codes.Internal, "%s failed", rpc)
}

func usageToProto(u TenantUsage) *controlplanev1.TenantUsage {
	return &controlplanev1.TenantUsage{
		TenantId:           string(u.TenantID),
		Created:            timestamppb.New(u.CreatedAt),
		ConnectorInstances: u.ConnectorInstances,
		DocsEmitted:        u.DocsEmitted,
		Suspended:          u.Suspended,
	}
}
