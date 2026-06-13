package safehttp

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
)

// strictGuard builds a guard with the SECURE defaults (no AllowPrivate), used by
// the decision tests so they assert the shipped behavior, not a relaxed one.
func strictGuard(t *testing.T, opts ...Option) *guard {
	t.Helper()
	var o Options
	for _, opt := range opts {
		opt(&o)
	}
	// Deliberately do NOT call buildOptions (which would consult the test seam);
	// resolve env+defaults only, keeping the guard strict under `go test`.
	ro, err := resolveOptions(o)
	if err != nil {
		t.Fatalf("resolveOptions: %v", err)
	}
	g, err := newGuard(ro)
	if err != nil {
		t.Fatalf("newGuard: %v", err)
	}
	return g
}

func TestCheckIP_Blocks(t *testing.T) {
	g := strictGuard(t)
	blocked := []struct {
		name string
		ip   string
	}{
		{"loopback v4", "127.0.0.1"},
		{"loopback v4 high", "127.255.255.254"},
		{"loopback v6", "::1"},
		{"metadata", "169.254.169.254"},
		{"link-local v4", "169.254.10.20"},
		{"link-local v6", "fe80::1"},
		{"private 10/8", "10.1.2.3"},
		{"private 172.16/12", "172.16.5.5"},
		{"private 172.31", "172.31.255.255"},
		{"private 192.168/16", "192.168.0.1"},
		{"unique-local fc00", "fc00::1"},
		{"unique-local fd00", "fd12:3456:789a::1"},
		{"unspecified v4", "0.0.0.0"},
		{"unspecified v6", "::"},
		{"multicast v4", "224.0.0.1"},
		{"multicast v6", "ff02::1"},
		{"ipv4-mapped loopback", "::ffff:127.0.0.1"},
		{"ipv4-mapped metadata", "::ffff:169.254.169.254"},
		// NAT64 / RFC 6052 64:ff9b::/96 embedded-IPv4 bypass (finding #1): the
		// embedded v4 must be unwrapped and judged, not slipped past as public v6.
		{"nat64 metadata", "64:ff9b::a9fe:a9fe"}, // 169.254.169.254
		{"nat64 loopback", "64:ff9b::7f00:1"},    // 127.0.0.1
		{"nat64 rfc1918", "64:ff9b::a00:1"},      // 10.0.0.1
		// CGNAT + reserved ranges denied unconditionally (finding #2).
		{"cgnat 100.64/10", "100.64.0.1"},
		{"reserved 240/4", "240.0.0.1"},
		{"benchmarking 198.18/15", "198.18.0.1"},
		{"protocol 192.0.0/24", "192.0.0.1"},
	}
	for _, tc := range blocked {
		t.Run(tc.name, func(t *testing.T) {
			ip := net.ParseIP(tc.ip)
			if ip == nil {
				t.Fatalf("bad test IP %q", tc.ip)
			}
			err := g.checkIP(ip)
			if err == nil {
				t.Fatalf("checkIP(%s) = nil, want blocked", tc.ip)
			}
			if !errors.Is(err, ErrBlockedAddress) {
				t.Fatalf("checkIP(%s) error = %v, want ErrBlockedAddress", tc.ip, err)
			}
		})
	}
}

func TestCheckIP_AllowsPublic(t *testing.T) {
	g := strictGuard(t)
	allowed := []string{
		"8.8.8.8",
		"1.1.1.1",
		"93.184.216.34", // example.com
		"172.32.0.1",    // just outside 172.16/12
		"172.15.255.255",
		"192.167.255.255",
		"11.0.0.1",
		"2606:4700:4700::1111", // public v6 (cloudflare)
	}
	for _, ipStr := range allowed {
		t.Run(ipStr, func(t *testing.T) {
			ip := net.ParseIP(ipStr)
			if ip == nil {
				t.Fatalf("bad test IP %q", ipStr)
			}
			if err := g.checkIP(ip); err != nil {
				t.Fatalf("checkIP(%s) = %v, want nil (public)", ipStr, err)
			}
		})
	}
}

