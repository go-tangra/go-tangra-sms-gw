//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	authv1 "github.com/go-tangra/go-tangra-auth/sdk/v4/api/proto/auth/v1"
	"github.com/go-tangra/go-tangra-auth/sdk/v4/pkg/authclient"
	gatewayv1 "github.com/go-tangra/go-tangra-portal/sdk/v4/api/proto/gateway/v1"
	freya "github.com/go-tangra/go-tangra/v4"
	"github.com/go-tangra/go-tangra/v4/freyatest/testutil"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/app/apptest"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/config"
	"github.com/go-tangra/go-tangra-sms-gw/v4/pkg/smsgwmanifest"
)

// registry records what the module registered with the platform: the last
// gateway manifest and backend, and the last auth registration.
type registry struct {
	mu          sync.Mutex
	register    *gatewayv1.RegisterRequest
	registers   int
	deregisters int
	auth        *authv1.RegisterPermissionsRequest
}

type platformGateway struct {
	gatewayv1.UnimplementedRegistryServer
	r *registry
}

func (g platformGateway) Register(_ context.Context, req *gatewayv1.RegisterRequest) (*gatewayv1.Lease, error) {
	g.r.mu.Lock()
	defer g.r.mu.Unlock()
	g.r.register, g.r.registers = req, g.r.registers+1
	return &gatewayv1.Lease{LeaseId: "lease-" + req.GetInstanceId(), Module: req.GetManifest().GetModule(), Ttl: durationpb.New(5 * time.Second),
		RenewEvery: durationpb.New(time.Second)}, nil
}

func (g platformGateway) Renew(_ context.Context, req *gatewayv1.RenewRequest) (*gatewayv1.Lease, error) {
	return &gatewayv1.Lease{LeaseId: req.GetLeaseId(), Module: "sms-gw", Ttl: durationpb.New(5 * time.Second), RenewEvery: durationpb.New(time.Second)}, nil
}

func (g platformGateway) Deregister(context.Context, *gatewayv1.DeregisterRequest) (*gatewayv1.DeregisterResponse, error) {
	g.r.mu.Lock()
	defer g.r.mu.Unlock()
	g.r.deregisters++
	return &gatewayv1.DeregisterResponse{}, nil
}

type platformAuth struct {
	authv1.UnimplementedAuthorizationServer
	r *registry
}

func (a platformAuth) RegisterPermissions(_ context.Context, req *authv1.RegisterPermissionsRequest) (*authv1.RegisterPermissionsResponse, error) {
	a.r.mu.Lock()
	defer a.r.mu.Unlock()
	a.r.auth = req
	return &authv1.RegisterPermissionsResponse{}, nil
}

// operator is a signed-in portal user: a module role or a built-in role in
// one tenant.
type operator struct {
	tenant, user, role string
	builtin            bool
}

var platformOperators = map[string]operator{
	"owner-a":   {TenantA, "a0000000-0000-4000-8000-000000000001", "owner", true},
	"sender-a":  {TenantA, "a0000000-0000-4000-8000-000000000002", "sender", false},
	"viewer-a":  {TenantA, "a0000000-0000-4000-8000-000000000003", "viewer", false},
	"monitor-a": {TenantA, "a0000000-0000-4000-8000-000000000004", "monitoring", false},
	"member-a":  {TenantA, "a0000000-0000-4000-8000-000000000005", "member", true},
	"admin-b":   {TenantB, "b0000000-0000-4000-8000-000000000001", "administrator", false},
}

// grants resolves operator permissions from what the module registered with
// auth, the way auth does: a module role, or a built-in role grant.
type grants struct{ r *registry }

func (g grants) Verify(_ context.Context, token string) (authclient.Identity, error) {
	o, ok := platformOperators[token]
	if !ok {
		return authclient.Identity{}, authclient.ErrUnauthenticated
	}
	return authclient.Identity{UserID: o.user, TenantID: o.tenant, SessionID: "s-" + token}, nil
}

func (g grants) Has(_ context.Context, tenant, user, perm string) (bool, error) {
	g.r.mu.Lock()
	reg := g.r.auth
	g.r.mu.Unlock()
	if reg == nil {
		return false, fmt.Errorf("auth has no registration for sms-gw")
	}
	for _, o := range platformOperators {
		if o.user != user || o.tenant != tenant {
			continue
		}
		var perms []string
		if o.builtin {
			for _, b := range reg.GetBuiltinGrants() {
				if b.GetRole() == o.role {
					perms = b.GetPermissions()
				}
			}
		} else {
			for _, r := range reg.GetRoles() {
				if r.GetSlug() == o.role {
					perms = r.GetPermissions()
				}
			}
		}
		return slices.Contains(perms, perm), nil
	}
	return false, nil
}

