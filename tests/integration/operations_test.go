//go:build integration

package integration

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/protobuf/types/known/durationpb"

	authv1 "github.com/go-tangra/go-tangra-auth/sdk/v4/api/proto/auth/v1"
	gatewayv1 "github.com/go-tangra/go-tangra-portal/sdk/v4/api/proto/gateway/v1"
	freya "github.com/go-tangra/go-tangra/v4"
	fconfig "github.com/go-tangra/go-tangra/v4/config"
	"github.com/go-tangra/go-tangra/v4/freyatest/testutil"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/app/apptest"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/config"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/housekeeper"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/repo"
)

// platform records what the module sent to the fake gateway and auth.
type platform struct {
	mu          sync.Mutex
	registers   []string // peer certificate serials of Register calls
	renews      int
	deregisters int
	permissions []string // peer serials of RegisterPermissions calls
}

func peerSerial(ctx context.Context) string {
	if p, ok := peer.FromContext(ctx); ok {
		if ti, ok := p.AuthInfo.(credentials.TLSInfo); ok && len(ti.State.PeerCertificates) > 0 {
			return ti.State.PeerCertificates[0].SerialNumber.String()
		}
	}
	return ""
}

func (p *platform) snapshot() (registers []string, renews, deregisters int, permissions []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.registers...), p.renews, p.deregisters, append([]string(nil), p.permissions...)
}

type fakeRegistry struct {
	gatewayv1.UnimplementedRegistryServer
	p *platform
}

func (f fakeRegistry) Register(ctx context.Context, r *gatewayv1.RegisterRequest) (*gatewayv1.Lease, error) {
	f.p.mu.Lock()
	defer f.p.mu.Unlock()
	f.p.registers = append(f.p.registers, peerSerial(ctx))
	return &gatewayv1.Lease{LeaseId: "lease-" + r.GetInstanceId(), Module: r.GetManifest().GetModule(), Ttl: durationpb.New(time.Second),
		RenewEvery: durationpb.New(200 * time.Millisecond)}, nil
}

func (f fakeRegistry) Renew(_ context.Context, r *gatewayv1.RenewRequest) (*gatewayv1.Lease, error) {
	f.p.mu.Lock()
	defer f.p.mu.Unlock()
	f.p.renews++
	return &gatewayv1.Lease{LeaseId: r.GetLeaseId(), Module: "sms-gw", Ttl: durationpb.New(time.Second), RenewEvery: durationpb.New(200 * time.Millisecond)}, nil
}

func (f fakeRegistry) Deregister(context.Context, *gatewayv1.DeregisterRequest) (*gatewayv1.DeregisterResponse, error) {
	f.p.mu.Lock()
	defer f.p.mu.Unlock()
	f.p.deregisters++
	return &gatewayv1.DeregisterResponse{}, nil
}

type fakeAuth struct {
	authv1.UnimplementedAuthorizationServer
	p *platform
}

func (f fakeAuth) RegisterPermissions(ctx context.Context, _ *authv1.RegisterPermissionsRequest) (*authv1.RegisterPermissionsResponse, error) {
	f.p.mu.Lock()
	defer f.p.mu.Unlock()
	f.p.permissions = append(f.p.permissions, peerSerial(ctx))
	return &authv1.RegisterPermissionsResponse{}, nil
}

// service runs a Freya service of the test trust domain on addr until stop.
func service(t *testing.T, ca *testutil.CA, name, addr string, register func(*freya.App)) (stop func()) {
	t.Helper()
	cfg := fconfig.Default()
	cfg.ServiceName, cfg.TrustDomain = name, "example.org"
	cfg.Server.GRPCAddr, cfg.Server.HTTPAddr, cfg.Admin.Addr = addr, "", "127.0.0.1:0"
	app, err := freya.New(cfg, freya.WithIdentityProvider(testutil.NewMemProvider(ca, ca.MustIssue(name, testutil.IssueOptions{}))), freya.WithAllowAllPolicy())
	if err != nil {
		t.Fatal(err)
	}
	register(app)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = app.Run(ctx); close(done) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			<-done
			app.Close()
		})
	}
	t.Cleanup(stop)
	return stop
}

func eventually(t *testing.T, what string, d time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func status(t *testing.T, c *http.Client, url string) int {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		return 0
	}
	resp.Body.Close()
	return resp.StatusCode
}

