package tenancygrpc

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/asker/asker/platform/tenancy"
)

// MetadataKey is the gRPC metadata key carrying the tenant identity between
// internal services. Its value is produced by tenancy.HeaderValue and parsed
// by tenancy.FromHeaderValue.
const MetadataKey = "x-asker-tenant"

// UnaryClientInterceptor returns a grpc.UnaryClientInterceptor that injects
// the tenancy.Context carried by the call's context.Context into outgoing
// metadata under MetadataKey.
//
// It fails closed: if the context carries no valid tenancy.Context the RPC is
// rejected locally with codes.FailedPrecondition — a tenant-less internal RPC
// is a programming error and must never reach the wire.
func UnaryClientInterceptor() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		tc, err := tenancy.FromContext(ctx)
		if err != nil {
			return status.Errorf(codes.FailedPrecondition, "tenancygrpc: refusing tenant-less call to %s: %v", method, err)
		}
		ctx = metadata.AppendToOutgoingContext(ctx, MetadataKey, tenancy.HeaderValue(tc))
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

// UnaryServerInterceptor returns a grpc.UnaryServerInterceptor that extracts
// the tenant identity from incoming metadata, validates it with
// tenancy.FromHeaderValue, and installs the resulting tenancy.Context with
// tenancy.WithContext before invoking the handler.
//
// It rejects with codes.Unauthenticated when the MetadataKey value is absent,
// syntactically invalid, or present more than once (an ambiguous tenant is
// treated as an attack, not a tie to break). See the package documentation
// for why a validated metadata value is trusted at all (ADR-009).
func UnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		md, ok := metadata.FromIncomingContext(ctx)
		if !ok {
			return nil, status.Errorf(codes.Unauthenticated, "tenancygrpc: missing metadata on %s", info.FullMethod)
		}
		// Deliberately not echoing received values into the error: they are
		// untrusted and would otherwise flow into logs verbatim.
		switch vals := md.Get(MetadataKey); len(vals) {
		case 0:
			return nil, status.Errorf(codes.Unauthenticated, "tenancygrpc: missing %s metadata on %s", MetadataKey, info.FullMethod)
		case 1:
			tc, err := tenancy.FromHeaderValue(vals[0])
			if err != nil {
				return nil, status.Errorf(codes.Unauthenticated, "tenancygrpc: invalid %s metadata on %s", MetadataKey, info.FullMethod)
			}
			return handler(tenancy.WithContext(ctx, tc), req)
		default:
			return nil, status.Errorf(codes.Unauthenticated, "tenancygrpc: %d %s metadata values on %s, want exactly 1", len(vals), MetadataKey, info.FullMethod)
		}
	}
}