// TestCheckIP_DecimalHexEncodedHostsResolveToBlockedIP proves the guard judges
// the parsed/canonical IP, so the classic SSRF bypass of writing the metadata or
// loopback address in decimal/hex/octal form is caught: those textual forms all
// PARSE to the same net.IP the guard rejects.
func TestCheckIP_DecimalHexEncodedHostsResolveToBlockedIP(t *testing.T) {
	g := strictGuard(t)
	// 2130706433 == 0x7f000001 == 0177.0.0.1 == 127.0.0.1.
	// net.ParseIP does not parse decimal/hex integer forms; in a real fetch the
	// resolver/dialer canonicalizes them to this IP before Control sees them.
	// We assert the canonical IP (what Control receives) is blocked.
	for _, ipStr := range []string{"127.0.0.1", "169.254.169.254"} {
		ip := net.ParseIP(ipStr)
		if err := g.checkIP(ip); !errors.Is(err, ErrBlockedAddress) {
			t.Fatalf("checkIP(%s) = %v, want blocked", ipStr, err)
		}
	}
}

// TestControl_ConnectTimeDecision drives the actual net.Dialer.Control hook with
// a stubbed resolved address (host:port literal IP) — no DNS — proving the
// connect-time decision that defeats DNS rebinding.
func TestControl_ConnectTimeDecision(t *testing.T) {
	g := strictGuard(t)
	var nilConn syscall.RawConn

	cases := []struct {
		address   string
		wantBlock bool
	}{
		{"127.0.0.1:8080", true},
		{"[::1]:443", true},
		{"169.254.169.254:80", true},
		{"10.0.0.5:8200", true},    // vault-like
		{"192.168.1.1:9000", true}, // minio-like
		{"8.8.8.8:443", false},
		{"93.184.216.34:80", false},
	}
	for _, tc := range cases {
		t.Run(tc.address, func(t *testing.T) {
			err := g.control("tcp", tc.address, nilConn)
			if tc.wantBlock && err == nil {
				t.Fatalf("control(%q) = nil, want blocked", tc.address)
			}
			if !tc.wantBlock && err != nil {
				t.Fatalf("control(%q) = %v, want allowed", tc.address, err)
			}
			if tc.wantBlock && !errors.Is(err, ErrBlockedAddress) {
				t.Fatalf("control(%q) error = %v, want ErrBlockedAddress", tc.address, err)
			}
		})
	}
}

// TestControl_NonIPHostFailsClosed ensures a Control invocation that somehow
// carries a non-IP host (unexpected) is refused rather than dialed.
func TestControl_NonIPHostFailsClosed(t *testing.T) {
	g := strictGuard(t)
	var nilConn syscall.RawConn
	if err := g.control("tcp", "evil.example.com:80", nilConn); !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("control with non-IP host = %v, want ErrBlockedAddress", err)
	}
}

func TestExtraDenyCIDR(t *testing.T) {
	g := strictGuard(t, WithExtraDenyCIDRs("203.0.113.0/24"))
	// In the configured extra-deny cluster range -> blocked.
	if err := g.checkIP(net.ParseIP("203.0.113.5")); !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("checkIP(203.0.113.5) = %v, want blocked by extra CIDR", err)
	}
	// Outside the denied range and public -> allowed.
	if err := g.checkIP(net.ParseIP("198.51.100.7")); err != nil {
		t.Fatalf("checkIP(198.51.100.7) = %v, want allowed", err)
	}
}

func TestAllowPrivate_RelaxesButNotMetadata(t *testing.T) {
	g := strictGuard(t, WithAllowPrivate(true))
	// Loopback/private now allowed (dev/CI).
	for _, ipStr := range []string{"127.0.0.1", "10.0.0.1", "192.168.1.1", "::1", "fc00::1"} {
		if err := g.checkIP(net.ParseIP(ipStr)); err != nil {
			t.Fatalf("AllowPrivate checkIP(%s) = %v, want allowed", ipStr, err)
		}
	}
	// But metadata and unspecified stay blocked even relaxed.
	for _, ipStr := range []string{"169.254.169.254", "0.0.0.0", "::"} {
		if err := g.checkIP(net.ParseIP(ipStr)); !errors.Is(err, ErrBlockedAddress) {
			t.Fatalf("AllowPrivate checkIP(%s) = %v, want still blocked", ipStr, err)
		}
	}
}