// TestIdentityRegistrationLifecycle: auth unavailable at startup and
// recovering, a gateway outage with an identity rotation in between (the
// pooled connection re-handshakes with the rotated SVID), and an expired
// identity that takes the module out of readiness, releases the lease and
// refuses mesh traffic until a valid identity arrives. The public Hermes
// edge keeps serving throughout.
func TestIdentityRegistrationLifecycle(t *testing.T) {
	env := Start(t, "")
	ca := testutil.MustCA("example.org")
	issue := func(ttl time.Duration) tls.Certificate {
		now := time.Now().Truncate(time.Second)
		return ca.MustIssue("sms-gw", testutil.IssueOptions{NotBefore: now.Add(-time.Second), NotAfter: now.Add(ttl)})
	}
	serial := func(c tls.Certificate) string {
		leaf, _ := x509.ParseCertificate(c.Certificate[0])
		return leaf.SerialNumber.String()
	}
	first := issue(time.Hour)
	prov := testutil.NewMemProvider(ca, first)
	gwAddr, authAddr := freeAddr(t), freeAddr(t)
	p := &platform{}
	stopGateway := service(t, ca, "gateway", gwAddr, func(a *freya.App) { gatewayv1.RegisterRegistryServer(a.GRPC(), fakeRegistry{p: p}) })

	run := apptest.Start(t, apptest.Options{DSN: env.DB.AppDSN, KEK: env.KEK, Register: true,
		Freya: []freya.Option{freya.WithIdentityProvider(prov), freya.WithAllowAllPolicy()},
		Configure: func(c *config.Config) {
			c.Discovery.Static = map[string][]string{"gateway": {gwAddr}, "auth": {authAddr}}
		}})
	a := run.App
	public := &http.Client{Timeout: 2 * time.Second}
	admin := "http://" + a.AdminAddr() + "/readyz"
	gw := tls.Certificate(ca.MustIssue("gateway", testutil.IssueOptions{}))
	mesh := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		Certificates: []tls.Certificate{gw}, MinVersion: tls.VersionTLS12, InsecureSkipVerify: true, // #nosec G402 -- chain verified below
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			leaf, err := x509.ParseCertificate(raw[0])
			if err != nil {
				return err
			}
			_, err = leaf.Verify(x509.VerifyOptions{Roots: ca.Pool(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}})
			return err
		}}, DisableKeepAlives: true}}
	ep, err := a.Freya.HTTP().Endpoint()
	if err != nil {
		t.Fatal(err)
	}
	management := "https://" + ep.Host + "/api/sms-gw/v1/providers"

	eventually(t, "registered with the gateway", 15*time.Second, func() bool { r, _, _, _ := p.snapshot(); return len(r) > 0 })
	if r, _, _, _ := p.snapshot(); r[0] != serial(first) {
		t.Fatalf("registered with serial %s, want %s", r[0], serial(first))
	}
	if code := status(t, mesh, management); code != http.StatusUnauthorized {
		t.Fatalf("mesh request with a valid identity and no operator token: %d", code)
	}

	// Auth was unavailable at startup: permissions arrive once it is up.
	if _, _, _, perms := p.snapshot(); len(perms) != 0 {
		t.Fatal("auth registration without auth")
	}
	service(t, ca, "auth", authAddr, func(a *freya.App) { authv1.RegisterAuthorizationServer(a.GRPC(), fakeAuth{p: p}) })
	eventually(t, "auth registration after auth recovered", 15*time.Second, func() bool { _, _, _, perms := p.snapshot(); return len(perms) > 0 })

	// Gateway outage; the identity rotates meanwhile.
	stopGateway()
	before, _, _, _ := p.snapshot()
	second := issue(time.Hour)
	prov.Rotate(second)
	time.Sleep(2 * time.Second)
	if code := status(t, public, run.Public+"/health"); code != 200 {
		t.Fatalf("public edge during the gateway outage: %d", code)
	}
	if code := status(t, public, admin); code != 200 {
		t.Fatalf("readiness must not depend on the gateway: %d", code)
	}
	service(t, ca, "gateway", gwAddr, func(a *freya.App) { gatewayv1.RegisterRegistryServer(a.GRPC(), fakeRegistry{p: p}) })
	eventually(t, "re-registration after the gateway outage", 40*time.Second, func() bool { r, _, _, _ := p.snapshot(); return len(r) > len(before) })
	if r, _, _, _ := p.snapshot(); r[len(r)-1] != serial(second) {
		t.Fatalf("re-registered with serial %s, want the rotated %s", r[len(r)-1], serial(second))
	}

	// Expiry: not ready, lease released, mesh refused; public edge serves.
	_, _, deregBefore, _ := p.snapshot()
	short := issue(2 * time.Second)
	prov.Rotate(short)
	eventually(t, "unready after identity expiry", 10*time.Second, func() bool { return status(t, public, admin) == http.StatusServiceUnavailable })
	eventually(t, "lease released after expiry", 10*time.Second, func() bool { _, _, d, _ := p.snapshot(); return d > deregBefore })
	if code := status(t, mesh, management); code == http.StatusUnauthorized || code == http.StatusOK {
		t.Fatalf("mesh traffic accepted with an expired identity: %d", code)
	}
	if code := status(t, public, run.Public+"/health"); code != 200 {
		t.Fatalf("public edge with an expired mesh identity: %d", code)
	}
	regBefore, _, _, _ := p.snapshot()
	renewed := issue(time.Hour)
	prov.Rotate(renewed)
	eventually(t, "ready after renewal", 10*time.Second, func() bool { return status(t, public, admin) == http.StatusOK })
	eventually(t, "registered again after renewal", 20*time.Second, func() bool { r, _, _, _ := p.snapshot(); return len(r) > len(regBefore) })
	// The pooled connection may still be the one authenticated before the
	// expiry (with a then-valid SVID); it never carries the expired one.
	if r, _, _, _ := p.snapshot(); r[len(r)-1] == serial(short) {
		t.Fatal("registered with the expired SVID")
	}
	if code := status(t, mesh, management); code != http.StatusUnauthorized {
		t.Fatalf("mesh request after renewal: %d", code)
	}

	// Clean shutdown drains the workers and withdraws the lease.
	_, _, deregBefore, _ = p.snapshot()
	run.Stop()
	if _, _, d, _ := p.snapshot(); d <= deregBefore {
		t.Fatal("shutdown must deregister")
	}
}

