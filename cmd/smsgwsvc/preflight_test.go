package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"text/template"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/go-tangra/go-tangra/v4/preflight"
)

var clock = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

const tenant = "00000000-0000-0000-0000-000000000001"

// env is a temporary deployment: secrets, policy, token and the endpoints
// the config points at.
type env struct {
	Dir, Env, TrustDomain, TokenFile, StateFile, DSN string
	Gateway, Auth, LCM, EnrollURL, Issuer            string
	Webhook, Insecure                                bool
	Extra                                            string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("kek", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("k"), 32)), 0o600)
	write("jwt.key", strings.Repeat("j", 40), 0o600)
	write("policy.yaml", "rules: []\n", 0o600)
	if err := os.Mkdir(filepath.Join(dir, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	e := &env{Dir: dir, Env: "dev", TrustDomain: "infra.example.org", StateFile: filepath.Join(dir, "state", "svid.json"),
		Gateway: closedAddr(t), Auth: closedAddr(t), LCM: closedAddr(t), EnrollURL: "https://" + closedAddr(t) + "/api/lcm/v1/enroll",
		Issuer: "https://" + closedAddr(t),
		DSN:    "postgres://smsgw_app:pw-s3cret@" + closedAddr(t) + "/sms_gw?sslmode=disable", Insecure: true}
	e.TokenFile = write("token", token(t, clock.Add(20*time.Minute), "spiffe://infra.example.org/svc/sms-gw"), 0o600)
	return e
}

var cfgTmpl = template.Must(template.New("cfg").Parse(`service_name: sms-gw
trust_domain: {{.TrustDomain}}
env: {{.Env}}
identity: { provider: provided }
enroll:
  enabled: true
  enroll_url: {{.EnrollURL}}
  lcm_grpc: {{.LCM}}
  tenant_id: "` + tenant + `"
  token_file: {{.TokenFile}}
  state_file: {{.StateFile}}
  insecure: {{.Insecure}}
authz: { source: file, path: {{.Dir}}/policy.yaml }
server: { grpc_addr: 127.0.0.1:0, http_addr: 127.0.0.1:0 }
admin: { addr: 127.0.0.1:0 }
discovery:
  static:
    gateway: ["{{.Gateway}}"]
    auth: ["{{.Auth}}"]
db: { dsn: "{{.DSN}}" }
kek: { source: file, path: {{.Dir}}/kek }
gateway: { service: gateway, issuer: {{.Issuer}}, auth_service: auth }
public: { http_addr: 127.0.0.1:0 }
public_auth: { jwt_secret: { file: {{.Dir}}/jwt.key } }
recipients: { allowed_prefixes: ["359"] }
webhook: { allow_http: {{.Webhook}}, allow_private: {{.Webhook}} }
{{.Extra}}`))

func (e *env) config(t *testing.T) string {
	t.Helper()
	var b bytes.Buffer
	if err := cfgTmpl.Execute(&b, e); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(e.Dir, "sms-gw.yaml")
	if err := os.WriteFile(p, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func token(t *testing.T, exp time.Time, paths ...string) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return enc(map[string]string{"alg": "EdDSA"}) + "." + enc(map[string]any{"aud": "lcm", "tid": tenant, "spiffe_paths": paths,
		"iat": exp.Add(-30 * time.Minute).Unix(), "nbf": exp.Add(-30 * time.Minute).Unix(), "exp": exp.Unix()}) + ".c2ln"
}

func closedAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func listen(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l.Addr().String()
}

// writeState writes an lcmidentity state file holding an SVID for id valid
// until notAfter.
func writeState(t *testing.T, path, id string, notAfter time.Time) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(id)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), URIs: []*url.URL{u}, NotBefore: notAfter.Add(-time.Hour), NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]string{"cert_pem": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))})
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

var testPlanner = planner{lookupEnv: func(string) (string, bool) { return "", false }, now: func() time.Time { return clock }}

func runPreflight(t *testing.T, path string, args ...string) (string, int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := preflight.Main(context.Background(), "sms-gw", append([]string{"-config", path}, args...), &out, &errOut, "", testPlanner.plan)
	if errOut.Len() > 0 {
		t.Fatalf("stderr: %s", errOut.String())
	}
	return out.String(), code
}

func results(t *testing.T, path string) []preflight.Result {
	t.Helper()
	return resultsWith(testPlanner, path)
}

func resultsWith(p planner, path string) []preflight.Result {
	return preflight.Run(context.Background(), p.plan(context.Background(), path))
}

