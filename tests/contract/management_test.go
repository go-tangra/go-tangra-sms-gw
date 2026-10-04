//go:build integration

package contract

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/go-tangra/go-tangra-auth/sdk/v4/pkg/authclient"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/app/apptest"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/authz"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/config"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/store/storetest"
	"github.com/go-tangra/go-tangra-sms-gw/v4/pkg/smsgwmanifest"
)

const (
	mgmtTenantA = "6b1d2a9e-3f00-4c1a-9d2e-0000000000aa"
	mgmtTenantB = "6b1d2a9e-3f00-4c1a-9d2e-0000000000bb"
	carrierTok  = "management-carrier-token-0123456789"
	cbSecret    = "management-callback-secret-0123456789"
)

// operators maps bearer tokens to verified identities and module roles.
var operators = map[string]struct {
	tenant, user string
	perms        []string
}{
	"admin-a":   {mgmtTenantA, "11111111-0000-4000-8000-00000000000a", smsgwmanifest.PermissionRefs()},
	"sender-a":  {mgmtTenantA, "22222222-0000-4000-8000-00000000000a", rolePerms("sender")},
	"viewer-a":  {mgmtTenantA, "33333333-0000-4000-8000-00000000000a", rolePerms("viewer")},
	"monitor-a": {mgmtTenantA, "44444444-0000-4000-8000-00000000000a", rolePerms("monitoring")},
	"admin-b":   {mgmtTenantB, "11111111-0000-4000-8000-00000000000b", smsgwmanifest.PermissionRefs()},
	"flaky-a":   {mgmtTenantA, "flaky", smsgwmanifest.PermissionRefs()},
}

func rolePerms(slug string) []string {
	for _, r := range smsgwmanifest.Roles {
		if r.Slug == slug {
			return r.Permissions
		}
	}
	panic("no module role " + slug)
}

type fakeAuth struct{}

func (fakeAuth) Verify(_ context.Context, token string) (authclient.Identity, error) {
	if token == "stale" {
		return authclient.Identity{}, authclient.ErrStale
	}
	o, ok := operators[token]
	if !ok {
		return authclient.Identity{}, authclient.ErrUnauthenticated
	}
	return authclient.Identity{UserID: o.user, TenantID: o.tenant, SessionID: "s-" + token}, nil
}