// mesh calls the management API the way the gateway does: over mTLS with
// the gateway's SVID, forwarding the operator's bearer token.
type mesh struct {
	t      *testing.T
	client *http.Client
	base   string
}

type reply struct {
	code int
	body string
}

func (r reply) json(t *testing.T) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(r.body), &v); err != nil {
		t.Fatalf("%d %s: %v", r.code, r.body, err)
	}
	return v
}

func (m mesh) do(method, path, token string, body any, hdr ...string) reply {
	m.t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, m.base+"/api/sms-gw/v1"+path, rd)
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := m.client.Do(req)
	if err != nil {
		m.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return reply{resp.StatusCode, string(b)}
}

func (m mesh) expect(r reply, code int, reason string) {
	m.t.Helper()
	if r.code != code || (reason != "" && !strings.Contains(r.body, `"reason":"`+reason+`"`)) {
		m.t.Fatalf("got %d %s, want %d %s", r.code, r.body, code, reason)
	}
}

// TestPlatformAcceptance is the end-to-end acceptance of the module on a
// V4 platform: the complete application with a real mesh identity
// registers with a gateway and auth (fakes speaking the real gRPC
// contracts, as no platform runs in CI; docs/validation.md records the same
// flow through the real portal in the local freya-stack), and every
// management call travels over mTLS as the gateway with the operator's
// token. Operator permissions are resolved from exactly what the module
// registered with auth. Covers registration (FR-009), portal management of
// every area with secret redaction (FR-010, FR-012, SC-004), the role
// matrix including the separate monitoring role and tenant isolation
// (FR-011, SC-002), 100 public sends through the mock carrier (FR-001–005,
// SC-006), 100 aggregated receipts and the signed client callback (FR-006–
// 008, SC-003), and restart with deregistration and re-registration
// (FR-013, SC-006).
func TestPlatformAcceptance(t *testing.T) {
	e := Start(t, "")
	ca := testutil.MustCA("example.org")
	reg := &registry{}
	gwAddr, authAddr := freeAddr(t), freeAddr(t)
	service(t, ca, "gateway", gwAddr, func(a *freya.App) { gatewayv1.RegisterRegistryServer(a.GRPC(), platformGateway{r: reg}) })
	service(t, ca, "auth", authAddr, func(a *freya.App) { authv1.RegisterAuthorizationServer(a.GRPC(), platformAuth{r: reg}) })
	start := func() *apptest.Running {
		return apptest.Start(t, apptest.Options{DSN: e.DB.AppDSN, KEK: e.KEK, Register: true, Verifier: grants{reg}, Checker: grants{reg},
			Freya: []freya.Option{freya.WithIdentityProvider(testutil.NewMemProvider(ca, ca.MustIssue("sms-gw", testutil.IssueOptions{}))), freya.WithAllowAllPolicy()},
			Configure: func(c *config.Config) {
				c.Discovery.Static = map[string][]string{"gateway": {gwAddr}, "auth": {authAddr}}
				c.RateLimits.SendPerMinute, c.RateLimits.SendBurst = 1e6, 1000
				c.RateLimits.LoginPerMinute, c.RateLimits.LoginBurst = 1e6, 1000
				c.RateLimits.DLRPerMinute, c.RateLimits.DLRBurst = 1e6, 1000
				c.Webhook.AllowHTTP, c.Webhook.AllowPrivate = true, true // loopback test receiver
			}})
	}
	run := start()
	gwCert := ca.MustIssue("gateway", testutil.IssueOptions{})
	ep, err := run.App.Freya.HTTP().Endpoint()
	if err != nil {
		t.Fatal(err)
	}
	m := mesh{t: t, base: "https://" + ep.Host, client: &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: 64, TLSClientConfig: &tls.Config{
		Certificates: []tls.Certificate{gwCert}, MinVersion: tls.VersionTLS12, InsecureSkipVerify: true, // #nosec G402 -- chain verified below
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			leaf, err := x509.ParseCertificate(raw[0])
			if err != nil {
				return err
			}
			_, err = leaf.Verify(x509.VerifyOptions{Roots: ca.Pool(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}})
			return err
		}}}}}

	// --- Registration (FR-009) ---------------------------------------------
	eventually(t, "gateway and auth registration", 20*time.Second, func() bool {
		reg.mu.Lock()
		defer reg.mu.Unlock()
		return reg.register != nil && reg.auth != nil
	})
	reg.mu.Lock()
	man, backend, authReg := reg.register.GetManifest(), reg.register.GetBackend(), reg.auth
	reg.mu.Unlock()
	want, err := smsgwmanifest.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	if man.GetModule() != "sms-gw" || !slices.Equal(man.GetPrefixes(), []string{"/api/sms-gw"}) || len(man.GetRoutes()) != len(want.Routes) ||
		len(man.GetNav()) != 6 || backend.GetHttpUrl() != m.base {
		t.Fatalf("registered manifest %v backend %v", man, backend)
	}
	for _, r := range man.GetRoutes() {
		if r.GetPublic() || r.GetPermission() == "" || !strings.HasPrefix(r.GetPath(), "/api/sms-gw/v1/") {
			t.Fatalf("route %v", r)
		}
	}
	for _, n := range man.GetNav() {
		if n.GetPath() == "/sms-gw/dashboard" && n.GetRequires() != "dashboard:read" {
			t.Fatalf("dashboard nav %v", n)
		}
	}
	roles := map[string][]string{}
	for _, r := range authReg.GetRoles() {
		roles[r.GetSlug()] = r.GetPermissions()
	}
	all := smsgwmanifest.PermissionRefs()
	if authReg.GetModule() != "sms-gw" || !authReg.GetDeclaresRoles() || len(authReg.GetPermissions()) != len(all) || len(roles) != 4 ||
		!slices.Equal(roles["administrator"], all) || !slices.Equal(roles["monitoring"], []string{"dashboard:read"}) ||
		slices.Contains(roles["viewer"], "dashboard:read") || slices.Contains(roles["sender"], "dashboard:read") {
		t.Fatalf("auth registration %v", authReg)
	}
	builtins := map[string]bool{}
	for _, b := range authReg.GetBuiltinGrants() {
		builtins[b.GetRole()] = slices.Equal(b.GetPermissions(), all)
	}
	if len(builtins) != 2 || !builtins["owner"] || !builtins["admin"] {
		t.Fatalf("built-in grants %v", authReg.GetBuiltinGrants())
	}
	// Every registered route refuses a mesh call without an operator token.
	params := strings.NewReplacer("{provider_id}", "1", "{template_id}", "1", "{client_id}", "1", "{block_id}", "1", "{message_id}", MessageA)
	for _, r := range man.GetRoutes() {
		req, _ := http.NewRequest(r.GetMethod(), m.base+params.Replace(r.GetPath()), nil)
		resp, err := m.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s %s without a token: %d", r.GetMethod(), r.GetPath(), resp.StatusCode)
		}
	}

	// --- Portal administration (FR-010, FR-012, SC-004) ---------------------
	dlrToken := strings.Repeat("d", 32)
	r := m.do("POST", "/providers", "owner-a", map[string]any{"name": "platform-carrier", "type": "voicecom", "config": map[string]string{
		"url": e.Carrier.URL + "/multichannel-api/sendmulti/", "sid": "9999", "encoding": "utf-8", "token": CarrierToken,
		"dlr_token": dlrToken, "callback_url": run.Public + "/dlr?dlr_token=" + dlrToken}})
	m.expect(r, 201, "")
	provider := strconv.Itoa(int(r.json(t)["id"].(float64)))
	r = m.do("POST", "/templates", "owner-a", map[string]any{"name": "platform-otp", "fragments": map[string]string{"body": "Your code is {{.code}}"}})
	m.expect(r, 201, "")
	template := strconv.Itoa(int(r.json(t)["id"].(float64)))
	r = m.do("POST", "/templates/"+template+"/preview", "sender-a", map[string]any{"properties": map[string]string{"code": "1234"}})
	m.expect(r, 200, "")
	if v := r.json(t); v["text"] != "Your code is 1234" || v["parts"] != float64(1) {
		t.Fatalf("preview %v", v)
	}
	const cbSecret = "platform-callback-secret-0123456789"
	r = m.do("POST", "/api-clients", "owner-a", map[string]any{"username": "platform_app", "callback_url": e.Receiver.URL + "/ok/platform", "callback_secret": cbSecret})
	m.expect(r, 201, "")
	password := r.json(t)["password"].(string)
	clientID := strconv.Itoa(int(r.json(t)["client"].(map[string]any)["id"].(float64)))
	r = m.do("POST", "/blocks", "owner-a", map[string]any{"recipient": "359888999999", "description": "complaint"})
	m.expect(r, 201, "")
	for _, p := range []string{"/providers/" + provider, "/providers", "/api-clients/" + clientID, "/api-clients", "/templates", "/blocks", "/provider-types"} {
		r := m.do("GET", p, "owner-a", nil)
		m.expect(r, 200, "")
		for _, secret := range []string{CarrierToken, dlrToken, cbSecret, password, "$2a$"} {
			if strings.Contains(r.body, secret) {
				t.Fatalf("GET %s discloses a secret", p)
			}
		}
	}

	// --- Public contract: 100 sends through the mock carrier (FR-001–005, SC-006)
	h := hermes{t, run.Public}
	login := func() string { // the generated password works on the public edge
		t.Helper()
		code, out := h.do("POST", "/hermes/v1/login", "", `{"username":"platform_app","password":"`+password+`"}`)
		if code != 200 {
			t.Fatalf("login: %d %v", code, out)
		}
		return out["access_token"].(string)
	}
	tok := login()
	before := len(e.Carrier.Submissions())
	ids := make([]string, 100)
	var wg sync.WaitGroup
	errs := make(chan string, 100)
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := fmt.Sprintf(`{"to":%d,"providerId":%s,"templateId":%s,"properties":{"code":"%04d"},"sms":{"from":"Platform"}}`, 359888300000+i, provider, template, i)
			code, out := h.do("POST", "/hermes/v1/sms", tok, body)
			if code != 200 {
				errs <- fmt.Sprintf("send %d: %d %v", i, code, out)
				return
			}
			ids[i] = out["data"].(map[string]any)["id"].(string)
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
	subs := e.Carrier.Submissions()[before:]
	seen := map[string]bool{}
	for _, s := range subs {
		seen[s.RequestID] = true
	}
	if len(subs) != 100 || len(seen) != 100 {
		t.Fatalf("carrier received %d submissions for %d messages", len(subs), len(seen))
	}
	for i, id := range ids {
		if !seen[id] {
			t.Fatalf("message %d (%s) never reached the carrier", i, id)
		}
	}
	for _, s := range subs {
		if !strings.HasPrefix(s.Sms.Text, "Your code is ") || s.Token != CarrierToken {
			t.Fatalf("submission %+v", s)
		}
	}
	// A blocked recipient is refused before persistence and the carrier.
	if code, _ := h.do("POST", "/hermes/v1/sms", tok, `{"to":359888999999,"providerId":`+provider+`,"templateId":`+template+`,"properties":{"code":"1"},"sms":{"from":"Platform"}}`); code == 200 {
		t.Fatal("blocked recipient accepted")
	}
	if n := len(e.Carrier.Submissions()); n != before+100 {
		t.Fatalf("blocked send reached the carrier: %d", n-before)
	}
	if v := m.do("GET", "/messages?api_client_username=platform_app&page_size=200", "viewer-a", nil).json(t); v["total"] != float64(100) {
		t.Fatalf("stored %v messages", v["total"])
	}

	// --- Receipts: 100 concurrent duplicates aggregate (FR-006–008, SC-003)
	first := ids[0]
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if code, body := e.Carrier.SendDLR(t, first, 1, 1700001000); code != 200 || body != `"DLR_OK"` {
				t.Errorf("receipt %d %s", code, body)
			}
		}()
	}
	wg.Wait()
	r = m.do("GET", "/messages/"+first+"/dlrs", "sender-a", nil)
	m.expect(r, 200, "")
	items := r.json(t)["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["parts_received"] != float64(100) || items[0].(map[string]any)["status"] != float64(1) {
		t.Fatalf("receipts %s", r.body)
	}
	// A late intermediate receipt keeps the terminal outcome.
	e.Carrier.SendDLR(t, first, 8, 1700001001)
	if v := m.do("GET", "/messages/"+first, "viewer-a", nil).json(t); v["status_code"] != float64(1) {
		t.Fatalf("terminal outcome replaced: %v", v)
	}
	if code, out := h.do("GET", "/hermes/v1/sms/dlr/"+first, tok, ""); code != 200 || out["total"] == float64(0) {
		t.Fatalf("poll %d %v", code, out)
	}
	waitFor(t, "signed client callback", func() bool { return len(e.Receiver.Deliveries("/ok/platform")) > 0 })
	for _, d := range e.Receiver.Deliveries("/ok/platform") {
		if !d.Signed(cbSecret) || !strings.Contains(string(d.Body), first) {
			t.Fatalf("callback %s", d.Body)
		}
	}

	// --- Role matrix and tenant isolation (FR-009, FR-011, SC-002) ---------
	send := map[string]any{"provider_id": json.Number(provider), "template_id": json.Number(template), "to": "359888400001", "properties": map[string]string{"code": "9"}}
	m.expect(m.do("POST", "/messages", "viewer-a", send), 403, "forbidden")
	m.expect(m.do("POST", "/messages", "monitor-a", send), 403, "forbidden")
	r = m.do("POST", "/messages", "sender-a", send)
	m.expect(r, 201, "")
	if a := r.json(t)["message"].(map[string]any)["actor"].(map[string]any); a["kind"] != "platform" {
		t.Fatalf("manual send actor %v", a)
	}
	for _, c := range []struct {
		method, path, token string
		body                any
		code                int
	}{
		{"POST", "/providers", "viewer-a", map[string]any{"name": "x", "type": "voicecom", "config": map[string]string{}}, 403},
		{"POST", "/providers", "sender-a", map[string]any{"name": "x", "type": "voicecom", "config": map[string]string{}}, 403},
		{"GET", "/api-clients", "viewer-a", nil, 403},
		{"POST", "/dashboard/instant", "viewer-a", map[string]any{"window": "1h"}, 403},
		{"POST", "/dashboard/instant", "sender-a", map[string]any{"window": "1h"}, 403},
		{"POST", "/dashboard/instant", "member-a", map[string]any{"window": "1h"}, 403},
		{"GET", "/messages", "member-a", nil, 403},
		{"GET", "/messages", "monitor-a", nil, 403},
		{"GET", "/providers", "monitor-a", nil, 403},
		{"POST", "/dashboard/instant", "monitor-a", map[string]any{"window": "1h"}, 200},
		{"POST", "/dashboard/range", "owner-a", map[string]any{"window": "6h"}, 200},
		{"GET", "/messages/" + first, "admin-b", nil, 404},
		{"GET", "/providers/" + provider, "admin-b", nil, 404},
		{"GET", "/api-clients/" + clientID, "admin-b", nil, 404},
		{"POST", "/messages", "admin-b", send, 400}, // tenant A's provider does not resolve in tenant B
		{"GET", "/providers", "unknown-token", nil, 401},
	} {
		if got := m.do(c.method, c.path, c.token, c.body); got.code != c.code {
			t.Errorf("%s %s as %s: %d %s, want %d", c.method, c.path, c.token, got.code, got.body, c.code)
		}
	}
	if v := m.do("GET", "/messages?api_client_username=platform_app", "admin-b", nil).json(t); v["total"] != float64(0) {
		t.Fatalf("tenant B sees tenant A messages: %v", v)
	}
	// Legacy identity headers never widen access.
	m.expect(m.do("GET", "/providers", "viewer-a", nil, "X-Tenant-Id", TenantB), 403, "forbidden")
	m.expect(m.do("POST", "/dashboard/instant", "viewer-a", map[string]any{"window": "1h"}, "X-Roles", "platform:admin"), 403, "forbidden")
	// The dashboard is available-or-explained, never an error.
	if v := m.do("POST", "/dashboard/instant", "monitor-a", map[string]any{"window": "1h"}).json(t); v["available"] != false {
		t.Fatalf("dashboard %v", v)
	}

	// --- Restart: lease released, re-registered, data and credentials kept (SC-006)
	reg.mu.Lock()
	registers, deregisters := reg.registers, reg.deregisters
	reg.mu.Unlock()
	run.Stop()
	reg.mu.Lock()
	if reg.deregisters <= deregisters {
		t.Fatal("shutdown kept the gateway lease")
	}
	reg.mu.Unlock()
	run = start()
	eventually(t, "re-registration after restart", 20*time.Second, func() bool {
		reg.mu.Lock()
		defer reg.mu.Unlock()
		return reg.registers > registers
	})
	ep, err = run.App.Freya.HTTP().Endpoint()
	if err != nil {
		t.Fatal(err)
	}
	m.base = "https://" + ep.Host
	h = hermes{t, run.Public}
	tok = login()
	if code, out := h.do("GET", "/hermes/v1/sms/"+ids[99], tok, ""); code != 200 || out["data"].(map[string]any)["id"] != ids[99] {
		t.Fatalf("message after restart %d %v", code, out)
	}
	if v := m.do("GET", "/messages/"+first+"/dlrs", "viewer-a", nil).json(t); len(v["items"].([]any)) != 2 {
		t.Fatalf("receipts after restart %v", v)
	}
}
