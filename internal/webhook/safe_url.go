package webhook

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// Policy relaxes destination checks for development receivers only.
type Policy struct {
	AllowHTTP    bool // plain http:// callback URLs
	AllowPrivate bool // loopback, private and other non-public addresses
}

// ErrBlocked marks a destination the policy refuses; it is not retried.
var ErrBlocked = errors.New("webhook: destination not allowed")

// blockedPrefixes are never reachable from a client-controlled URL: the
// legacy set (loopback, link-local, RFC 1918, CGNAT, multicast, unspecified,
// ULA) plus this-network, IETF protocol assignments, benchmarking,
// documentation, reserved, broadcast and the translation prefixes that can
// embed a private IPv4 address.
var blockedPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24",
		"192.0.2.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
		"::/128", "::1/128", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001:db8::/32", "2002::/16", "fc00::/7", "fe80::/10", "ff00::/8"} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// BlockedIP reports an address a callback must never reach.
func BlockedIP(ip netip.Addr) bool {
	if !ip.IsValid() {
		return true
	}
	ip = ip.Unmap()
	for _, p := range blockedPrefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// ValidateURL is the static check of a callback URL (management writes and
// every dispatch): http(s) only, plain http only when allowed, a host, no
// credentials, and no literal or well-known local destination. Hostnames are
// resolved and checked again at connection time.
func ValidateURL(raw string, p Policy) error {
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || !u.IsAbs() {
		return fmt.Errorf("%w: invalid url", ErrBlocked)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "http":
		if !p.AllowHTTP {
			return fmt.Errorf("%w: http callback URLs are not allowed", ErrBlocked)
		}
	default:
		return fmt.Errorf("%w: unsupported scheme", ErrBlocked)
	}
	if u.User != nil {
		return fmt.Errorf("%w: credentials in url", ErrBlocked)
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" {
		return fmt.Errorf("%w: missing host", ErrBlocked)
	}
	if p.AllowPrivate {
		return nil
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || host == "ip6-localhost" || host == "ip6-loopback" {
		return fmt.Errorf("%w: local host", ErrBlocked)
	}
	if ip, err := netip.ParseAddr(host); err == nil && BlockedIP(ip) {
		return fmt.Errorf("%w: non-public address", ErrBlocked)
	}
	return nil
}

// Resolver resolves host names (net.DefaultResolver).
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// safeDialer resolves once, refuses the host if any address is blocked
// (split-horizon answers cannot pick a private one) and dials the vetted
// literals; the connect-time control hook re-checks the final address, so
// DNS rebinding between check and connect cannot reach a private address.
type safeDialer struct {
	resolver Resolver
	inner    net.Dialer
	policy   Policy
}

func newSafeDialer(r Resolver, p Policy) *safeDialer {
	d := &safeDialer{resolver: r, policy: p, inner: net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}}
	if !p.AllowPrivate {
		d.inner.Control = blockAtConnect
	}
	return d
}

func (d *safeDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	var ips []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		ips = []netip.Addr{ip}
	} else if ips, err = d.resolver.LookupNetIP(ctx, "ip", host); err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("webhook: no addresses for host")
	}
	if !d.policy.AllowPrivate {
		for _, ip := range ips {
			if BlockedIP(ip) {
				return nil, fmt.Errorf("%w: host resolves to a non-public address", ErrBlocked)
			}
		}
	}
	var last error
	for _, ip := range ips {
		conn, err := d.inner.DialContext(ctx, network, net.JoinHostPort(ip.Unmap().String(), port))
		if err == nil {
			return conn, nil
		}
		last = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, last
}

func blockAtConnect(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("%w: refusing non-literal address at connect", ErrBlocked)
	}
	if BlockedIP(ap.Addr()) {
		return fmt.Errorf("%w: non-public address at connect", ErrBlocked)
	}
	return nil
}

// noRedirect makes a 3xx the final response; redirects are never followed.
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
