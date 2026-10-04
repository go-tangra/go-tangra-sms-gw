package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/audit"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/hermes"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/repo"
)

const (
	secret = "test-only-jwt-secret-0123456789abcdef"
	tenant = "0b2f6a1e-4c55-4c8e-9d1a-000000000a0a"
)

type memStore struct {
	mu        sync.Mutex
	clients   map[int64]repo.APIClient
	logins    []repo.LoginEvent
	lastLogin map[int64]string
	revoked   map[string]repo.Revocation
}

func (m *memStore) ClientByUsername(_ context.Context, u string) (repo.APIClient, error) {
	for _, c := range m.clients {
		if c.Username == u {
			return c, nil
		}
	}
	return repo.APIClient{}, repo.ErrNotFound
}

func (m *memStore) ClientByID(_ context.Context, id int64) (repo.APIClient, error) {
	if c, ok := m.clients[id]; ok {
		return c, nil
	}
	return repo.APIClient{}, repo.ErrNotFound
}

func (m *memStore) RecordLogin(_ context.Context, _ string, id int64, ip string, _ time.Time) error {
	m.lastLogin[id] = ip
	return nil
}

func (m *memStore) AddLogin(_ context.Context, e repo.LoginEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.logins = append(m.logins, e)
	return nil
}

func (m *memStore) Revoke(_ context.Context, r repo.Revocation) error {
	m.revoked[r.JTI] = r
	return nil
}

func (m *memStore) IsRevoked(_ context.Context, t, jti string, now time.Time) (bool, error) {
	r, ok := m.revoked[jti]
	return ok && r.TenantID == t && r.ExpiresAt.After(now), nil
}

type auditLog struct{ events []audit.Event }

func (a *auditLog) Record(e audit.Event) error { a.events = append(a.events, e); return e.Validate() }

