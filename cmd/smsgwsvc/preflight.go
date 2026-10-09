package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"gopkg.in/yaml.v3"

	"github.com/go-tangra/go-tangra/v4/preflight"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/config"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/sealed"
)

// dbTimeout bounds one database probe (connect + SELECT 1).
const dbTimeout = 5 * time.Second

// preflightCmd checks the configuration and the environment the service
// would start in and reports every problem at once: the configuration with
// the real loader and validation, the files it references, the enrolment
// token (decoded locally, not verified), reachability of the core and the
// database. Nothing is changed; the token is not consumed.
func preflightCmd(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	p := planner{lookupEnv: os.LookupEnv, now: time.Now}
	return preflight.Main(ctx, "sms-gw", args, stdout, stderr, "deploy/dev.yaml", p.plan)
}

// planner builds the checks; lookupEnv and now are seams for tests.
type planner struct {
	lookupEnv func(string) (string, bool)
	now       func() time.Time
}

func (p planner) plan(_ context.Context, path string) []preflight.Check {
	cfg, checks, ok := p.loadConfig(path)
	if !ok {
		return checks
	}
	checks = append(checks, configChecks(cfg)...)
	checks = append(checks, fileChecks(cfg)...)
	enrolled := false
	if cfg.Enroll.Enabled {
		var ec []preflight.Check
		ec, enrolled = p.enrollChecks(cfg)
		checks = append(checks, ec...)
	}
	checks = append(checks, reachChecks(cfg, enrolled)...)
	return append(checks, dbChecks(cfg)...)
}

// loadConfig loads the file with the service's strict loader and applies
// the legacy environment like a real start. Unknown keys are reported one by
// one and the rest of the file is still checked; any other load error ends
// the plan (ok false).
func (p planner) loadConfig(path string) (config.Config, []preflight.Check, bool) {
	const name = "config: load"
	cfg, err := config.Load(path)
	var checks []preflight.Check
	var te *yaml.TypeError
	switch {
	case errors.As(err, &te):
		for _, msg := range te.Errors {
			checks = append(checks, preflight.Static(name, preflight.Failf("%s: %s", path, msg).
				WithFix("remove or rename the key: the loader refuses keys it does not know (check spelling and indentation)")))
		}
	case err != nil:
		return cfg, []preflight.Check{preflight.Static(name, preflight.Failf("%s", oneLine(err)).
			WithFix("the file must exist, be readable and be valid YAML"))}, false
	default:
		checks = append(checks, preflight.Static(name, preflight.Passf("%s parsed (unknown keys refused)", path)))
	}
	applied, obsolete, err := config.ApplyLegacyEnv(&cfg, p.lookupEnv)
	switch {
	case err != nil:
		checks = append(checks, preflight.Static("config: legacy environment", preflight.Failf("%s", oneLine(err))))
	case len(obsolete) > 0:
		checks = append(checks, preflight.Static("config: legacy environment",
			preflight.Warnf("obsolete legacy variables ignored: %s", strings.Join(obsolete, ", ")).WithFix("remove them from the environment")))
	case len(applied) > 0:
		checks = append(checks, preflight.Static("config: legacy environment", preflight.Passf("applied: %s", strings.Join(applied, ", "))))
	}
	return cfg, checks, true
}

// configChecks reports every validation problem and every accepted insecure
// opt-out.
func configChecks(cfg config.Config) []preflight.Check {
	const name = "config: validation"
	errs := cfg.ValidateAll()
	if len(errs) == 0 {
		return append([]preflight.Check{preflight.Static(name, preflight.Passf("valid for env %q (production rules: %s)", cfg.Env, yesNo(cfg.IsProduction())))},
			warningChecks(cfg, nil)...)
	}
	checks := make([]preflight.Check, 0, len(errs))
	for _, err := range errs {
		r := preflight.Failf("%s", strings.TrimPrefix(err.Error(), "config: "))
		if fix := validationFix(err.Error()); fix != "" {
			r = r.WithFix("%s", fix)
		}
		checks = append(checks, preflight.Static(name, r))
	}
	return append(checks, warningChecks(cfg, errs)...)
}

