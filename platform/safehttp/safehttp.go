// Package safehttp provides an SSRF-hardened HTTP client for outbound fetches
// against URLs that originate, even indirectly, from tenant-supplied
// configuration (connector feed_url / endpoint / export_url / base_url, OAuth
// API base URLs, and any redirect target they reach).
//
// # Threat
//
// A connector fetches a tenant-controlled URL server-side. Without a guard that
// URL can name an internal address — the cloud metadata endpoint
// (169.254.169.254, which mints IAM credentials), loopback, RFC1918 / unique-
// local ranges, or cluster-internal service names (vespa, control-plane, vault,
// minio). The fetched body is indexed and becomes searchable (an exfiltration
// oracle), and for OAuth connectors the decrypted bearer token is sent in the
// request to whatever host the URL resolves to. A naive "validate the URL string
// then dial it" check is also defeated by DNS rebinding: the name resolves to a
// public IP at validation time and to 169.254.169.254 at connect time.
//
// # Defense
//
// The guard runs at CONNECT time, on the RESOLVED IP of every connection (the
// initial request and every redirect hop), inside net.Dialer.Control — the hook
// the runtime invokes with the concrete remote address after DNS resolution and
// immediately before connect(2). That closes the resolve-then-connect TOCTOU /
// DNS-rebinding window because the IP the kernel is about to talk to is the IP we
// inspect. A connection to a blocked IP never opens.
//
// The client is SECURE BY DEFAULT: the guard is on, only http and https are
// allowed, redirects are capped and re-checked, and timeouts are bounded.
// Relaxations (allowing private space for a dev/CI compose stack, extra denied
// CIDRs for cluster ranges, an optional host allowlist) are explicit, come from
// [Options] or environment variables, and never the other way round — there is
// no env var that can be set to a value that *opens* loopback in production
// without the operator deliberately setting ASKER_SAFEHTTP_ALLOW_PRIVATE.
//
// # Configuration (environment, read once at NewClient/NewTransport)
//
//	ASKER_SAFEHTTP_ALLOW_PRIVATE=1
//	    Disable the private/loopback/link-local/ULA guard entirely. This is the
//	    dev/CI/compose escape hatch (the whole stack lives on 127.0.0.1 and a
//	    flat compose network); it is LOUDLY logged once and must never be set in
//	    production. The metadata-IP and unspecified-IP checks still apply unless
//	    additionally allowlisted, so even a relaxed dev client will not silently
//	    reach 169.254.169.254.
//	ASKER_SAFEHTTP_EXTRA_DENY_CIDRS=10.99.0.0/16,fd00::/8
//	    Comma-separated extra CIDRs to deny (e.g. the cluster's pod/service
//	    ranges) on top of the built-in private/loopback/etc set.
//	ASKER_SAFEHTTP_ALLOW_HOSTS=example.com,api.example.org
//	    Optional exact-match hostname allowlist. When set, only these hosts may
//	    be dialed (still subject to the IP guard). Empty means "any public host".
package safehttp

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// defaultDialTimeout bounds establishing one TCP connection.
	defaultDialTimeout = 10 * time.Second
	// defaultTimeout bounds a whole request (connect + headers + body) when the
	// caller sets none. Connectors override this with their own per-fetch budget.
	defaultTimeout = 30 * time.Second
	// defaultMaxRedirects caps redirect hops; each hop is re-checked by the
	// guard, so an attacker cannot bounce a public first hop into an internal
	// second hop, but we also refuse to follow an unbounded chain.
	defaultMaxRedirects = 5
	// DefaultMaxResponseBytes is the suggested ceiling for ReadResponse; a
	// connector with a known smaller bound should pass its own.
	DefaultMaxResponseBytes int64 = 64 << 20

	envAllowPrivate  = "ASKER_SAFEHTTP_ALLOW_PRIVATE"
	envExtraDenyCIDR = "ASKER_SAFEHTTP_EXTRA_DENY_CIDRS"
	envAllowHosts    = "ASKER_SAFEHTTP_ALLOW_HOSTS"
)

