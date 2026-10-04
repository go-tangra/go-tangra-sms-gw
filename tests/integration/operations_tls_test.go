//go:build integration

package integration

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/app"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/app/apptest"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/config"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/store/storetest"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// readiness polls /readyz until identity and database are ok (or 10 s).
func readiness(t *testing.T, a *app.App) app.Readiness {
	t.Helper()
	var r app.Readiness
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		resp, err := http.Get("http://" + a.AdminAddr() + "/readyz")
		if err != nil {
			t.Fatal(err)
		}
		r = app.Readiness{}
		err = json.NewDecoder(resp.Body).Decode(&r)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if (r.Identity == "ok" && r.Database == "ok") || time.Now().After(deadline) {
			return r
		}
	}
}

// publicCert writes a public keypair signed by its own test CA (never the
// mesh CA) and returns the paths and the CA pool.
func publicCert(t *testing.T, nb, na time.Time) (cert, key string, pool *x509.CertPool) {
	t.Helper()
	dir := t.TempDir()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "public test ca"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	pool = x509.NewCertPool()
	pool.AddCert(ca)
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leafTpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "sms.example.test"}, DNSNames: []string{"sms.example.test"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: nb, NotAfter: na, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, leafTpl, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	kder, _ := x509.MarshalECPrivateKey(leafKey)
	cert, key = filepath.Join(dir, "public.crt"), filepath.Join(dir, "public.key")
	_ = os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	_ = os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600)
	return cert, key, pool
}

// TestPublicStaticTLS serves the Hermes routes over HTTPS with a static
// public keypair, separate from the mesh identity.
func TestPublicStaticTLS(t *testing.T) {
	e := Start(t, "")
	cert, key, pool := publicCert(t, time.Now().Add(-time.Minute), time.Now().Add(time.Hour))
	run := apptest.Start(t, apptest.Options{DSN: e.DB.AppDSN, KEK: e.KEK, Configure: func(c *config.Config) {
		c.Public.HTTPSAddr, c.Public.TLSCertFile, c.Public.TLSKeyFile = freeAddr(t), cert, key
	}})
	if r := readiness(t, run.App); r.PublicTLS != app.TLSStatic || r.Identity != "ok" {
		t.Fatalf("readiness %+v", r)
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "sms.example.test"}}}
	base := "https://" + run.App.PublicHTTPSAddr()
	resp, err := client.Get(base + "/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.TLS == nil || resp.TLS.PeerCertificates[0].Subject.CommonName != "sms.example.test" {
		t.Fatalf("health over https: %d", resp.StatusCode)
	}
	resp, err = client.Post(base+"/hermes/v1/login", "application/json", strings.NewReader(`{"username":"client_a","password":"`+Password+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "access_token") {
		t.Fatalf("login over https: %d %s", resp.StatusCode, body)
	}
	// TLS 1.1 is refused; the plain listener still serves.
	if _, err := tls.Dial("tcp", run.App.PublicHTTPSAddr(), &tls.Config{RootCAs: pool, ServerName: "sms.example.test", MaxVersion: tls.VersionTLS11}); err == nil {
		t.Fatal("tls 1.1 accepted")
	}
	if code, _ := (hermes{t, run.Public}).do("GET", "/health", "", ""); code != 200 {
		t.Fatalf("plain health %d", code)
	}
}