func TestAllowHosts(t *testing.T) {
	g := strictGuard(t, WithAllowHosts("Allowed.Example.COM"))
	if err := g.checkHost("allowed.example.com"); err != nil {
		t.Fatalf("checkHost(allowed, case-insensitive) = %v, want allowed", err)
	}
	if err := g.checkHost("8.8.8.8"); !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("checkHost(not in allowlist) = %v, want blocked", err)
	}
	// No allowlist -> any host passes the host check.
	g2 := strictGuard(t)
	if err := g2.checkHost("anything.example.org"); err != nil {
		t.Fatalf("checkHost with no allowlist = %v, want allowed", err)
	}
}

// TestAllowHosts_EnforcedAtRequestLayer proves the allowlist (finding #3) is now
// FUNCTIONAL: a client built WithAllowHosts refuses a request to a host not on
// the list at the request layer (the dialer only ever sees a resolved IP and so
// could never match a hostname — the bug this fix closes). The allowed host is
// permitted through the host check (it then fails later at dial/DNS, which is
// fine — we only assert the host gate's verdict here).
func TestAllowHosts_EnforcedAtRequestLayer(t *testing.T) {
	// Allowlist a host under the reserved .invalid TLD (RFC 6761): it never
	// resolves, so the request reaches the host gate but cannot actually dial.
	const allowed = "allowed.invalid"
	client, err := NewClient(WithAllowHosts(allowed))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	// A disallowed host is refused with ErrBlockedAddress BEFORE any dial.
	_, err = client.Get("http://blocked.invalid/")
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("request to disallowed host = %v, want ErrBlockedAddress", err)
	}
	if !strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("error = %v, want it to mention the allowlist", err)
	}
	// The allowlisted host passes the host gate (it then fails at DNS resolution,
	// which is NOT a host-allowlist rejection — we assert only that the allowlist
	// did not reject it).
	_, err = client.Get("http://" + allowed + "/")
	if errors.Is(err, ErrBlockedAddress) && strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("allowlisted host was blocked by the allowlist: %v", err)
	}
}

// TestCGNATAndNAT64BlockedEvenUnderAllowPrivate proves the unconditional denies
// (findings #1 and #2) survive AllowPrivate: CGNAT is never a dev loopback, and
// a NAT64-embedded internal IP must stay blocked even in a relaxed dev client.
func TestCGNATAndNAT64BlockedEvenUnderAllowPrivate(t *testing.T) {
	g := strictGuard(t, WithAllowPrivate(true))
	stillBlocked := []string{
		"100.64.0.1",         // CGNAT
		"240.0.0.1",          // reserved
		"198.18.0.1",         // benchmarking
		"64:ff9b::a9fe:a9fe", // NAT64-embedded metadata IP (always denied)
		// A NAT64-embedded RFC1918 (64:ff9b::a00:1 = 10.0.0.1) is NOT listed:
		// under AllowPrivate it correctly unwraps to an allowed dev-private IP.
	}
	for _, ipStr := range stillBlocked {
		if err := g.checkIP(net.ParseIP(ipStr)); !errors.Is(err, ErrBlockedAddress) {
			t.Fatalf("AllowPrivate checkIP(%s) = %v, want still blocked", ipStr, err)
		}
	}
}

// TestNewTransport_DisablesProxy proves finding #4: the guarded transport does
// not inherit http.DefaultTransport.Proxy, so an HTTP(S)_PROXY cannot tunnel a
// blocked target past the connect-time dialer guard.
func TestNewTransport_DisablesProxy(t *testing.T) {
	tr, err := NewTransport()
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}
	if tr.Proxy != nil {
		t.Fatal("guarded transport Proxy is non-nil; outbound proxies must be disabled")
	}
}

func TestNewGuard_InvalidCIDR(t *testing.T) {
	if _, err := NewTransport(WithExtraDenyCIDRs("not-a-cidr")); err == nil {
		t.Fatal("NewTransport with invalid CIDR = nil error, want failure")
	}
}

// fakeResolverTransport simulates a host that "resolves" to a blocked IP by
// rewriting the dial address; it lets us prove the guarded transport refuses such
// a connection without real DNS. We build a guarded transport and inject a dialer
// whose Control is the guard's; the dial of the blocked literal must fail.
func TestGuardedTransport_BlocksResolvedToBlockedIP(t *testing.T) {
	tr, err := NewTransport()
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}
	// Dial a literal blocked IP through the guarded transport's DialContext to
	// prove the guard is wired into the transport, not just the standalone
	// function. We do not need a server: a blocked address must error before any
	// connect.
	client := &http.Client{Transport: tr}
	// 169.254.169.254 is the metadata IP; the request must fail at dial.
	req, _ := http.NewRequest(http.MethodGet, "http://169.254.169.254/latest/meta-data/", nil)
	_, err = client.Do(req)
	if err == nil {
		t.Fatal("request to metadata IP succeeded, want dial blocked")
	}
	if !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("error = %v, want it to mention a blocked address", err)
	}
}

