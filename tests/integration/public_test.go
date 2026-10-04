//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/app/apptest"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/config"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/repo"
)

type hermes struct {
	t    *testing.T
	base string
}

func (h hermes) do(method, path, token, body string) (int, map[string]any) {
	h.t.Helper()
	req, _ := http.NewRequest(method, h.base+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func (h hermes) login(user string) string {
	h.t.Helper()
	code, out := h.do("POST", "/hermes/v1/login", "", `{"username":"`+user+`","password":"`+Password+`"}`)
	if code != 200 {
		h.t.Fatalf("login %s: %d %v", user, code, out)
	}
	return out["access_token"].(string)
}

func sendBody(provider, template int64, to uint64) string {
	return `{"to":` + strconv.FormatUint(to, 10) + `,"providerId":` + strconv.FormatInt(provider, 10) + `,"templateId":` + strconv.FormatInt(template, 10) +
		`,"sms":{"from":"Fixture"}}`
}

// TestPublicAcceptance is the US1 independent test: two tenants, two client
// owners and a viewer; 100 accepted sends reach the mock carrier exactly
// once each; blocked and foreign-reference sends leave no record and make
// no carrier call; no read crosses an owner or tenant.
func TestPublicAcceptance(t *testing.T) {
	e := Start(t, "")
	run := apptest.Start(t, apptest.Options{DSN: e.DB.AppDSN, KEK: e.KEK, Configure: func(c *config.Config) {
		c.RateLimits.SendPerMinute, c.RateLimits.SendBurst = 1e6, 1000
		c.RateLimits.LoginPerMinute, c.RateLimits.LoginBurst = 1e6, 1000
	}})
	e.SealProviders(t, run.Public)
	h := hermes{t, run.Public}
	ctx := context.Background()
	a, a2, viewer, admin, b := h.login("client_a"), h.login("client_a2"), h.login("viewer_a"), h.login("admin_a"), h.login("client_b")
	if code, out := h.do("POST", "/hermes/v1/login", "", `{"username":"disabled_a","password":"`+Password+`"}`); code != 401 || out["message"] != "account disabled" {
		t.Fatalf("disabled login %d %v", code, out)
	}

	// 100 concurrent accepted sends.
	var wg sync.WaitGroup
	ids := make([]string, 100)
	codes := make([]int, 100)
	for i := range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, out := h.do("POST", "/hermes/v1/sms", a, sendBody(ProviderA, 2, uint64(359888100000+i)))
			codes[i] = code
			if data, ok := out["data"].(map[string]any); ok {
				ids[i], _ = data["id"].(string)
			}
		}()
	}
	wg.Wait()
	seen := map[string]bool{}
	for i, id := range ids {
		if codes[i] != 200 || id == "" || seen[id] {
			t.Fatalf("send %d: %d %q", i, codes[i], id)
		}
		seen[id] = true
	}
	subs := e.Carrier.Submissions()
	if len(subs) != 100 {
		t.Fatalf("carrier received %d submissions", len(subs))
	}
	for _, s := range subs {
		if !seen[s.RequestID] {
			t.Fatalf("carrier request id %s is not a stored message", s.RequestID)
		}
		delete(seen, s.RequestID)
	}
	l, err := e.Repo.ListMessages(ctx, repo.ClientView(TenantA, 1), repo.MessageFilter{Recipient: "3598881"}, repo.Page{Size: 500})
	if err != nil || l.Total != 100 {
		t.Fatalf("stored %d %v", l.Total, err)
	}
	for _, m := range l.Items {
		if m.StatusCode != 0 || m.StatusMessage != "sms_provider_accepted" || m.Actor != repo.ClientActor(1) {
			t.Fatalf("message %+v", m)
		}
	}
	full, err := e.Repo.GetMessage(ctx, repo.TenantView(TenantA), l.Items[0].ID)
	if err != nil || len(full.RawRequest) == 0 || strings.Contains(string(full.RawRequest), CarrierToken) || strings.Contains(string(full.RawRequest), DLRTokens[ProviderA]) {
		t.Fatalf("stored evidence carries credentials or is missing: %v", err)
	}

	// Rejected sends: nothing stored, nothing submitted.
	before, _ := e.Repo.ListMessages(ctx, repo.TenantView(TenantA), repo.MessageFilter{}, repo.Page{})
	beforeB, _ := e.Repo.ListMessages(ctx, repo.TenantView(TenantB), repo.MessageFilter{}, repo.Page{})
	for _, c := range []struct {
		token, body string
		code        int
		message     string
	}{
		{a, sendBody(ProviderA, 2, 359888000301), 400, "recipient 359888000301 is blocked for provider mock-a"},
		{a, sendBody(ProviderA, 2, 359888000302), 400, "recipient 359888000302 is blocked for provider mock-a"},
		{a, sendBody(ProviderAOff, 2, 359888000400), 400, "provider mock-a-off is inactive"},
		{a, sendBody(ProviderA, 3, 359888000400), 400, "template off is inactive"},
		{viewer, sendBody(ProviderA, 2, 359888000400), 403, "sending SMS requires API_CLIENT authority"},
		{admin, sendBody(ProviderA, 2, 359888000400), 403, "sending SMS requires API_CLIENT authority"},
		// Tenant B cannot use tenant A's provider or template, nor learn they exist.
		{b, sendBody(ProviderA, 2, 359888000400), 404, "provider not found"},
		{b, sendBody(ProviderB, 1, 359888000400), 404, "template not found"},
		{a, sendBody(ProviderB, 2, 359888000400), 404, "provider not found"},
	} {
		if code, out := h.do("POST", "/hermes/v1/sms", c.token, c.body); code != c.code || out["message"] != c.message {
			t.Fatalf("%s: %d %v", c.body, code, out)
		}
	}
	after, _ := e.Repo.ListMessages(ctx, repo.TenantView(TenantA), repo.MessageFilter{}, repo.Page{})
	afterB, _ := e.Repo.ListMessages(ctx, repo.TenantView(TenantB), repo.MessageFilter{}, repo.Page{})
	if after.Total != before.Total || afterB.Total != beforeB.Total || len(e.Carrier.Submissions()) != 100 {
		t.Fatal("rejected send stored a message or reached the carrier")
	}
	// Tenant B's own provider and template work for tenant B.
	if code, out := h.do("POST", "/hermes/v1/sms", b, sendBody(ProviderB, 4, 359888000401)); code != 200 || out["data"].(map[string]any)["text"] != "Hi <no value>" {
		t.Fatalf("tenant B send %d %v", code, out)
	}

	// Reads stay with the owner.
	for _, c := range []struct {
		token, path string
		code        int
	}{
		{a, "/hermes/v1/sms/" + MessageA, 200},
		{a, "/hermes/v1/sms/dlr/" + MessageA, 200},
		{a2, "/hermes/v1/sms/" + MessageA, 404},
		{a2, "/hermes/v1/sms/dlr/" + MessageA, 404},
		{viewer, "/hermes/v1/sms/" + MessageA, 404},
		{admin, "/hermes/v1/sms/" + MessageA, 404},
		{b, "/hermes/v1/sms/" + MessageA, 404},
		{b, "/v1/sms/dlr/" + MessageA, 404},
		{a, "/hermes/v1/sms/" + MessageB, 404},
		{a, "/hermes/v1/sms/" + MessageOperator, 404},
		{b, "/hermes/v1/sms/" + MessageB, 200},
	} {
		if code, out := h.do("GET", c.path, c.token, ""); code != c.code {
			t.Fatalf("%s: %d %v", c.path, code, out)
		}
	}
	if _, out := h.do("GET", "/hermes/v1/sms/dlr/"+MessageA, a, ""); out["total"] != float64(1) {
		t.Fatalf("receipts %v", out)
	}
	for _, c := range []struct {
		token string
		total float64
	}{{a, 101}, {a2, 0}, {viewer, 0}, {admin, 0}, {b, 2}} {
		if code, out := h.do("GET", "/hermes/v1/sms?pageSize=1", c.token, ""); code != 200 || out["total"] != c.total || len(out["items"].([]any)) > 1 {
			t.Fatalf("list total %v, want %v", out["total"], c.total)
		}
	}
}

