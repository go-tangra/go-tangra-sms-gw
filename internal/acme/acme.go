// Package acme obtains, caches and renews the public Hermes HTTPS
// certificate from an ACME CA (autocert). It never touches the mesh
// identity: the account key and certificates live in the persistent cache
// directory. Every failure here is reported by the caller and leaves the
// plain public listener, the mesh and the admin listener serving.
package acme

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	xacme "golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/config"
)

// ChallengePath is where the CA fetches http-01 tokens.
const ChallengePath = "/.well-known/acme-challenge/"

// Challenge types.
const (
	ChallengeHTTP01    = "http-01"
	ChallengeTLSALPN01 = "tls-alpn-01"
)

// Manager owns the account key, certificate cache, host policy and
// challenge responders of the public HTTPS listener.
type Manager struct {
	cfg config.ACME
	mgr *autocert.Manager
	log *slog.Logger
}

// New builds the manager from a validated configuration. It fails on an
// unusable cache directory, CA bundle or EAB key; reachability of the CA is
// only known at issuance (see Warm).
func New(cfg config.ACME, log *slog.Logger) (*Manager, error) {
	if err := ensureCacheDir(cfg.CacheDir); err != nil {
		return nil, err
	}
	client := &xacme.Client{DirectoryURL: cfg.DirectoryURL}
	if cfg.DirectoryCABundle != "" {
		hc, err := bundleClient(cfg.DirectoryCABundle)
		if err != nil {
			return nil, err
		}
		client.HTTPClient = hc
	}
	am := &autocert.Manager{
		Prompt:      func(string) bool { return cfg.AcceptTOS },
		Cache:       autocert.DirCache(cfg.CacheDir),
		HostPolicy:  autocert.HostWhitelist(cfg.Domains...),
		RenewBefore: cfg.RenewBefore,
		Email:       cfg.Email,
		Client:      client,
	}
	if cfg.EABKeyID != "" {
		raw, err := cfg.EABHMACKey.Read()
		if err != nil {
			return nil, fmt.Errorf("acme: eab key: %w", err)
		}
		key, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(strings.TrimSpace(raw), "="))
		if err != nil || len(key) == 0 {
			return nil, errors.New("acme: eab_hmac_key must be base64url")
		}
		am.ExternalAccountBinding = &xacme.ExternalAccountBinding{KID: cfg.EABKeyID, Key: key}
	}
	if cfg.Challenge == ChallengeHTTP01 {
		// HTTPHandler enables http-01 inside autocert (side effect).
		am.HTTPHandler(http.NotFoundHandler())
	}
	log.Info("acme enabled", "domains", cfg.Domains, "challenge", cfg.Challenge, "directory", cfg.DirectoryURL, "cache", cfg.CacheDir,
		"renew_before", cfg.RenewBefore.String())
	return &Manager{cfg: cfg, mgr: am, log: log}, nil
}

// TLSConfig is the HTTPS listener configuration; certificates are issued on
// the first handshake for a whitelisted name and cached from then on.
func (m *Manager) TLSConfig() *tls.Config {
	c := m.mgr.TLSConfig()
	c.MinVersion = tls.VersionTLS12
	if m.cfg.Challenge == ChallengeHTTP01 {
		// autocert always tries tls-alpn-01 first; without the ALPN entry
		// that attempt fails at once and the CA falls through to http-01.
		c.NextProtos = withoutALPN(c.NextProtos)
	}
	return c
}

// ChallengeHandler answers http-01 tokens under ChallengePath on the plain
// public listener (nil for tls-alpn-01); anything else there is 404.
func (m *Manager) ChallengeHandler() http.Handler {
	if m.cfg.Challenge != ChallengeHTTP01 {
		return nil
	}
	return stripHostPort(m.mgr.HTTPHandler(http.NotFoundHandler()))
}

// RedirectHandler serves a dedicated port-80 listener: challenge tokens,
// and a redirect to https for everything else.
func (m *Manager) RedirectHandler() http.Handler { return stripHostPort(m.mgr.HTTPHandler(nil)) }

// Warm requests the certificate of every domain as a handshake would, so a
// DNS, reachability or rate-limit problem shows at start. Failures are
// logged, never fatal.
func (m *Manager) Warm(ctx context.Context) {
	for _, d := range m.cfg.Domains {
		if ctx.Err() != nil {
			return
		}
		start := time.Now()
		cert, err := m.mgr.GetCertificate(&tls.ClientHelloInfo{ServerName: d, CipherSuites: []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
			SupportedCurves: []tls.CurveID{tls.CurveP256}, SignatureSchemes: []tls.SignatureScheme{tls.ECDSAWithP256AndSHA256},
			SupportedVersions: []uint16{tls.VersionTLS13, tls.VersionTLS12}})
		switch {
		case err != nil:
			m.log.Error("acme certificate unavailable", "domain", d, "after", time.Since(start).Truncate(time.Millisecond).String(), "err", err)
		case cert != nil && cert.Leaf != nil:
			m.log.Info("acme certificate ready", "domain", d, "expires", cert.Leaf.NotAfter.Format(time.RFC3339))
		default:
			m.log.Info("acme certificate ready", "domain", d)
		}
	}
}

// stripHostPort drops a port from Host before the host policy compares it
// (proxies forwarding $http_host send "name:443").
func stripHostPort(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil || host == "" {
			next.ServeHTTP(w, r)
			return
		}
		r2 := r.Clone(r.Context())
		r2.Host = host
		next.ServeHTTP(w, r2)
	})
}

// ensureCacheDir creates the cache and proves it is writable, so a volume
// owned by another user fails at start instead of at the first handshake.
func ensureCacheDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("acme: cache dir %s: %w", dir, err)
	}
	probe := filepath.Join(dir, ".write-probe")
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		return fmt.Errorf("acme: cache dir %s is not writable", dir)
	}
	if err := os.Remove(probe); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("acme: cache dir %s: %w", dir, err)
	}
	return nil
}

// bundleClient trusts only the given PEM bundle for the ACME conversation.
func bundleClient(path string) (*http.Client, error) {
	pem, err := os.ReadFile(path) // #nosec G304 -- operator-supplied CA bundle
	if err != nil {
		return nil, fmt.Errorf("acme: directory_ca_bundle %s is not readable", path)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("acme: directory_ca_bundle %s contains no PEM certificates", path)
	}
	return &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}, nil
}

func withoutALPN(protos []string) []string {
	out := protos[:0:0]
	for _, p := range protos {
		if p != xacme.ALPNProto {
			out = append(out, p)
		}
	}
	return out
}
