package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Obsolete legacy variables: the V4 runtime replaces what they configured
// (mesh identity, registration, admin gRPC). They are reported, never used.
var obsoleteEnv = []string{
	"GRPC_ADVERTISE_ADDR", "HTTP_ADVERTISE_ADDR", "ADMIN_GRPC_ENDPOINT", "FRONTEND_ENTRY_URL",
	"SMSGW_CA_CERT_PATH", "SMSGW_SERVER_CERT_PATH", "SMSGW_SERVER_KEY_PATH",
	"LCM_BOOTSTRAP_ENDPOINT", "MODULE_BOOTSTRAP_SECRET", "LCM_CA_FINGERPRINT", "CERTS_DIR",
}

// ApplyLegacyEnv maps the legacy SMS_GW_* environment onto c (environment
// wins over the file) and returns the names it applied and the obsolete
// names it ignored. Unlike the legacy service, an unparseable value is an
// error instead of a silent fallback. Secrets become references: the JWT
// secret and the EAB key are read from their variables at start, the values
// are never copied into the configuration.
func ApplyLegacyEnv(c *Config, lookup func(string) (string, bool)) (applied, obsolete []string, err error) {
	get := func(k string) (string, bool) {
		v, ok := lookup(k)
		v = strings.TrimSpace(v)
		if ok && v != "" {
			applied = append(applied, k)
			return v, true
		}
		return "", false
	}
	str := func(k string, dst *string) {
		if v, ok := get(k); ok {
			*dst = v
		}
	}
	ref := func(k string, dst *SecretRef) {
		if _, ok := get(k); ok {
			*dst = SecretRef{Env: k}
		}
	}
	num := func(k string, set func(float64)) {
		if err != nil {
			return
		}
		if v, ok := get(k); ok {
			f, perr := strconv.ParseFloat(v, 64)
			if perr != nil || f <= 0 {
				err = fmt.Errorf("config: %s must be a positive number", k)
				return
			}
			set(f)
		}
	}
	flag := func(k string, dst *bool) {
		if err != nil {
			return
		}
		if v, ok := get(k); ok {
			b, perr := strconv.ParseBool(v)
			if perr != nil {
				err = fmt.Errorf("config: %s must be a boolean", k)
				return
			}
			*dst = b
		}
	}
	dur := func(k string, dst *time.Duration) {
		if err != nil {
			return
		}
		if v, ok := get(k); ok {
			d, perr := time.ParseDuration(v)
			if perr != nil || d <= 0 {
				err = fmt.Errorf("config: %s must be a positive duration", k)
				return
			}
			*dst = d
		}
	}
	list := func(k string, dst *[]string) {
		if v, ok := get(k); ok {
			var out []string
			for _, p := range strings.Split(v, ",") {
				if p = strings.TrimPrefix(strings.TrimSpace(p), "+"); p != "" {
					out = append(out, p)
				}
			}
			*dst = out
		}
	}

	ref("SMS_GW_JWT_SECRET", &c.PublicAuth.JWTSecret)
	num("SMS_GW_ACCESS_TTL_SECONDS", func(f float64) { c.PublicAuth.AccessTTLSeconds = int(f) })
	num("SMS_GW_REFRESH_TTL_SECONDS", func(f float64) { c.PublicAuth.RefreshTTLSeconds = int(f) })
	str("HTTP_PUBLIC_BIND", &c.Public.HTTPAddr)
	str("SMS_GW_TLS_BIND", &c.Public.HTTPSAddr)
	str("SMS_GW_TLS_CERT_PATH", &c.Public.TLSCertFile)
	str("SMS_GW_TLS_KEY_PATH", &c.Public.TLSKeyFile)
	list("TRUSTED_PROXY_CIDRS", &c.Public.TrustedProxies)
	num("SMS_GW_LOGIN_RPM", func(f float64) { c.RateLimits.LoginPerMinute = f })
	num("SMS_GW_LOGIN_BURST", func(f float64) { c.RateLimits.LoginBurst = int(f) })
	num("SMS_GW_SEND_RPM", func(f float64) { c.RateLimits.SendPerMinute = f })
	num("SMS_GW_SEND_BURST", func(f float64) { c.RateLimits.SendBurst = int(f) })
	num("SMS_GW_DLR_RPM", func(f float64) { c.RateLimits.DLRPerMinute = f })
	num("SMS_GW_DLR_BURST", func(f float64) { c.RateLimits.DLRBurst = int(f) })
	num("SMS_GW_MSISDN_MIN_DIGITS", func(f float64) { c.Recipients.MinDigits = int(f) })
	list("SMS_GW_ALLOWED_PREFIXES", &c.Recipients.AllowedPrefixes)
	list("SMS_GW_BLOCKED_PREFIXES", &c.Recipients.BlockedPrefixes)
	if v, ok := get("SMS_GW_ALLOW_HTTP_WEBHOOKS"); ok {
		c.Webhook.AllowHTTP = v == "1"
	}
	dur("SMS_GW_HOUSEKEEPING_INTERVAL", &c.Retention.Interval)
	str("SMS_GW_PROMETHEUS_URL", &c.Monitoring.PrometheusURL)
	flag("SMS_GW_ACME_ENABLED", &c.ACME.Enabled)
	list("SMS_GW_ACME_DOMAINS", &c.ACME.Domains)
	str("SMS_GW_ACME_EMAIL", &c.ACME.Email)
	str("SMS_GW_ACME_DIRECTORY_URL", &c.ACME.DirectoryURL)
	str("SMS_GW_ACME_DIRECTORY_CA_BUNDLE", &c.ACME.DirectoryCABundle)
	str("SMS_GW_ACME_CACHE_DIR", &c.ACME.CacheDir)
	flag("SMS_GW_ACME_ACCEPT_TOS", &c.ACME.AcceptTOS)
	dur("SMS_GW_ACME_RENEW_BEFORE", &c.ACME.RenewBefore)
	str("SMS_GW_ACME_CHALLENGE", &c.ACME.Challenge)
	str("SMS_GW_ACME_HTTP_BIND", &c.ACME.HTTPAddr)
	flag("SMS_GW_ACME_PREFETCH", &c.ACME.Prefetch)
	str("SMS_GW_ACME_EAB_KID", &c.ACME.EABKeyID)
	ref("SMS_GW_ACME_EAB_HMAC_KEY", &c.ACME.EABHMACKey)
	flag("SMS_GW_ACME_ALLOW_INSECURE_DIRECTORY", &c.ACME.AllowInsecureDir)
	for i := range c.ACME.Domains {
		c.ACME.Domains[i] = strings.ToLower(c.ACME.Domains[i])
	}
	if err != nil {
		return nil, nil, err
	}
	for _, k := range obsoleteEnv {
		if v, ok := lookup(k); ok && strings.TrimSpace(v) != "" {
			obsolete = append(obsolete, k)
		}
	}
	return applied, obsolete, nil
}