// ErrBlockedAddress is the sentinel returned (wrapped) when the guard refuses an
// address. Callers can errors.Is against it; the wrapped text names the offending
// IP but never a token or the request's secret query string.
var ErrBlockedAddress = errors.New("safehttp: connection to a blocked address refused")

// ErrBlockedScheme is returned when a non-http(s) URL is dialed.
var ErrBlockedScheme = errors.New("safehttp: only http and https are allowed")

// ErrTooManyRedirects is returned when a redirect chain exceeds the hop cap.
var ErrTooManyRedirects = errors.New("safehttp: too many redirects")

// metadataIP is the cloud instance-metadata address (AWS/GCP/Azure/OpenStack
// IMDS). It is link-local so the link-local check already covers it, but it is
// the single most important target so it is denied explicitly and unconditionally
// — even ASKER_SAFEHTTP_ALLOW_PRIVATE will not open it unless an operator also
// allowlists its host.
var metadataIP = net.IPv4(169, 254, 169, 254)

// Options configures a client/transport. The zero value is secure: guard on, no
// relaxations. Fields left zero fall back to the environment, then to the secure
// default.
type Options struct {
	// AllowPrivate, when true, disables the private/loopback/link-local/ULA
	// guard (NOT the metadata-IP or unspecified-IP checks). Mirrors
	// ASKER_SAFEHTTP_ALLOW_PRIVATE. Use only for dev/CI.
	AllowPrivate bool

	// ExtraDenyCIDRs are additional CIDR ranges to deny (cluster pod/service
	// ranges). Merged with ASKER_SAFEHTTP_EXTRA_DENY_CIDRS.
	ExtraDenyCIDRs []string

	// AllowHosts, when non-empty, is an exact-match hostname allowlist; only
	// these hosts may be dialed. Merged with ASKER_SAFEHTTP_ALLOW_HOSTS.
	AllowHosts []string

	// Timeout is the whole-request timeout (default defaultTimeout). It maps to
	// http.Client.Timeout.
	Timeout time.Duration

	// DialTimeout bounds one TCP connect (default defaultDialTimeout).
	DialTimeout time.Duration

	// MaxRedirects caps redirect hops (default defaultMaxRedirects). A negative
	// value disables redirect following.
	MaxRedirects int

	// Base is the transport whose DialContext is wrapped with the guarded
	// dialer. When nil, a clone of http.DefaultTransport is used. Any provided
	// transport has its dialing replaced so the guard cannot be bypassed.
	Base *http.Transport

	// Logger receives the one-time loud warning when AllowPrivate is enabled.
	// Defaults to slog.Default().
	Logger *slog.Logger

	// allowAllForTest, when true, makes the guard a no-op. It is set ONLY by the
	// in-process test seam (TestingAllowLoopback) and can never be reached from
	// configuration, so production builds cannot disable the guard.
	allowAllForTest bool
}

// guard holds the compiled denylist/allowlist decision state.
type guard struct {
	allowPrivate bool
	allowAll     bool
	extraDeny    []*net.IPNet
	allowHosts   map[string]struct{}
}

// resolveOptions merges o with the environment, applying secure defaults.
func resolveOptions(o Options) (Options, error) {
	if !o.AllowPrivate && envTrue(os.Getenv(envAllowPrivate)) {
		o.AllowPrivate = true
	}
	if env := strings.TrimSpace(os.Getenv(envExtraDenyCIDR)); env != "" {
		o.ExtraDenyCIDRs = append(o.ExtraDenyCIDRs, splitlist(env)...)
	}
	if env := strings.TrimSpace(os.Getenv(envAllowHosts)); env != "" {
		o.AllowHosts = append(o.AllowHosts, splitlist(env)...)
	}
	if o.Timeout == 0 {
		o.Timeout = defaultTimeout
	}
	if o.DialTimeout == 0 {
		o.DialTimeout = defaultDialTimeout
	}
	if o.MaxRedirects == 0 {
		o.MaxRedirects = defaultMaxRedirects
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return o, nil
}

// newGuard compiles o into a guard, parsing the extra CIDRs.
func newGuard(o Options) (*guard, error) {
	g := &guard{
		allowPrivate: o.AllowPrivate,
		allowAll:     o.allowAllForTest,
	}
	for _, c := range o.ExtraDenyCIDRs {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			return nil, fmt.Errorf("safehttp: invalid extra-deny CIDR %q: %w", c, err)
		}
		g.extraDeny = append(g.extraDeny, n)
	}
	if len(o.AllowHosts) > 0 {
		g.allowHosts = make(map[string]struct{}, len(o.AllowHosts))
		for _, h := range o.AllowHosts {
			h = strings.TrimSpace(strings.ToLower(h))
			if h != "" {
				g.allowHosts[h] = struct{}{}
			}
		}
	}
	return g, nil
}

