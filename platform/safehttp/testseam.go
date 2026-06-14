package safehttp

import (
	"sync/atomic"
	"testing"
)

// testLoopbackOptIn is set by [TestingAllowLoopback]. It only has effect when the
// process is a Go test binary; production binaries never call testing.Testing()
// true, so this can never relax the shipped guard.
var testLoopbackOptIn atomic.Bool

// TestingAllowLoopback relaxes the private/loopback guard for every safehttp
// client and transport constructed for the remainder of the test process,
// registering t.Cleanup to restore the secure default. It exists so connector
// contract/unit tests — which point a connector at an httptest.Server on
// 127.0.0.1 — can run against the REAL guarded client without each test passing a
// transport option through the connector's New().
//
// It is a no-op outside a test binary (it takes a *testing.T, and
// testing.Testing() gates the effect), so it cannot be reached from production
// code and cannot weaken the shipped guard. The metadata-IP and unspecified-IP
// checks remain in force even when this is set.
//
// Connector test packages call this once (e.g. in TestMain or an init helper):
//
//	func TestMain(m *testing.M) {
//		// dial loopback replay servers under the real guarded client
//		safehttp.SetTestingAllowLoopback(true)
//		os.Exit(m.Run())
//	}
func TestingAllowLoopback(t *testing.T) {
	t.Helper()
	prev := testLoopbackOptIn.Load()
	testLoopbackOptIn.Store(true)
	t.Cleanup(func() { testLoopbackOptIn.Store(prev) })
}

// SetTestingAllowLoopback is the TestMain-friendly form of
// [TestingAllowLoopback] (TestMain has no *testing.T). It only takes effect in a
// test binary. Pass true before m.Run() to allow connector tests to dial their
// loopback replay servers through the guarded client.
func SetTestingAllowLoopback(allow bool) {
	if testing.Testing() {
		testLoopbackOptIn.Store(allow)
	}
}

// testLoopbackAllowed reports whether the in-process test seam is active. It is
// true only in a test binary that opted in.
func testLoopbackAllowed() bool {
	return testing.Testing() && testLoopbackOptIn.Load()
}
