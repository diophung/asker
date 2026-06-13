package connectortest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

// readFile reads a file's bytes (a thin wrapper so tests can stay terse).
func readFile(path string) ([]byte, error) { return os.ReadFile(path) }

// contains reports whether haystack contains needle (byte-slice convenience for
// body assertions).
func contains(haystack []byte, needle string) bool {
	return strings.Contains(string(haystack), needle)
}

// sha256Hex returns the lowercase-hex SHA-256 of s, for building body_sha256
// pins in tests.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// newTestServer starts an httptest.Server for handler and closes it on cleanup.
func newTestServer(handler http.HandlerFunc) *httptest.Server {
	return httptest.NewServer(handler)
}

// capturingTB wraps a real *testing.T but intercepts Errorf/Fatalf (so a
// deliberate failure can be asserted without failing the enclosing test) and
// records Cleanup funcs so a test can run them on demand and observe the
// failure they raise. Fatalf does NOT call runtime.Goexit; callers that drive a
// capturingTB must not rely on Fatalf aborting the goroutine.
type capturingTB struct {
	testing.TB
	mu       sync.Mutex
	failed   bool
	errors   []string
	cleanups []func()
}

func (c *capturingTB) Helper() {}

func (c *capturingTB) Errorf(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failed = true
	c.errors = append(c.errors, fmt.Sprintf(format, args...))
}

func (c *capturingTB) Fatalf(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failed = true
	c.errors = append(c.errors, fmt.Sprintf(format, args...))
}

func (c *capturingTB) Logf(format string, args ...any) {}

func (c *capturingTB) Cleanup(f func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cleanups = append(c.cleanups, f)
}

// runCleanups runs registered cleanups in reverse order, like the testing
// package does.
func (c *capturingTB) runCleanups() {
	c.mu.Lock()
	funcs := append([]func(){}, c.cleanups...)
	c.cleanups = nil
	c.mu.Unlock()
	for i := len(funcs) - 1; i >= 0; i-- {
		funcs[i]()
	}
}

func (c *capturingTB) contains(substr string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range c.errors {
		if strings.Contains(m, substr) {
			return true
		}
	}
	return false
}
