// Package config loads the sms-gw configuration: the Freya framework config,
// the separate public Hermes listener and the module sections. Secrets are
// referenced (file or environment variable), never written into the file.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	fconfig "github.com/go-tangra/go-tangra/v4/config"
	"gopkg.in/yaml.v3"
)

// Config is the sms-gw service configuration.
type Config struct {
	fconfig.Config `yaml:",inline"`

	DB         DB         `yaml:"db"`
	KEK        KEK        `yaml:"kek"`
	Gateway    Gateway    `yaml:"gateway"`
	Enroll     Enroll     `yaml:"enroll"`
	Public     Public     `yaml:"public"`
	PublicAuth PublicAuth `yaml:"public_auth"`
	ACME       ACME       `yaml:"acme"`
	RateLimits RateLimits `yaml:"rate_limits"`
	Recipients Recipients `yaml:"recipients"`
	Webhook    Webhook    `yaml:"webhook"`
	Retention  Retention  `yaml:"retention"`
	Monitoring Monitoring `yaml:"monitoring"`
	Query      Query      `yaml:"query"`
}

// DB configures PostgreSQL.
type DB struct {
	DSN        string `yaml:"dsn"`         // application role (no BYPASSRLS)
	MigrateDSN string `yaml:"migrate_dsn"` // migration role; empty = DSN
	MaxConns   int32  `yaml:"max_conns"`
}

// KEK names where the 32-byte key-encryption key for sealed secrets comes from.
type KEK struct {
	Source string `yaml:"source"` // file | env
	Path   string `yaml:"path"`
	Env    string `yaml:"env"`
}

// Gateway names the application gateway, the platform token issuer and the
// auth service used to verify operators.
type Gateway struct {
	Service     string `yaml:"service"`
	Issuer      string `yaml:"issuer"`
	AuthService string `yaml:"auth_service"`
}

// Enroll obtains the mesh SVID by enrolling with lcm over the network
// (identity.provider: provided).
type Enroll struct {
	Enabled       bool   `yaml:"enabled"`
	EnrollURL     string `yaml:"enroll_url"`
	LCMGRPCTarget string `yaml:"lcm_grpc"`
	TenantID      string `yaml:"tenant_id"`
	TokenFile     string `yaml:"token_file"`
	StateFile     string `yaml:"state_file"`
	Insecure      bool   `yaml:"insecure"`
}

// Public is the legacy Hermes edge: plain HTTP and optional HTTPS with their
// own keys, never the mesh identity.
type Public struct {
	HTTPAddr       string   `yaml:"http_addr"`
	HTTPSAddr      string   `yaml:"https_addr"`
	TLSCertFile    string   `yaml:"tls_cert_file"`
	TLSKeyFile     string   `yaml:"tls_key_file"`
	TrustedProxies []string `yaml:"trusted_proxies"`
	MaxBodyBytes   int64    `yaml:"max_body_bytes"`
}

// TLSEnabled reports a static public certificate.
func (p Public) TLSEnabled() bool { return p.TLSCertFile != "" || p.TLSKeyFile != "" }

// PublicAuth configures Hermes client tokens.
type PublicAuth struct {
	JWTSecret         SecretRef `yaml:"jwt_secret"`
	AccessTTLSeconds  int       `yaml:"access_ttl_seconds"`
	RefreshTTLSeconds int       `yaml:"refresh_ttl_seconds"`
}

// AccessTTL is the access token lifetime.
func (p PublicAuth) AccessTTL() time.Duration { return time.Duration(p.AccessTTLSeconds) * time.Second }

// RefreshTTL is the refresh token lifetime.
func (p PublicAuth) RefreshTTL() time.Duration {
	return time.Duration(p.RefreshTTLSeconds) * time.Second
}

// ACME obtains the public certificate automatically.
type ACME struct {
	Enabled           bool          `yaml:"enabled"`
	Domains           []string      `yaml:"domains"`
	Email             string        `yaml:"email"`
	DirectoryURL      string        `yaml:"directory_url"`
	DirectoryCABundle string        `yaml:"directory_ca_bundle"`
	CacheDir          string        `yaml:"cache_dir"`
	AcceptTOS         bool          `yaml:"accept_tos"`
	RenewBefore       time.Duration `yaml:"renew_before"`
	Challenge         string        `yaml:"challenge"`
	HTTPAddr          string        `yaml:"http_addr"`
	Prefetch          bool          `yaml:"prefetch"`
	EABKeyID          string        `yaml:"eab_kid"`
	EABHMACKey        SecretRef     `yaml:"eab_hmac_key"`
	AllowInsecureDir  bool          `yaml:"allow_insecure_directory"`
}

// RateLimits are the public token-bucket limits (requests per minute and burst).
type RateLimits struct {
	LoginPerMinute float64 `yaml:"login_per_minute"`
	LoginBurst     int     `yaml:"login_burst"`
	SendPerMinute  float64 `yaml:"send_per_minute"`
	SendBurst      int     `yaml:"send_burst"`
	DLRPerMinute   float64 `yaml:"dlr_per_minute"`
	DLRBurst       int     `yaml:"dlr_burst"`
}