// warningChecks reports accepted insecure opt-outs, except those a
// validation failure already names.
func warningChecks(cfg config.Config, errs []error) []preflight.Check {
	var checks []preflight.Check
	for _, w := range cfg.Warnings() {
		key, _, _ := strings.Cut(w, ":")
		if slices.ContainsFunc(errs, func(err error) bool { return strings.Contains(err.Error(), key) }) {
			continue
		}
		checks = append(checks, preflight.Static("config: warning", preflight.Warnf("%s", w)))
	}
	return checks
}

// validationFixes maps validation messages to remedies, first match wins.
var validationFixes = []struct{ match, fix string }{
	{"sslmode=verify-full", "add sslmode=verify-full&sslrootcert=<CA of the database server> to the DSN; a database without TLS cannot back env: production"},
	{"webhook.allow_http", "remove webhook.allow_http and webhook.allow_private (development only)"},
	{"enroll.insecure", "remove enroll.insecure: the enroll URL must present a certificate this host trusts"},
	{"allow_non_loopback", "bind admin.addr to 127.0.0.1, or set admin.allow_non_loopback: true to accept the exposure"},
	{"trust_domain", "use the core's trust domain, as in the token's spiffe://<trust domain>/svc/... path"},
	{"tenant_id must be a uuid", "copy the tenant id from the portal (the tenant the enrolment token is minted for)"},
	{"identity.provider provided requires", "set enroll.enabled: true with the enroll section, or use another identity provider"},
	{"enroll.enabled requires", "set identity.provider: provided"},
}

func validationFix(msg string) string {
	for _, f := range validationFixes {
		if strings.Contains(msg, f.match) {
			return f.fix
		}
	}
	return ""
}

// fileChecks checks every file and directory the configuration references.
func fileChecks(cfg config.Config) []preflight.Check {
	var checks []preflight.Check
	switch cfg.KEK.Source {
	case "file":
		checks = append(checks, kekCheck("file: kek.path", cfg.KEK))
	case "env":
		checks = append(checks, kekCheck("secret: kek.env", cfg.KEK))
	}
	checks = append(checks, secretCheck("secret: public_auth.jwt_secret", cfg.PublicAuth.JWTSecret, 32))
	if cfg.ACME.Enabled {
		if cfg.ACME.EABHMACKey.Set() {
			checks = append(checks, secretCheck("secret: acme.eab_hmac_key", cfg.ACME.EABHMACKey, 1))
		}
		if cfg.ACME.DirectoryCABundle != "" {
			checks = append(checks, preflight.FileReadable("file: acme.directory_ca_bundle", cfg.ACME.DirectoryCABundle))
		}
		if cfg.ACME.CacheDir != "" {
			checks = append(checks, preflight.DirWritable("dir: acme.cache_dir", cfg.ACME.CacheDir))
		}
	}
	if cfg.Public.TLSCertFile != "" && cfg.Public.TLSKeyFile != "" {
		checks = append(checks, preflight.KeyPair("file: public tls cert/key", cfg.Public.TLSCertFile, cfg.Public.TLSKeyFile, nil))
	}
	if cfg.Authz.Source == "file" && cfg.Authz.Path != "" {
		checks = append(checks, preflight.FileReadable("file: authz.path", cfg.Authz.Path))
	}
	if cfg.Identity.Provider == "file" {
		f := cfg.Identity.File
		checks = append(checks, preflight.FileReadable("file: identity.file.cert", f.Cert),
			preflight.FileReadable("file: identity.file.key", f.Key), preflight.FileReadable("file: identity.file.bundle", f.Bundle))
	}
	if cfg.Enroll.Enabled && cfg.Enroll.StateFile != "" {
		checks = append(checks, preflight.DirWritable("dir: enroll.state_file", filepath.Dir(cfg.Enroll.StateFile)))
	}
	seen := map[string]bool{}
	for _, d := range dsns(cfg) {
		params := dsnParams(d.dsn)
		for _, k := range []string{"sslrootcert", "sslcert", "sslkey"} {
			if v := params[k]; v != "" && v != "system" && !seen[v] {
				seen[v] = true
				checks = append(checks, preflight.FileReadable("file: "+d.name+" "+k, v))
			}
		}
	}
	return checks
}