// checkIP is the pure decision: it returns nil if ip may be dialed and a wrapped
// ErrBlockedAddress otherwise. It is always strict regardless of AllowPrivate for
// the metadata IP and the unspecified address; AllowPrivate only relaxes the
// generic private/loopback/link-local/ULA families. This function does NO DNS and
// is the unit under test.
func (g *guard) checkIP(ip net.IP) error {
	if g.allowAll {
		return nil
	}
	if ip == nil {
		return fmt.Errorf("%w: nil IP", ErrBlockedAddress)
	}
	// Normalize so an IPv4-mapped IPv6 address (::ffff:127.0.0.1) is judged as
	// the IPv4 it really is, not slipped past the v4 checks.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}

	// Always denied, even with AllowPrivate: the metadata IP and the
	// unspecified address (0.0.0.0 / :: route to "this host").
	if ip.Equal(metadataIP) {
		return fmt.Errorf("%w: %s is the cloud metadata endpoint", ErrBlockedAddress, ip)
	}
	if ip.IsUnspecified() {
		return fmt.Errorf("%w: %s is the unspecified address", ErrBlockedAddress, ip)
	}

	if g.extraDenied(ip) {
		return fmt.Errorf("%w: %s is in a denied CIDR", ErrBlockedAddress, ip)
	}

	if g.allowPrivate {
		// Dev/CI: the generic private families are permitted (metadata and
		// unspecified were already refused above).
		return nil
	}

	switch {
	case ip.IsLoopback(): // 127/8, ::1
		return fmt.Errorf("%w: %s is loopback", ErrBlockedAddress, ip)
	case ip.IsLinkLocalUnicast(): // 169.254/16, fe80::/10
		return fmt.Errorf("%w: %s is link-local", ErrBlockedAddress, ip)
	case ip.IsLinkLocalMulticast(): // 224.0.0/24, ff02::/16
		return fmt.Errorf("%w: %s is link-local multicast", ErrBlockedAddress, ip)
	case ip.IsInterfaceLocalMulticast():
		return fmt.Errorf("%w: %s is interface-local multicast", ErrBlockedAddress, ip)
	case ip.IsMulticast():
		return fmt.Errorf("%w: %s is multicast", ErrBlockedAddress, ip)
	case ip.IsPrivate(): // 10/8, 172.16/12, 192.168/16, fc00::/7 (ULA)
		return fmt.Errorf("%w: %s is private", ErrBlockedAddress, ip)
	}
	// net.IP.IsPrivate covers fc00::/7 (unique-local) per RFC 4193, but be
	// explicit and defensive for the high half (fd00::/8) in case of a stdlib
	// quirk on some platforms.
	if isULA(ip) {
		return fmt.Errorf("%w: %s is unique-local", ErrBlockedAddress, ip)
	}
	return nil
}

// extraDenied reports whether ip falls in any configured extra-deny CIDR.
func (g *guard) extraDenied(ip net.IP) bool {
	for _, n := range g.extraDeny {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// checkHost enforces the optional allowlist on the connection's host (the host
// portion of the dialed address, lowercased). With no allowlist, any host is
// permitted (still subject to checkIP).
func (g *guard) checkHost(host string) error {
	if g.allowAll || len(g.allowHosts) == 0 {
		return nil
	}
	if _, ok := g.allowHosts[strings.ToLower(host)]; ok {
		return nil
	}
	return fmt.Errorf("%w: host %q is not in the allowlist", ErrBlockedAddress, host)
}

// isULA reports whether ip is an IPv6 unique-local address (fc00::/7).
func isULA(ip net.IP) bool {
	v6 := ip.To16()
	if v6 == nil || ip.To4() != nil {
		return false
	}
	return v6[0]&0xfe == 0xfc
}

// control is the net.Dialer.Control hook. The runtime calls it once per dial
// attempt with the resolved address (host already an IP literal here), so this is
// the connect-time, post-resolution check that defeats DNS rebinding.
func (g *guard) control(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		// Control always receives host:port; treat a parse failure as the whole
		// string being the host.
		host = address
	}
	if err := g.checkHost(host); err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// Control is invoked with the resolved literal IP, so a non-IP here is
		// unexpected; fail closed.
		return fmt.Errorf("%w: %q did not resolve to an IP", ErrBlockedAddress, host)
	}
	return g.checkIP(ip)
}

