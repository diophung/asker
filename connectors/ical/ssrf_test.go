package ical

import (
	"os"
	"testing"

	"github.com/asker/asker/platform/safehttp"
)

// TestMain relaxes the safehttp SSRF guard for this test binary so the contract
// and unit tests — which point the connector at an httptest.Server on
// 127.0.0.1 via feed_url/base_url — can dial their loopback replay servers
// through the REAL guarded client. The relaxation is a test-only seam
// (testing.Testing()-gated) and never affects production binaries; the guard's
// own rejection of loopback/metadata/private addresses is proven directly in
// platform/safehttp.
func TestMain(m *testing.M) {
	safehttp.SetTestingAllowLoopback(true)
	os.Exit(m.Run())
}