// portal serves the enrol endpoint (405 to GET) and the platform JWKS, like
// the core's gateway; trust is a TLS config that trusts it.
func portal(t *testing.T) (srv *httptest.Server, trust *tls.Config) {
	t.Helper()
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == preflight.JWKSPath {
			_, _ = io.WriteString(w, `{"keys":[{"kty":"OKP","crv":"Ed25519","kid":"k1","x":"J-dVacDYm0zmy2-X1K6XQWwsCnsjz5FQQ7K8U3wqHPE","use":"sig"}]}`)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	t.Cleanup(srv.Close)
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	return srv, &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
}

// find returns the results named name.
func find(res []preflight.Result, name string) []preflight.Result {
	var out []preflight.Result
	for _, r := range res {
		if r.Name == name {
			out = append(out, r)
		}
	}
	return out
}

func expect(t *testing.T, res []preflight.Result, name string, status preflight.Status, detail string) preflight.Result {
	t.Helper()
	for _, r := range find(res, name) {
		if r.Status == status && strings.Contains(r.Detail, detail) {
			return r
		}
	}
	t.Fatalf("no %s %q containing %q in:\n%s", status, name, detail, dump(res))
	return preflight.Result{}
}

func dump(res []preflight.Result) string {
	var b strings.Builder
	for _, r := range res {
		fmt.Fprintf(&b, "%s %s: %s | %s\n", r.Status, r.Name, r.Detail, r.Fix)
	}
	return b.String()
}

// TestPreflightReportsTheIncident is the installation that crash-looped:
// every problem is reported in one run.
func TestPreflightReportsTheIncident(t *testing.T) {
	e := newEnv(t)
	e.Env, e.TrustDomain, e.Webhook, e.Insecure = "production", "example.org", true, false
	e.Extra = "public_extra: 1\n"
	if err := os.WriteFile(e.TokenFile, []byte(token(t, clock.Add(-47*time.Minute), "spiffe://infra.example.org/svc/sms-gw")), 0o600); err != nil {
		t.Fatal(err)
	}
	res := results(t, e.config(t))

	r := expect(t, res, "config: load", preflight.Fail, "field public_extra not found")
	if !strings.Contains(r.Fix, "remove or rename") {
		t.Fatalf("fix %q", r.Fix)
	}
	r = expect(t, res, "config: validation", preflight.Fail, "sslmode=verify-full")
	if !strings.Contains(r.Fix, "sslrootcert") {
		t.Fatalf("fix %q", r.Fix)
	}
	expect(t, res, "config: validation", preflight.Fail, "webhook.allow_http")
	if len(find(res, "config: warning")) != 0 {
		t.Fatalf("warnings already failed as validation must not repeat:\n%s", dump(res))
	}
	r = expect(t, res, "enrolment token: expiry", preflight.Fail, "expired 47 min ago")
	if !strings.Contains(r.Fix, "Gateway operations > Enrolment tokens") {
		t.Fatalf("fix %q", r.Fix)
	}
	r = expect(t, res, "enrolment token: identity", preflight.Fail, "does not authorise spiffe://example.org/svc/sms-gw")
	if !strings.Contains(r.Fix, "trust_domain example.org but the token names spiffe://infra.example.org/svc/sms-gw") {
		t.Fatalf("fix %q", r.Fix)
	}
	expect(t, res, "reach: gateway", preflight.Fail, "connection refused")
	expect(t, res, "reach: auth", preflight.Fail, "connection refused")
	expect(t, res, "reach: lcm (enroll.lcm_grpc)", preflight.Fail, "connection refused")
	expect(t, res, "reach: enroll (enroll.enroll_url)", preflight.Fail, "connection refused")
	r = expect(t, res, "database: db.dsn", preflight.Fail, "connection refused")
	if strings.Contains(dump(res), "pw-s3cret") {
		t.Fatal("the database password must never be reported")
	}
	if strings.Contains(r.Fix, "core") {
		t.Fatalf("database fix must not talk about the core: %q", r.Fix)
	}
	out, code := runPreflight(t, e.config(t))
	if code != 1 || !strings.Contains(out, "FAILED:") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

func TestPreflightHealthyEnvironment(t *testing.T) {
	e := newEnv(t)
	e.Gateway, e.Auth, e.LCM = listen(t), listen(t), listen(t)
	srv, trust := portal(t)
	e.EnrollURL, e.Issuer = srv.URL+"/api/lcm/v1/enroll", srv.URL
	p := testPlanner
	p.issuerTLS = trust
	res := resultsWith(p, e.config(t))
	for _, r := range res {
		if r.Status == preflight.Fail && r.Name != "database: db.dsn" {
			t.Errorf("unexpected failure %s: %s", r.Name, r.Detail)
		}
	}
	expect(t, res, "config: load", preflight.Pass, "parsed")
	expect(t, res, "config: validation", preflight.Pass, `valid for env "dev"`)
	expect(t, res, "config: warning", preflight.Warn, "enroll.insecure")
	expect(t, res, "file: kek.path", preflight.Pass, "32-byte key")
	expect(t, res, "secret: public_auth.jwt_secret", preflight.Pass, "(40 bytes)")
	expect(t, res, "file: authz.path", preflight.Pass, "readable")
	expect(t, res, "dir: enroll.state_file", preflight.Pass, "writable")
	expect(t, res, "enrolment: state", preflight.Skip, "no saved SVID")
	expect(t, res, "enrolment token: format", preflight.Pass, "signature NOT verified")
	expect(t, res, "enrolment token: expiry", preflight.Pass, "expires in 20 min")
	expect(t, res, "enrolment token: tenant", preflight.Pass, tenant)
	expect(t, res, "enrolment token: identity", preflight.Pass, "spiffe://infra.example.org/svc/sms-gw")
	expect(t, res, "reach: gateway", preflight.Pass, "reachable")
	expect(t, res, "reach: lcm (enroll.lcm_grpc)", preflight.Pass, "reachable")
	expect(t, res, "reach: enroll (enroll.enroll_url)", preflight.Warn, "NOT verified")
	expect(t, res, "issuer: gateway.issuer origin", preflight.Pass, "is the enrolment URL's origin")
	expect(t, res, "issuer: gateway.issuer signing keys", preflight.Pass, "publishes 1 platform signing key")
	if strings.Contains(dump(res), strings.TrimSpace(token(t, clock.Add(20*time.Minute), "spiffe://infra.example.org/svc/sms-gw"))) {
		t.Fatal("the token must never be reported")
	}
}

// The remote install that enrolled and registered, then refused every
// console request: gateway.issuer was not the portal origin auth signs with.
func TestPreflightWrongIssuer(t *testing.T) {
	srv, trust := portal(t)
	p := testPlanner
	p.issuerTLS = trust
	for _, tc := range []struct {
		name, issuer string
		originStatus preflight.Status
		origin, keys string
	}{
		{"nothing listens there (localhost:8443)", "https://" + closedAddr(t), preflight.Warn, "differs from the enrolment URL's origin", "connection refused"},
		{"right host, no keys at that path", srv.URL + "/auth", preflight.Pass, "is the enrolment URL's origin", "answered HTTP 405"},
		{"placeholder never rendered", "'@@GATEWAY_ISSUER@@'", preflight.Fail, "not an https origin", "not an https origin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.EnrollURL, e.Issuer = srv.URL+"/api/lcm/v1/enroll", tc.issuer
			res := resultsWith(p, e.config(t))
			expect(t, res, "issuer: gateway.issuer origin", tc.originStatus, tc.origin)
			r := expect(t, res, "issuer: gateway.issuer signing keys", preflight.Fail, tc.keys)
			if !strings.Contains(r.Fix, "portal's public origin") {
				t.Fatalf("fix %q", r.Fix)
			}
		})
	}
	// -offline still catches a wrong host by comparing with the enrolment URL.
	e := newEnv(t)
	e.EnrollURL, e.Issuer = "https://portal.example.org:8443/api/lcm/v1/enroll", "https://localhost:8443"
	out, _ := runPreflight(t, e.config(t), "-offline")
	if !strings.Contains(out, "issuer origin https://localhost:8443 differs from the enrolment URL's origin https://portal.example.org:8443") {
		t.Fatalf("offline run:\n%s", out)
	}
}

func TestPreflightSecureEnrollURLUsesSystemRoots(t *testing.T) {
	e := newEnv(t)
	e.Insecure = false
	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // the refused handshake is the point
	srv.StartTLS()
	defer srv.Close()
	e.EnrollURL = srv.URL + "/api/lcm/v1/enroll"
	r := expect(t, results(t, e.config(t)), "reach: enroll (enroll.enroll_url)", preflight.Fail, "TLS: certificate signed by an authority this host does not trust")
	if r.Fix == "" {
		t.Fatal("TLS failure needs a fix hint")
	}
}

func TestPreflightEnrolledSkipsTheToken(t *testing.T) {
	e := newEnv(t)
	writeState(t, e.StateFile, "spiffe://infra.example.org/svc/sms-gw", clock.Add(time.Hour))
	if err := os.Remove(e.TokenFile); err != nil { // consumed long ago
		t.Fatal(err)
	}
	res := results(t, e.config(t))
	expect(t, res, "enrolment: state", preflight.Pass, "enrolled: saved SVID spiffe://infra.example.org/svc/sms-gw valid until")
	for _, n := range []string{"format", "expiry", "tenant", "identity"} {
		expect(t, res, "enrolment token: "+n, preflight.Skip, "enrolment already done")
	}
	expect(t, res, "reach: enroll (enroll.enroll_url)", preflight.Warn, "only needed to enrol again")
}

func TestPreflightUnusableStateFallsBackToTheToken(t *testing.T) {
	for name, write := range map[string]func(t *testing.T, p string){
		"expired": func(t *testing.T, p string) {
			writeState(t, p, "spiffe://infra.example.org/svc/sms-gw", clock.Add(30*time.Second))
		},
		"other identity": func(t *testing.T, p string) {
			writeState(t, p, "spiffe://infra.example.org/svc/mail-gw", clock.Add(time.Hour))
		},
		"garbage": func(t *testing.T, p string) {
			if err := os.WriteFile(p, []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			write(t, e.StateFile)
			res := results(t, e.config(t))
			expect(t, res, "enrolment: state", preflight.Warn, "will enrol again with the token")
			expect(t, res, "enrolment token: expiry", preflight.Pass, "expires in")
		})
	}
}

func TestPreflightFileProblems(t *testing.T) {
	e := newEnv(t)
	if err := os.WriteFile(filepath.Join(e.Dir, "kek"), []byte("too short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(e.Dir, "jwt.key"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(e.Dir, "policy.yaml")); err != nil {
		t.Fatal(err)
	}
	e.DSN = "postgres://smsgw_app:pw@db/sms_gw?sslmode=verify-full&sslrootcert=" + filepath.Join(e.Dir, "pg-ca.pem")
	e.Extra = "acme: { enabled: false }\n"
	res := results(t, e.config(t))
	r := expect(t, res, "file: kek.path", preflight.Fail, "kek: KEK must be 32 bytes")
	if !strings.Contains(r.Fix, "32 random bytes") {
		t.Fatalf("fix %q", r.Fix)
	}
	expect(t, res, "secret: public_auth.jwt_secret", preflight.Fail, "mode 0640 or stricter (has 0644)")
	expect(t, res, "file: authz.path", preflight.Fail, "does not exist")
	expect(t, res, "file: db.dsn sslrootcert", preflight.Fail, "pg-ca.pem does not exist")
	r = expect(t, res, "database: db.dsn", preflight.Fail, "cannot use the DSN: failed to configure TLS")
	if strings.Contains(r.Detail, "cannot parse") || strings.Contains(r.Detail, "smsgw_app:") {
		t.Fatalf("the DSN must not be echoed: %q", r.Detail)
	}
}

func TestPreflightShortSecretAndMissingTokenFile(t *testing.T) {
	e := newEnv(t)
	if err := os.WriteFile(filepath.Join(e.Dir, "jwt.key"), []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.TokenFile = filepath.Join(e.Dir, "absent-token")
	res := results(t, e.config(t))
	expect(t, res, "secret: public_auth.jwt_secret", preflight.Fail, "is 5 bytes; at least 32")
	expect(t, res, "enrolment token: format", preflight.Fail, "does not exist")
	expect(t, res, "enrolment token: expiry", preflight.Skip, "token unusable")
}

func TestPreflightMissingDiscoveryEntry(t *testing.T) {
	e := newEnv(t)
	path := e.config(t)
	raw, _ := os.ReadFile(path)
	raw = bytes.Replace(raw, []byte(`    auth: ["`+e.Auth+`"]`+"\n"), nil, 1)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	r := expect(t, results(t, path), "discovery: auth", preflight.Fail, `no endpoint for service "auth"`)
	if !strings.Contains(r.Fix, "discovery.static.auth") {
		t.Fatalf("fix %q", r.Fix)
	}
}

func TestPreflightUnloadableConfig(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("service_name: [unclosed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{bad, filepath.Join(dir, "absent.yaml")} {
		res := results(t, path)
		if len(res) != 1 || res[0].Name != "config: load" || res[0].Status != preflight.Fail {
			t.Fatalf("%s:\n%s", path, dump(res))
		}
	}
}

func TestPreflightLegacyEnvironment(t *testing.T) {
	cases := []struct {
		env    map[string]string
		status preflight.Status
		detail string
	}{
		{map[string]string{"SMS_GW_ACCESS_TTL_SECONDS": "3600"}, preflight.Pass, "applied: SMS_GW_ACCESS_TTL_SECONDS"},
		{map[string]string{"SMS_GW_ACCESS_TTL_SECONDS": "-1"}, preflight.Fail, "must be a positive number"},
		{map[string]string{"CERTS_DIR": "/certs"}, preflight.Warn, "obsolete legacy variables ignored: CERTS_DIR"},
	}
	for _, tc := range cases {
		e := newEnv(t)
		p := planner{now: testPlanner.now, lookupEnv: func(k string) (string, bool) { v, ok := tc.env[k]; return v, ok }}
		res := preflight.Run(context.Background(), p.plan(context.Background(), e.config(t)))
		expect(t, res, "config: legacy environment", tc.status, tc.detail)
	}
}

func TestPreflightJSON(t *testing.T) {
	e := newEnv(t)
	out, code := runPreflight(t, e.config(t), "-json")
	if code != 1 {
		t.Fatalf("exit %d (the database is unreachable)", code)
	}
	var rep struct {
		Module  string `json:"module"`
		OK      bool   `json:"ok"`
		Results []preflight.Result
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("%v:\n%s", err, out)
	}
	if rep.Module != "sms-gw" || rep.OK || len(rep.Results) < 10 {
		t.Fatalf("%+v", rep)
	}
}

func TestDSNParams(t *testing.T) {
	cases := []struct {
		dsn  string
		want map[string]string
	}{
		{"postgres://u:p@h:5432/d?sslmode=verify-full&sslrootcert=/ca.pem", map[string]string{"sslmode": "verify-full", "sslrootcert": "/ca.pem"}},
		{"host=h user=u password='a b' sslmode=require sslrootcert=/x/ca.pem", map[string]string{"host": "h", "user": "u", "password": "a b", "sslmode": "require", "sslrootcert": "/x/ca.pem"}},
		{"postgres://h/d", map[string]string{}},
	}
	for _, tc := range cases {
		got := dsnParams(tc.dsn)
		if len(got) != len(tc.want) {
			t.Fatalf("%s: %v", tc.dsn, got)
		}
		for k, v := range tc.want {
			if got[k] != v {
				t.Errorf("%s: %s = %q, want %q", tc.dsn, k, got[k], v)
			}
		}
	}
}

func TestRedact(t *testing.T) {
	dsn := "postgres://u:hunter22@h/d"
	if got := redact("failed for "+dsn+" with hunter22", dsn); strings.Contains(got, "hunter22") {
		t.Fatalf("%q", got)
	}
	if got := redact("pw=topsecret", "host=h password=topsecret"); strings.Contains(got, "topsecret") {
		t.Fatalf("%q", got)
	}
}

func TestDBFailureClassifies(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		detail string
		fix    string
	}{
		{"bad password", &pgconn.PgError{Code: "28P01", Message: `password authentication failed for user "u"`}, "authentication failed", "pg_hba.conf"},
		{"no database", &pgconn.PgError{Code: "3D000", Message: `database "x" does not exist`}, `database "x" does not exist`, "create the database"},
		{"other server error", &pgconn.PgError{Code: "53300", Message: "too many connections"}, "SQLSTATE 53300", ""},
		{"no tls on server", errors.New("failed to connect to `host=h`: server refused TLS connection"), "the server does not offer TLS", "enable ssl"},
		{"untrusted server", fmt.Errorf("connect: %w", x509.UnknownAuthorityError{}), "TLS: x509: certificate signed by unknown authority", "sslrootcert"},
		{"refused", &net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}, "connection refused", "accepts connections from this host"},
		{"password echoed", errors.New("weird failure mentioning hunter22"), "xxxxx", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := dbFailure("h:5432/d as u", "verify-full", tc.err, "postgres://u:hunter22@h/d")
			if r.Status != preflight.Fail || !strings.Contains(r.Detail, tc.detail) || !strings.Contains(r.Fix, tc.fix) {
				t.Fatalf("%+v", r)
			}
			if strings.Contains(r.Detail, "hunter22") {
				t.Fatal("password reported")
			}
		})
	}
}

// Every check that contacts another host is marked Network, so -offline skips
// it (CI validates bundles where neither the core nor the database exists).
func TestPreflightNetworkChecksAreMarked(t *testing.T) {
	if !dbCheck("database: db.dsn", "postgres://u@127.0.0.1:1/db?sslmode=verify-full").Network {
		t.Fatal("the database check must be a network check")
	}
}
