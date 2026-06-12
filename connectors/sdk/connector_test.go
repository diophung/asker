package sdk

import (
	"context"
	"testing"
)

func TestAuthTypeString(t *testing.T) {
	tests := []struct {
		a    AuthType
		want string
	}{
		{AuthNone, "none"},
		{AuthOAuth2, "oauth2"},
		{AuthToken, "token"},
		{AuthType(42), "authtype(42)"},
	}
	for _, tt := range tests {
		if got := tt.a.String(); got != tt.want {
			t.Errorf("AuthType(%d).String() = %q, want %q", int(tt.a), got, tt.want)
		}
	}
}

func TestNopCheckpoint(t *testing.T) {
	var cp Checkpoint = NopCheckpoint // must satisfy the Checkpoint type
	if err := cp(context.Background(), Cursor("anything")); err != nil {
		t.Errorf("NopCheckpoint returned %v, want nil", err)
	}
}