// kekCheck loads the KEK exactly like the service (a 32-byte key, raw or
// base64) after checking the file itself.
func kekCheck(name string, k config.KEK) preflight.Check {
	return preflight.Check{Name: name, Run: func(ctx context.Context) preflight.Result {
		if k.Source == "file" {
			if r := preflight.FileReadable(name, k.Path).Run(ctx); r.Status != preflight.Pass {
				return r
			}
		}
		src := k.Path
		if k.Source == "env" {
			src = "$" + k.Env
		}
		if _, err := sealed.LoadKEK(k.Source, k.Path, k.Env); err != nil {
			r := preflight.Failf("%s: %s", src, strings.TrimPrefix(oneLine(err), "sealed: "))
			if errors.Is(err, sealed.ErrKEK) {
				r = r.WithFix("the KEK must be 32 random bytes, raw or base64 (e.g. openssl rand -base64 32)")
			}
			return r
		}
		return preflight.Passf("%s holds a 32-byte key", src)
	}}
}

// secretCheck reads a secret like the service (file mode 0640 or stricter,
// non-empty) and checks its minimum length. The value is never reported.
func secretCheck(name string, s config.SecretRef, minLen int) preflight.Check {
	return preflight.Check{Name: name, Run: func(context.Context) preflight.Result {
		v, err := s.Read()
		if err != nil {
			return preflight.Failf("%s", strings.TrimPrefix(oneLine(err), "config: ")).
				WithFix("the file must exist, be readable by the service and have mode 0640 or stricter")
		}
		src := s.File
		if src == "" {
			src = "$" + s.Env
		}
		if len(v) < minLen {
			return preflight.Failf("%s is %d bytes; at least %d are required", src, len(v), minLen).
				WithFix("generate a longer secret (e.g. openssl rand -base64 48)")
		}
		return preflight.Passf("%s readable (%d bytes)", src, len(v))
	}}
}

// persistedSVID is the part of lcmidentity's state file preflight reads.
type persistedSVID struct {
	CertPEM string `json:"cert_pem"`
}

// enrollChecks reports the saved SVID and, while enrolment is still to come,
// the enrolment token. enrolled is true when a valid SVID is saved (the
// service renews over mTLS and never reads the token again).
func (p planner) enrollChecks(cfg config.Config) ([]preflight.Check, bool) {
	const name = "enrolment: state"
	want := cfg.LocalSPIFFEID()
	state, err := p.savedSVID(cfg.Enroll.StateFile, want)
	tokenChecks := preflight.EnrollTokenChecks("enrolment token", cfg.Enroll.TokenFile, preflight.EnrollExpect{
		TrustDomain: cfg.TrustDomain, ServiceName: cfg.ServiceName, TenantID: cfg.Enroll.TenantID, Now: p.now})
	switch {
	case errors.Is(err, os.ErrNotExist):
		return append([]preflight.Check{preflight.Static(name,
			preflight.Skipf("no saved SVID at %s yet: the first start enrols with the token", cfg.Enroll.StateFile))}, tokenChecks...), false
	case err != nil:
		return append([]preflight.Check{preflight.Static(name,
			preflight.Warnf("saved SVID at %s is unusable (%s): the service will enrol again with the token", cfg.Enroll.StateFile, err))}, tokenChecks...), false
	}
	checks := []preflight.Check{preflight.Static(name,
		preflight.Passf("enrolled: saved SVID %s valid until %s; renewals use mTLS, the token is not needed", want, state.NotAfter.UTC().Format("2006-01-02 15:04 UTC")))}
	for _, c := range tokenChecks {
		checks = append(checks, preflight.Static(c.Name, preflight.Skipf("enrolment already done (saved SVID valid)")))
	}
	return checks, true
}

// savedSVID returns the certificate in the lcmidentity state file when the
// service would reuse it: it names want and stays valid for over a minute.
func (p planner) savedSVID(path, want string) (*x509.Certificate, error) {
	b, err := os.ReadFile(path) // #nosec G304 -- operator-supplied state path
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		return nil, errors.New("not readable")
	}
	var rec persistedSVID
	if json.Unmarshal(b, &rec) != nil {
		return nil, errors.New("not an SVID state file")
	}
	blk, _ := pem.Decode([]byte(rec.CertPEM))
	if blk == nil {
		return nil, errors.New("no certificate")
	}
	crt, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return nil, errors.New("certificate does not parse")
	}
	if len(crt.URIs) != 1 || crt.URIs[0].String() != want {
		return nil, fmt.Errorf("certificate is not for %s", want)
	}
	if crt.NotAfter.Sub(p.now()) < time.Minute {
		return nil, fmt.Errorf("expired at %s", crt.NotAfter.UTC().Format("2006-01-02 15:04 UTC"))
	}
	return crt, nil
}

