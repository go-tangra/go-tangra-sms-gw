// Package trustedproxy resolves the public client address: X-Real-IP or the
// leftmost X-Forwarded-For entry count only when the connection comes from a
// configured trusted proxy; otherwise the socket address is used, so a direct
// caller cannot choose its rate-limit key or recorded address.
package trustedproxy

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// Resolver holds the trusted proxy prefixes.
type Resolver struct{ trusted []netip.Prefix }

// New parses addresses or CIDR prefixes; an invalid entry is an error.
func New(entries []string) (*Resolver, error) {
	r := &Resolver{}
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		p, err := netip.ParsePrefix(e)
		if err != nil {
			a, aerr := netip.ParseAddr(e)
			if aerr != nil {
				return nil, fmt.Errorf("trustedproxy: %q is not an address or CIDR", e)
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		r.trusted = append(r.trusted, p.Masked())
	}
	return r, nil
}

// Trusted reports whether ip is a configured proxy.
func (r *Resolver) Trusted(ip string) bool {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	a = a.Unmap()
	for _, p := range r.trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// ClientIP is the address to attribute the request to.
func (r *Resolver) ClientIP(req *http.Request) string {
	socket := stripPort(req.RemoteAddr)
	if !r.Trusted(socket) {
		return socket
	}
	if v := strings.TrimSpace(req.Header.Get("X-Real-IP")); v != "" {
		if net.ParseIP(v) == nil {
			return socket
		}
		return v
	}
	if v := req.Header.Get("X-Forwarded-For"); v != "" {
		first, _, _ := strings.Cut(v, ",")
		if first = strings.TrimSpace(first); net.ParseIP(first) != nil {
			return first
		}
	}
	return socket
}

func stripPort(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return strings.TrimSuffix(strings.TrimPrefix(addr, "["), "]")
}
