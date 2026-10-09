package authz

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	authv1 "github.com/go-tangra/go-tangra-auth/sdk/v4/api/proto/auth/v1"
	"github.com/go-tangra/go-tangra-auth/sdk/v4/pkg/authclient"
	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	issuer  = "https://auth.example.org"
	tenantA = "0b2f6a1e-4c55-4c8e-9d1a-000000000a0a"
	tenantB = "0b2f6a1e-4c55-4c8e-9d1a-000000000b0b"
	user    = "4f0c0a52-1111-4222-8333-944455556666"
)

type keys struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func newKeys(t *testing.T) keys {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return keys{pub, priv}
}

func (k keys) token(t *testing.T, tenant string) string {
	t.Helper()
	now := time.Now()
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, authclient.Claims{
		RegisteredClaims: jwt.RegisteredClaims{Issuer: issuer, Subject: user, ID: "jti-1", IssuedAt: jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(5 * time.Minute))},
		TenantID: tenant, SessionID: "sess-1", Roles: []string{"member"},
	})
	tok.Header["kid"] = "k1"
	s, err := tok.SignedString(k.priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func verifier(t *testing.T, k keys, revs authclient.RevocationSource) *authclient.Verifier {
	t.Helper()
	v := authclient.New(authclient.Config{Issuer: issuer}, authclient.StaticKeys{"k1": k.pub}, revs)
	if err := v.RefreshKeys(context.Background()); err != nil {
		t.Fatal(err)
	}
	return v
}

type perms map[string]bool

func (p perms) Has(_ context.Context, tenant, u, permission string) (bool, error) {
	return p[tenant+"|"+u+"|"+permission], nil
}

type failing struct{}

func (failing) Has(context.Context, string, string, string) (bool, error) {
	return false, errors.New("auth down")
}

type neverSynced struct{}

func (neverSynced) Since(context.Context, string) ([]authclient.Revocation, string, error) {
	return nil, "", errors.New("unreachable")
}

func serve(a *Authz, header http.Header, permissions ...string) (*httptest.ResponseRecorder, Operator) {
	var got Operator
	h := a.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = FromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}), permissions...)
	r := httptest.NewRequest(http.MethodGet, "/api/sms-gw/v1/providers", nil)
	for k, v := range header {
		r.Header[k] = v
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w, got
}

func bearer(tok string) http.Header { return http.Header{"Authorization": {"Bearer " + tok}} }