// TestPublicTLSFailureIsNonfatal: an expired public certificate or an
// unwritable ACME cache disables public HTTPS only; the plain listener,
// readiness and the admin listener keep working and report it.
func TestPublicTLSFailureIsNonfatal(t *testing.T) {
	e := Start(t, "")
	cert, key, _ := publicCert(t, time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour))
	ro := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*config.Config){
		"expired static": func(c *config.Config) { c.Public.TLSCertFile, c.Public.TLSKeyFile = cert, key },
		"missing static": func(c *config.Config) { c.Public.TLSCertFile, c.Public.TLSKeyFile = cert+".missing", key },
		"acme bad bundle": func(c *config.Config) {
			c.ACME = config.ACME{Enabled: true, Domains: []string{"sms-gw.test"}, DirectoryURL: "https://127.0.0.1:1/dir", CacheDir: t.TempDir(), AcceptTOS: true,
				RenewBefore: 24 * time.Hour, Challenge: "http-01", DirectoryCABundle: key}
		},
	}
	if os.Geteuid() != 0 {
		cases["acme unwritable cache"] = func(c *config.Config) {
			c.ACME = config.ACME{Enabled: true, Domains: []string{"sms-gw.test"}, DirectoryURL: "https://127.0.0.1:1/dir", CacheDir: ro, AcceptTOS: true,
				RenewBefore: 24 * time.Hour, Challenge: "http-01"}
		}
	}
	for name, configure := range cases {
		t.Run(name, func(t *testing.T) {
			https := freeAddr(t)
			run := apptest.Start(t, apptest.Options{DSN: e.DB.AppDSN, KEK: e.KEK, Configure: func(c *config.Config) {
				c.Public.HTTPSAddr = https
				configure(c)
			}})
			defer run.Stop()
			r := readiness(t, run.App)
			if r.PublicTLS != app.TLSUnavailable || r.Identity != "ok" || r.Database != "ok" {
				t.Fatalf("readiness %+v", r)
			}
			if run.App.PublicHTTPSAddr() != "" {
				t.Fatal("https listener bound with an unusable certificate")
			}
			if conn, err := net.DialTimeout("tcp", https, time.Second); err == nil {
				conn.Close()
				t.Fatal("https port open")
			}
			if code, _ := (hermes{t, run.Public}).do("GET", "/health", "", ""); code != 200 {
				t.Fatalf("plain health %d", code)
			}
		})
	}
}

// pebbleImage is pinned: later pebble releases finalize orders
// asynchronously without a Location header, and x/crypto's
// CreateOrderCert then polls an empty order URL.
const pebbleImage = "ghcr.io/letsencrypt/pebble:2.6.0"

type pebble struct {
	c         testcontainers.Container
	directory string
	bundle    string
	mgmt      string
}

// startPebble runs a local ACME CA (never a public one) on the host network
// so it can reach the challenge port on loopback; challengeAddr is where it
// validates http-01 for sms-gw.test.
func startPebble(t *testing.T, challengeAddr string, validity time.Duration) *pebble {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	acmeAddr, mgmt, alpn := freeAddr(t), freeAddr(t), freeAddr(t)
	_, httpPort, _ := net.SplitHostPort(challengeAddr)
	_, alpnPort, _ := net.SplitHostPort(alpn)
	cfg := fmt.Sprintf(`{"pebble":{"listenAddress":%q,"managementListenAddress":%q,"certificate":"test/certs/localhost/cert.pem",
"privateKey":"test/certs/localhost/key.pem","httpPort":%s,"tlsPort":%s,"ocspResponderURL":"","externalAccountBindingRequired":false,
"retryAfter":{"authz":1,"order":1},"keyAlgorithm":"ecdsa","certificateValidityPeriod":%d}}`,
		acmeAddr, mgmt, httpPort, alpnPort, int(validity.Seconds()))
	if err := os.WriteFile(filepath.Join(dir, "pebble.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{Started: true, ContainerRequest: testcontainers.ContainerRequest{
		Image: pebbleImage, Cmd: []string{"-config", "/cfg/pebble.json"},
		Env:   map[string]string{"PEBBLE_VA_NOSLEEP": "1", "PEBBLE_WFE_NONCEREJECT": "0", "PEBBLE_AUTHZREUSE": "0"},
		Files: []testcontainers.ContainerFile{{HostFilePath: filepath.Join(dir, "pebble.json"), ContainerFilePath: "/cfg/pebble.json", FileMode: 0o644}},
		HostConfigModifier: func(hc *container.HostConfig) {
			hc.NetworkMode = "host"
			hc.ExtraHosts = []string{"sms-gw.test:127.0.0.1"}
		},
		WaitingFor: wait.ForLog("ACME directory available").WithStartupTimeout(time.Minute)}})
	if err != nil {
		storetest.Unavailable(t, "pebble unavailable (docker): %v", err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			if logs, err := c.Logs(context.Background()); err == nil {
				raw, _ := io.ReadAll(logs)
				t.Logf("pebble:\n%s", raw)
			}
		}
		_ = testcontainers.TerminateContainer(c)
	})
	rc, err := c.CopyFileFromContainer(ctx, "/test/certs/pebble.minica.pem")
	if err != nil {
		t.Fatal(err)
	}
	pemBytes, _ := io.ReadAll(rc)
	rc.Close()
	bundle := filepath.Join(dir, "pebble.minica.pem")
	if err := os.WriteFile(bundle, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return &pebble{c: c, directory: "https://" + acmeAddr + "/dir", bundle: bundle, mgmt: mgmt}
}