// Recipients is the destination policy applied before any send.
type Recipients struct {
	MinDigits       int      `yaml:"min_digits"`
	AllowedPrefixes []string `yaml:"allowed_prefixes"`
	BlockedPrefixes []string `yaml:"blocked_prefixes"`
}

// Webhook bounds outbound delivery callbacks.
type Webhook struct {
	AllowHTTP    bool `yaml:"allow_http"`    // development only
	AllowPrivate bool `yaml:"allow_private"` // development only: private/loopback receivers
	QueueSize    int  `yaml:"queue_size"`
	Workers      int  `yaml:"workers"`
}

// Retention configures the message retention worker.
type Retention struct {
	Interval time.Duration `yaml:"interval"`
}

// Monitoring is the optional Prometheus backend of the dashboard.
type Monitoring struct {
	PrometheusURL string `yaml:"prometheus_url"`
}

// Query bounds list queries.
type Query struct {
	DefaultPageSize int `yaml:"default_page_size"`
	MaxPageSize     int `yaml:"max_page_size"`
}

// SecretRef points at a secret: a mounted file or an environment variable.
type SecretRef struct {
	File string `yaml:"file"`
	Env  string `yaml:"env"`
}

// Set reports whether a source is configured.
func (s SecretRef) Set() bool { return s.File != "" || s.Env != "" }

// Read returns the secret. A file must be regular, mode 0640 or stricter and
// non-empty; one trailing newline is dropped. Errors never contain the value.
func (s SecretRef) Read() (string, error) {
	switch {
	case s.File != "" && s.Env != "":
		return "", errors.New("config: secret has both file and env")
	case s.File != "":
		fi, err := os.Stat(s.File)
		if err != nil || !fi.Mode().IsRegular() {
			return "", fmt.Errorf("config: secret file %s is not readable", s.File)
		}
		if fi.Mode().Perm()&^0o640 != 0 {
			return "", fmt.Errorf("config: secret file %s must have mode 0640 or stricter (has %04o)", s.File, fi.Mode().Perm())
		}
		raw, err := os.ReadFile(s.File) // #nosec G304 -- operator-supplied secret path
		if err != nil {
			return "", fmt.Errorf("config: secret file %s is not readable", s.File)
		}
		v := strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
		if v == "" {
			return "", fmt.Errorf("config: secret file %s is empty", s.File)
		}
		return v, nil
	case s.Env != "":
		v := os.Getenv(s.Env)
		if v == "" {
			return "", fmt.Errorf("config: secret environment variable %s is empty", s.Env)
		}
		return v, nil
	}
	return "", errors.New("config: secret is not configured")
}

// Default returns the secure defaults, including the legacy Hermes values.
func Default() Config {
	c := Config{
		Config:     fconfig.Default(),
		DB:         DB{MaxConns: 16},
		KEK:        KEK{Source: "file"},
		Gateway:    Gateway{Service: "gateway", AuthService: "auth"},
		Public:     Public{HTTPAddr: "0.0.0.0:9901", HTTPSAddr: "0.0.0.0:9443", MaxBodyBytes: 64 << 10},
		PublicAuth: PublicAuth{AccessTTLSeconds: 7200, RefreshTTLSeconds: 604800},
		ACME:       ACME{DirectoryURL: "https://acme-v02.api.letsencrypt.org/directory", RenewBefore: 30 * 24 * time.Hour, Challenge: "tls-alpn-01"},
		RateLimits: RateLimits{LoginPerMinute: 10, LoginBurst: 5, SendPerMinute: 100, SendBurst: 20, DLRPerMinute: 500, DLRBurst: 100},
		Recipients: Recipients{MinDigits: 7},
		Webhook:    Webhook{QueueSize: 4096, Workers: 4},
		Retention:  Retention{Interval: time.Hour},
		Query:      Query{DefaultPageSize: 50, MaxPageSize: 500},
	}
	c.ServiceName = "sms-gw"
	return c
}

// Load reads YAML over Default(); unknown fields are rejected. Not yet validated.
func Load(path string) (Config, error) {
	cfg := Default()
	raw, err := os.ReadFile(path) // #nosec G304 -- operator-supplied config path
	if err != nil {
		return cfg, fmt.Errorf("config: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("config: %s: %w", path, err)
	}
	return cfg, nil
}

// Warnings lists accepted insecure opt-outs (logged at start).
func (c Config) Warnings() []string {
	w := c.Config.Warnings()
	if c.Webhook.AllowHTTP {
		w = append(w, "webhook.allow_http: delivery callbacks may use plain http (development only)")
	}
	if c.Webhook.AllowPrivate {
		w = append(w, "webhook.allow_private: delivery callbacks may reach private or loopback addresses (development only)")
	}
	if c.ACME.AllowInsecureDir {
		w = append(w, "acme.allow_insecure_directory: the ACME directory may use plain http (development only)")
	}
	if c.Enroll.Insecure {
		w = append(w, "enroll.insecure: the enrollment endpoint certificate is not verified (development only)")
	}
	if len(c.Recipients.AllowedPrefixes) == 0 {
		w = append(w, "recipients.allowed_prefixes is empty: every destination prefix is permitted")
	}
	return w
}