// TestPublicPageCap: a requested page above query.max_page_size returns at
// most the cap (documented security change: unbounded pageSize).
func TestPublicPageCap(t *testing.T) {
	e := Start(t, "")
	run := apptest.Start(t, apptest.Options{DSN: e.DB.AppDSN, KEK: e.KEK, Configure: func(c *config.Config) {
		c.Query.DefaultPageSize, c.Query.MaxPageSize = 2, 3
		c.RateLimits.SendPerMinute, c.RateLimits.SendBurst = 1e6, 1000
	}})
	e.SealProviders(t, run.Public)
	h := hermes{t, run.Public}
	a := h.login("client_a")
	for i := range 5 {
		if code, _ := h.do("POST", "/hermes/v1/sms", a, sendBody(ProviderA, 2, uint64(359888200000+i))); code != 200 {
			t.Fatal(code)
		}
	}
	for path, want := range map[string]int{"/hermes/v1/sms": 2, "/hermes/v1/sms?pageSize=100000": 3, "/hermes/v1/sms?pageSize=3&page=2": 3} {
		if _, out := h.do("GET", path, a, ""); len(out["items"].([]any)) != want || out["total"] != float64(6) {
			t.Fatalf("%s: %d items of %v", path, len(out["items"].([]any)), out["total"])
		}
	}
}
