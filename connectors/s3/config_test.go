package s3

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/asker/asker/connectors/sdk"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
	"github.com/asker/asker/platform/tenancy"
)

func TestParseConfig(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		raw         string
		wantErr     bool
		wantHost    string
		wantSSL     bool
		wantBucket  string
		wantPrefix  string
		wantErrText string
	}{
		{name: "minimal", raw: `{"endpoint":"s3.example.com","bucket":"b"}`, wantHost: "s3.example.com", wantBucket: "b"},
		{name: "with prefix and ssl", raw: `{"endpoint":"s3.example.com","bucket":"b","prefix":"docs/","use_ssl":true}`, wantHost: "s3.example.com", wantSSL: true, wantBucket: "b", wantPrefix: "docs/"},
		{name: "http scheme stripped", raw: `{"endpoint":"http://127.0.0.1:9000","bucket":"b"}`, wantHost: "127.0.0.1:9000", wantSSL: false, wantBucket: "b"},
		{name: "https scheme infers ssl", raw: `{"endpoint":"https://127.0.0.1:9000","bucket":"b"}`, wantHost: "127.0.0.1:9000", wantSSL: true, wantBucket: "b"},
		{name: "empty config", raw: ``, wantErr: true, wantErrText: "required"},
		{name: "bad json", raw: `{not json`, wantErr: true},
		{name: "missing endpoint", raw: `{"bucket":"b"}`, wantErr: true, wantErrText: "endpoint"},
		{name: "missing bucket", raw: `{"endpoint":"e"}`, wantErr: true, wantErrText: "bucket"},
		{name: "bad url endpoint", raw: `{"endpoint":"http://","bucket":"b"}`, wantErr: true, wantErrText: "endpoint"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conf, err := parseConfig([]byte(tc.raw))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (conf=%+v)", conf)
				}
				if tc.wantErrText != "" && !strings.Contains(err.Error(), tc.wantErrText) {
					t.Errorf("error %q does not contain %q", err.Error(), tc.wantErrText)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if conf.Endpoint != tc.wantHost {
				t.Errorf("endpoint = %q, want %q", conf.Endpoint, tc.wantHost)
			}
			if conf.UseSSL != tc.wantSSL {
				t.Errorf("use_ssl = %v, want %v", conf.UseSSL, tc.wantSSL)
			}
			if conf.Bucket != tc.wantBucket {
				t.Errorf("bucket = %q, want %q", conf.Bucket, tc.wantBucket)
			}
			if conf.Prefix != tc.wantPrefix {
				t.Errorf("prefix = %q, want %q", conf.Prefix, tc.wantPrefix)
			}
		})
	}
}

func TestSplitToken(t *testing.T) {
	t.Parallel()
	id, secret, err := splitToken([]byte("AKID:SECRET"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "AKID" || secret != "SECRET" {
		t.Errorf("split = %q,%q want AKID,SECRET", id, secret)
	}

	for _, bad := range []string{"", "noseparator", ":secret", "id:"} {
		if _, _, err := splitToken([]byte(bad)); err == nil {
			t.Errorf("splitToken(%q) expected error", bad)
		} else if strings.Contains(err.Error(), bad) && bad != "" {
			t.Errorf("splitToken error leaked the credential %q: %v", bad, err)
		}
	}
}

func TestNewClientUsesDefaultRegion(t *testing.T) {
	t.Parallel()
	c := New().(*Connector)
	cl, err := c.newClient(instanceConfig{Endpoint: "127.0.0.1:9000", Bucket: "b"}, []byte("id:secret"))
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	if cl.bucket != "b" {
		t.Errorf("bucket = %q, want b", cl.bucket)
	}
	if _, err := c.newClient(instanceConfig{Endpoint: "127.0.0.1:9000", Bucket: "b"}, []byte("bad")); err == nil {
		t.Error("newClient with bad token expected error")
	}
}

func TestWithLogger(t *testing.T) {
	t.Parallel()
	// A nil logger leaves the default in place; a real logger is adopted.
	if New(WithLogger(nil)).(*Connector).log == nil {
		t.Error("WithLogger(nil) cleared the default logger")
	}
	l := slog.New(slog.NewTextHandler(io.Discard, nil))
	if New(WithLogger(l)).(*Connector).log != l {
		t.Error("WithLogger did not adopt the provided logger")
	}
}

func TestFullSyncRejectsBadConfig(t *testing.T) {
	t.Parallel()
	tcx, err := tenancy.FromClaims(map[string]any{"tenant_id": "t", "sub": "u"})
	if err != nil {
		t.Fatalf("tenancy.FromClaims: %v", err)
	}
	cfg := sdk.Config{Tenant: tcx, ConfigJSON: []byte(`{"bucket":"b"}`), Token: []byte("id:secret"), Checkpoint: sdk.NopCheckpoint}
	if _, err := New().FullSync(context.Background(), cfg, func(context.Context, *askerv1.Document) error { return nil }); err == nil {
		t.Error("FullSync with missing endpoint expected an error")
	}
}