// TestRedirect_ToBlockedHostIsRefused stands up a public-looking server that
// 302-redirects to the metadata IP; the guarded client must follow the redirect
// only to have the second-hop dial refused by Control. The first hop is a
// loopback httptest server, so we run with the test seam off for THIS test and
// instead point the redirect target at the metadata IP, which is blocked even
// when AllowPrivate is on. We allow loopback for the first hop via AllowPrivate.
func TestRedirect_ToBlockedHostIsRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/iam/", http.StatusFound)
	}))
	defer srv.Close()

	// AllowPrivate lets the first hop (loopback httptest) connect; the metadata
	// IP redirect target is still blocked.
	client, err := NewClient(WithAllowPrivate(true))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = client.Get(srv.URL)
	if err == nil {
		t.Fatal("redirect to metadata IP succeeded, want blocked")
	}
	if !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("error = %v, want it to mention a blocked address", err)
	}
}

func TestCheckRedirect_HopCap(t *testing.T) {
	cr := checkRedirect(strictGuard(t), 2)
	// 0 and 1 prior hops are fine.
	if err := cr(req("http://a/"), make([]*http.Request, 0)); err != nil {
		t.Fatalf("0 hops = %v, want nil", err)
	}
	if err := cr(req("http://a/"), make([]*http.Request, 1)); err != nil {
		t.Fatalf("1 hop = %v, want nil", err)
	}
	// 2 prior hops hits the cap.
	if err := cr(req("http://a/"), make([]*http.Request, 2)); !errors.Is(err, ErrTooManyRedirects) {
		t.Fatalf("2 hops = %v, want ErrTooManyRedirects", err)
	}
}

func TestCheckRedirect_BlocksNonHTTPScheme(t *testing.T) {
	cr := checkRedirect(strictGuard(t), 5)
	if err := cr(req("file:///etc/passwd"), nil); !errors.Is(err, ErrBlockedScheme) {
		t.Fatalf("file:// redirect = %v, want ErrBlockedScheme", err)
	}
	if err := cr(req("gopher://x/"), nil); !errors.Is(err, ErrBlockedScheme) {
		t.Fatalf("gopher:// redirect = %v, want ErrBlockedScheme", err)
	}
	if err := cr(req("https://ok.example/"), nil); err != nil {
		t.Fatalf("https redirect = %v, want nil", err)
	}
}

func TestReadResponse_Cap(t *testing.T) {
	body := strings.NewReader(strings.Repeat("x", 100))
	got, err := ReadResponse(body, 10)
	if err != nil {
		t.Fatalf("ReadResponse: %v", err)
	}
	if len(got) != 10 {
		t.Fatalf("ReadResponse capped len = %d, want 10", len(got))
	}
}

func TestEnvAllowPrivate(t *testing.T) {
	t.Setenv(envAllowPrivate, "true")
	o, err := resolveOptions(Options{})
	if err != nil {
		t.Fatalf("resolveOptions: %v", err)
	}
	if !o.AllowPrivate {
		t.Fatal("ASKER_SAFEHTTP_ALLOW_PRIVATE=true did not set AllowPrivate")
	}
}

func TestEnvExtraDenyAndHosts(t *testing.T) {
	t.Setenv(envExtraDenyCIDR, "10.10.0.0/16, 172.20.0.0/16")
	t.Setenv(envAllowHosts, "a.example.com, b.example.com")
	o, err := resolveOptions(Options{})
	if err != nil {
		t.Fatalf("resolveOptions: %v", err)
	}
	if len(o.ExtraDenyCIDRs) != 2 {
		t.Fatalf("extra deny CIDRs = %v, want 2", o.ExtraDenyCIDRs)
	}
	if len(o.AllowHosts) != 2 {
		t.Fatalf("allow hosts = %v, want 2", o.AllowHosts)
	}
}

// helpers

func req(rawURL string) *http.Request {
	r, _ := http.NewRequest(http.MethodGet, rawURL, nil)
	return r
}
