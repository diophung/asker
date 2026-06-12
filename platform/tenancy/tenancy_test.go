package tenancy

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestFromClaims(t *testing.T) {
	tests := []struct {
		name        string
		claims      map[string]any
		wantErr     error
		wantTenant  TenantID
		wantSubject string
	}{
		{
			name:        "tenant_id only",
			claims:      map[string]any{"tenant_id": "acme"},
			wantTenant:  "acme",
			wantSubject: "acme",
		},
		{
			name:        "sub fallback",
			claims:      map[string]any{"sub": "user-123"},
			wantTenant:  "user-123",
			wantSubject: "user-123",
		},
		{
			name:        "tenant_id takes precedence over sub",
			claims:      map[string]any{"tenant_id": "acme", "sub": "user-123"},
			wantTenant:  "acme",
			wantSubject: "user-123",
		},
		{
			name:        "whitespace trimmed from tenant_id and sub",
			claims:      map[string]any{"tenant_id": "  acme \n", "sub": "\tuser-123 "},
			wantTenant:  "acme",
			wantSubject: "user-123",
		},
		{
			name:        "valid tenant_id with non-string sub falls back to tenant for subject",
			claims:      map[string]any{"tenant_id": "acme", "sub": 42},
			wantTenant:  "acme",
			wantSubject: "acme",
		},
		{
			name:        "valid tenant_id with whitespace-only sub falls back to tenant for subject",
			claims:      map[string]any{"tenant_id": "acme", "sub": "   "},
			wantTenant:  "acme",
			wantSubject: "acme",
		},
		{
			name:    "tenant_id number rejected",
			claims:  map[string]any{"tenant_id": float64(123)},
			wantErr: ErrInvalidTenant,
		},
		{
			name:    "tenant_id nil rejected",
			claims:  map[string]any{"tenant_id": nil},
			wantErr: ErrInvalidTenant,
		},
		{
			name:    "tenant_id bool rejected",
			claims:  map[string]any{"tenant_id": true},
			wantErr: ErrInvalidTenant,
		},
		{
			name:    "tenant_id empty string rejected",
			claims:  map[string]any{"tenant_id": ""},
			wantErr: ErrInvalidTenant,
		},
		{
			name:    "tenant_id whitespace-only rejected",
			claims:  map[string]any{"tenant_id": " \t\n "},
			wantErr: ErrInvalidTenant,
		},
		{
			name:    "invalid tenant_id does not fall back to valid sub",
			claims:  map[string]any{"tenant_id": "", "sub": "user-123"},
			wantErr: ErrInvalidTenant,
		},
		{
			name:    "sub non-string rejected when tenant_id absent",
			claims:  map[string]any{"sub": int64(7)},
			wantErr: ErrInvalidTenant,
		},
		{
			name:    "sub nil rejected when tenant_id absent",
			claims:  map[string]any{"sub": nil},
			wantErr: ErrInvalidTenant,
		},
		{
			name:    "sub empty string rejected when tenant_id absent",
			claims:  map[string]any{"sub": ""},
			wantErr: ErrInvalidTenant,
		},
		{
			name:        "keycloak-style UUID sub accepted",
			claims:      map[string]any{"sub": "ee9cbc18-4962-4026-9dda-1c866056c8bc"},
			wantTenant:  "ee9cbc18-4962-4026-9dda-1c866056c8bc",
			wantSubject: "ee9cbc18-4962-4026-9dda-1c866056c8bc",
		},
		{
			name:        "dots, underscores, and dashes accepted",
			claims:      map[string]any{"tenant_id": "org-1.team_a"},
			wantTenant:  "org-1.team_a",
			wantSubject: "org-1.team_a",
		},
		{
			name:    "tenant_id with path separator rejected",
			claims:  map[string]any{"tenant_id": "acme/other"},
			wantErr: ErrInvalidTenant,
		},
		{
			name:    "tenant_id with embedded newline rejected",
			claims:  map[string]any{"tenant_id": "ac\nme"},
			wantErr: ErrInvalidTenant,
		},
		{
			name:    "tenant_id with zero-width space rejected",
			claims:  map[string]any{"tenant_id": "\u200b"}, // not unicode whitespace, so it survives TrimSpace
			wantErr: ErrInvalidTenant,
		},
		{
			name:    "tenant_id with non-ASCII rejected",
			claims:  map[string]any{"tenant_id": "acmé"},
			wantErr: ErrInvalidTenant,
		},
		{
			name:    "single dot rejected (path segment)",
			claims:  map[string]any{"tenant_id": "."},
			wantErr: ErrInvalidTenant,
		},
		{
			name:    "double dot rejected (path traversal segment)",
			claims:  map[string]any{"tenant_id": ".."},
			wantErr: ErrInvalidTenant,
		},
		{
			name:    "over-long tenant_id rejected",
			claims:  map[string]any{"tenant_id": strings.Repeat("a", 129)},
			wantErr: ErrInvalidTenant,
		},
		{
			name:        "max-length tenant_id accepted",
			claims:      map[string]any{"tenant_id": strings.Repeat("a", 128)},
			wantTenant:  TenantID(strings.Repeat("a", 128)),
			wantSubject: strings.Repeat("a", 128),
		},
		{
			name:    "sub with invalid syntax rejected when tenant_id absent",
			claims:  map[string]any{"sub": "user/123"},
			wantErr: ErrInvalidTenant,
		},
		{
			name:    "neither claim present",
			claims:  map[string]any{"iss": "http://localhost:8081/realms/asker"},
			wantErr: ErrNoTenant,
		},
		{
			name:    "empty claims map",
			claims:  map[string]any{},
			wantErr: ErrNoTenant,
		},
		{
			name:    "nil claims map",
			claims:  nil,
			wantErr: ErrNoTenant,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tc, err := FromClaims(tt.claims)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("FromClaims() error = %v, want %v", err, tt.wantErr)
				}
				if tc != (Context{}) {
					t.Fatalf("FromClaims() returned non-zero Context %+v on error", tc)
				}
				return
			}
			if err != nil {
				t.Fatalf("FromClaims() unexpected error: %v", err)
			}
			if got := tc.TenantID(); got != tt.wantTenant {
				t.Errorf("TenantID() = %q, want %q", got, tt.wantTenant)
			}
			if got := tc.Subject(); got != tt.wantSubject {
				t.Errorf("Subject() = %q, want %q", got, tt.wantSubject)
			}
		})
	}
}

