//go:build integration

package app

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-tangra/go-tangra-auth/sdk/v4/pkg/authclient"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/store/storetest"
)

type denyAll struct{}

func (denyAll) Verify(context.Context, string) (authclient.Identity, error) {
	return authclient.Identity{}, authclient.ErrUnauthenticated
}
func (denyAll) Has(context.Context, string, string, string) (bool, error) { return false, nil }

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestLifecycle(t *testing.T) {
	db := storetest.Start(t)
	c := testConfig(t, db.AppDSN)
	c.DB.MigrateDSN = db.OwnerDSN
	o := testOptions()
	o.Migrate, o.Verifier, o.Checker = true, denyAll{}, denyAll{}
	a, err := Build(context.Background(), c, o)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if a.PublicAddr() != c.Public.HTTPAddr {
		t.Fatalf("public listener on %s", a.PublicAddr())
	}
	extra, err := a.AddServer("extra", freePort(t), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("extra")) }), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.AddServer("clash", c.Public.HTTPAddr, http.NotFoundHandler(), nil); err == nil {
		t.Fatal("second listener on the public address")
	}
	var stopped atomic.Bool
	a.Go(func(ctx context.Context) { <-ctx.Done(); stopped.Store(true) })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	admin := "http://" + a.AdminAddr()
	deadline := time.Now().Add(10 * time.Second)
	for {
		code, body := get(t, admin+"/readyz")
		if code == 200 && strings.Contains(body, `"database":"ok"`) && strings.Contains(body, `"identity":"ok"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not ready: %d %s", code, body)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if code, _ := get(t, admin+"/healthz"); code != 200 {
		t.Fatal(code)
	}
	if code, body := get(t, admin+"/metrics"); code != 200 || !strings.Contains(body, "freya_up 1") {
		t.Fatal(code)
	}
	public := "http://" + a.PublicAddr()
	if code, body := get(t, public+"/health"); code != 200 || body != "{\"status\":\"ok\"}\n" {
		t.Fatal(code, body)
	}
	// Neither management, admin nor UI routes are served on the public listener.
	for _, path := range []string{"/api/sms-gw/v1/providers", "/metrics", "/readyz", "/ui/mf-manifest.json", "/"} {
		if code, _ := get(t, public+path); code != 404 {
			t.Fatalf("public %s: %d", path, code)
		}
	}
	if code, body := get(t, "http://"+extra.String()+"/"); code != 200 || body != "extra" {
		t.Fatal(code, body)
	}
	w := httptest.NewRecorder()
	a.Management.ServeHTTP(w, httptest.NewRequest("GET", "/api/sms-gw/v1/unknown", nil))
	if w.Code != 404 || !strings.Contains(w.Body.String(), "not_found") {
		t.Fatal(w.Code, w.Body)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("run did not drain")
	}
	if !stopped.Load() {
		t.Fatal("worker not stopped")
	}
	if _, err := net.DialTimeout("tcp", a.PublicAddr(), time.Second); err == nil {
		t.Fatal("public listener still open after shutdown")
	}
}

func TestReadinessReportsDatabaseOutage(t *testing.T) {
	db := storetest.Start(t)
	c := testConfig(t, db.AppDSN)
	o := testOptions()
	o.Verifier, o.Checker = denyAll{}, denyAll{}
	a, err := Build(context.Background(), c, o)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, ok := a.Ready(context.Background()); !ok {
		t.Fatal("not ready")
	}
	a.Store.Close()
	if r, ok := a.Ready(context.Background()); ok || r.Database != "unreachable" {
		t.Fatalf("outage not reported: %+v", r)
	}
}

func TestFailedBuildAfterBindReleasesListeners(t *testing.T) {
	db := storetest.Start(t)
	c := testConfig(t, db.AppDSN)
	o := testOptions()
	o.Verifier, o.Checker = denyAll{}, denyAll{}
	o.Register = func(*App) error { return errors.New("story wiring failed") }
	if _, err := Build(context.Background(), c, o); err == nil {
		t.Fatal("register failure ignored")
	}
	for _, addr := range []string{c.Server.HTTPAddr, c.Server.GRPCAddr, c.Admin.Addr, c.Public.HTTPAddr} {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatalf("failed build leaked %s: %v", addr, err)
		}
		_ = l.Close()
	}
}