func TestRequire(t *testing.T) {
	k := newKeys(t)
	other := newKeys(t)
	allowed := perms{tenantA + "|" + user + "|" + ProvidersRead: true}
	a := New(verifier(t, k, nil), allowed)
	hermes := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"sub": "1", "authority": "API_CLIENT", "kind": "access", "iss": "sms-gw",
		"exp": time.Now().Add(time.Hour).Unix(), "tid": tenantA})
	hermesTok, _ := hermes.SignedString([]byte("capture-only-jwt-secret-0123456789abcdef"))

	for name, tc := range map[string]struct {
		a      *Authz
		header http.Header
		perms  []string
		code   int
		reason string
	}{
		"allowed":                {a, bearer(k.token(t, tenantA)), []string{ProvidersRead}, 204, ""},
		"missing permission":     {a, bearer(k.token(t, tenantA)), []string{ProvidersManage}, 403, "forbidden"},
		"all permissions needed": {a, bearer(k.token(t, tenantA)), []string{ProvidersRead, ProvidersManage}, 403, "forbidden"},
		"no permission declared": {a, bearer(k.token(t, tenantA)), nil, 403, "forbidden"},
		"other tenant token":     {a, bearer(k.token(t, tenantB)), []string{ProvidersRead}, 403, "forbidden"},
		"no token":               {a, http.Header{}, []string{ProvidersRead}, 401, "unauthenticated"},
		"basic scheme":           {a, http.Header{"Authorization": {"Basic dTpw"}}, []string{ProvidersRead}, 401, "unauthenticated"},
		"foreign signing key":    {a, bearer(other.token(t, tenantA)), []string{ProvidersRead}, 401, "unauthenticated"},
		"hermes token":           {a, bearer(hermesTok), []string{ProvidersRead}, 401, "unauthenticated"},
		"non-uuid tenant":        {a, bearer(k.token(t, "tenant-a")), []string{ProvidersRead}, 401, "unauthenticated"},
		"forged legacy tenant":   {a, http.Header{"Authorization": {"Bearer " + k.token(t, tenantA)}, "X-Md-Global-Tenant-Id": {tenantB}}, []string{ProvidersRead}, 403, "forbidden"},
		"forged legacy roles":    {a, http.Header{"X-Md-Global-Roles": {"platform:admin"}}, []string{ProvidersRead}, 403, "forbidden"},
		"forged tenant header":   {a, http.Header{"Authorization": {"Bearer " + k.token(t, tenantA)}, "X-Tenant-Id": {tenantA}}, []string{ProvidersRead}, 403, "forbidden"},
		"auth decision outage":   {New(verifier(t, k, nil), failing{}), bearer(k.token(t, tenantA)), []string{ProvidersRead}, 503, "temporarily_unavailable"},
		"revocation feed stale":  {New(verifier(t, k, neverSynced{}), allowed), bearer(k.token(t, tenantA)), []string{ProvidersRead}, 503, "temporarily_unavailable"},
		"no verifier":            {New(nil, allowed), bearer(k.token(t, tenantA)), []string{ProvidersRead}, 503, "temporarily_unavailable"},
		"no checker":             {New(verifier(t, k, nil), nil), bearer(k.token(t, tenantA)), []string{ProvidersRead}, 503, "temporarily_unavailable"},
		"nil authz fails closed": {nil, bearer(k.token(t, tenantA)), []string{ProvidersRead}, 503, "temporarily_unavailable"},
	} {
		t.Run(name, func(t *testing.T) {
			w, op := serve(tc.a, tc.header, tc.perms...)
			if w.Code != tc.code {
				t.Fatalf("code %d body %s", w.Code, w.Body)
			}
			if tc.code == 204 {
				if op.TenantID != tenantA || op.UserID != user || op.SessionID != "sess-1" {
					t.Fatalf("operator %+v", op)
				}
				return
			}
			if !strings.Contains(w.Body.String(), `"reason":"`+tc.reason+`"`) || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("body %s", w.Body)
			}
			if (tc.code == 401) != (w.Header().Get("WWW-Authenticate") != "") {
				t.Fatal("WWW-Authenticate only on 401")
			}
		})
	}
}

type authzClient struct {
	authv1.AuthorizationClient
	res *authv1.CheckResponse
	err error
	req *authv1.CheckRequest
}

func (a *authzClient) Check(_ context.Context, r *authv1.CheckRequest, _ ...grpc.CallOption) (*authv1.CheckResponse, error) {
	a.req = r
	return a.res, a.err
}

func TestAuthCheckerOutageIsNotADecision(t *testing.T) {
	for _, tc := range []struct {
		name    string
		client  *authzClient
		allowed bool
		err     bool
	}{
		{"allowed", &authzClient{res: &authv1.CheckResponse{Allowed: true}}, true, false},
		{"denied", &authzClient{res: &authv1.CheckResponse{}}, false, false},
		{"permission denied", &authzClient{err: status.Error(codes.PermissionDenied, "no")}, false, false},
		{"not found", &authzClient{err: status.Error(codes.NotFound, "no user")}, false, false},
		{"unavailable", &authzClient{err: status.Error(codes.Unavailable, "down")}, false, true},
		{"timeout", &authzClient{err: status.Error(codes.DeadlineExceeded, "slow")}, false, true},
		{"internal", &authzClient{err: errors.New("boom")}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ok, err := AuthChecker{Client: tc.client}.Has(context.Background(), tenantA, user, MessagesSend)
			if ok != tc.allowed || (err != nil) != tc.err {
				t.Fatal(ok, err)
			}
			if r := tc.client.req; r.GetTenantId() != tenantA || r.GetUserId() != user || r.GetResource() != "messages" || r.GetAction() != "send" {
				t.Fatalf("request %+v", r)
			}
		})
	}
	if ok, err := (AuthChecker{}).Has(context.Background(), tenantA, user, "malformed"); ok || err != nil {
		t.Fatal("malformed permission must be a denial")
	}
	if _, err := (AuthChecker{}).Has(context.Background(), tenantA, user, ProvidersRead); err == nil {
		t.Fatal("missing client must be unavailable")
	}
}

