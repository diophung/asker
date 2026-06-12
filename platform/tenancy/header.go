package tenancy

import "fmt"

// HeaderValue returns the wire representation of tc's tenant identity for
// transport headers (Kafka record headers, gRPC metadata). It is the inverse
// of FromHeaderValue. The subject is intentionally NOT carried: internal hops
// are tenant-scoped, and a header is not a verified credential for a user.
func HeaderValue(tc Context) string { return string(tc.tenantID) }

// FromHeaderValue reconstructs a Context from a transport header value
// produced by HeaderValue. The value is re-validated against the same tenant
// syntax rules as FromClaims ([A-Za-z0-9._-], 1-128 chars, not "." or ".."),
// so a forged or corrupted header can never yield a usable Context. Unlike
// claim values, header values are machine-generated, so no whitespace
// trimming is applied: any deviation from the exact syntax is rejected.
//
// The header carries no subject, so the returned Context's Subject() equals
// the tenant — matching the FromClaims fallback when "sub" is unusable.
//
// Trust model (ADR-009): callers may only trust this value on the
// compose/cluster-internal network, where the sender is the gateway or
// connector-hub and derived the tenant from a VERIFIED JWT. mTLS hardens
// that network boundary in M4.
func FromHeaderValue(v string) (Context, error) {
	if !validTenant(v) {
		return Context{}, fmt.Errorf("%w: header value", ErrInvalidTenant)
	}
	return Context{tenantID: TenantID(v), subject: v}, nil
}