// TestRetentionBoundaries runs the retention worker twice over two tenants:
// expired messages and their receipts go together, zero retention and
// younger messages stay, and each provider's window applies only to it.
func TestRetentionBoundaries(t *testing.T) {
	env := Start(t, "")
	ctx := context.Background()
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC) // seeded messages: 2026-01-02T00:00Z
	owner, err := pgx.Connect(ctx, env.DB.OwnerDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(ctx)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := owner.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	count := func(q string, args ...any) (n int) {
		t.Helper()
		if err := owner.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	exec("UPDATE sms_provider SET retention_days = 1 WHERE id = 1")
	type msg struct {
		id       string
		tenant   string
		provider int64
		age      time.Duration
		receipts int
		kept     bool
	}
	msgs := []msg{
		{"10000000-0000-4000-8000-000000000001", TenantA, 1, 48 * time.Hour, 2, false},
		{"10000000-0000-4000-8000-000000000002", TenantA, 1, 25 * time.Hour, 1, false},
		{"10000000-0000-4000-8000-000000000003", TenantA, 1, 24*time.Hour + time.Second, 0, false},
		{"10000000-0000-4000-8000-000000000004", TenantA, 1, 24*time.Hour - time.Minute, 1, true}, // inside the window
		{"10000000-0000-4000-8000-000000000005", TenantA, 2, 400 * 24 * time.Hour, 1, true},       // retention 0: forever
		{"10000000-0000-4000-8000-000000000006", TenantB, 3, 10 * 24 * time.Hour, 1, true},        // tenant B, 30 days
		{"10000000-0000-4000-8000-000000000007", TenantB, 3, 40 * 24 * time.Hour, 2, false},
	}
	for _, m := range msgs {
		at := now.Add(-m.age)
		exec(`INSERT INTO sms_message (id, tenant_id, actor_kind, platform_actor, recipient, provider_id, message, create_time, update_time)
			VALUES ($1, $2, 'platform', 'retention-test', '359888000500', $3, 'x', $4, $4)`, m.id, m.tenant, m.provider, at)
		for s := range m.receipts {
			exec(`INSERT INTO sms_dlr (tenant_id, message_id, message_status, create_time, update_time) VALUES ($1, $2, $3, $4, $4)`, m.tenant, m.id, s, at)
		}
	}
	h := housekeeper.New(housekeeper.Config{Store: env.Repo, Batch: 2, Now: func() time.Time { return now }})
	n, outcome := h.Sweep(ctx)
	if n != 4 || outcome != "ok" {
		t.Fatalf("first sweep deleted %d (%s), want 4", n, outcome)
	}
	if n, outcome = h.Sweep(ctx); n != 0 || outcome != "ok" {
		t.Fatalf("second sweep deleted %d (%s), want 0", n, outcome)
	}
	for _, m := range msgs {
		_, err := env.Repo.GetMessage(ctx, repo.TenantView(m.tenant), m.id)
		receipts := count("SELECT count(*) FROM sms_dlr WHERE message_id = $1", m.id)
		switch {
		case m.kept && (err != nil || receipts != m.receipts):
			t.Errorf("%s must be kept with %d receipts: %v, %d", m.id, m.receipts, err, receipts)
		case !m.kept && (!errors.Is(err, repo.ErrNotFound) || receipts != 0):
			t.Errorf("%s must be deleted with its receipts: %v, %d", m.id, err, receipts)
		}
	}
	// The seeded messages (recent, tenant A provider 1 and tenant B) remain.
	if c := count("SELECT count(*) FROM sms_message WHERE id::text LIKE '00000000-%'"); c != 3 {
		t.Fatalf("seeded messages: %d", c)
	}
}