func newService(t *testing.T) (*Service, *memStore, *auditLog) {
	t.Helper()
	hash, _ := bcrypt.GenerateFromPassword([]byte("Passw0rd!"), 4)
	st := &memStore{clients: map[int64]repo.APIClient{}, lastLogin: map[int64]string{}, revoked: map[string]repo.Revocation{}}
	for i, c := range []struct{ user, auth, status string }{
		{"client_a", "API_CLIENT", "ON"}, {"viewer_v", "API_VIEWER", "ON"}, {"admin_x", "API_ADMIN", "ON"}, {"disabled_d", "API_CLIENT", "OFF"},
	} {
		id := int64(i + 1)
		st.clients[id] = repo.APIClient{ID: id, TenantID: tenant, Username: c.user, PasswordHash: string(hash), Authority: c.auth, Status: c.status}
	}
	iss, err := NewIssuer(secret, 2*time.Hour, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	a := &auditLog{}
	return NewService(st, iss, a, nil, nil), st, a
}

func wantErr(t *testing.T, err error, code int, message string) {
	t.Helper()
	var e *hermes.Error
	if !errors.As(err, &e) || e.Code != code || e.Message != message {
		t.Fatalf("error %v, want %d %q", err, code, message)
	}
}

func decode(t *testing.T, tok string) (map[string]any, map[string]any) {
	t.Helper()
	parts := strings.Split(tok, ".")
	var h, c map[string]any
	for i, dst := range []*map[string]any{&h, &c} {
		b, _ := base64.RawURLEncoding.DecodeString(parts[i])
		if err := json.Unmarshal(b, dst); err != nil {
			t.Fatal(err)
		}
	}
	return h, c
}

func forge(t *testing.T, claims jwt.MapClaims, key string) string {
	t.Helper()
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(key))
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestIssuerSecretLength(t *testing.T) {
	if _, err := NewIssuer(strings.Repeat("x", 31), 0, 0); err == nil {
		t.Fatal("short secret accepted")
	}
}

func TestTokenPairClaims(t *testing.T) {
	s, _, _ := newService(t)
	tok, err := s.Login(context.Background(), "client_a", "Passw0rd!", "192.0.2.1", "ua")
	if err != nil || tok.ExpiresIn != 7200 {
		t.Fatal(tok, err)
	}
	ah, ac := decode(t, tok.Access)
	_, rc := decode(t, tok.Refresh)
	if ah["alg"] != "HS256" || ah["typ"] != "JWT" {
		t.Fatalf("header %v", ah)
	}
	for k, v := range map[string]any{"authority": "API_CLIENT", "kind": "access", "username": "client_a", "iss": "sms-gw", "sub": "1"} {
		if ac[k] != v {
			t.Fatalf("access claim %s = %v", k, ac[k])
		}
	}
	jti, _ := ac["jti"].(string)
	if rc["kind"] != "refresh" || rc["jti"] != jti || len(jti) != 32 || ac["nbf"] != ac["iat"] ||
		ac["exp"].(float64)-ac["iat"].(float64) != 7200 || rc["exp"].(float64)-rc["iat"].(float64) != 604800 {
		t.Fatalf("claims %v %v", ac, rc)
	}
}

func TestParseFailures(t *testing.T) {
	s, _, _ := newService(t)
	now := time.Now().Unix()
	base := func(over jwt.MapClaims) jwt.MapClaims {
		c := jwt.MapClaims{"authority": "API_CLIENT", "kind": "access", "username": "client_a", "sub": "1", "iss": "sms-gw", "jti": strings.Repeat("f", 32),
			"iat": now, "nbf": now, "exp": now + 7200}
		for k, v := range over {
			if v == nil {
				delete(c, k)
			} else {
				c[k] = v
			}
		}
		return c
	}
	none, _ := jwt.NewWithClaims(jwt.SigningMethodNone, base(nil)).SignedString(jwt.UnsafeAllowNoneSignatureType)
	for _, c := range []struct {
		name, header string
		code         int
		message      string
	}{
		{"missing", "", 401, "missing bearer token"},
		{"basic scheme", "Basic Zm9vOmJhcg==", 401, "invalid token"},
		{"lowercase bearer", "bearer " + forge(t, base(nil), secret), 401, "invalid token"},
		{"garbage", "Bearer not-a-jwt", 401, "invalid token"},
		{"expired", "Bearer " + forge(t, base(jwt.MapClaims{"exp": now - 100}), secret), 401, "token expired"},
		{"not yet valid", "Bearer " + forge(t, base(jwt.MapClaims{"nbf": now + 3600}), secret), 401, "token expired"},
		{"wrong secret", "Bearer " + forge(t, base(nil), "another-secret-another-secret-123"), 401, "invalid token"},
		{"alg none", "Bearer " + none, 401, "invalid token"},
		{"missing kind", "Bearer " + forge(t, base(jwt.MapClaims{"kind": nil}), secret), 401, "wrong token kind"},
		{"refresh as access", "Bearer " + forge(t, base(jwt.MapClaims{"kind": "refresh"}), secret), 401, "wrong token kind"},
		{"unknown client", "Bearer " + forge(t, base(jwt.MapClaims{"sub": "999999"}), secret), 404, "api client not found"},
		{"non-numeric subject", "Bearer " + forge(t, base(jwt.MapClaims{"sub": "abc"}), secret), 404, "api client not found"},
		{"disabled account", "Bearer " + forge(t, base(jwt.MapClaims{"sub": "4"}), secret), 401, "account disabled"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := s.Authenticate(context.Background(), c.header)
			wantErr(t, err, c.code, c.message)
		})
	}
}

func TestEffectiveAuthority(t *testing.T) {
	s, _, _ := newService(t)
	now := time.Now().Unix()
	claims := func(sub, authority string) string {
		return "Bearer " + forge(t, jwt.MapClaims{"authority": authority, "kind": "access", "sub": sub, "jti": strings.Repeat("a", 32), "exp": now + 60}, secret)
	}
	for _, c := range []struct {
		sub, authority, want string
		send, read           bool
	}{
		{"1", "API_CLIENT", "API_CLIENT", true, true},
		{"2", "API_VIEWER", "API_VIEWER", false, true},
		{"3", "API_ADMIN", "API_ADMIN", false, false}, // stored, but no public privilege
		{"1", "ROOT", "", false, false},               // unsupported claim
		{"2", "API_CLIENT", "", false, false},         // claim disagrees with the account
	} {
		caller, err := s.Authenticate(context.Background(), claims(c.sub, c.authority))
		if err != nil {
			t.Fatal(err)
		}
		_, read := caller.View()
		if caller.Authority != c.want || caller.CanSend() != c.send || read != c.read {
			t.Fatalf("%s/%s: %q send=%v read=%v", c.sub, c.authority, caller.Authority, caller.CanSend(), read)
		}
	}
}

