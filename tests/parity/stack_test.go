//go:build parity

package parity

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-tangra/go-tangra-auth/sdk/v4/pkg/authclient"
	freya "github.com/go-tangra/go-tangra/v4"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/app"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/config"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/parity"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/store"
)

const (
	jwtSecret = "parity-jwt-secret-0123456789abcdef-0123456789"
	password  = "Passw0rd!"
	tenant    = "00000000-0000-0000-0000-000000000001" // the production tenant
	dlrTokenA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

// profile is one rate-limit configuration applied identically to both.
type profile struct {
	name                  string
	loginRPM, sendRPM     float64
	loginBurst, sendBurst int
	trustLoopbackProxy    bool
}

var (
	// Production v3 (SMS_GW_*_RPM/BURST on sms-gw) with loopback as the
	// trusted proxy, as HAProxy will be.
	profProd = profile{"prod", 600, 6000, 100, 200, true}
	// Both services' built-in defaults, no trusted proxy.
	profDefaults = profile{"defaults", 10, 100, 5, 20, false}
)

func repoRoot() string {
	wd, _ := os.Getwd()
	return filepath.Join(wd, "..", "..")
}

func freePort(t testing.TB) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func run(t testing.TB, dir string, env []string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return string(out)
}

// ---------- PostgreSQL ----------

type pg struct {
	bin, dir string
	port     int
}

func pgBin(t testing.TB) string {
	if v := os.Getenv("SMSGW_PARITY_PG_BIN"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	if d := filepath.Join(home, ".cache", "tangra-sdk", "pg-16.9.0", "bin"); fileExists(filepath.Join(d, "pg_ctl")) {
		return d
	}
	if p, err := exec.LookPath("pg_ctl"); err == nil {
		return filepath.Dir(p)
	}
	t.Skip("no PostgreSQL binaries: set SMSGW_PARITY_PG_BIN")
	return ""
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func startPG(t testing.TB, bin, dir string) *pg {
	t.Helper()
	p := &pg{bin: bin, dir: dir, port: freePort(t)}
	run(t, "", nil, filepath.Join(bin, "initdb"), "-D", dir, "-U", "postgres", "--auth=trust", "-E", "UTF8", "--locale=C")
	run(t, "", nil, filepath.Join(bin, "pg_ctl"), "-D", dir, "-l", dir+".log", "-w", "-o",
		fmt.Sprintf("-p %d -c listen_addresses=127.0.0.1 -c unix_socket_directories='' -c fsync=off -c max_connections=200", p.port), "start")
	t.Cleanup(func() { _ = exec.Command(filepath.Join(bin, "pg_ctl"), "-D", dir, "-m", "immediate", "stop").Run() })
	return p
}

func (p *pg) dsn(db, user, pass string) string {
	u := user
	if pass != "" {
		u += ":" + pass
	}
	return fmt.Sprintf("postgres://%s@127.0.0.1:%d/%s?sslmode=disable", u, p.port, db)
}

func (p *pg) exec(t testing.TB, db string, stmts ...string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, p.dsn(db, "postgres", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for _, s := range stmts {
		if _, err := conn.Exec(ctx, s); err != nil {
			t.Fatalf("sql: %v\n%s", err, s)
		}
	}
}

// ---------- the stack ----------

type stack struct {
	t        *testing.T
	dir      string
	v3Src    string
	v3Bin    string
	mockBin  string
	migBin   string
	liveBin  string
	pg3, pg4 *pg
	carrier  string // mock carrier base URL
	fake     *httptest.Server
	certs    string
	caFP     string

	v3     *exec.Cmd
	v3Port int
	v3Logs int
	v4     *app.App
	v4Stop func()
	v4Cfg  string // config file (for smsgw-migrate and the in-process service)
	style  string // the legacy build's protobuf-go JSON flavour
}

func newStack(t *testing.T) *stack {
	s := &stack{t: t, dir: t.TempDir()}
	repo := os.Getenv("SMSGW_PARITY_V3_SRC")
	if repo == "" {
		repo = filepath.Join(repoRoot(), "..", "go-tangra-sms-gw")
	}
	if !fileExists(filepath.Join(repo, "cmd", "server")) {
		t.Skipf("legacy checkout not found at %s (SMSGW_PARITY_V3_SRC)", repo)
	}
	ref := os.Getenv("SMSGW_PARITY_V3_REF")
	if ref == "" {
		ref = "v3.0.9" // the version production runs
	}
	bin := pgBin(t)
	s.build(repo, ref)
	s.pg3 = startPG(t, bin, filepath.Join(s.dir, "pg3"))
	s.pg4 = startPG(t, bin, filepath.Join(s.dir, "pg4"))
	s.pg3.exec(t, "postgres", "CREATE DATABASE sms_gw")
	s.pg4.exec(t, "postgres", "CREATE DATABASE sms_gw",
		"CREATE ROLE smsgw_app LOGIN PASSWORD 'app' NOBYPASSRLS")
	s.startCarriers()
	s.makeCerts()
	return s
}

// build extracts the legacy source at ref (git archive: the checkout is
// never modified) and builds it, the legacy mock carrier and smsgw-migrate.
func (s *stack) build(repo, ref string) {
	t := s.t
	rev := strings.TrimSpace(run(t, repo, nil, "git", "rev-parse", ref+"^{commit}"))
	s.v3Src = filepath.Join(s.dir, "v3src")
	if err := os.MkdirAll(s.v3Src, 0o700); err != nil {
		t.Fatal(err)
	}
	run(t, "", nil, "sh", "-c", `git -C "$1" archive "$2" | tar -x -C "$3"`, "sh", repo, rev, s.v3Src)
	t.Logf("legacy baseline %s = %s (git archive of %s)", ref, rev, repo)
	env := append(os.Environ(), "CGO_ENABLED=0", "GOWORK=off", "GOFLAGS=-mod=readonly")
	s.v3Bin = filepath.Join(s.dir, "sms-gw-v3")
	s.mockBin = filepath.Join(s.dir, "test-linkmobility")
	s.migBin = filepath.Join(s.dir, "smsgw-migrate")
	run(t, s.v3Src, env, "go", "build", "-buildvcs=false", "-o", s.v3Bin, "./cmd/server")
	run(t, s.v3Src, env, "go", "build", "-buildvcs=false", "-o", s.mockBin, "./cmd/test-linkmobility")
	run(t, repoRoot(), env, "go", "build", "-o", s.migBin, "./cmd/smsgw-migrate")
	s.liveBin = filepath.Join(s.dir, "smsgw-parity")
	run(t, repoRoot(), env, "go", "build", "-o", s.liveBin, "./cmd/smsgw-parity")
	style, err := parity.LegacyJSONStyle(s.v3Bin)
	if err != nil {
		t.Fatal(err)
	}
	s.style = style
	t.Logf("legacy build JSON style: %s (V4 public.legacy_json_style set to match)", style)
}

func (s *stack) startCarriers(mockArgs ...string) {
	t := s.t
	port := freePort(t)
	if len(mockArgs) == 0 {
		mockArgs = []string{"--no-dlr-rate=1"} // receipts are driven by the corpus
	}
	cmd := exec.Command(s.mockBin, append([]string{fmt.Sprintf("--addr=127.0.0.1:%d", port)}, mockArgs...)...)
	cmd.Env = []string{"SMSGW_TEST_SEED=1"}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	s.carrier = fmt.Sprintf("http://127.0.0.1:%d", port)
	waitHTTP(t, s.carrier+"/healthz")
	s.fake = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/reject"):
			_, _ = w.Write([]byte(`{"return_code":2001,"return_message":"Invalid SID"}`))
		case strings.HasPrefix(r.URL.Path, "/http500"):
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"error":"carrier exploded"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(s.fake.Close)
}

func waitHTTP(t testing.TB, url string) {
	t.Helper()
	c := &http.Client{Timeout: time.Second}
	for range 150 {
		if resp, err := c.Get(url); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s did not become healthy", url)
}

// makeCerts writes the legacy CERTS_DIR layout (ca, server, client) so the
// legacy certificate bootstrap finds a valid keypair and never dials LCM.
func (s *stack) makeCerts() {
	t := s.t
	s.certs = filepath.Join(s.dir, "certs")
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "parity-ca"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(365 * 24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "sms-gw-service"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(200 * 24 * time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	caKeyDER, _ := x509.MarshalECPrivateKey(caKey)
	write := func(rel string, typ string, b []byte) {
		p := filepath.Join(s.certs, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: b}), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("ca/ca.crt", "CERTIFICATE", caDER)
	write("ca/ca.key", "EC PRIVATE KEY", caKeyDER)
	for _, sub := range []string{"server/server", "client/client"} {
		write(sub+".crt", "CERTIFICATE", der)
		write(sub+".key", "EC PRIVATE KEY", keyDER)
	}
	sum := sha256.Sum256(caDER)
	s.caFP = hex.EncodeToString(sum[:])
}

// ---------- legacy v3 ----------

func (s *stack) startV3(p profile) {
	t := s.t
	cfg := filepath.Join(s.dir, "v3configs")
	if !fileExists(cfg) {
		_ = os.MkdirAll(cfg, 0o700)
		for _, f := range []string{"data.yaml", "logger.yaml", "server.yaml"} {
			b, err := os.ReadFile(filepath.Join(s.v3Src, "configs", f))
			if err != nil {
				t.Fatal(err)
			}
			txt := string(b)
			txt = regexp.MustCompile(`postgresql://[^"]+`).ReplaceAllString(txt, fmt.Sprintf("postgresql://postgres@127.0.0.1:%d/sms_gw?sslmode=disable", s.pg3.port))
			txt = strings.ReplaceAll(txt, "0.0.0.0:9900", fmt.Sprintf("127.0.0.1:%d", freePort(t)))
			txt = strings.ReplaceAll(txt, "0.0.0.0:9901", fmt.Sprintf("127.0.0.1:%d", freePort(t)))
			if err := os.WriteFile(filepath.Join(cfg, f), []byte(txt), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if s.v3Port == 0 {
		s.v3Port = freePort(t)
	}
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + s.dir, "CERTS_DIR=" + s.certs, "LCM_CA_FINGERPRINT=" + s.caFP,
		"LCM_BOOTSTRAP_ENDPOINT=127.0.0.1:1", "MODULE_BOOTSTRAP_SECRET=unused", "SMS_GW_JWT_SECRET=" + jwtSecret,
		fmt.Sprintf("HTTP_PUBLIC_BIND=127.0.0.1:%d", s.v3Port), "SMS_GW_ALLOW_HTTP_WEBHOOKS=1",
		"SMS_GW_ACCESS_TTL_SECONDS=7200", "SMS_GW_REFRESH_TTL_SECONDS=604800"}
	if p.name != profDefaults.name {
		env = append(env, "SMS_GW_LOGIN_RPM="+ftoa(p.loginRPM), "SMS_GW_LOGIN_BURST="+strconv.Itoa(p.loginBurst),
			"SMS_GW_SEND_RPM="+ftoa(p.sendRPM), "SMS_GW_SEND_BURST="+strconv.Itoa(p.sendBurst))
	}
	if p.trustLoopbackProxy {
		env = append(env, "TRUSTED_PROXY_CIDRS=127.0.0.1/32")
	}
	s.v3Logs++
	logf, err := os.Create(filepath.Join(s.dir, fmt.Sprintf("v3-%d-%s.log", s.v3Logs, p.name)))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(s.v3Bin, "-c", cfg)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = s.dir, env, logf, logf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	s.v3 = cmd
	t.Cleanup(s.stopV3)
	waitHTTP(t, s.v3URL()+"/health")
}

func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

func (s *stack) v3URL() string { return fmt.Sprintf("http://127.0.0.1:%d", s.v3Port) }

func (s *stack) stopV3() {
	if s.v3 == nil {
		return
	}
	_ = s.v3.Process.Signal(os.Interrupt)
	done := make(chan struct{})
	go func() { _ = s.v3.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		_ = s.v3.Process.Kill()
		<-done
	}
	s.v3 = nil
}

// seedV3 inserts the parity seed into the legacy database (after the legacy
// service created its schema).
func (s *stack) seedV3() {
	t := s.t
	raw, err := os.ReadFile(filepath.Join(repoRoot(), "tests", "parity", "seed.sql"))
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := bcrypt.GenerateFromPassword([]byte(password), 4)
	sql := strings.NewReplacer(":hash", string(hash), ":carrier", s.carrier, ":fake", s.fake.URL).Replace(string(raw))
	s.pg3.exec(t, "sms_gw", sql)
}

// ---------- V4 ----------

// importV4 migrates the V4 schema and imports the legacy database with
// cmd/smsgw-migrate, as production was populated.
func (s *stack) importV4() {
	t := s.t
	s.prepareV4()
	src := filepath.Join(s.dir, "legacy-import.yaml")
	if err := os.WriteFile(src, []byte("source:\n  dsn: { env: SMSGW_LEGACY_DSN }\ntenants:\n  prod: \""+tenant+"\"\nplatform_actor: legacy-admin\nexclude_orphans: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "SMSGW_LEGACY_DSN="+s.pg3.dsn("sms_gw", "postgres", ""))
	out := run(t, s.dir, env, s.migBin, "-source-config", src, "-config", s.v4Cfg, "-tenant", "prod", "-apply")
	if err := os.WriteFile(filepath.Join(s.dir, "import-report.json"), []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("smsgw-migrate -apply: %d bytes of report", len(out))
}

// prepareV4 migrates the V4 schema and writes its configuration
// (deploy/dev.yaml with this stack's database, KEK and JWT secret).
func (s *stack) prepareV4() {
	t := s.t
	if err := store.Migrate(context.Background(), s.pg4.dsn("sms_gw", "postgres", "")); err != nil {
		t.Fatal(err)
	}
	kek := filepath.Join(s.dir, "kek")
	jwt := filepath.Join(s.dir, "jwt")
	if err := os.WriteFile(kek, []byte(strings.Repeat("k", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jwt, []byte(jwtSecret), 0o600); err != nil {
		t.Fatal(err)
	}
	dev, err := os.ReadFile(filepath.Join(repoRoot(), "deploy", "dev.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	txt := string(dev)
	for _, r := range []struct{ re, with string }{
		{`(?m)^  dsn: .*$`, "  dsn: " + s.pg4.dsn("sms_gw", "smsgw_app", "app")},
		{`(?m)^  migrate_dsn: .*$`, "  migrate_dsn: " + s.pg4.dsn("sms_gw", "postgres", "")},
		{`(?m)^  path: deploy/kek.dev$`, "  path: " + kek},
		{`(?m)^  jwt_secret: .*$`, "  jwt_secret: { file: " + jwt + " }"},
		{`(?m)^authz: .*$`, "authz: { source: file, path: " + filepath.Join(repoRoot(), "deploy", "policy.yaml") + " }"},
	} {
		re := regexp.MustCompile(r.re)
		if !re.MatchString(txt) {
			t.Fatalf("deploy/dev.yaml: %s not found", r.re)
		}
		txt = re.ReplaceAllString(txt, r.with)
	}
	s.v4Cfg = filepath.Join(s.dir, "v4.yaml")
	if err := os.WriteFile(s.v4Cfg, []byte(txt), 0o600); err != nil {
		t.Fatal(err)
	}
}

type denyAll struct{}

func (denyAll) Verify(context.Context, string) (authclient.Identity, error) {
	return authclient.Identity{}, authclient.ErrUnauthenticated
}
func (denyAll) Has(context.Context, string, string, string) (bool, error) { return false, nil }

// startV4 runs the complete V4 application in process (the mesh runtime
// uses the in-memory development CA, as the integration tests do; no
// gateway or auth service is needed for the public listener).
func (s *stack) startV4(p profile) (url string) {
	t := s.t
	cfg, err := config.Load(s.v4Cfg)
	if err != nil {
		t.Fatal(err)
	}
	ports := make([]int, 4)
	for i := range ports {
		ports[i] = freePort(t)
	}
	addr := func(i int) string { return fmt.Sprintf("127.0.0.1:%d", ports[i]) }
	cfg.Server.HTTPAddr, cfg.Server.GRPCAddr, cfg.Admin.Addr, cfg.Public.HTTPAddr = addr(0), addr(1), addr(2), addr(3)
	cfg.Public.HTTPSAddr = ""
	cfg.Public.LegacyJSONStyle = s.style
	cfg.RateLimits.LoginPerMinute, cfg.RateLimits.LoginBurst = p.loginRPM, p.loginBurst
	cfg.RateLimits.SendPerMinute, cfg.RateLimits.SendBurst = p.sendRPM, p.sendBurst
	cfg.Public.TrustedProxies = nil
	if p.trustLoopbackProxy {
		cfg.Public.TrustedProxies = []string{"127.0.0.1/32"}
	}
	logf, err := os.OpenFile(filepath.Join(s.dir, "v4-"+p.name+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	a, err := app.Build(context.Background(), cfg, app.Options{Verifier: denyAll{}, Checker: denyAll{}, NoRegistration: true,
		Logger: slog.NewJSONHandler(logf, nil), Freya: []freya.Option{freya.WithInsecureLocalDev(), freya.WithAllowAllPolicy()}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	s.v4 = a
	s.v4Stop = func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("v4 did not drain")
		}
		a.Close()
		_ = logf.Close()
	}
	t.Cleanup(s.stopV4)
	url = "http://" + a.PublicAddr()
	waitHTTP(t, url+"/health")
	return url
}

func (s *stack) stopV4() {
	if s.v4Stop != nil {
		s.v4Stop()
		s.v4Stop = nil
	}
}