type counting struct {
	calls atomic.Int32
	err   error
}

func (c *counting) Has(context.Context, string, string, string) (bool, error) {
	c.calls.Add(1)
	return true, c.err
}

func TestCache(t *testing.T) {
	next := &counting{}
	c := NewCache(next, time.Minute, 2)
	now := time.Now()
	c.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if ok, err := c.Has(context.Background(), tenantA, user, ProvidersRead); !ok || err != nil {
			t.Fatal(ok, err)
		}
	}
	if next.calls.Load() != 1 {
		t.Fatalf("decision not cached: %d calls", next.calls.Load())
	}
	now = now.Add(2 * time.Minute)
	_, _ = c.Has(context.Background(), tenantA, user, ProvidersRead)
	if next.calls.Load() != 2 {
		t.Fatal("expired decision reused")
	}
	if _, _ = c.Has(context.Background(), tenantB, user, ProvidersRead); next.calls.Load() != 3 {
		t.Fatal("decision shared across tenants")
	}
	_, _ = c.Has(context.Background(), tenantA, user, ProvidersManage)
	if len(c.m) > 2 {
		t.Fatalf("cache unbounded: %d", len(c.m))
	}
	failing := &counting{err: errors.New("down")}
	fc := NewCache(failing, time.Minute, 10)
	for i := 0; i < 2; i++ {
		if _, err := fc.Has(context.Background(), tenantA, user, ProvidersRead); err == nil {
			t.Fatal("error swallowed")
		}
	}
	if failing.calls.Load() != 2 {
		t.Fatal("error cached")
	}
}

// Refusals are logged with the verifier's reason, once a minute per reason,
// and the token never reaches the log.
func TestRefusalsAreLogged(t *testing.T) {
	k := newKeys(t)
	other := newKeys(t)
	var buf strings.Builder
	now := time.Now()
	a := New(verifier(t, k, nil), perms{}).WithLogger(slog.New(slog.NewJSONHandler(&buf, nil)))
	a.now = func() time.Time { return now }

	bad := other.token(t, tenantA) // signed by an unknown key
	for i := 0; i < 3; i++ {
		if w, _ := serve(a, bearer(bad), ProvidersRead); w.Code != http.StatusUnauthorized {
			t.Fatalf("bad signature → %d", w.Code)
		}
	}
	if w, _ := serve(a, http.Header{}, ProvidersRead); w.Code != http.StatusUnauthorized {
		t.Fatalf("no token → %d", w.Code)
	}
	if w, _ := serve(a, http.Header{"X-Tenant-Id": {tenantA}}, ProvidersRead); w.Code != http.StatusForbidden {
		t.Fatalf("legacy header → %d", w.Code)
	}
	out := buf.String()
	if strings.Contains(out, bad) || strings.Contains(out, strings.Split(bad, ".")[2]) {
		t.Fatal("the token must never be logged")
	}
	if n := strings.Count(out, `"reason":"token_rejected"`); n != 1 {
		t.Fatalf("token_rejected logged %d times (throttled to 1):\n%s", n, out)
	}
	if !strings.Contains(out, "signature is invalid") {
		t.Fatalf("the verifier's reason is missing:\n%s", out)
	}
	for _, r := range []string{"no_bearer_token", "legacy_identity_header"} {
		if !strings.Contains(out, `"reason":"`+r+`"`) {
			t.Fatalf("%s not logged:\n%s", r, out)
		}
	}

	now = now.Add(refusalLogEvery)
	serve(a, bearer(bad), ProvidersRead)
	if n := strings.Count(buf.String(), `"reason":"token_rejected"`); n != 2 {
		t.Fatalf("after the throttle window token_rejected logged %d times, want 2", n)
	}
}
