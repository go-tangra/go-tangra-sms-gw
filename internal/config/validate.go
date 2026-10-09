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

// Validate checks the Freya config and every module section. It returns the
// first problem found.
func (c Config) Validate() error {
	if errs := c.ValidateAll(); len(errs) > 0 {
		return errs[0]
	}
	return nil
}

// ValidateAll runs the same checks as Validate but keeps going and returns
// every problem, in the order Validate meets them (its first element is
// Validate's error). Used by preflight.
func (c Config) ValidateAll() []error {
	errs := c.Config.ValidateAll()
	add := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	prod := c.IsProduction()
	if c.Server.HTTPAddr == "" {
		add(errors.New("config: server.http_addr is required (management API on the mesh)"))
	}
	errs = append(errs, c.validateDB(prod)...)
	switch c.KEK.Source {
	case "file":
		if c.KEK.Path == "" {
			add(errors.New("config: kek.path is required for kek.source file"))
		}
	case "env":
		if c.KEK.Env == "" {
			add(errors.New("config: kek.env is required for kek.source env"))
		}
	default:
		add(errors.New("config: kek.source must be file or env"))
	}
	if c.Gateway.Service == "" || c.Gateway.AuthService == "" {
		add(errors.New("config: gateway.service and gateway.auth_service are required"))
	}
	if iu, err := url.Parse(c.Gateway.Issuer); err != nil || iu.Scheme != "https" || iu.Host == "" {
		add(errors.New("config: gateway.issuer must be an https origin"))
	}
	add(c.validateIdentity())
	add(c.validateListeners())
	errs = append(errs, c.validatePublic(prod)...)
	add(c.validateACME(prod))
	return append(errs, c.validateLimits(prod)...)
}

func (c Config) validateDB(prod bool) []error {
	var errs []error
	if c.DB.DSN == "" {
		errs = append(errs, errors.New("config: db.dsn is required"))
	}
	if prod {
		for _, d := range []struct{ name, dsn string }{{"db.dsn", c.DB.DSN}, {"db.migrate_dsn", c.DB.MigrateDSN}} {
			if d.dsn != "" && !strings.Contains(d.dsn, "sslmode=verify-full") && !strings.Contains(d.dsn, "sslmode=verify-ca") {
				errs = append(errs, fmt.Errorf("config: db dsn must use sslmode=verify-full (or verify-ca) in production (%s)", d.name))
			}
		}
	}
	if c.DB.MaxConns < 1 || c.DB.MaxConns > 256 {
		errs = append(errs, errors.New("config: db.max_conns must be within [1, 256]"))
	}
	return errs
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

func (c Config) validatePublic(prod bool) []error {
	var errs []error
	p := c.Public
	if (p.TLSCertFile == "") != (p.TLSKeyFile == "") {
		errs = append(errs, errors.New("config: public.tls_cert_file and public.tls_key_file go together"))
	}
	if p.TLSEnabled() && c.ACME.Enabled {
		errs = append(errs, errors.New("config: public static TLS and acme.enabled are mutually exclusive"))
	}
	if p.MaxBodyBytes < 1<<10 || p.MaxBodyBytes > 1<<20 {
		errs = append(errs, errors.New("config: public.max_body_bytes must be within [1 KiB, 1 MiB]"))
	}
	for _, cidr := range p.TrustedProxies {
		if _, err := netip.ParsePrefix(cidr); err != nil {
			if _, err := netip.ParseAddr(cidr); err != nil {
				errs = append(errs, fmt.Errorf("config: public.trusted_proxies entry %q is not an address or CIDR", cidr))
			}
		}
	}
	a := c.PublicAuth
	if !a.JWTSecret.Set() || (a.JWTSecret.File != "" && a.JWTSecret.Env != "") {
		errs = append(errs, errors.New("config: public_auth.jwt_secret needs exactly one of file or env"))
	}
	if a.AccessTTLSeconds < 60 || a.AccessTTLSeconds > 86400 {
		errs = append(errs, errors.New("config: public_auth.access_ttl_seconds must be within [60, 86400]"))
	}
	if a.RefreshTTLSeconds < a.AccessTTLSeconds || a.RefreshTTLSeconds > 90*86400 {
		errs = append(errs, errors.New("config: public_auth.refresh_ttl_seconds must be within [access_ttl_seconds, 7776000]"))
	}
	if prod && (c.Webhook.AllowHTTP || c.Webhook.AllowPrivate) {
		errs = append(errs, errors.New("config: webhook.allow_http and webhook.allow_private are not permitted in production"))
	}
	return errs
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
	if a.HTTPAddr != "" && a.Challenge != "http-01" {
		return errors.New("config: acme.http_addr serves http-01 challenges only")
	}
	if a.RenewBefore < 24*time.Hour || a.RenewBefore > 60*24*time.Hour {
		return errors.New("config: acme.renew_before must be within [24h, 1440h]")
	}
	if (a.EABKeyID != "") != a.EABHMACKey.Set() {
		return errors.New("config: acme.eab_kid and acme.eab_hmac_key go together")
	}
	return nil
}

func (c Config) validateLimits(prod bool) []error {
	var errs []error
	r := c.RateLimits
	if r.LoginPerMinute <= 0 || r.SendPerMinute <= 0 || r.DLRPerMinute <= 0 || r.LoginBurst < 1 || r.SendBurst < 1 || r.DLRBurst < 1 {
		errs = append(errs, errors.New("config: rate_limits values must be positive"))
	}
	rc := c.Recipients
	if rc.MinDigits < 1 || rc.MinDigits > 15 {
		errs = append(errs, errors.New("config: recipients.min_digits must be within [1, 15]"))
	}
	for _, p := range append(append([]string(nil), rc.AllowedPrefixes...), rc.BlockedPrefixes...) {
		if !digitsRE.MatchString(p) {
			errs = append(errs, fmt.Errorf("config: recipients prefix %q must be 1 to 15 digits without +", p))
		}
	}
	if c.Webhook.QueueSize < 1 || c.Webhook.QueueSize > 1<<16 || c.Webhook.Workers < 1 || c.Webhook.Workers > 64 {
		errs = append(errs, errors.New("config: webhook.queue_size must be within [1, 65536] and webhook.workers within [1, 64]"))
	}
	if c.Retention.Interval < time.Minute || c.Retention.Interval > 24*time.Hour {
		errs = append(errs, errors.New("config: retention.interval must be within [1m, 24h]"))
	}
	if m := c.Monitoring.PrometheusURL; m != "" {
		u, err := url.Parse(m)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
			errs = append(errs, errors.New("config: monitoring.prometheus_url must be an http(s) URL without credentials"))
		} else if prod && u.Scheme != "https" {
			errs = append(errs, errors.New("config: monitoring.prometheus_url must use https in production"))
		}
	}
	q := c.Query
	if q.MaxPageSize < 1 || q.MaxPageSize > 10000 || q.DefaultPageSize < 1 || q.DefaultPageSize > q.MaxPageSize {
		errs = append(errs, errors.New("config: query.default_page_size and query.max_page_size must satisfy 1 <= default <= max <= 10000"))
	}
	return errs
}