func TestContextRoundTrip(t *testing.T) {
	want, err := FromClaims(map[string]any{"tenant_id": "acme", "sub": "user-123"})
	if err != nil {
		t.Fatalf("FromClaims() unexpected error: %v", err)
	}

	ctx := WithContext(context.Background(), want)
	got, err := FromContext(ctx)
	if err != nil {
		t.Fatalf("FromContext() unexpected error: %v", err)
	}
	if got != want {
		t.Errorf("FromContext() = %+v, want %+v", got, want)
	}
}

func TestFromContextErrors(t *testing.T) {
	tests := []struct {
		name string
		ctx  context.Context
	}{
		{name: "empty context", ctx: context.Background()},
		{name: "zero Context stored", ctx: WithContext(context.Background(), Context{})},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tc, err := FromContext(tt.ctx)
			if !errors.Is(err, ErrNoTenant) {
				t.Fatalf("FromContext() error = %v, want %v", err, ErrNoTenant)
			}
			if tc != (Context{}) {
				t.Fatalf("FromContext() returned non-zero Context %+v on error", tc)
			}
		})
	}
}

func TestMustFromContextPanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("MustFromContext() did not panic on empty context")
		}
		err, ok := r.(error)
		if !ok || !errors.Is(err, ErrNoTenant) {
			t.Fatalf("MustFromContext() panic = %v, want %v", r, ErrNoTenant)
		}
	}()
	MustFromContext(context.Background())
}

func TestMustFromContextReturnsStored(t *testing.T) {
	want, err := FromClaims(map[string]any{"sub": "user-123"})
	if err != nil {
		t.Fatalf("FromClaims() unexpected error: %v", err)
	}

	got := MustFromContext(WithContext(context.Background(), want))
	if got != want {
		t.Errorf("MustFromContext() = %+v, want %+v", got, want)
	}
	if got.TenantID() != "user-123" || got.Subject() != "user-123" {
		t.Errorf("unexpected tenant/subject: %q / %q", got.TenantID(), got.Subject())
	}
}