// warnOnce guards the single loud AllowPrivate warning per process.
var warnOnce sync.Once

// NewTransport builds an *http.Transport whose dialing is guarded. It is safe to
// hand to clients that take a Transport (e.g. minio-go's Options.Transport) or to
// wrap as the base of a credential-injecting RoundTripper.
func NewTransport(opts ...Option) (*http.Transport, error) {
	o, err := buildOptions(opts...)
	if err != nil {
		return nil, err
	}
	g, err := newGuard(o)
	if err != nil {
		return nil, err
	}
	maybeWarn(o, g)

	base := o.Base
	if base == nil {
		base = http.DefaultTransport.(*http.Transport).Clone()
	} else {
		base = base.Clone()
	}
	d := &net.Dialer{
		Timeout:   o.DialTimeout,
		KeepAlive: 30 * time.Second,
		Control:   g.control,
	}
	base.DialContext = d.DialContext
	// A guarded plaintext dialer is not enough for TLS hosts; clearing
	// DialTLSContext forces the transport to use DialContext (then wrap TLS
	// itself), so HTTPS connections are guarded too.
	base.DialTLSContext = nil
	return base, nil
}

// NewClient builds an *http.Client whose transport is guarded (NewTransport) and
// whose CheckRedirect re-applies the guard on every hop and caps the chain.
func NewClient(opts ...Option) (*http.Client, error) {
	o, err := buildOptions(opts...)
	if err != nil {
		return nil, err
	}
	tr, err := NewTransport(opts...)
	if err != nil {
		return nil, err
	}
	return &http.Client{
		Transport:     tr,
		Timeout:       o.Timeout,
		CheckRedirect: checkRedirect(o.MaxRedirects),
	}, nil
}

// NewClientOrDefault is the non-erroring form of [NewClient] for connector
// constructors that have no error return. On a configuration error (e.g. a
// malformed ASKER_SAFEHTTP_EXTRA_DENY_CIDRS) it logs and falls back to a guarded
// client built from valid options only, so a misconfiguration degrades to the
// secure default rather than to an unguarded client. It never returns nil.
func NewClientOrDefault(opts ...Option) *http.Client {
	c, err := NewClient(opts...)
	if err != nil {
		slog.Default().Error("safehttp: client config invalid, falling back to guarded defaults", "err", err)
		c, err = NewClient()
		if err != nil {
			// Cannot happen with no options; fail closed with a refuse-all client.
			return &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
				return nil, fmt.Errorf("%w: guarded client unavailable", ErrBlockedAddress)
			})}
		}
	}
	return c
}

// GuardedBase returns a guarded *http.Transport suitable for use as the `base`
// RoundTripper of a credential-injecting transport (the connectors'
// bearerTransport pattern). On a configuration error it logs and falls back to a
// guarded transport with default options so a connector constructor that cannot
// return an error still gets a guarded dial rather than the raw
// http.DefaultTransport.
func GuardedBase(opts ...Option) http.RoundTripper {
	tr, err := NewTransport(opts...)
	if err != nil {
		slog.Default().Error("safehttp: falling back to default-option guarded transport", "err", err)
		tr, err = NewTransport()
		if err != nil {
			// Cannot happen with no options; if it somehow does, fail closed by
			// returning a transport that refuses every dial.
			return roundTripperFunc(func(*http.Request) (*http.Response, error) {
				return nil, fmt.Errorf("%w: guarded transport unavailable", ErrBlockedAddress)
			})
		}
	}
	return tr
}

