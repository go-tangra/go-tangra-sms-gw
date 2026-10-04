package app

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	freya "github.com/go-tangra/go-tangra/v4"
	fauthz "github.com/go-tangra/go-tangra/v4/authz"
	"github.com/go-tangra/go-tangra/v4/identity"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/config"
)

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// testConfig is the dev config on free loopback ports with a usable JWT
// secret file; the database DSN is the caller's.
func testConfig(t *testing.T, dsn string) config.Config {
	t.Helper()
	c, err := config.Load("../../deploy/dev.yaml")
	if err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(t.TempDir(), "jwt")
	if err := os.WriteFile(secret, []byte(strings.Repeat("s", 40)), 0o600); err != nil {
		t.Fatal(err)
	}
	c.PublicAuth.JWTSecret = config.SecretRef{File: secret}
	c.Server.HTTPAddr, c.Server.GRPCAddr, c.Admin.Addr, c.Public.HTTPAddr = freePort(t), freePort(t), freePort(t), freePort(t)
	c.DB.DSN, c.DB.MigrateDSN = dsn, ""
	c.Authz.Path = "../../deploy/policy.yaml"
	return c
}

func testOptions() Options {
	return Options{KEK: make([]byte, 32), Freya: []freya.Option{freya.WithInsecureLocalDev(), freya.WithAllowAllPolicy()}}
}

func TestUnreachableDatabaseBindsNothing(t *testing.T) {
	c := testConfig(t, "postgres://smsgw_app:x@127.0.0.1:1/sms_gw?sslmode=disable&connect_timeout=1")
	if _, err := Build(context.Background(), c, testOptions()); err == nil {
		t.Fatal("unreachable database accepted")
	}
	for _, addr := range []string{c.Server.HTTPAddr, c.Server.GRPCAddr, c.Admin.Addr} {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatalf("failed build leaked %s: %v", addr, err)
		}
		_ = l.Close()
	}
}

func TestBuildRefusesWeakSecrets(t *testing.T) {
	c := testConfig(t, "postgres://smsgw_app:x@127.0.0.1:1/sms_gw?sslmode=disable")
	short := filepath.Join(t.TempDir(), "short")
	_ = os.WriteFile(short, []byte("too-short"), 0o600)
	c.PublicAuth.JWTSecret = config.SecretRef{File: short}
	if _, err := Build(context.Background(), c, testOptions()); err == nil || !strings.Contains(err.Error(), "32 bytes") {
		t.Fatalf("short jwt secret: %v", err)
	}
	c = testConfig(t, "postgres://smsgw_app:x@127.0.0.1:1/sms_gw?sslmode=disable")
	o := testOptions()
	o.KEK = make([]byte, 16)
	if _, err := Build(context.Background(), c, o); err == nil {
		t.Fatal("short KEK accepted")
	}
}

func TestPolicyAdmitsOnlyTheGateway(t *testing.T) {
	f, err := os.Open("../../deploy/policy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	p, err := fauthz.Load(f)
	if err != nil {
		t.Fatal(err)
	}
	peer := func(s string) identity.SPIFFEID {
		id, err := identity.ParseSPIFFEID(s)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	gateway, notification := peer("spiffe://example.org/svc/gateway"), peer("spiffe://example.org/svc/notification")
	for _, op := range []string{"GET /api/sms-gw/v1/providers", "POST /api/sms-gw/v1/messages", "PATCH /api/sms-gw/v1/templates/1", "DELETE /api/sms-gw/v1/blocks/2", "GET /ui/mf-manifest.json"} {
		if !p.Authorize(context.Background(), gateway, "sms-gw", op).Allowed {
			t.Fatalf("gateway refused %s", op)
		}
		if p.Authorize(context.Background(), notification, "sms-gw", op).Allowed {
			t.Fatalf("other service allowed %s", op)
		}
	}
	if p.Authorize(context.Background(), gateway, "sms-gw", "/sms_gw.service.v1.SmsService/Send").Allowed {
		t.Fatal("legacy gRPC operation allowed")
	}
}
