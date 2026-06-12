package tenancy

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// TenantID identifies a tenant. It is always non-empty in a valid Context.
type TenantID string

// Context carries the tenant identity extracted from a verified JWT.
// The zero value is invalid; construct one with FromClaims.
type Context struct {
	tenantID TenantID
	subject  string
}

// TenantID returns the tenant identifier.
func (c Context) TenantID() TenantID { return c.tenantID }

// Subject returns the JWT "sub" claim when it was present, otherwise the
// tenant value itself.
func (c Context) Subject() string { return c.subject }

var (
	// ErrNoTenant is returned when no tenant is present in the context or claims.
	ErrNoTenant = errors.New("tenancy: no tenant in context or claims")
	// ErrInvalidTenant is returned when a tenant claim is present but is not a
	// string matching the tenant syntax rules.
	ErrInvalidTenant = errors.New("tenancy: invalid tenant claim")
)

// tenantPattern bounds what a derived tenant may look like. Tenant IDs are
// embedded downstream in storage keys, Vespa group selectors, and Kafka
// message keys, so they must be short and contain no separators, control
// characters, or non-ASCII (Keycloak subject UUIDs always satisfy this).
// The rule is recorded in ADR-002.
var tenantPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// validTenant reports whether s is acceptable as a tenant identifier.
func validTenant(s string) bool {
	// "." and ".." would act as path segments if a tenant ever reaches a
	// filesystem- or URL-shaped key, so they are rejected outright.
	if s == "." || s == ".." {
		return false
	}
	return tenantPattern.MatchString(s)
}

// FromClaims derives a Context from verified JWT claims. It prefers the
// "tenant_id" claim and falls back to "sub" only when "tenant_id" is absent.
// The chosen claim must be a string that, after trimming whitespace, matches
// the tenant syntax rules ([A-Za-z0-9._-], 1-128 chars, not "." or "..");
// anything else yields ErrInvalidTenant — a present-but-invalid "tenant_id"
// never falls back to "sub". If neither claim exists, FromClaims returns
// ErrNoTenant.
func FromClaims(claims map[string]any) (Context, error) {
	tenantRaw, tenantPresent := claims["tenant_id"]
	subRaw, subPresent := claims["sub"]

	if !tenantPresent && !subPresent {
		return Context{}, ErrNoTenant
	}

	tenantClaim, tenantVal := "tenant_id", tenantRaw
	if !tenantPresent {
		tenantClaim, tenantVal = "sub", subRaw
	}
	tenant, ok := stringClaim(tenantVal)
	if !ok || !validTenant(tenant) {
		return Context{}, fmt.Errorf("%w: %q", ErrInvalidTenant, tenantClaim)
	}

	// Subject is the "sub" claim when it is a usable string (even when the
	// tenant came from "tenant_id"); otherwise it falls back to the tenant.
	subject := tenant
	if subPresent {
		if s, ok := stringClaim(subRaw); ok {
			subject = s
		}
	}

	return Context{tenantID: TenantID(tenant), subject: subject}, nil
}

// stringClaim reports whether v is a string that is non-empty after trimming
// whitespace, returning the trimmed value.
func stringClaim(v any) (string, bool) {
	s, ok := v.(string)
	if !ok {
		return "", false
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	return s, true
}

// ctxKey is the unexported context key type for storing a Context.
type ctxKey struct{}

// WithContext returns a copy of ctx carrying tc.
func WithContext(ctx context.Context, tc Context) context.Context {
	return context.WithValue(ctx, ctxKey{}, tc)
}

// FromContext extracts the Context stored by WithContext. It returns
// ErrNoTenant when none is stored or when the stored Context is the invalid
// zero value, so a returned Context always has a non-empty TenantID.
func FromContext(ctx context.Context) (Context, error) {
	tc, ok := ctx.Value(ctxKey{}).(Context)
	if !ok || tc.tenantID == "" {
		return Context{}, ErrNoTenant
	}
	return tc, nil
}

// MustFromContext is like FromContext but panics when no valid Context is
// present. Use only where the middleware chain guarantees one.
func MustFromContext(ctx context.Context) Context {
	tc, err := FromContext(ctx)
	if err != nil {
		panic(err)
	}
	return tc
}
