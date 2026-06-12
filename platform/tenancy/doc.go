// Package tenancy is the single chokepoint for tenant identity in Asker.
//
// A tenancy.Context is the ONLY way to construct a data-access context. It is
// derived exclusively from the claims of a verified JWT via FromClaims — never
// from request bodies, query parameters, or headers. Every internal RPC, every
// Kafka message, and every Vespa query MUST carry a tenancy.Context (attached
// to a context.Context with WithContext and recovered with FromContext).
//
// Passing raw tenant strings between components is forbidden by convention and
// enforced in code review: any function touching tenant-scoped data must accept
// a tenancy.Context (or a context.Context carrying one), not a string. The zero
// Context is invalid; FromContext refuses to return it, so a Context with an
// empty TenantID can never flow into data access through this package.
package tenancy