// reachChecks dials every core endpoint the service will use. The enroll URL
// is only needed for a first enrolment, so once enrolled its failure is a
// warning.
func reachChecks(cfg config.Config, enrolled bool) []preflight.Check {
	var checks []preflight.Check
	for _, svc := range []string{cfg.Gateway.Service, cfg.Gateway.AuthService} {
		if svc != "" && len(cfg.Discovery.Static[svc]) == 0 {
			checks = append(checks, preflight.Static("discovery: "+svc, preflight.Failf("no endpoint for service %q in discovery.static", svc).
				WithFix("add discovery.static.%s: [\"<core host>:<port>\"] (the core's %s gRPC port)", svc, svc)))
		}
	}
	names := make([]string, 0, len(cfg.Discovery.Static))
	for n := range cfg.Discovery.Static {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		for _, addr := range cfg.Discovery.Static[n] {
			checks = append(checks, preflight.TCPDial("reach: "+n, addr, preflight.DialTimeout))
		}
	}
	if !cfg.Enroll.Enabled {
		return checks
	}
	if cfg.Enroll.LCMGRPCTarget != "" {
		checks = append(checks, preflight.TCPDial("reach: lcm (enroll.lcm_grpc)", cfg.Enroll.LCMGRPCTarget, preflight.DialTimeout))
	}
	if cfg.Enroll.EnrollURL != "" {
		// The same server verification as lcmidentity's first enrolment.
		var tc *tls.Config
		if cfg.Enroll.Insecure {
			tc = &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13} // #nosec G402 -- mirrors enroll.insecure (development only), reported as a warning
		}
		probe := preflight.HTTPSProbe("reach: enroll (enroll.enroll_url)", cfg.Enroll.EnrollURL, tc, preflight.DialTimeout)
		if enrolled {
			run := probe.Run
			probe.Run = func(ctx context.Context) preflight.Result {
				r := run(ctx)
				if r.Status == preflight.Fail {
					r.Status = preflight.Warn
					r.Detail += " (only needed to enrol again)"
				}
				return r
			}
		}
		checks = append(checks, probe)
	}
	return checks
}

type namedDSN struct{ name, dsn string }

// dsns lists the configured connection strings, the migration one only when
// it differs.
func dsns(cfg config.Config) []namedDSN {
	var out []namedDSN
	if cfg.DB.DSN != "" {
		out = append(out, namedDSN{"db.dsn", cfg.DB.DSN})
	}
	if cfg.DB.MigrateDSN != "" && cfg.DB.MigrateDSN != cfg.DB.DSN {
		out = append(out, namedDSN{"db.migrate_dsn", cfg.DB.MigrateDSN})
	}
	return out
}

func dbChecks(cfg config.Config) []preflight.Check {
	var checks []preflight.Check
	for _, d := range dsns(cfg) {
		checks = append(checks, dbCheck("database: "+d.name, d.dsn))
	}
	return checks
}

// dbCheck connects with dsn exactly as the driver will (sslmode and its
// files included) and runs SELECT 1. Connection, TLS and authentication
// failures are reported distinctly; the password is never reported.
func dbCheck(name, dsn string) preflight.Check {
	return preflight.Check{Name: name, Network: true, Run: func(ctx context.Context) preflight.Result {
		pc, err := pgx.ParseConfig(dsn)
		if err != nil {
			// The driver quotes the whole DSN; keep only the reason.
			msg := parseDSNRE.ReplaceAllString(oneLine(err), "")
			return preflight.Failf("cannot use the DSN: %s", redact(msg, dsn)).
				WithFix("check the DSN syntax and the files it names (sslrootcert, sslcert, sslkey)")
		}
		target := fmt.Sprintf("%s/%s as %s", net.JoinHostPort(pc.Host, fmt.Sprint(pc.Port)), pc.Database, pc.User)
		mode := dsnParams(dsn)["sslmode"]
		if mode == "" {
			mode = "prefer"
		}
		ctx, cancel := context.WithTimeout(ctx, dbTimeout)
		defer cancel()
		conn, err := pgx.ConnectConfig(ctx, pc)
		if err != nil {
			return dbFailure(target, mode, err, dsn)
		}
		defer func() { _ = conn.Close(context.Background()) }()
		var one int
		if err := conn.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
			return preflight.Failf("%s: SELECT 1 failed: %s", target, redact(oneLine(err), dsn))
		}
		tlsState := "without TLS"
		if conn.PgConn().Conn() != nil {
			if _, ok := conn.PgConn().Conn().(*tls.Conn); ok {
				tlsState = "over TLS"
			}
		}
		return preflight.Passf("%s: connected %s (sslmode=%s), SELECT 1 ok", target, tlsState, mode)
	}}
}

