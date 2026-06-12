// Package tenancygrpc propagates tenancy.Context across internal gRPC hops
// via the "x-asker-tenant" metadata key, fail-closed in both directions:
// the client interceptor refuses to send a tenant-less internal RPC
// (codes.FailedPrecondition), and the server interceptor refuses any request
// without exactly one valid tenant metadata value (codes.Unauthenticated).
//
// Trust model (ADR-009): internal services accept the tenant metadata value
// without re-verifying a JWT because only the gateway and connector-hub —
// which derived the tenant from a VERIFIED JWT via tenancy.FromClaims — can
// reach them on the compose/cluster-internal network. The metadata value is
// still re-validated against the tenant syntax allowlist on receipt, so a
// malformed value can never become a usable tenancy.Context. mTLS hardens the
// network boundary itself in M4 (ADR-009).
package tenancygrpc
