//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/app/apptest"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/config"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/repo"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/sealed"
)

// receipt calls a receipt route the way a carrier does, requires the source
// acknowledgement and returns how long it took.
func receipt(t *testing.T, base, path, id string, status int, to uint64, token string, ts int64) time.Duration {
	t.Helper()
	q := url.Values{"request_id": {id}, "channel": {"sms"}, "sid": {"9999"}, "message_status": {strconv.Itoa(status)},
		"to": {strconv.FormatUint(to, 10)}, "from": {"Fixture"}, "timestamp": {strconv.FormatInt(ts, 10)}, "dlr_token": {token}}
	began := time.Now()
	resp, err := http.Get(base + path + "?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(b) != `"DLR_OK"` {
		t.Fatalf("receipt answered %d %s", resp.StatusCode, b)
	}
	return time.Since(began)
}

func setCallback(t *testing.T, e *Env, tenant string, id int64, u, secret string) {
	t.Helper()
	ctx := context.Background()
	c, err := e.Repo.GetClient(ctx, tenant, id)
	if err != nil {
		t.Fatal(err)
	}
	c.CallbackURL = u
	if c.CallbackSecretSealed, err = e.Envelope.SealString(secret, sealed.CallbackAD(tenant, id)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Repo.UpdateClient(ctx, c); err != nil {
		t.Fatal(err)
	}
}

func receiptStatuses(t *testing.T, e *Env, tenant, id string) map[uint32]int {
	t.Helper()
	l, err := e.Repo.ListReceipts(context.Background(), repo.TenantView(tenant), id)
	if err != nil {
		t.Fatal(err)
	}
	out := map[uint32]int{}
	for _, d := range l.Items {
		out[d.MessageStatus] = d.PartsReceived
	}
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestDeliveryAcceptance is the US2 independent test on seeded messages:
// receipts in duplicate, out of order and with forged or foreign tokens,
// polling through Hermes, signed and unsigned pushes to a local receiver,
// a failing receiver that does not hold up ingestion or polling, and a
// restart that keeps every receipt.
func TestDeliveryAcceptance(t *testing.T) {
	e := Start(t, "")
	ctx := context.Background()
	start := func() *apptest.Running {
		return apptest.Start(t, apptest.Options{DSN: e.DB.AppDSN, KEK: e.KEK, Configure: func(c *config.Config) {
			c.Webhook.AllowHTTP, c.Webhook.AllowPrivate = true, true // loopback test receiver
		}})
	}
	run := start()
	e.SealProviders(t, run.Public)
	setCallback(t, e, TenantA, 1, e.Receiver.URL+"/ok/a", "secret-a")
	setCallback(t, e, TenantA, 2, e.Receiver.URL+"/fail/a2", "secret-a2")
	setCallback(t, e, TenantB, 6, e.Receiver.URL+"/ok/b", "")
	tokA, tokB := DLRTokens[ProviderA], DLRTokens[ProviderB]

	// Seeded message A already has one status-8 receipt.
	for _, s := range []struct {
		path, token string
		status      int
		ts          int64
	}{
		{"/dlr", tokA, 8, 1700000001},
		{"/hermes/v1/sms/dlr", tokA, 1, 1700000010},
		{"/dlr", tokA, 2, 1700000020},                // late terminal-to-terminal
		{"/dlr", "wrong-" + tokA, 16, 1700000030},    // forged
		{"/dlr", "", 16, 1700000031},                 // missing token
		{"/hermes/v1/sms/dlr", tokB, 16, 1700000032}, // another tenant's token
		{"/dlr", tokA[:16], 16, 1700000033},          // prefix of the token
	} {
		receipt(t, run.Public, s.path, MessageA, s.status, 359888000100, s.token, s.ts)
	}
	if got := receiptStatuses(t, e, TenantA, MessageA); len(got) != 3 || got[8] != 2 || got[1] != 1 || got[2] != 1 {
		t.Fatalf("message A receipts %v", got)
	}
	m, _ := e.Repo.GetMessage(ctx, repo.TenantView(TenantA), MessageA)
	if m.StatusCode != 1 || m.StatusMessage != "sms_delivered" || m.DLRTs != 1700000010 {
		t.Fatalf("message A ended %d %q %d", m.StatusCode, m.StatusMessage, m.DLRTs)
	}

	// Tenant B resolves its own provider token; tenant A's does nothing.
	receipt(t, run.Public, "/dlr", MessageB, 1, 359888000200, tokA, 1700000040)
	if got := receiptStatuses(t, e, TenantB, MessageB); len(got) != 0 {
		t.Fatalf("tenant A token changed tenant B: %v", got)
	}
	receipt(t, run.Public, "/dlr", MessageB, 1, 359888000200, tokB, 1700000041)
	if got := receiptStatuses(t, e, TenantB, MessageB); got[1] != 1 {
		t.Fatalf("tenant B receipt %v", got)
	}
	// An operator's message takes receipts but has nobody to call back.
	receipt(t, run.Public, "/dlr", MessageOperator, 1, 359888000101, tokA, 1700000042)

	// Push: one signed callback per accepted receipt of client_a, one
	// unsigned for client_b, none for the operator message.
	waitFor(t, "callbacks", func() bool {
		return len(e.Receiver.Deliveries("/ok/a")) == 3 && len(e.Receiver.Deliveries("/ok/b")) == 1
	})
	for _, d := range e.Receiver.Deliveries("/ok/a") {
		var p struct {
			MessageID string `json:"message_id"`
		}
		if !d.Signed("secret-a") || json.Unmarshal(d.Body, &p) != nil || p.MessageID != MessageA {
			t.Fatalf("client_a callback %s %v", d.Body, d.Header)
		}
	}
	if b := e.Receiver.Deliveries("/ok/b")[0]; b.Header.Get("X-Smsgw-Signature") != "" || len(e.Receiver.Deliveries("/")) != 4 {
		t.Fatal("unsigned or misrouted callback")
	}

	// Poll: client_a sees its receipts, client_b does not.
	h := hermes{t, run.Public}
	a, b := h.login("client_a"), h.login("client_b")
	if code, out := h.do("GET", "/hermes/v1/sms/dlr/"+MessageA, a, ""); code != 200 || out["total"] != float64(3) {
		t.Fatalf("poll %d %v", code, out)
	}
	if code, _ := h.do("GET", "/hermes/v1/sms/dlr/"+MessageA, b, ""); code != 404 {
		t.Fatalf("foreign poll %d", code)
	}

	// A failing receiver neither delays the acknowledgement nor polling.
	a2 := h.login("client_a2")
	code, out := h.do("POST", "/hermes/v1/sms", a2, sendBody(ProviderA, 2, 359888000777))
	if code != 200 {
		t.Fatalf("send %d %v", code, out)
	}
	id := out["data"].(map[string]any)["id"].(string)
	if took := receipt(t, run.Public, "/dlr", id, 1, 359888000777, tokA, 1700000050); took > 2*time.Second {
		t.Fatalf("receipt acknowledgement waited for the callback: %v", took)
	}
	if code, out := h.do("GET", "/hermes/v1/sms/"+id, a2, ""); code != 200 || out["data"].(map[string]any)["status"] != float64(1) {
		t.Fatalf("poll during failing push %d %v", code, out)
	}
	waitFor(t, "first failing attempt", func() bool { return len(e.Receiver.Deliveries("/fail/a2")) >= 1 })

	// Restart: receipts persist and ingestion continues on the new instance;
	// the failing callback's remaining retries are not carried over.
	run.Stop()
	run = start()
	attempts := len(e.Receiver.Deliveries("/fail/a2"))
	receipt(t, run.Public, "/dlr", MessageA, 8, 359888000100, tokA, 1700000060)
	if got := receiptStatuses(t, e, TenantA, MessageA); got[8] != 3 || got[1] != 1 {
		t.Fatalf("receipts after restart %v", got)
	}
	waitFor(t, "callback after restart", func() bool { return len(e.Receiver.Deliveries("/ok/a")) == 4 })
	time.Sleep(time.Second)
	if n := len(e.Receiver.Deliveries("/fail/a2")); n != attempts {
		t.Fatalf("callback retries survived the restart: %d -> %d", attempts, n)
	}
	h = hermes{t, run.Public}
	if code, out := h.do("GET", "/hermes/v1/sms/dlr/"+id, h.login("client_a2"), ""); code != 200 || out["total"] != float64(1) {
		t.Fatalf("poll after restart %d %v", code, out)
	}
}
