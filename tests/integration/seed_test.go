//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/repo"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/sealed"
)

func TestSeedIsTenantScoped(t *testing.T) {
	e := Start(t, "http://127.0.0.1:1")
	ctx := context.Background()
	for tenant, want := range map[string][4]int{TenantA: {5, 2, 3, 2}, TenantB: {1, 1, 1, 1}} {
		clients, _ := e.Repo.ListClients(ctx, tenant, repo.Page{})
		providers, _ := e.Repo.ListProviders(ctx, tenant, repo.Page{})
		templates, _ := e.Repo.ListTemplates(ctx, tenant, repo.Page{})
		messages, err := e.Repo.ListMessages(ctx, repo.TenantView(tenant), repo.MessageFilter{}, repo.Page{})
		if err != nil {
			t.Fatal(err)
		}
		if got := [4]int{clients.Total, providers.Total, templates.Total, messages.Total}; got != want {
			t.Fatalf("tenant %s: %v want %v", tenant, got, want)
		}
	}
	c, err := e.Repo.ClientByUsername(ctx, "client_b")
	if err != nil || c.TenantID != TenantB || bcrypt.CompareHashAndPassword([]byte(c.PasswordHash), []byte(Password)) != nil {
		t.Fatalf("seeded login %+v %v", c, err)
	}
	if _, err := e.Repo.GetMessage(ctx, repo.ClientView(TenantA, 1), MessageOperator); !errors.Is(err, repo.ErrNotFound) {
		t.Fatal("operator message visible to a Hermes client")
	}
	if l, err := e.Repo.ListReceipts(ctx, repo.ClientView(TenantA, 1), MessageA); err != nil || l.Total != 1 || l.Items[0].MessageStatus != 8 {
		t.Fatalf("seeded receipt %+v %v", l, err)
	}
	for recipient, provider := range map[string]int64{"359888000301": ProviderA, "359888000302": ProviderAOff} {
		if ok, err := e.Repo.IsBlocked(ctx, TenantA, provider, recipient); err != nil || !ok {
			t.Fatalf("block %s not enforced", recipient)
		}
	}
	if ok, _ := e.Repo.IsBlocked(ctx, TenantA, ProviderA, "359888000303"); ok {
		t.Fatal("disabled block enforced")
	}
	if ok, _ := e.Repo.IsBlocked(ctx, TenantB, ProviderB, "359888000302"); ok {
		t.Fatal("tenant A block applied to tenant B")
	}
	// Sequences continue after the explicit seed ids.
	n, err := e.Repo.CreateClient(ctx, repo.APIClient{TenantID: TenantB, Username: "fresh_b", PasswordHash: "h", Authority: "API_CLIENT", Status: repo.On})
	if err != nil || n.ID != 7 {
		t.Fatalf("sequence %d %v", n.ID, err)
	}
	// Seeded provider configurations are sealed to their tenant and row.
	p, _ := e.Repo.GetProvider(ctx, TenantA, ProviderA)
	cfg, err := e.Envelope.OpenConfig(p.ConfigSealed, sealed.ProviderAD(TenantA, ProviderA))
	if err != nil || cfg["dlr_token"] != DLRTokens[ProviderA] || cfg["token"] != CarrierToken {
		t.Fatalf("sealed config %v %v", cfg, err)
	}
	if bytes.Contains(p.ConfigSealed, []byte(CarrierToken)) || p.ConfigPublic["token"] != nil {
		t.Fatal("carrier token stored in clear")
	}
	if _, err := e.Envelope.OpenConfig(p.ConfigSealed, sealed.ProviderAD(TenantB, ProviderA)); err == nil {
		t.Fatal("sealed config opens for another tenant")
	}
}

func TestMockCarrierAndReceiver(t *testing.T) {
	var gotQuery string
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`"DLR_OK"`))
	}))
	defer gw.Close()
	e := Start(t, gw.URL)
	submit := func(id string) *http.Response {
		body, _ := json.Marshal(map[string]any{"sid": 9999, "request_id": id, "to": 359888000100, "send_order": []string{"sms"},
			"callback_url": e.ProviderConfig(ProviderA, gw.URL)["callback_url"], "sms": map[string]any{"text": "ping", "from": "Fixture"}})
		resp, err := http.Post(e.Carrier.URL+"/multichannel-api/sendmulti/", "application/json", bytes.NewReader(body))
		if err != nil {
			return nil
		}
		return resp
	}
	for mode, want := range map[string]int{Accept: 200, Reject: 200, HTTP500: 500} {
		e.Carrier.SetMode(mode)
		if resp := submit(repo.NewID()); resp == nil || resp.StatusCode != want {
			t.Fatalf("%s: %v", mode, resp)
		}
	}
	e.Carrier.SetMode(Drop)
	if resp := submit(repo.NewID()); resp != nil {
		t.Fatal("dropped submission answered")
	}
	e.Carrier.SetMode(Accept)
	id := repo.NewID()
	submit(id)
	if code, body := e.Carrier.SendDLR(t, id, 1, 1700000000); code != 200 || body != `"DLR_OK"` {
		t.Fatal(code, body)
	}
	for _, want := range []string{"dlr_token=" + DLRTokens[ProviderA], "message_status=1", "request_id=" + id, "to=359888000100", "timestamp=1700000000"} {
		if !strings.Contains(gotQuery, want) {
			t.Fatalf("receipt query %s lacks %s", gotQuery, want)
		}
	}
	// The receiver verifies the legacy signed callback golden.
	raw, err := os.ReadFile("../fixtures/legacy/callback.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden struct{ Body, Timestamp, Signature, Secret string }
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	if !Signed(golden.Timestamp, golden.Signature, []byte(golden.Body), golden.Secret) || Signed(golden.Timestamp, golden.Signature, []byte(golden.Body+" "), golden.Secret) {
		t.Fatal("signature check disagrees with the legacy golden")
	}
	for _, path := range []string{"/ok/a", "/fail/f", "/redirect/r"} {
		req, _ := http.NewRequest(http.MethodPost, e.Receiver.URL+path, strings.NewReader(golden.Body))
		req.Header.Set("X-SmsGw-Timestamp", golden.Timestamp)
		req.Header.Set("X-SmsGw-Signature", golden.Signature)
		resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if d := e.Receiver.Deliveries("/ok/a"); len(d) != 1 || !d[0].Signed(golden.Secret) {
		t.Fatal("receiver did not record a verifiable delivery")
	}
	if len(e.Receiver.Deliveries("/ok/redirected")) != 0 {
		t.Fatal("redirect followed")
	}
}
