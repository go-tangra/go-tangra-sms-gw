package acme

import (
	"crypto/tls"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	xacme "golang.org/x/crypto/acme"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/config"
)

func cfg(t *testing.T, challenge string) config.ACME {
	t.Helper()
	return config.ACME{Enabled: true, Domains: []string{"sms.example.test"}, DirectoryURL: "https://127.0.0.1:1/dir", CacheDir: filepath.Join(t.TempDir(), "acme"),
		AcceptTOS: true, RenewBefore: 30 * 24 * time.Hour, Challenge: challenge}
}

func newManager(t *testing.T, c config.ACME) *Manager {
	t.Helper()
	m, err := New(c, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestNewCreatesAndProbesCacheDir(t *testing.T) {
	c := cfg(t, ChallengeHTTP01)
	newManager(t, c)
	fi, err := os.Stat(c.CacheDir)
	if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("cache dir: %v %v", fi, err)
	}
	if _, err := os.Stat(filepath.Join(c.CacheDir, ".write-probe")); !os.IsNotExist(err) {
		t.Fatal("probe left behind")
	}
}

func TestNewFailsOnUnusableCacheOrBundle(t *testing.T) {
	if os.Geteuid() != 0 {
		c := cfg(t, ChallengeHTTP01)
		ro := filepath.Join(t.TempDir(), "ro")
		if err := os.Mkdir(ro, 0o500); err != nil {
			t.Fatal(err)
		}
		c.CacheDir = ro
		if _, err := New(c, slog.New(slog.DiscardHandler)); err == nil {
			t.Fatal("unwritable cache accepted")
		}
	}
	c := cfg(t, ChallengeHTTP01)
	bad := filepath.Join(t.TempDir(), "bundle.pem")
	_ = os.WriteFile(bad, []byte("not pem"), 0o600)
	c.DirectoryCABundle = bad
	if _, err := New(c, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("bundle without certificates accepted")
	}
	c.DirectoryCABundle = filepath.Join(t.TempDir(), "missing.pem")
	if _, err := New(c, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("missing bundle accepted")
	}
}

func TestEABKeyDecoding(t *testing.T) {
	c := cfg(t, ChallengeHTTP01)
	t.Setenv("TEST_EAB", "c2VjcmV0LWhtYWMta2V5")
	c.EABKeyID, c.EABHMACKey = "kid-1", config.SecretRef{Env: "TEST_EAB"}
	m := newManager(t, c)
	if m.mgr.ExternalAccountBinding == nil || string(m.mgr.ExternalAccountBinding.Key) != "secret-hmac-key" {
		t.Fatal("eab key not decoded")
	}
	t.Setenv("TEST_EAB", "%%%")
	if _, err := New(c, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("invalid base64url accepted")
	}
}

func TestTLSConfigALPN(t *testing.T) {
	h := newManager(t, cfg(t, ChallengeHTTP01)).TLSConfig()
	if h.MinVersion != tls.VersionTLS12 || slices.Contains(h.NextProtos, xacme.ALPNProto) {
		t.Fatalf("http-01: %v %v", h.MinVersion, h.NextProtos)
	}
	a := newManager(t, cfg(t, ChallengeTLSALPN01)).TLSConfig()
	if !slices.Contains(a.NextProtos, xacme.ALPNProto) {
		t.Fatalf("tls-alpn-01 needs %s: %v", xacme.ALPNProto, a.NextProtos)
	}
	if withoutALPN([]string{"h2", xacme.ALPNProto, "http/1.1"})[1] != "http/1.1" {
		t.Fatal("alpn removal")
	}
}

func TestChallengeHandlers(t *testing.T) {
	if newManager(t, cfg(t, ChallengeTLSALPN01)).ChallengeHandler() != nil {
		t.Fatal("tls-alpn-01 has no http handler")
	}
	h := newManager(t, cfg(t, ChallengeHTTP01)).ChallengeHandler()
	for host, want := range map[string]int{"sms.example.test": 404, "sms.example.test:443": 404, "other.example.test": 403} {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, ChallengePath+"unknown", nil)
		r.Host = host
		h.ServeHTTP(rec, r)
		if rec.Code != want {
			t.Errorf("%s: %d, want %d", host, rec.Code, want)
		}
	}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/hermes/v1/me", nil)
	r.Host = "sms.example.test:80"
	newManager(t, cfg(t, ChallengeHTTP01)).RedirectHandler().ServeHTTP(rec, r)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "https://sms.example.test/hermes/v1/me" {
		t.Fatalf("redirect: %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestStripHostPort(t *testing.T) {
	var got string
	h := stripHostPort(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r.Host }))
	for in, want := range map[string]string{"a.example.test:8080": "a.example.test", "a.example.test": "a.example.test", "[::1]:80": "::1"} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Host = in
		h.ServeHTTP(httptest.NewRecorder(), r)
		if got != want {
			t.Errorf("%s → %s", in, got)
		}
	}
}

func TestCertificateRefusesForeignDomain(t *testing.T) {
	m := newManager(t, cfg(t, ChallengeHTTP01))
	if _, err := m.TLSConfig().GetCertificate(&tls.ClientHelloInfo{ServerName: "evil.example.test"}); err == nil {
		t.Fatal("certificate for a host outside the policy")
	}
}
