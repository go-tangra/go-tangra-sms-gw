package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func devConfig(t *testing.T) Config {
	t.Helper()
	c, err := Load("../../deploy/dev.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestDevConfigValidates(t *testing.T) {
	c := devConfig(t)
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.ServiceName != "sms-gw" || c.Public.HTTPAddr != "127.0.0.1:9901" || c.PublicAuth.AccessTTL() != 2*time.Hour || c.PublicAuth.RefreshTTL() != 7*24*time.Hour {
		t.Fatalf("unexpected dev config %+v", c.Public)
	}
}

func TestContainerConfigValidates(t *testing.T) {
	c, err := Load("../../deploy/container.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.ACME.CacheDir != "/acme" || c.KEK.Path != "/secrets/kek" {
		t.Fatalf("container config %+v %+v", c.ACME, c.KEK)
	}
}

func TestDefaultsKeepLegacyValues(t *testing.T) {
	d := Default()
	r := d.RateLimits
	if d.Public.HTTPAddr != "0.0.0.0:9901" || d.Public.HTTPSAddr != "0.0.0.0:9443" || r.LoginPerMinute != 10 || r.LoginBurst != 5 ||
		r.SendPerMinute != 100 || r.SendBurst != 20 || r.DLRPerMinute != 500 || r.DLRBurst != 100 || d.Recipients.MinDigits != 7 ||
		d.Webhook.QueueSize != 4096 || d.Webhook.Workers != 4 || d.Retention.Interval != time.Hour || d.Query.DefaultPageSize != 50 {
		t.Fatalf("defaults drifted from the legacy service: %+v", d)
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte("service_name: sms-gw\npublic:\n  jwt_secret: inline\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("unknown field accepted")
	}
}

func TestValidateRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*Config)
		want   string
	}{
		"public shares mesh http port": {func(c *Config) { c.Public.HTTPAddr = "0.0.0.0:9984" }, "separate listeners"},
		"public shares admin":          {func(c *Config) { c.Public.HTTPAddr = c.Admin.Addr }, "separate listeners"},
		"https shares http when tls": {func(c *Config) {
			c.Public.TLSCertFile, c.Public.TLSKeyFile, c.Public.HTTPSAddr = "c", "k", c.Public.HTTPAddr
		}, "separate listeners"},
		"cert without key":          {func(c *Config) { c.Public.TLSCertFile = "c" }, "go together"},
		"static tls and acme":       {func(c *Config) { c.Public.TLSCertFile, c.Public.TLSKeyFile = "c", "k"; enableACME(c) }, "mutually exclusive"},
		"acme without tos":          {func(c *Config) { enableACME(c); c.ACME.AcceptTOS = false }, "accept_tos"},
		"acme wildcard":             {func(c *Config) { enableACME(c); c.ACME.Domains = []string{"*.example.org"} }, "not a host name"},
		"acme http directory":       {func(c *Config) { enableACME(c); c.ACME.DirectoryURL = "http://ca.example.org/dir" }, "directory_url"},
		"acme eab half":             {func(c *Config) { enableACME(c); c.ACME.EABKeyID = "kid" }, "eab"},
		"acme http addr alpn":       {func(c *Config) { enableACME(c); c.ACME.HTTPAddr = "127.0.0.1:8080" }, "http-01 challenges only"},
		"jwt secret missing":        {func(c *Config) { c.PublicAuth.JWTSecret = SecretRef{} }, "jwt_secret"},
		"jwt secret ambiguous":      {func(c *Config) { c.PublicAuth.JWTSecret = SecretRef{File: "a", Env: "B"} }, "jwt_secret"},
		"short access ttl":          {func(c *Config) { c.PublicAuth.AccessTTLSeconds = 1 }, "access_ttl"},
		"refresh below access":      {func(c *Config) { c.PublicAuth.RefreshTTLSeconds = 60 }, "refresh_ttl"},
		"bad proxy":                 {func(c *Config) { c.Public.TrustedProxies = []string{"10.0.0.0/33"} }, "trusted_proxies"},
		"plus prefix":               {func(c *Config) { c.Recipients.AllowedPrefixes = []string{"+359"} }, "prefix"},
		"min digits":                {func(c *Config) { c.Recipients.MinDigits = 16 }, "min_digits"},
		"zero burst":                {func(c *Config) { c.RateLimits.SendBurst = 0 }, "rate_limits"},
		"no db":                     {func(c *Config) { c.DB.DSN = "" }, "db.dsn"},
		"bad kek":                   {func(c *Config) { c.KEK.Source = "inline" }, "kek.source"},
		"http issuer":               {func(c *Config) { c.Gateway.Issuer = "http://localhost" }, "issuer"},
		"no mesh http":              {func(c *Config) { c.Server.HTTPAddr = "" }, "server.http_addr"},
		"provided without enroll":   {func(c *Config) { c.Identity.Provider = "provided" }, "requires enroll"},
		"enroll with file identity": {func(c *Config) { enableEnroll(c); c.Identity.Provider = "file" }, "provided"},
		"enroll incomplete":         {func(c *Config) { enableEnroll(c); c.Enroll.StateFile = "" }, "enroll requires"},
		"monitoring credentials":    {func(c *Config) { c.Monitoring.PrometheusURL = "http://u:p@prom:9090" }, "prometheus_url"},
		"page size above max":       {func(c *Config) { c.Query.DefaultPageSize = 1000 }, "page_size"},
		"retention too fast":        {func(c *Config) { c.Retention.Interval = time.Second }, "retention"},
		"prod plaintext webhook": {func(c *Config) {
			c.Env = "production"
			c.DB.DSN = "postgres://a@db/x?sslmode=verify-full"
			c.DB.MigrateDSN = ""
		}, "webhook.allow_http"},
		"prod plaintext db": {func(c *Config) { c.Env = "production"; c.Webhook = Webhook{QueueSize: 1, Workers: 1} }, "sslmode"},
	} {
		t.Run(name, func(t *testing.T) {
			c := devConfig(t)
			tc.mutate(&c)
			if err := c.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}

func TestValidateAccepts(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"acme":       enableACME,
		"enroll":     enableEnroll,
		"static tls": func(c *Config) { c.Public.TLSCertFile, c.Public.TLSKeyFile = "c", "k" },
		"ephemeral ports": func(c *Config) {
			c.Public.HTTPAddr, c.Server.HTTPAddr, c.Admin.Addr = "127.0.0.1:0", "127.0.0.1:0", "127.0.0.1:0"
		},
		"proxy address": func(c *Config) { c.Public.TrustedProxies = []string{"10.0.0.1", "192.168.0.0/16"} },
	} {
		t.Run(name, func(t *testing.T) {
			c := devConfig(t)
			mutate(&c)
			if err := c.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func enableACME(c *Config) {
	c.ACME.Enabled, c.ACME.Domains, c.ACME.AcceptTOS, c.ACME.CacheDir = true, []string{"sms.example.org"}, true, "/var/lib/sms-gw/acme"
}

func enableEnroll(c *Config) {
	c.Identity.Provider = "provided"
	c.Enroll = Enroll{Enabled: true, EnrollURL: "https://edge.example.org/enroll", LCMGRPCTarget: "lcm:9101",
		TenantID: "00000000-0000-0000-0000-000000000001", TokenFile: "/run/secrets/enroll", StateFile: "/var/lib/sms-gw/enroll.json"}
}

func TestApplyLegacyEnv(t *testing.T) {
	env := map[string]string{
		"SMS_GW_JWT_SECRET": "not-copied-into-config-0123456789abcdef", "SMS_GW_ACCESS_TTL_SECONDS": "3600",
		"HTTP_PUBLIC_BIND": "0.0.0.0:19901", "SMS_GW_TLS_BIND": "0.0.0.0:19443", "SMS_GW_TLS_CERT_PATH": "/c", "SMS_GW_TLS_KEY_PATH": "/k",
		"TRUSTED_PROXY_CIDRS": "10.0.0.0/8, 192.168.1.1", "SMS_GW_LOGIN_RPM": "20", "SMS_GW_SEND_BURST": "40",
		"SMS_GW_ALLOWED_PREFIXES": "+359, 44", "SMS_GW_ALLOW_HTTP_WEBHOOKS": "1", "SMS_GW_HOUSEKEEPING_INTERVAL": "30m",
		"SMS_GW_ACME_ENABLED": "true", "SMS_GW_ACME_DOMAINS": "SMS.Example.org", "SMS_GW_ACME_EAB_HMAC_KEY": "hmac",
		"GRPC_ADVERTISE_ADDR": "sms-gw:9900", "CERTS_DIR": "/app/certs",
	}
	c := Default()
	applied, obsolete, err := ApplyLegacyEnv(&c, func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicAuth.JWTSecret != (SecretRef{Env: "SMS_GW_JWT_SECRET"}) || c.ACME.EABHMACKey != (SecretRef{Env: "SMS_GW_ACME_EAB_HMAC_KEY"}) {
		t.Fatalf("secrets must stay references: %+v", c.PublicAuth.JWTSecret)
	}
	if c.PublicAuth.AccessTTLSeconds != 3600 || c.Public.HTTPAddr != "0.0.0.0:19901" || c.Public.HTTPSAddr != "0.0.0.0:19443" ||
		c.Public.TLSCertFile != "/c" || strings.Join(c.Public.TrustedProxies, ",") != "10.0.0.0/8,192.168.1.1" ||
		c.RateLimits.LoginPerMinute != 20 || c.RateLimits.SendBurst != 40 || strings.Join(c.Recipients.AllowedPrefixes, ",") != "359,44" ||
		!c.Webhook.AllowHTTP || c.Retention.Interval != 30*time.Minute || !c.ACME.Enabled || c.ACME.Domains[0] != "sms.example.org" {
		t.Fatalf("legacy env not mapped: %+v", c)
	}
	if len(applied) != 15 || strings.Join(obsolete, ",") != "GRPC_ADVERTISE_ADDR,CERTS_DIR" {
		t.Fatalf("applied %v obsolete %v", applied, obsolete)
	}
	for _, bad := range []map[string]string{{"SMS_GW_LOGIN_RPM": "fast"}, {"SMS_GW_SEND_RPM": "-1"}, {"SMS_GW_ACME_ENABLED": "yes please"}, {"SMS_GW_HOUSEKEEPING_INTERVAL": "hourly"}} {
		c := Default()
		if _, _, err := ApplyLegacyEnv(&c, func(k string) (string, bool) { v, ok := bad[k]; return v, ok }); err == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
}

func TestSecretRefRead(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	if err := os.WriteFile(good, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if v, err := (SecretRef{File: good}).Read(); err != nil || v != "s3cret" {
		t.Fatal(v, err)
	}
	loose := filepath.Join(dir, "loose")
	if err := os.WriteFile(loose, []byte("s3cret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := (SecretRef{File: loose}).Read(); err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("world-readable secret accepted or leaked: %v", err)
	}
	t.Setenv("SMSGW_TEST_SECRET", "from-env")
	if v, err := (SecretRef{Env: "SMSGW_TEST_SECRET"}).Read(); err != nil || v != "from-env" {
		t.Fatal(v, err)
	}
	if _, err := (SecretRef{Env: "SMSGW_TEST_UNSET"}).Read(); err == nil {
		t.Fatal("empty env accepted")
	}
}
