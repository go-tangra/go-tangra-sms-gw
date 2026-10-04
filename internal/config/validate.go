package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var (
	uuidRE   = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	digitsRE = regexp.MustCompile(`^[0-9]{1,15}$`)
	hostRE   = regexp.MustCompile(`^(\*\.)?([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)
)

// Validate checks the Freya config and every module section.
func (c Config) Validate() error {
	if err := c.Config.Validate(); err != nil {
		return err
	}
	prod := c.IsProduction()
	if c.Server.HTTPAddr == "" {
		return errors.New("config: server.http_addr is required (management API on the mesh)")
	}
	if err := c.validateDB(prod); err != nil {
		return err
	}
	switch c.KEK.Source {
	case "file":
		if c.KEK.Path == "" {
			return errors.New("config: kek.path is required for kek.source file")
		}
	case "env":
		if c.KEK.Env == "" {
			return errors.New("config: kek.env is required for kek.source env")
		}
	default:
		return errors.New("config: kek.source must be file or env")
	}
	if c.Gateway.Service == "" || c.Gateway.AuthService == "" {
		return errors.New("config: gateway.service and gateway.auth_service are required")
	}
	if iu, err := url.Parse(c.Gateway.Issuer); err != nil || iu.Scheme != "https" || iu.Host == "" {
		return errors.New("config: gateway.issuer must be an https origin")
	}
	if err := c.validateIdentity(); err != nil {
		return err
	}
	if err := c.validateListeners(); err != nil {
		return err
	}
	if err := c.validatePublic(prod); err != nil {
		return err
	}
	if err := c.validateACME(prod); err != nil {
		return err
	}
	return c.validateLimits(prod)
}

func (c Config) validateDB(prod bool) error {
	if c.DB.DSN == "" {
		return errors.New("config: db.dsn is required")
	}
	for _, dsn := range []string{c.DB.DSN, c.DB.MigrateDSN} {
		if prod && dsn != "" && !strings.Contains(dsn, "sslmode=verify-full") && !strings.Contains(dsn, "sslmode=verify-ca") {
			return errors.New("config: db dsn must use sslmode=verify-full (or verify-ca) in production")
		}
	}
	if c.DB.MaxConns < 1 || c.DB.MaxConns > 256 {
		return errors.New("config: db.max_conns must be within [1, 256]")
	}
	return nil
}

// validateIdentity refuses conflicting identities: network enrollment and a
// provided identity go together, nothing else may claim the mesh SVID.
func (c Config) validateIdentity() error {
	e := c.Enroll
	provided := c.Identity.Provider == "provided"
	if !e.Enabled {
		if provided {
			return errors.New("config: identity.provider provided requires enroll.enabled")
		}
		return nil
	}
	if !provided {
		return errors.New("config: enroll.enabled requires identity.provider: provided")
	}
	if e.EnrollURL == "" || e.LCMGRPCTarget == "" || e.TenantID == "" || e.TokenFile == "" || e.StateFile == "" {
		return errors.New("config: enroll requires enroll_url, lcm_grpc, tenant_id, token_file and state_file")
	}
	if u, err := url.Parse(e.EnrollURL); err != nil || u.Scheme != "https" || u.Host == "" {
		return errors.New("config: enroll.enroll_url must be an https URL")
	}
	if !uuidRE.MatchString(e.TenantID) {
		return errors.New("config: enroll.tenant_id must be a uuid")
	}
	if e.Insecure && c.IsProduction() {
		return errors.New("config: enroll.insecure is not permitted in production")
	}
	return nil
}

// validateListeners keeps the public edge apart from the mesh, admin and
// ACME listeners.
func (c Config) validateListeners() error {
	type listener struct{ name, addr string }
	ls := []listener{{"server.grpc_addr", c.Server.GRPCAddr}, {"server.http_addr", c.Server.HTTPAddr}, {"admin.addr", c.Admin.Addr}, {"public.http_addr", c.Public.HTTPAddr}}
	if c.Public.TLSEnabled() || c.ACME.Enabled {
		ls = append(ls, listener{"public.https_addr", c.Public.HTTPSAddr})
	}
	if c.ACME.Enabled && c.ACME.HTTPAddr != "" {
		ls = append(ls, listener{"acme.http_addr", c.ACME.HTTPAddr})
	}
	for i, a := range ls {
		if a.addr == "" {
			return fmt.Errorf("config: %s is required", a.name)
		}
		if _, _, err := net.SplitHostPort(a.addr); err != nil {
			return fmt.Errorf("config: %s must be host:port", a.name)
		}
		for _, b := range ls[:i] {
			if overlaps(a.addr, b.addr) {
				return fmt.Errorf("config: %s and %s must be separate listeners", b.name, a.name)
			}
		}
	}
	return nil
}

// overlaps reports two listen addresses that would bind the same socket
// (same port and the same or an unspecified host). Ephemeral ports never do.
func overlaps(a, b string) bool {
	ha, pa, _ := net.SplitHostPort(a)
	hb, pb, _ := net.SplitHostPort(b)
	if pa != pb || pa == "0" {
		return false
	}
	unspec := func(h string) bool { ip := net.ParseIP(h); return h == "" || (ip != nil && ip.IsUnspecified()) }
	return ha == hb || unspec(ha) || unspec(hb)
}

func (c Config) validatePublic(prod bool) error {
	p := c.Public
	if (p.TLSCertFile == "") != (p.TLSKeyFile == "") {
		return errors.New("config: public.tls_cert_file and public.tls_key_file go together")
	}
	if p.TLSEnabled() && c.ACME.Enabled {
		return errors.New("config: public static TLS and acme.enabled are mutually exclusive")
	}
	if p.MaxBodyBytes < 1<<10 || p.MaxBodyBytes > 1<<20 {
		return errors.New("config: public.max_body_bytes must be within [1 KiB, 1 MiB]")
	}
	for _, cidr := range p.TrustedProxies {
		if _, err := netip.ParsePrefix(cidr); err != nil {
			if _, err := netip.ParseAddr(cidr); err != nil {
				return fmt.Errorf("config: public.trusted_proxies entry %q is not an address or CIDR", cidr)
			}
		}
	}
	a := c.PublicAuth
	if !a.JWTSecret.Set() || (a.JWTSecret.File != "" && a.JWTSecret.Env != "") {
		return errors.New("config: public_auth.jwt_secret needs exactly one of file or env")
	}
	if a.AccessTTLSeconds < 60 || a.AccessTTLSeconds > 86400 {
		return errors.New("config: public_auth.access_ttl_seconds must be within [60, 86400]")
	}
	if a.RefreshTTLSeconds < a.AccessTTLSeconds || a.RefreshTTLSeconds > 90*86400 {
		return errors.New("config: public_auth.refresh_ttl_seconds must be within [access_ttl_seconds, 7776000]")
	}
	if prod && (c.Webhook.AllowHTTP || c.Webhook.AllowPrivate) {
		return errors.New("config: webhook.allow_http and webhook.allow_private are not permitted in production")
	}
	return nil
}

func (c Config) validateACME(prod bool) error {
	a := c.ACME
	if !a.Enabled {
		return nil
	}
	if len(a.Domains) == 0 || len(a.Domains) > 20 {
		return errors.New("config: acme.domains needs 1 to 20 host names")
	}
	for _, d := range a.Domains {
		if !hostRE.MatchString(d) || strings.HasPrefix(d, "*.") {
			return fmt.Errorf("config: acme.domains entry %q is not a host name", d)
		}
	}
	if !a.AcceptTOS {
		return errors.New("config: acme.accept_tos must be true to use an ACME CA")
	}
	if a.CacheDir == "" {
		return errors.New("config: acme.cache_dir is required (persistent volume)")
	}
	u, err := url.Parse(a.DirectoryURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && !(u.Scheme == "http" && a.AllowInsecureDir)) {
		return errors.New("config: acme.directory_url must be an https URL")
	}
	if a.AllowInsecureDir && prod {
		return errors.New("config: acme.allow_insecure_directory is not permitted in production")
	}
	if a.Challenge != "http-01" && a.Challenge != "tls-alpn-01" {
		return errors.New("config: acme.challenge must be http-01 or tls-alpn-01")
	}
	if a.RenewBefore < 24*time.Hour || a.RenewBefore > 60*24*time.Hour {
		return errors.New("config: acme.renew_before must be within [24h, 1440h]")
	}
	if (a.EABKeyID != "") != a.EABHMACKey.Set() {
		return errors.New("config: acme.eab_kid and acme.eab_hmac_key go together")
	}
	return nil
}

func (c Config) validateLimits(prod bool) error {
	r := c.RateLimits
	if r.LoginPerMinute <= 0 || r.SendPerMinute <= 0 || r.DLRPerMinute <= 0 || r.LoginBurst < 1 || r.SendBurst < 1 || r.DLRBurst < 1 {
		return errors.New("config: rate_limits values must be positive")
	}
	rc := c.Recipients
	if rc.MinDigits < 1 || rc.MinDigits > 15 {
		return errors.New("config: recipients.min_digits must be within [1, 15]")
	}
	for _, p := range append(append([]string(nil), rc.AllowedPrefixes...), rc.BlockedPrefixes...) {
		if !digitsRE.MatchString(p) {
			return fmt.Errorf("config: recipients prefix %q must be 1 to 15 digits without +", p)
		}
	}
	if c.Webhook.QueueSize < 1 || c.Webhook.QueueSize > 1<<16 || c.Webhook.Workers < 1 || c.Webhook.Workers > 64 {
		return errors.New("config: webhook.queue_size must be within [1, 65536] and webhook.workers within [1, 64]")
	}
	if c.Retention.Interval < time.Minute || c.Retention.Interval > 24*time.Hour {
		return errors.New("config: retention.interval must be within [1m, 24h]")
	}
	if m := c.Monitoring.PrometheusURL; m != "" {
		u, err := url.Parse(m)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
			return errors.New("config: monitoring.prometheus_url must be an http(s) URL without credentials")
		}
		if prod && u.Scheme != "https" {
			return errors.New("config: monitoring.prometheus_url must use https in production")
		}
	}
	q := c.Query
	if q.MaxPageSize < 1 || q.MaxPageSize > 10000 || q.DefaultPageSize < 1 || q.DefaultPageSize > q.MaxPageSize {
		return errors.New("config: query.default_page_size and query.max_page_size must satisfy 1 <= default <= max <= 10000")
	}
	return nil
}