// checkRedirect builds an http.Client.CheckRedirect that caps hops. The guard
// itself runs at dial time on every hop's connection, so this function's job is
// the hop cap and refusing a redirect to a non-http(s) scheme; the IP/host re-
// check happens unconditionally in control when the next hop connects.
func checkRedirect(max int) func(req *http.Request, via []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if max < 0 {
			return http.ErrUseLastResponse
		}
		if len(via) >= max {
			return fmt.Errorf("%w: stopped after %d", ErrTooManyRedirects, max)
		}
		if req.URL != nil {
			if s := strings.ToLower(req.URL.Scheme); s != "http" && s != "https" {
				return fmt.Errorf("%w: redirect to %q", ErrBlockedScheme, req.URL.Scheme)
			}
		}
		return nil
	}
}

// maybeWarn emits the one-time loud warning when the guard is relaxed.
func maybeWarn(o Options, g *guard) {
	if g.allowPrivate {
		warnOnce.Do(func() {
			o.Logger.Warn("safehttp: SSRF guard relaxed (private/loopback addresses allowed) " +
				"— this is acceptable only in dev/CI; NEVER set ASKER_SAFEHTTP_ALLOW_PRIVATE in production")
		})
	}
}

// ReadResponse reads at most max bytes of body and returns ErrBlockedScheme-free
// content; it is the body-size-cap helper connectors use so a hostile response
// cannot exhaust memory. A max <= 0 uses DefaultMaxResponseBytes.
func ReadResponse(body io.Reader, max int64) ([]byte, error) {
	if max <= 0 {
		max = DefaultMaxResponseBytes
	}
	return io.ReadAll(io.LimitReader(body, max))
}

// Option mutates Options.
type Option func(*Options)

// WithAllowPrivate sets AllowPrivate.
func WithAllowPrivate(allow bool) Option { return func(o *Options) { o.AllowPrivate = allow } }

// WithExtraDenyCIDRs appends extra denied CIDRs.
func WithExtraDenyCIDRs(cidrs ...string) Option {
	return func(o *Options) { o.ExtraDenyCIDRs = append(o.ExtraDenyCIDRs, cidrs...) }
}

// WithAllowHosts appends to the host allowlist.
func WithAllowHosts(hosts ...string) Option {
	return func(o *Options) { o.AllowHosts = append(o.AllowHosts, hosts...) }
}

// WithTimeout sets the whole-request timeout.
func WithTimeout(d time.Duration) Option { return func(o *Options) { o.Timeout = d } }

// WithDialTimeout sets the per-connect timeout.
func WithDialTimeout(d time.Duration) Option { return func(o *Options) { o.DialTimeout = d } }

// WithMaxRedirects sets the redirect hop cap (negative disables following).
func WithMaxRedirects(n int) Option { return func(o *Options) { o.MaxRedirects = n } }

// WithLogger sets the logger for the one-time relaxation warning.
func WithLogger(l *slog.Logger) Option { return func(o *Options) { o.Logger = l } }

// WithBaseTransport supplies the transport whose dialing is wrapped.
func WithBaseTransport(t *http.Transport) Option { return func(o *Options) { o.Base = t } }

// buildOptions applies opts then resolves env + defaults.
//
// As a final step it consults the in-process test seam: when the binary is a Go
// test binary (testing.Testing()) AND a test has opted in via
// [TestingAllowLoopback], the private-address guard is relaxed. This lets the
// connector contract/unit suites — which point a connector at an httptest server
// on 127.0.0.1 via base_url/feed_url/endpoint — exercise the real guarded client
// without each test having to thread an option through New(). It can ONLY be
// reached from a test (testing.Testing() is false in any production binary), so
// it cannot weaken the shipped guard. safehttp's OWN guard tests do not call it;
// they assert the strict decision directly.
func buildOptions(opts ...Option) (Options, error) {
	var o Options
	for _, opt := range opts {
		opt(&o)
	}
	if testLoopbackAllowed() {
		o.AllowPrivate = true
	}
	return resolveOptions(o)
}

// roundTripperFunc adapts a function to http.RoundTripper.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func envTrue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func splitlist(s string) []string {
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