func TestLoginOutcomesAndLog(t *testing.T) {
	s, st, _ := newService(t)
	ctx := context.Background()
	for _, c := range []struct {
		user, pass string
		code       int
		message    string
	}{
		{"client_a", "", 400, "username and password are required"},
		{"", "", 400, "username and password are required"},
		{"nobody", "x", 401, "invalid credentials"},
		{"client_a", "wrong", 401, "invalid credentials"},
		{"disabled_d", "Passw0rd!", 401, "account disabled"},
	} {
		_, err := s.Login(ctx, c.user, c.pass, "192.0.2.1", "ua")
		wantErr(t, err, c.code, c.message)
	}
	if _, err := s.Login(ctx, "client_a", "Passw0rd!", "192.0.2.9", "ua"); err != nil {
		t.Fatal(err)
	}
	want := []struct {
		client int64
		reason string
	}{{0, "missing credentials"}, {0, "missing credentials"}, {0, "unknown user"}, {1, "wrong password"}, {4, "account disabled"}, {1, ""}}
	if len(st.logins) != len(want) {
		t.Fatalf("logins %+v", st.logins)
	}
	for i, w := range want {
		e := st.logins[i]
		if e.ClientID != w.client || e.ErrorMessage != w.reason || e.Success != (w.reason == "") || (w.client == 0) != (e.TenantID == "") {
			t.Fatalf("login %d: %+v", i, e)
		}
	}
	if st.lastLogin[1] != "192.0.2.9" {
		t.Fatal("last login not recorded")
	}
}

func TestRefresh(t *testing.T) {
	s, st, _ := newService(t)
	ctx := context.Background()
	v, _ := s.Login(ctx, "viewer_v", "Passw0rd!", "", "")
	// The new pair carries the account's current authority, not the claim.
	c := st.clients[2]
	c.Authority = "API_CLIENT"
	st.clients[2] = c
	n, err := s.Refresh(ctx, v.Refresh)
	if err != nil {
		t.Fatal(err)
	}
	if _, claims := decode(t, n.Access); claims["authority"] != "API_CLIENT" {
		t.Fatalf("authority %v", claims["authority"])
	}
	// No rotation: the presented refresh token still works.
	if _, err := s.Refresh(ctx, v.Refresh); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	refresh := func(sub string) string {
		return forge(t, jwt.MapClaims{"kind": "refresh", "sub": sub, "jti": strings.Repeat("b", 32), "exp": now + 600}, secret)
	}
	_, err = s.Refresh(ctx, refresh("99999999999999999999"))
	wantErr(t, err, 401, "invalid subject")
	_, err = s.Refresh(ctx, refresh("999999"))
	wantErr(t, err, 404, "api client not found")
	_, err = s.Refresh(ctx, refresh("4"))
	wantErr(t, err, 401, "account disabled")
	_, err = s.Refresh(ctx, v.Access)
	wantErr(t, err, 401, "wrong token kind")
	_, err = s.Refresh(ctx, "")
	wantErr(t, err, 401, "missing bearer token")
}

func TestLogoutRevokesThePairPersistently(t *testing.T) {
	s, st, log := newService(t)
	ctx := context.Background()
	tok, _ := s.Login(ctx, "client_a", "Passw0rd!", "", "")
	// An expired access token cannot be parsed; the refresh token (same
	// jti) still revokes the pair.
	expired := forge(t, jwt.MapClaims{"kind": "access", "sub": "1", "jti": strings.Repeat("e", 32), "exp": time.Now().Unix() - 10}, secret)
	s.Logout(ctx, "Bearer "+expired, tok.Refresh)
	_, err := s.Authenticate(ctx, "Bearer "+tok.Access)
	wantErr(t, err, 401, "invalid token")
	_, err = s.Refresh(ctx, tok.Refresh)
	wantErr(t, err, 401, "invalid token")
	_, claims := decode(t, tok.Refresh)
	r := st.revoked[claims["jti"].(string)]
	if r.TenantID != tenant || r.ClientID != 1 || r.ExpiresAt.Unix() != int64(claims["exp"].(float64)) {
		t.Fatalf("revocation %+v", r)
	}
	if len(st.revoked) != 1 || len(log.events) != 1 || log.events[0].Action != audit.ClientLogout {
		t.Fatalf("revoked %v audit %+v", st.revoked, log.events)
	}
	// Garbage and missing tokens are ignored.
	s.Logout(ctx, "Bearer garbage", "garbage")
	s.Logout(ctx, "", "")
	if len(st.revoked) != 1 {
		t.Fatal("garbage revoked something")
	}
}

func TestAbilities(t *testing.T) {
	if len(Abilities("API_CLIENT")) != 4 || len(Abilities("API_VIEWER")) != 3 || Abilities("API_ADMIN") == nil || len(Abilities("ROOT")) != 0 {
		t.Fatal("abilities")
	}
}
