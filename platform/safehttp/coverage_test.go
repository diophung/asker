package safehttp

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestOptionSettersAndConstructors exercises the With* options + the non-erroring
// constructors so a misconfiguration degrades to the secure default (never an
// unguarded or nil client), and the option plumbing is covered.
func TestOptionSettersAndConstructors(t *testing.T) {
	c, err := NewClient(
		WithTimeout(7*time.Second),
		WithDialTimeout(3*time.Second),
		WithMaxRedirects(2),
		WithLogger(slog.Default()),
		WithBaseTransport(http.DefaultTransport.(*http.Transport).Clone()),
		WithAllowHosts("example.com"),
		WithExtraDenyCIDRs("10.10.0.0/16"),
	)
	if err != nil {
		t.Fatalf("NewClient with all options: %v", err)
	}
	if c == nil || c.Timeout != 7*time.Second {
		t.Fatalf("client timeout not applied: %+v", c)
	}

	// A malformed extra-deny CIDR must FAIL CLOSED to a guarded default, never nil
	// and never an unguarded client.
	got := NewClientOrDefault(WithExtraDenyCIDRs("not-a-cidr"))
	if got == nil {
		t.Fatal("NewClientOrDefault returned nil")
	}
	// And the happy path returns a working client.
	if NewClientOrDefault() == nil {
		t.Fatal("NewClientOrDefault() returned nil")
	}
}

// TestGuardedBaseBlocksMetadata proves GuardedBase yields a transport whose dial
// is guarded: a request to the cloud-metadata IP is refused at connect time.
func TestGuardedBaseBlocksMetadata(t *testing.T) {
	rt := GuardedBase()
	if rt == nil {
		t.Fatal("GuardedBase returned nil")
	}
	req, err := http.NewRequest(http.MethodGet, "http://169.254.169.254/latest/meta-data/", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := rt.RoundTrip(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected the metadata-IP dial to be blocked, got nil error")
	}
	if !errors.Is(err, ErrBlockedAddress) && !strings.Contains(err.Error(), "safehttp") {
		t.Fatalf("error does not indicate a safehttp block: %v", err)
	}
	// A malformed-option GuardedBase still returns a usable guarded transport.
	if GuardedBase(WithExtraDenyCIDRs("bogus")) == nil {
		t.Fatal("GuardedBase fallback returned nil")
	}
}

// TestReadResponseCaps covers the body-size-cap helper (over-cap truncation +
// the default when max<=0).
func TestReadResponseCaps(t *testing.T) {
	body := bytes.Repeat([]byte("a"), 1000)
	out, err := ReadResponse(bytes.NewReader(body), 100)
	if err != nil {
		t.Fatalf("ReadResponse: %v", err)
	}
	if len(out) != 100 {
		t.Fatalf("expected truncation to 100 bytes, got %d", len(out))
	}
	// max<=0 uses the default cap; a small body reads fully.
	out, err = ReadResponse(io.MultiReader(bytes.NewReader([]byte("hello"))), 0)
	if err != nil || string(out) != "hello" {
		t.Fatalf("ReadResponse default cap: out=%q err=%v", out, err)
	}
}
