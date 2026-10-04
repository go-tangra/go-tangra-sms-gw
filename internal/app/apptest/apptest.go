//go:build integration

// Package apptest runs the complete sms-gw application against a test
// database for black-box tests of its listeners. The mesh runtime uses an
// in-memory development CA and operator verification denies everyone.
package apptest

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/go-tangra/go-tangra-auth/sdk/v4/pkg/authclient"
	freya "github.com/go-tangra/go-tangra/v4"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/app"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/config"
)

// Options configure one application instance.
type Options struct {
	DSN       string // application role
	KEK       []byte
	JWTSecret string
	Configure func(*config.Config)
}

// Running is a started application.
type Running struct {
	App    *app.App
	Public string // http://host:port of the public Hermes listener
	stop   func()
}

// Stop drains the application (idempotent).
func (r *Running) Stop() { r.stop() }

type denyAll struct{}

func (denyAll) Verify(context.Context, string) (authclient.Identity, error) {
	return authclient.Identity{}, authclient.ErrUnauthenticated
}
func (denyAll) Has(context.Context, string, string, string) (bool, error) { return false, nil }

// Root is the repository root.
func Root() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}

func freePort(t testing.TB) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// Start builds and runs the application on free loopback ports.
func Start(t testing.TB, o Options) *Running {
	t.Helper()
	root := Root()
	c, err := config.Load(filepath.Join(root, "deploy", "dev.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(t.TempDir(), "jwt")
	if o.JWTSecret == "" {
		o.JWTSecret = strings.Repeat("s", 40)
	}
	if err := os.WriteFile(secret, []byte(o.JWTSecret), 0o600); err != nil {
		t.Fatal(err)
	}
	c.PublicAuth.JWTSecret = config.SecretRef{File: secret}
	c.Server.HTTPAddr, c.Server.GRPCAddr, c.Admin.Addr, c.Public.HTTPAddr = freePort(t), freePort(t), freePort(t), freePort(t)
	c.DB.DSN, c.DB.MigrateDSN = o.DSN, ""
	c.Authz.Path = filepath.Join(root, "deploy", "policy.yaml")
	if o.Configure != nil {
		o.Configure(&c)
	}
	a, err := app.Build(context.Background(), c, app.Options{KEK: o.KEK, Verifier: denyAll{}, Checker: denyAll{},
		Freya: []freya.Option{freya.WithInsecureLocalDev(), freya.WithAllowAllPolicy()}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	r := &Running{App: a, Public: "http://" + a.PublicAddr()}
	stopped := false
	r.stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("application did not drain")
		}
		a.Close()
	}
	t.Cleanup(r.stop)
	deadline := time.Now().Add(15 * time.Second)
	for {
		if conn, err := net.DialTimeout("tcp", a.PublicAddr(), time.Second); err == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("public listener not serving")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return r
}
