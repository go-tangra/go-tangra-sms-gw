package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/go-tangra/go-tangra-auth/sdk/v4/pkg/authclient"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/authz"
	"github.com/go-tangra/go-tangra-sms-gw/v4/pkg/smsgwmanifest"
)

type allowAll struct{}

func (allowAll) Verify(_ context.Context, token string) (authclient.Identity, error) {
	if token != "ok" {
		return authclient.Identity{}, authclient.ErrUnauthenticated
	}
	return authclient.Identity{UserID: "u1", TenantID: "6b1d2a9e-3f00-4c1a-9d2e-0000000000aa"}, nil
}
func (allowAll) Has(context.Context, string, string, string) (bool, error) { return true, nil }

func newServer(t *testing.T) *Server {
	t.Helper()
	s, err := New(Config{Authz: authz.New(allowAll{}, allowAll{})})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestHandlerManifestParity: every declared operation has a handler that
// enforces exactly the permission the gateway manifest registers.
func TestHandlerManifestParity(t *testing.T) {
	s := newServer(t)
	if m := s.Missing(); len(m) != 0 {
		t.Fatalf("missing handlers: %v", m)
	}
	man, err := smsgwmanifest.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	handled := s.Handled()
	if len(handled) != len(man.Routes) {
		t.Fatalf("%d handlers, %d manifest routes", len(handled), len(man.Routes))
	}
	for _, r := range man.Routes {
		if got := handled[Route{r.Method, r.Path}]; got != r.Permission {
			t.Errorf("%s %s: handler enforces %q, manifest %q", r.Method, r.Path, got, r.Permission)
		}
	}
	if err := s.handle("GET", "/api/sms-gw/v1/undeclared", authz.ProvidersRead, nil); err == nil {
		t.Fatal("undeclared route accepted")
	}
	s2 := &Server{ops: map[Route]operation{{"GET", "/x"}: {permission: authz.ProvidersRead}}, mux: http.NewServeMux(), handled: map[Route]string{}}
	if err := s2.handle("GET", "/x", authz.ProvidersManage, nil); err == nil {
		t.Fatal("permission mismatch accepted")
	}
	for _, p := range authz.AllPermissions {
		found := false
		for _, ref := range smsgwmanifest.PermissionRefs() {
			found = found || ref == p
		}
		if !found {
			t.Errorf("authz permission %s not declared", p)
		}
	}
}

// TestChainOrder: refusals happen before validation and before any store
// access (the server has no store here).
func TestChainOrder(t *testing.T) {
	s := newServer(t)
	for _, c := range []struct {
		method, path, token, body string
		code                      int
		reason                    string
	}{
		{"GET", "/api/sms-gw/v1/providers", "", "", 401, "unauthenticated"},
		{"POST", "/api/sms-gw/v1/templates", "", "{bad", 401, "unauthenticated"},
		{"POST", "/api/sms-gw/v1/templates", "ok", "{bad", 400, "malformed_body"},
		{"POST", "/api/sms-gw/v1/templates", "ok", `{"name":"x"}`, 400, "validation_failed"},
		{"GET", "/api/sms-gw/v1/providers?page_size=500", "ok", "", 400, "validation_failed"},
		{"GET", "/api/sms-gw/v1/providers/abc", "ok", "", 400, "validation_failed"},
		{"DELETE", "/api/sms-gw/v1/provider-types", "ok", "", 405, "method_not_allowed"},
		{"GET", "/api/sms-gw/v2/providers", "ok", "", 404, "not_found"},
		{"POST", "/api/sms-gw/v1/dashboard/instant", "ok", `{"window":"1h"}`, 200, ""},
	} {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		req.Header.Set("Content-Type", "application/json")
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		if w.Code != c.code || !strings.Contains(w.Body.String(), c.reason) {
			t.Errorf("%s %s: %d %s", c.method, c.path, w.Code, w.Body)
		}
		if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s %s: headers %v", c.method, c.path, w.Header())
		}
	}
}

func TestRemoteHandler(t *testing.T) {
	h := RemoteHandler(fstest.MapFS{"mf-manifest.json": {Data: []byte("{}")}, "assets/app-abcdef12.js": {Data: []byte("x")}})
	for path, want := range map[string]int{"/mf-manifest.json": 200, "/assets/app-abcdef12.js": 200, "/": 404, "/assets/": 404, "/missing.js": 404} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != want {
			t.Errorf("%s: %d", path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/assets/app-abcdef12.js", nil))
	if !strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
		t.Fatal(w.Header())
	}
}
