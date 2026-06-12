package tenancy

import (
	"errors"
	"strings"
	"testing"
)

func TestHeaderValueRoundTrip(t *testing.T) {
	tests := []struct {
		name   string
		claims map[string]any
	}{
		{name: "simple tenant", claims: map[string]any{"tenant_id": "acme"}},
		{name: "keycloak UUID sub", claims: map[string]any{"sub": "ee9cbc18-4962-4026-9dda-1c866056c8bc"}},
		{name: "dots underscores dashes", claims: map[string]any{"tenant_id": "org-1.team_a"}},
		{name: "max length", claims: map[string]any{"tenant_id": strings.Repeat("a", 128)}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			orig, err := FromClaims(tt.claims)
			if err != nil {
				t.Fatalf("FromClaims() unexpected error: %v", err)
			}

			v := HeaderValue(orig)
			if v != string(orig.TenantID()) {
				t.Fatalf("HeaderValue() = %q, want %q", v, orig.TenantID())
			}

			got, err := FromHeaderValue(v)
			if err != nil {
				t.Fatalf("FromHeaderValue(%q) unexpected error: %v", v, err)
			}
			if got.TenantID() != orig.TenantID() {
				t.Errorf("TenantID() = %q, want %q", got.TenantID(), orig.TenantID())
			}
			// The header carries no subject: Subject() must equal the tenant.
			if got.Subject() != string(orig.TenantID()) {
				t.Errorf("Subject() = %q, want %q", got.Subject(), orig.TenantID())
			}
		})
	}
}

func TestHeaderValueDropsSubject(t *testing.T) {
	orig, err := FromClaims(map[string]any{"tenant_id": "acme", "sub": "user-123"})
	if err != nil {
		t.Fatalf("FromClaims() unexpected error: %v", err)
	}

	got, err := FromHeaderValue(HeaderValue(orig))
	if err != nil {
		t.Fatalf("FromHeaderValue() unexpected error: %v", err)
	}
	if got.TenantID() != "acme" {
		t.Errorf("TenantID() = %q, want %q", got.TenantID(), "acme")
	}
	if got.Subject() != "acme" {
		t.Errorf("Subject() = %q, want %q (subject must not survive the header)", got.Subject(), "acme")
	}
}

func TestFromHeaderValueInvalid(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "empty", value: ""},
		{name: "whitespace only", value: "  "},
		{name: "leading whitespace not trimmed", value: " acme"},
		{name: "trailing whitespace not trimmed", value: "acme\n"},
		{name: "path separator", value: "acme/other"},
		{name: "single dot", value: "."},
		{name: "double dot", value: ".."},
		{name: "non-ASCII", value: "acmé"},
		{name: "zero-width space", value: "\u200b"}, // not unicode whitespace, so no trimming would save it anyway
		{name: "embedded newline", value: "ac\nme"},
		{name: "over-long", value: strings.Repeat("a", 129)},
		{name: "null byte", value: "ac\x00me"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tc, err := FromHeaderValue(tt.value)
			if !errors.Is(err, ErrInvalidTenant) {
				t.Fatalf("FromHeaderValue(%q) error = %v, want %v", tt.value, err, ErrInvalidTenant)
			}
			if tc != (Context{}) {
				t.Fatalf("FromHeaderValue(%q) returned non-zero Context %+v on error", tt.value, tc)
			}
		})
	}
}