func cachedLeaf(t *testing.T, cacheDir string) *x509.Certificate {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(cacheDir, "sms-gw.test"))
	if err != nil {
		return nil
	}
	for rest := raw; ; {
		var b *pem.Block
		if b, rest = pem.Decode(rest); b == nil {
			return nil
		}
		if b.Type == "CERTIFICATE" {
			leaf, err := x509.ParseCertificate(b.Bytes)
			if err != nil {
				t.Fatal(err)
			}
			return leaf
		}
	}
}

func servedLeaf(t *testing.T, addr string) *x509.Certificate {
	t.Helper()
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 60 * time.Second}, "tcp", addr, &tls.Config{ServerName: "sms-gw.test", InsecureSkipVerify: true}) // #nosec G402 -- chain checked below against pebble's root
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	defer conn.Close()
	return conn.ConnectionState().PeerCertificates[0]
}

// TestPublicACME issues the public certificate from a local pebble CA over
// http-01 answered on the plain public listener, caches it, renews it when
// it is inside renew_before, and reuses the cache after a restart without
// the CA.
func TestPublicACME(t *testing.T) {
	e := Start(t, "")
	plain := freeAddr(t)
	// A one-hour certificate is always inside the 24 h renew_before, so
	// autocert renews it as soon as it is issued (deterministically, since
	// the renewal jitter is at most one hour).
	p := startPebble(t, plain, time.Hour)
	cache := filepath.Join(t.TempDir(), "acme")
	configure := func(c *config.Config) {
		c.Public.HTTPAddr, c.Public.HTTPSAddr = plain, freeAddr(t)
		c.ACME = config.ACME{Enabled: true, Domains: []string{"sms-gw.test"}, Email: "ops@example.test", DirectoryURL: p.directory,
			DirectoryCABundle: p.bundle, CacheDir: cache, AcceptTOS: true, RenewBefore: 24 * time.Hour, Challenge: "http-01"}
	}
	run := apptest.Start(t, apptest.Options{DSN: e.DB.AppDSN, KEK: e.KEK, Configure: configure})
	if r := readiness(t, run.App); r.PublicTLS != app.TLSACME {
		t.Fatalf("readiness %+v", r)
	}
	first := servedLeaf(t, run.App.PublicHTTPSAddr())
	if len(first.DNSNames) != 1 || first.DNSNames[0] != "sms-gw.test" || !strings.Contains(first.Issuer.CommonName, "Pebble") {
		t.Fatalf("issued %v by %s", first.DNSNames, first.Issuer.CommonName)
	}
	if c := cachedLeaf(t, cache); c == nil {
		t.Fatal("certificate not cached")
	}
	// Hermes over the issued certificate.
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{ServerName: "sms-gw.test", InsecureSkipVerify: true}}} // #nosec G402 -- test CA
	resp, err := client.Get("https://" + run.App.PublicHTTPSAddr() + "/health")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("health over acme https: %v", err)
	}
	resp.Body.Close()

	// Renewal: the cache and the served certificate move to a new serial.
	deadline := time.Now().Add(90 * time.Second)
	var renewed *x509.Certificate
	for time.Now().Before(deadline) {
		if c := cachedLeaf(t, cache); c != nil && c.SerialNumber.Cmp(first.SerialNumber) != 0 {
			renewed = c
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if renewed == nil {
		t.Fatal("certificate inside renew_before was not renewed")
	}
	if s := servedLeaf(t, run.App.PublicHTTPSAddr()); s.SerialNumber.Cmp(first.SerialNumber) == 0 {
		t.Fatal("renewed certificate not served")
	}

	// Restart without the CA: the cached certificate is served.
	run.Stop()
	if err := testcontainers.TerminateContainer(p.c); err != nil {
		t.Fatal(err)
	}
	cached := cachedLeaf(t, cache)
	again := apptest.Start(t, apptest.Options{DSN: e.DB.AppDSN, KEK: e.KEK, Configure: configure})
	served := servedLeaf(t, again.App.PublicHTTPSAddr())
	if served.SerialNumber.Cmp(cached.SerialNumber) != 0 {
		t.Fatalf("restart served %s, cache holds %s", served.SerialNumber, cached.SerialNumber)
	}
	if r := readiness(t, again.App); r.PublicTLS != app.TLSACME || r.Identity != "ok" {
		t.Fatalf("readiness after restart %+v", r)
	}
}