func (fakeAuth) Has(_ context.Context, tenant, user, perm string) (bool, error) {
	if user == "flaky" {
		return false, errors.New("auth unavailable")
	}
	for _, o := range operators {
		if o.user == user && o.tenant == tenant {
			for _, p := range o.perms {
				if p == perm {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

var _ authz.Checker = fakeAuth{}

type mgmt struct {
	t       *testing.T
	run     *apptest.Running
	carrier *httptest.Server
	mu      sync.Mutex
	sent    []string
}

func startManagement(t *testing.T) *mgmt {
	db := storetest.Start(t)
	m := &mgmt{t: t}
	m.carrier = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		m.sent = append(m.sent, string(b))
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"return_code":0,"return_message":"Message accepted","channels":{"sms":{"send_order":1,"message_parts":1}}}`))
	}))
	t.Cleanup(m.carrier.Close)
	m.run = apptest.Start(t, apptest.Options{DSN: db.AppDSN, KEK: make([]byte, 32), Verifier: fakeAuth{}, Checker: fakeAuth{},
		Configure: func(c *config.Config) { c.Recipients.MinDigits = 7 }})
	return m
}

type resp struct {
	code int
	body string
	hdr  http.Header
}

func (r resp) json(t *testing.T) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(r.body), &v); err != nil {
		t.Fatalf("%d %s: %v", r.code, r.body, err)
	}
	return v
}

func (m *mgmt) do(method, path, token string, body any, hdr ...string) resp {
	m.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		raw, _ := json.Marshal(b)
		rd = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, "/api/sms-gw/v1"+path, rd)
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	m.run.App.Management.ServeHTTP(w, req)
	return resp{w.Code, w.Body.String(), w.Header()}
}

func (m *mgmt) expect(r resp, code int, reason string) {
	m.t.Helper()
	if r.code != code || (reason != "" && !strings.Contains(r.body, `"reason":"`+reason+`"`)) {
		m.t.Fatalf("got %d %s, want %d %s", r.code, r.body, code, reason)
	}
}

func id(t *testing.T, v map[string]any) string {
	t.Helper()
	n, ok := v["id"].(float64)
	if !ok {
		t.Fatalf("no id in %v", v)
	}
	return strconv.FormatInt(int64(n), 10)
}

func (m *mgmt) createProvider(token, name string) map[string]any {
	m.t.Helper()
	r := m.do("POST", "/providers", token, map[string]any{"name": name, "type": "voicecom", "config": map[string]string{
		"url": m.carrier.URL + "/send", "sid": "9999", "encoding": "utf-8", "callback_url": "https://edge.example.org/dlr", "token": carrierTok}})
	m.expect(r, 201, "")
	return r.json(m.t)
}

// TestManagementRolesTenantsAndSecrets is the US3 independent test against
// the complete application: administrator, sender and viewer of one tenant
// and an administrator of another, with secret redaction throughout.
func TestManagementRolesTenantsAndSecrets(t *testing.T) {
	m := startManagement(t)
	noSecret := func(r resp, extra ...string) {
		t.Helper()
		for _, s := range append([]string{carrierTok, cbSecret, "password_hash", "$2a$", "$2b$", "sealed"}, extra...) {
			if strings.Contains(r.body, s) {
				t.Fatalf("response discloses %q: %s", s, r.body)
			}
		}
		if r.hdr.Get("Cache-Control") != "no-store" {
			t.Fatalf("cacheable response: %v", r.hdr)
		}
	}

	// Providers: credentials are write-only; a blank dlr_token is generated once.
	pa := m.createProvider("admin-a", "carrier-a")
	gen, _ := pa["generated_secrets"].(map[string]any)
	dlrToken, _ := gen["dlr_token"].(string)
	if len(dlrToken) != 32 || pa["config"].(map[string]any)["token"] != "__set__" || pa["config"].(map[string]any)["dlr_token"] != "__set__" {
		t.Fatalf("%v", pa)
	}
	paID := id(t, pa)
	for _, tok := range []string{"admin-a", "sender-a", "viewer-a"} {
		r := m.do("GET", "/providers/"+paID, tok, nil)
		m.expect(r, 200, "")
		noSecret(r, dlrToken)
	}
	r := m.do("GET", "/providers?sort=name&order=desc&page=9&page_size=10", "viewer-a", nil)
	m.expect(r, 200, "")
	if v := r.json(t); v["total"] != float64(1) || v["page"] != float64(1) || v["sort"] != "name" || v["order"] != "desc" {
		t.Fatalf("%v", v)
	}
	noSecret(r, dlrToken)
	m.expect(m.do("GET", "/providers?sort=config", "admin-a", nil), 400, "validation_failed")
	m.expect(m.do("POST", "/providers", "viewer-a", map[string]any{"name": "x", "type": "voicecom", "config": map[string]string{}}), 403, "forbidden")
	m.expect(m.do("POST", "/providers", "sender-a", map[string]any{"name": "x", "type": "voicecom", "config": map[string]string{}}), 403, "forbidden")
	m.expect(m.do("POST", "/providers", "admin-a", map[string]any{"name": "x", "type": "nope", "config": map[string]string{}}), 400, "validation_failed")
	m.expect(m.do("POST", "/providers", "admin-a", map[string]any{"name": "x", "type": "voicecom", "tenant_id": mgmtTenantB, "config": map[string]string{}}), 400, "validation_failed")
	m.expect(m.do("POST", "/providers", "admin-a", map[string]any{"name": "x", "type": "voicecom", "config": map[string]string{"sid": "1", "url": "https://c", "encoding": "utf-8", "evil": "1"}}), 400, "validation_failed")
	m.expect(m.do("POST", "/providers", "admin-a", map[string]any{"name": "carrier-a", "type": "voicecom", "config": map[string]string{"sid": "1", "url": "https://c", "encoding": "utf-8"}}), 409, "conflict")
	// Sending the redacted view back keeps the credentials; "" clears one.
	shown := m.do("GET", "/providers/"+paID, "admin-a", nil).json(t)["config"].(map[string]any)
	shown["sid"] = "7777"
	r = m.do("PATCH", "/providers/"+paID, "admin-a", map[string]any{"config": shown})
	m.expect(r, 200, "")
	if c := r.json(t)["config"].(map[string]any); c["sid"] != "7777" || c["token"] != "__set__" || c["dlr_token"] != "__set__" {
		t.Fatalf("%v", c)
	}
	pb := m.createProvider("admin-b", "carrier-b")
	pbID := id(t, pb)
	for _, c := range []struct{ method, path string }{{"GET", "/providers/" + paID}, {"PATCH", "/providers/" + paID}, {"DELETE", "/providers/" + paID}} {
		var body any
		if c.method == "PATCH" {
			body = map[string]any{"enabled": false}
		}
		m.expect(m.do(c.method, c.path, "admin-b", body), 404, "not_found")
	}
	if v := m.do("GET", "/providers", "admin-b", nil).json(t); v["total"] != float64(1) {
		t.Fatalf("tenant B sees %v", v)
	}
	if v := m.do("GET", "/provider-types", "sender-a", nil).json(t); len(v["items"].([]any)) == 0 {
		t.Fatal("no provider types")
	}

	// Templates and preview.
	m.expect(m.do("POST", "/templates", "admin-a", map[string]any{"name": "bad", "fragments": map[string]string{"body": "{{.x"}}), 400, "validation_failed")
	m.expect(m.do("POST", "/templates", "admin-a", map[string]any{"name": "nobody", "fragments": map[string]string{"subject": "x"}}), 400, "validation_failed")
	ta := m.do("POST", "/templates", "admin-a", map[string]any{"name": "otp", "fragments": map[string]string{"body": "Code {{.code}} for {{.name}}"}})
	m.expect(ta, 201, "")
	taID := id(t, ta.json(t))
	if v := ta.json(t)["variables"].([]any); len(v) != 2 {
		t.Fatalf("%v", v)
	}
	m.expect(m.do("POST", "/templates", "sender-a", map[string]any{"name": "x", "fragments": map[string]string{"body": "x"}}), 403, "forbidden")
	r = m.do("POST", "/templates/"+taID+"/preview", "sender-a", map[string]any{"properties": map[string]string{"code": "1234"}, "encoding": "gsm-03-38"})
	m.expect(r, 200, "")
	if v := r.json(t); v["text"] != "Code 1234 for <no value>" || v["parts"] != float64(1) || v["limit"] != float64(160) || len(v["missing"].([]any)) != 1 {
		t.Fatalf("%v", v)
	}
	m.expect(m.do("POST", "/templates/"+taID+"/preview", "admin-b", map[string]any{}), 404, "not_found")
	tb := m.do("POST", "/templates", "admin-b", map[string]any{"name": "otp", "fragments": map[string]string{"body": "B {{.code}}"}})
	m.expect(tb, 201, "")
	tbID := id(t, tb.json(t))

	// API clients: generated password once, never a hash or secret; the
	// account works on the public listener.
	m.expect(m.do("GET", "/api-clients", "viewer-a", nil), 403, "forbidden")
	m.expect(m.do("POST", "/api-clients", "admin-a", map[string]any{"username": "acme_app", "callback_url": "ftp://cb.example.org"}), 400, "validation_failed")
	m.expect(m.do("POST", "/api-clients", "admin-a", map[string]any{"username": "acme_app", "callback_url": "https://u:p@cb.example.org/x"}), 400, "validation_failed")
	m.expect(m.do("POST", "/api-clients", "admin-a", map[string]any{"username": "acme_app", "authority": "API_ADMIN"}), 400, "validation_failed")
	r = m.do("POST", "/api-clients", "admin-a", map[string]any{"username": "acme_app", "callback_url": "https://cb.example.org/dlr", "callback_secret": cbSecret})
	m.expect(r, 201, "")
	noSecret(r)
	created := r.json(t)
	pw, _ := created["password"].(string)
	client := created["client"].(map[string]any)
	if len(pw) != 32 || client["callback_secret_set"] != true || client["authority"] != "API_CLIENT" {
		t.Fatalf("%v", created)
	}
	clID := id(t, client)
	m.expect(m.do("POST", "/api-clients", "admin-b", map[string]any{"username": "acme_app"}), 409, "conflict")
	login := func(password string) int {
		res, err := http.Post(m.run.Public+"/hermes/v1/login", "application/json", strings.NewReader(`{"username":"acme_app","password":"`+password+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if code := login(pw); code != 200 {
		t.Fatalf("login with generated password: %d", code)
	}
	r = m.do("GET", "/api-clients/"+clID, "admin-a", nil)
	m.expect(r, 200, "")
	noSecret(r, pw)
	m.expect(m.do("PATCH", "/api-clients/"+clID, "admin-a", map[string]any{"username": "renamed"}), 400, "validation_failed")
	r = m.do("PATCH", "/api-clients/"+clID, "admin-a", map[string]any{"callback_secret": "__set__", "email": "ops@example.org"})
	m.expect(r, 200, "")
	if v := r.json(t); v["callback_secret_set"] != true || v["email"] != "ops@example.org" {
		t.Fatalf("%v", v)
	}
	if v := m.do("PATCH", "/api-clients/"+clID, "admin-a", map[string]any{"callback_secret": ""}).json(t); v["callback_secret_set"] != false {
		t.Fatalf("%v", v)
	}
	m.expect(m.do("POST", "/api-clients/"+clID+"/reset-password", "admin-b", nil), 404, "not_found")
	r = m.do("POST", "/api-clients/"+clID+"/reset-password", "admin-a", nil)
	m.expect(r, 200, "")
	pw2, _ := r.json(t)["password"].(string)
	if pw2 == "" || pw2 == pw || login(pw) != 401 || login(pw2) != 200 {
		t.Fatal("password reset did not replace the password")
	}
	r = m.do("POST", "/api-clients/"+clID+"/reset-password", "admin-a", map[string]any{"password": "chosen-password"})
	m.expect(r, 200, "")
	if r.body != "{}\n" || login("chosen-password") != 200 {
		t.Fatalf("supplied password: %s", r.body)
	}

	// Blocks reference only the tenant's providers.
	m.expect(m.do("POST", "/blocks", "admin-a", map[string]any{"recipient": "359888000001", "provider_id": json.Number(pbID)}), 400, "validation_failed")
	m.expect(m.do("POST", "/blocks", "viewer-a", map[string]any{"recipient": "359888000001"}), 403, "forbidden")
	r = m.do("POST", "/blocks", "admin-a", map[string]any{"recipient": "359888000001", "provider_id": json.Number(paID), "description": "complaint"})
	m.expect(r, 201, "")
	blk := r.json(t)
	if blk["created_by"] != operators["admin-a"].user || blk["provider_id"] != float64(mustInt(paID)) {
		t.Fatalf("%v", blk)
	}
	r = m.do("PATCH", "/blocks/"+id(t, blk), "admin-a", map[string]any{"provider_id": nil})
	m.expect(r, 200, "")
	if r.json(t)["provider_id"] != nil {
		t.Fatal("provider_id null did not widen the block")
	}
	m.expect(m.do("GET", "/blocks/"+id(t, blk), "admin-b", nil), 404, "not_found")

	// Manual send: same pipeline, separate platform actor, tenant-scoped reads.
	send := func(token, provider, template, to string) resp {
		return m.do("POST", "/messages", token, map[string]any{"provider_id": json.Number(provider), "template_id": json.Number(template), "to": to,
			"properties": map[string]string{"code": "4242", "name": "Ann"}})
	}
	m.expect(send("viewer-a", paID, taID, "359888000002"), 403, "forbidden")
	m.expect(send("sender-a", paID, taID, "359888000001"), 400, "send_rejected")
	m.expect(send("sender-a", pbID, taID, "359888000002"), 400, "send_rejected")
	m.expect(send("sender-a", paID, tbID, "359888000002"), 400, "send_rejected")
	r = send("sender-a", paID, taID, "359888000002")
	m.expect(r, 201, "")
	sent := r.json(t)
	msg := sent["message"].(map[string]any)
	actor := msg["actor"].(map[string]any)
	if actor["kind"] != "platform" || actor["user_id"] != operators["sender-a"].user || msg["text"] != "Code 4242 for Ann" || msg["status_code"] != float64(0) {
		t.Fatalf("%v", sent)
	}
	m.mu.Lock()
	if len(m.sent) != 1 {
		t.Fatalf("carrier saw %d submissions", len(m.sent))
	}
	m.mu.Unlock()
	msgID := msg["id"].(string)
	r = m.do("GET", "/messages/"+msgID, "viewer-a", nil)
	m.expect(r, 200, "")
	noSecret(r, dlrToken)
	if !strings.Contains(r.body, `"evidence"`) {
		t.Fatalf("no evidence: %s", r.body)
	}
	m.expect(m.do("GET", "/messages/"+msgID, "admin-b", nil), 404, "not_found")
	m.expect(m.do("GET", "/messages/"+msgID+"/dlrs", "admin-b", nil), 404, "not_found")
	if v := m.do("GET", "/messages/"+msgID+"/dlrs", "sender-a", nil).json(t); v["total"] != float64(0) {
		t.Fatalf("%v", v)
	}
	if v := m.do("GET", "/messages?recipient=35988800&status=0", "viewer-a", nil).json(t); v["total"] != float64(1) {
		t.Fatalf("%v", v)
	}
	if v := m.do("GET", "/messages?api_client_username=acme_app", "viewer-a", nil).json(t); v["total"] != float64(0) {
		t.Fatalf("%v", v)
	}
	if v := m.do("GET", "/messages", "admin-b", nil).json(t); v["total"] != float64(0) {
		t.Fatalf("tenant B sees %v", v)
	}
	m.expect(m.do("GET", "/messages?recipient=abc", "viewer-a", nil), 400, "validation_failed")

	// References: in-use records are refused, deletion is tenant-scoped.
	m.expect(m.do("DELETE", "/providers/"+paID, "admin-a", nil), 409, "in_use")
	m.expect(m.do("DELETE", "/templates/"+taID, "admin-a", nil), 409, "in_use")
	m.expect(m.do("DELETE", "/blocks/"+id(t, blk), "admin-b", nil), 404, "not_found")
	m.expect(m.do("DELETE", "/blocks/"+id(t, blk), "admin-a", nil), 204, "")
	m.expect(m.do("DELETE", "/api-clients/"+clID, "admin-a", nil), 204, "")
	if code := login("chosen-password"); code != 401 {
		t.Fatalf("deleted client logged in: %d", code)
	}

	// The dashboard (deployment-wide totals) needs the monitoring role;
	// without monitoring configured it reports itself unavailable.
	r = m.do("POST", "/dashboard/instant", "monitor-a", map[string]any{"window": "1h"})
	m.expect(r, 200, "")
	if v := r.json(t); v["available"] != false || v["reason"] != "not_configured" {
		t.Fatalf("%v", v)
	}
	m.expect(m.do("POST", "/dashboard/instant", "admin-a", map[string]any{"window": "1h"}), 200, "")
	m.expect(m.do("POST", "/dashboard/range", "sender-a", map[string]any{"window": "1h"}), 403, "forbidden")
	m.expect(m.do("POST", "/dashboard/instant", "viewer-a", map[string]any{"window": "1h"}), 403, "forbidden")
	m.expect(m.do("POST", "/dashboard/range", "viewer-a", map[string]any{"window": "1h"}), 403, "forbidden")
	m.expect(m.do("POST", "/dashboard/range", "monitor-a", map[string]any{"window": "30d"}), 400, "validation_failed")
	m.expect(m.do("POST", "/dashboard/instant", "monitor-a", map[string]any{"window": "1h", "queries": []string{"up"}}), 400, "validation_failed")
	// Monitoring grants nothing else.
	for _, p := range []string{"/providers", "/templates", "/api-clients", "/blocks", "/messages"} {
		m.expect(m.do("GET", p, "monitor-a", nil), 403, "forbidden")
	}
}

func mustInt(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

// TestManagementRefusals covers authentication, forged identity headers,
// verification outages and the listener separation.
func TestManagementRefusals(t *testing.T) {
	m := startManagement(t)
	m.expect(m.do("GET", "/providers", "", nil), 401, "unauthenticated")
	m.expect(m.do("GET", "/providers", "nobody", nil), 401, "unauthenticated")
	for _, h := range [][]string{{"X-Md-Global-Tenant-Id", mgmtTenantB}, {"X-Md-Global-Roles", "platform:admin"}, {"X-Tenant-Id", mgmtTenantB},
		{"X-User-Id", "root"}, {"X-Roles", "sms:admin"}, {"X-Authority", "API_ADMIN"}} {
		m.expect(m.do("GET", "/providers", "admin-a", nil, h...), 403, "forbidden")
		m.expect(m.do("GET", "/providers", "", nil, h...), 403, "forbidden")
	}
	m.expect(m.do("GET", "/providers", "stale", nil), 503, "temporarily_unavailable")
	m.expect(m.do("GET", "/providers", "flaky-a", nil), 503, "temporarily_unavailable")
	m.expect(m.do("PUT", "/providers", "admin-a", nil), 405, "method_not_allowed")
	m.expect(m.do("GET", "/nothing", "admin-a", nil), 404, "not_found")
	m.expect(m.do("GET", "/providers/0", "admin-a", nil), 400, "validation_failed")
	m.expect(m.do("GET", "/messages/not-a-uuid", "admin-a", nil), 400, "validation_failed")
	m.expect(m.do("POST", "/templates", "admin-a", "{not json"), 400, "malformed_body")
	m.expect(m.do("POST", "/templates", "admin-a", `{"name":"big","fragments":{"body":"`+strings.Repeat("x", 70<<10)+`"}}`), 413, "body_too_large")
	// Authorization runs before validation: an unauthenticated caller learns nothing about the schema.
	m.expect(m.do("POST", "/templates", "", "{not json"), 401, "unauthenticated")
	m.expect(m.do("POST", "/templates", "viewer-a", "{not json"), 403, "forbidden")

	// A Hermes token is not a platform token.
	r := m.do("POST", "/api-clients", "admin-a", map[string]any{"username": "hermes_user", "password": "hermes-password"})
	m.expect(r, 201, "")
	res, err := http.Post(m.run.Public+"/hermes/v1/login", "application/json", strings.NewReader(`{"username":"hermes_user","password":"hermes-password"}`))
	if err != nil {
		t.Fatal(err)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.NewDecoder(res.Body).Decode(&tok)
	res.Body.Close()
	if tok.AccessToken == "" {
		t.Fatal("no Hermes token")
	}
	m.expect(m.do("GET", "/providers", tok.AccessToken, nil), 401, "unauthenticated")

	// The public listener serves no management route, and management no Hermes route.
	for _, path := range []string{"/api/sms-gw/v1/providers", "/api/sms-gw/v1/messages", "/api/sms-gw/v1/dashboard/instant"} {
		req, _ := http.NewRequest("GET", m.run.Public+path, nil)
		req.Header.Set("Authorization", "Bearer admin-a")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 404 {
			t.Fatalf("public %s: %d", path, res.StatusCode)
		}
	}
	for _, path := range []string{"/hermes/v1/login", "/hermes/v1/sms", "/dlr", "/v1/sms"} {
		w := httptest.NewRecorder()
		m.run.App.Management.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 404 {
			t.Fatalf("management %s: %d", path, w.Code)
		}
	}
}
