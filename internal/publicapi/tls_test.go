package publicapi

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writePair(t *testing.T, dir, name string, nb, na time.Time) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "sms.example.test"},
		DNSNames: []string{"sms.example.test"}, NotBefore: nb, NotAfter: na, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, _ := x509.MarshalECPrivateKey(key)
	cert, keyPath := filepath.Join(dir, name+".crt"), filepath.Join(dir, name+".key")
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600); err != nil {
		t.Fatal(err)
	}
	return cert, keyPath
}

func TestLoadStaticTLS(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	okCert, okKey := writePair(t, dir, "ok", now.Add(-time.Hour), now.Add(time.Hour))
	oldCert, oldKey := writePair(t, dir, "old", now.Add(-2*time.Hour), now.Add(-time.Hour))
	futCert, futKey := writePair(t, dir, "future", now.Add(time.Hour), now.Add(2*time.Hour))

	cfg, err := LoadStaticTLS(okCert, okKey)
	if err != nil || cfg == nil {
		t.Fatalf("valid pair: %v", err)
	}
	if cfg.MinVersion != tls.VersionTLS12 || len(cfg.Certificates) != 1 || cfg.Certificates[0].Leaf == nil {
		t.Fatalf("config: %+v", cfg)
	}
	if cfg, err := LoadStaticTLS("", ""); cfg != nil || err != nil {
		t.Fatal("no paths is not configured, not an error")
	}
	for name, paths := range map[string][2]string{
		"expired":      {oldCert, oldKey},
		"not yet":      {futCert, futKey},
		"mismatched":   {okCert, oldKey},
		"cert only":    {okCert, ""},
		"key only":     {"", okKey},
		"missing file": {filepath.Join(dir, "none.crt"), okKey},
	} {
		if cfg, err := LoadStaticTLS(paths[0], paths[1]); err == nil || cfg != nil {
			t.Errorf("%s: accepted", name)
		}
	}
	_, err = LoadStaticTLS(oldCert, oldKey)
	if !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired reason: %v", err)
	}
}

func TestChallengeRouteAheadOfHermes(t *testing.T) {
	hermes := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("hermes")) })
	challenge := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("challenge")) })
	h := WithChallenge(hermes, challenge)
	for path, want := range map[string]string{
		"/.well-known/acme-challenge/tok": "challenge",
		"/hermes/v1/me":                   "hermes",
		"/.well-known/other":              "hermes",
		"/dlr":                            "hermes",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Body.String() != want {
			t.Errorf("%s served by %q", path, rec.Body.String())
		}
	}
	if WithChallenge(hermes, nil) == nil {
		t.Fatal("nil challenge keeps the Hermes handler")
	}
}