// dbFailure classifies a failed connection: authentication and server
// errors, TLS, then the network.
func dbFailure(target, mode string, err error, dsn string) preflight.Result {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "28P01", "28000":
			return preflight.Failf("%s: authentication failed (%s)", target, pgErr.Message).
				WithFix("check the role and password in the DSN, and pg_hba.conf for this host")
		case "3D000":
			return preflight.Failf("%s: %s", target, pgErr.Message).WithFix("create the database, or correct its name in the DSN")
		}
		return preflight.Failf("%s: server refused: %s (SQLSTATE %s)", target, pgErr.Message, pgErr.Code)
	}
	msg := redact(oneLine(err), dsn)
	var (
		unknown  x509.UnknownAuthorityError
		hostname x509.HostnameError
		invalid  x509.CertificateInvalidError
		verify   *tls.CertificateVerificationError
	)
	switch {
	case strings.Contains(msg, "server refused TLS connection"):
		return preflight.Failf("%s: TLS: the server does not offer TLS (sslmode=%s)", target, mode).
			WithFix("enable ssl on the database server; production requires sslmode=verify-full")
	case errors.As(err, &unknown), errors.As(err, &hostname), errors.As(err, &invalid), errors.As(err, &verify),
		strings.Contains(msg, "x509:"), strings.Contains(msg, "tls:"):
		return preflight.Failf("%s: TLS: %s", target, tlsReason(msg)).
			WithFix("sslrootcert must name the CA that signed the server certificate, and with verify-full the DSN host must be in the certificate")
	}
	r := preflight.DialFailure(target, err, dbTimeout)
	r.Detail = redact(r.Detail, dsn)
	if r.Fix != "" {
		r.Fix = "check the host and port in the DSN, and that the database accepts connections from this host"
	}
	return r
}

// tlsReason keeps the part of a driver error after the last "tls:"/"x509:".
func tlsReason(msg string) string {
	for _, k := range []string{"x509: ", "tls: "} {
		if i := strings.LastIndex(msg, k); i >= 0 {
			return msg[i:]
		}
	}
	return msg
}

var parseDSNRE = regexp.MustCompile("cannot parse `[^`]*`: ")

var kvRE = regexp.MustCompile(`(\w+)\s*=\s*('(?:[^'\\]|\\.)*'|\S+)`)

// dsnParams returns the parameters of a postgres:// URL (its query) or of a
// keyword/value DSN.
func dsnParams(dsn string) map[string]string {
	out := map[string]string{}
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return out
		}
		for k, v := range u.Query() {
			if len(v) > 0 {
				out[k] = v[0]
			}
		}
		return out
	}
	for _, m := range kvRE.FindAllStringSubmatch(dsn, -1) {
		out[m[1]] = strings.Trim(m[2], "'")
	}
	return out
}

// redact removes the DSN's password and the DSN itself from msg.
func redact(msg, dsn string) string {
	pw := ""
	if u, err := url.Parse(dsn); err == nil && u.User != nil {
		pw, _ = u.User.Password()
	} else if v := dsnParams(dsn)["password"]; v != "" {
		pw = v
	}
	if dsn != "" {
		msg = strings.ReplaceAll(msg, dsn, "<dsn>")
	}
	if pw != "" {
		msg = strings.ReplaceAll(msg, pw, "xxxxx")
	}
	return msg
}

func oneLine(err error) string { return strings.Join(strings.Fields(err.Error()), " ") }

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
