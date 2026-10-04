//go:build integration

package contract

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/app/apptest"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/repo"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/sealed"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/store/storetest"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/webhook"
)

// ---------- receipt observations of the legacy replay ----------

// checkMessageState compares the receipt-driven message fields with a
// recorded legacy row.
func (r *replay) checkMessageState(t *testing.T, c, key, id string) {
	t.Helper()
	var want struct {
		StatusCode    int32           `json:"status_code"`
		StatusMessage string          `json:"status_message"`
		DLRTs         json.RawMessage `json:"dlr_ts"`
	}
	loadFixture(t, c).observed(t, key, &want)
	m, err := r.repo.GetMessage(context.Background(), repo.TenantView(tenant), id)
	if err != nil {
		t.Fatal(err)
	}
	if m.StatusCode != want.StatusCode || m.StatusMessage != want.StatusMessage || r.normalize(strconv.FormatInt(m.DLRTs, 10)) != strings.Trim(string(want.DLRTs), `"`) {
		t.Errorf("%s %s: message %d %q dlr_ts %d, legacy %d %q %s", c, key, m.StatusCode, m.StatusMessage, m.DLRTs, want.StatusCode, want.StatusMessage, want.DLRTs)
	}
}

var receiptFields = []string{"id", "channel", "sid", "status_text", "message_status", "recipient", "sender", "timestamp", "remote_address", "parts_received"}

// checkReceiptRows compares the stored receipts of a message, ids included,
// with the recorded legacy rows.
func (r *replay) checkReceiptRows(t *testing.T, c, key, id string) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(loadFixture(t, c).Observed[key]))
	dec.UseNumber()
	var want []map[string]any
	if err := dec.Decode(&want); err != nil {
		t.Fatal(err)
	}
	conn := r.owner(t)
	defer conn.Close(context.Background())
	rows, err := conn.Query(context.Background(), `SELECT id, channel, sid, status_text, message_status, recipient, sender, "timestamp", remote_address, parts_received
		FROM sms_dlr WHERE message_id = $1 ORDER BY id`, id)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			t.Fatal(err)
		}
		var parts []string
		for i, v := range vals {
			parts = append(parts, receiptFields[i]+"="+r.normalize(fmt.Sprint(v)))
		}
		got = append(got, strings.Join(parts, " "))
	}
	var legacy []string
	for _, row := range want {
		var parts []string
		for _, f := range receiptFields {
			parts = append(parts, f+"="+fmt.Sprint(row[f]))
		}
		legacy = append(legacy, strings.Join(parts, " "))
	}
	if strings.Join(got, "\n") != strings.Join(legacy, "\n") {
		t.Errorf("%s %s receipts:\n got  %s\n want %s", c, key, strings.Join(got, "\n      "), strings.Join(legacy, "\n      "))
	}
}

// checkCarrierReceipt compares the acknowledgement the mock carrier got.
func (r *replay) checkCarrierReceipt(t *testing.T, res result) {
	t.Helper()
	var want []struct {
		HTTPStatus int    `json:"http_status"`
		Body       string `json:"body"`
	}
	loadFixture(t, "send-roundtrip").observed(t, "carrier_dlr_callbacks", &want)
	if len(want) != 1 || res.status != want[0].HTTPStatus || res.body != want[0].Body {
		t.Errorf("send-roundtrip carrier receipt: %d %s, legacy %+v", res.status, res.body, want)
	}
}

func receiptQuery(id string, status int, to uint64, token string) string {
	return url.Values{"request_id": {id}, "channel": {"sms"}, "sid": {"9999"}, "message_status": {strconv.Itoa(status)}, "to": {strconv.FormatUint(to, 10)},
		"from": {"Capture"}, "timestamp": {strconv.Itoa(fixedTS)}, "dlr_token": {token}}.Encode()
}

// concurrency is dlr-100-concurrent-same-status: 100 identical receipts,
// 50 at a time, aggregate into one row with parts_received 100.
func (r *replay) concurrency(t *testing.T) {
	res := r.raw("POST", "/hermes/v1/sms", r.sendBody("manual", "static", 359888000230, nil), r.bearer("client_a"))
	id := r.dataID(t, res)
	r.name(id, "msg:concurrency")
	q := receiptQuery(id, 8, 359888000230, tManual)
	statuses := map[int]bool{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 50)
	for range 100 {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			res, err := r.do(r.base, "GET", "/dlr?"+q, "", nil)
			mu.Lock()
			defer mu.Unlock()
			if err != nil || res.body != `"DLR_OK"` {
				statuses[-1] = true
				return
			}
			statuses[res.status] = true
		}()
	}
	wg.Wait()
	var got, want []int
	for s := range statuses {
		got = append(got, s)
	}
	sort.Ints(got)
	f := loadFixture(t, "dlr-100-concurrent-same-status")
	f.observed(t, "http_statuses", &want)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("dlr-100-concurrent-same-status: statuses %v, legacy %v", got, want)
	}
	r.checkReceiptRows(t, "dlr-100-concurrent-same-status", "dlr_rows", id)
	r.checkMessageState(t, "dlr-100-concurrent-same-status", "db_row", id)
}

// webhookClients is the capture's trigger: client_b (no secret), client_f
// (failing receiver) and client_r (redirecting receiver) each send one
// message that receives a delivered receipt.
func (r *replay) webhookClients(t *testing.T) {
	for _, c := range []struct {
		user string
		to   uint64
	}{{"client_b", 359888000240}, {"client_f", 359888000241}, {"client_r", 359888000242}} {
		res := r.raw("POST", "/hermes/v1/sms", r.sendBody("manual", "static", c.to, nil), r.bearer(c.user))
		id := r.dataID(t, res)
		r.name(id, "msg:webhook-"+strconv.FormatUint(c.to, 10))
		if res := r.raw("GET", "/dlr?"+receiptQuery(id, 1, c.to, tManual), "", nil); res.status != 200 || res.body != `"DLR_OK"` {
			t.Fatalf("receipt for %s: %d %s", c.user, res.status, res.body)
		}
	}
}

// describe is the capture's summary of one received callback.
func (r *replay) describe(d delivery) map[string]any {
	names := make([]string, 0, len(d.header))
	for k := range d.header {
		names = append(names, k)
	}
	sort.Strings(names)
	ts, sig := d.header.Get(webhook.TimestampHeader), d.header.Get(webhook.SignatureHeader)
	out := map[string]any{"path": d.path, "content_type": d.header.Get("Content-Type"), "body": r.normalize(string(d.body)), "header_names": names, "signed": sig != ""}
	if sig != "" {
		n, _ := strconv.ParseInt(ts, 10, 64)
		out["signature_valid"] = webhook.Sign(callbackSecret, n, d.body) == sig
		out["signature_hex_len"] = len(sig)
		out["timestamp_is_unix_seconds"] = math.Abs(float64(time.Now().Unix()-n)) < 600
	}
	return out
}

func canonical(v any) string {
	b, _ := json.Marshal(v)
	var x any
	_ = json.Unmarshal(b, &x)
	b, _ = json.Marshal(x)
	return string(b)
}

// webhooks compares the client callbacks with webhook-signed,
// webhook-unsigned, webhook-retry-on-500 and webhook-redirect-not-followed.
func (r *replay) webhooks(t *testing.T) {
	prefix := func(p string) func(string) bool { return func(s string) bool { return strings.HasPrefix(s, p) } }
	var signed struct {
		Total int `json:"deliveries_total"`
	}
	loadFixture(t, "webhook-signed").observed(t, "deliveries_total", &signed.Total)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if len(r.receiver.deliveries(prefix("/fail"))) >= 6 && len(r.receiver.deliveries(prefix("/redirect"))) >= 6 &&
			len(r.receiver.deliveries(prefix("/ok/a"))) >= signed.Total {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond) // nothing further may arrive
	for _, c := range []struct{ name, prefix string }{{"webhook-signed", "/ok/a"}, {"webhook-unsigned", "/ok/b"}} {
		f := loadFixture(t, c.name)
		var total int
		var allValid, allSigned bool
		var examples []map[string]any
		f.observed(t, "deliveries_total", &total)
		f.observed(t, "all_signatures_valid", &allValid)
		f.observed(t, "all_signed", &allSigned)
		f.observed(t, "examples", &examples)
		got := r.receiver.deliveries(prefix(c.prefix))
		valid, everySigned := true, true
		bodies := map[string]map[string]any{}
		for _, d := range got {
			desc := r.describe(d)
			if v, ok := desc["signature_valid"]; ok && v != true {
				valid = false
			}
			everySigned = everySigned && desc["signed"] == true
			bodies[desc["body"].(string)] = desc
		}
		if len(got) != total || valid != allValid || everySigned != allSigned {
			t.Errorf("%s: %d deliveries (legacy %d), valid %v signed %v", c.name, len(got), total, valid, everySigned)
		}
		for _, e := range examples {
			if g, ok := bodies[e["body"].(string)]; !ok || canonical(g) != canonical(e) {
				t.Errorf("%s example:\n got  %s\n want %s", c.name, canonical(g), canonical(e))
			}
		}
	}
	for _, c := range []struct{ name, prefix string }{{"webhook-retry-on-500", "/fail"}, {"webhook-redirect-not-followed", "/redirect"}} {
		f := loadFixture(t, c.name)
		var attempts int
		var gaps []float64
		var first map[string]any
		f.observed(t, "attempts", &attempts)
		f.observed(t, "gaps_seconds", &gaps)
		f.observed(t, "first", &first)
		got := r.receiver.deliveries(prefix(c.prefix))
		if len(got) != attempts {
			t.Errorf("%s: %d attempts, legacy %d", c.name, len(got), attempts)
			continue
		}
		for i := 1; i < len(got); i++ {
			if gap := got[i].at.Sub(got[i-1].at).Seconds(); math.Abs(gap-gaps[i-1]) > 0.1 {
				t.Errorf("%s: gap %d is %.2fs, legacy %.1fs", c.name, i, gap, gaps[i-1])
			}
		}
		if g := r.describe(got[0]); canonical(g) != canonical(first) {
			t.Errorf("%s first:\n got  %s\n want %s", c.name, canonical(g), canonical(first))
		}
		if c.name == "webhook-redirect-not-followed" {
			var hits int
			f.observed(t, "redirect_target_hits", &hits)
			if n := len(r.receiver.deliveries(func(p string) bool { return p == "/ok" })); n != hits {
				t.Errorf("%s: redirect target reached %d times", c.name, n)
			}
		}
	}
}

// ---------- receipt races (independent of the US1 send path) ----------

// TestReceiptTerminalRace seeds a message directly and races delivered,
// failed, intermediate and forged receipts on both receipt routes: every
// call is acknowledged, each status aggregates into one row with the exact
// count, the message ends in one of the terminal states and keeps it, and
// forged receipts leave no trace.
func TestReceiptTerminalRace(t *testing.T) {
	ctx := context.Background()
	db := storetest.Start(t)
	kek := []byte(strings.Repeat("k", 32))
	env, err := sealed.NewEnvelope(kek)
	if err != nil {
		t.Fatal(err)
	}
	rp := db.Repo()
	if _, err := rp.CreateClient(ctx, repo.APIClient{ID: 1, TenantID: tenant, Username: "client_a", PasswordHash: "x", Authority: "API_CLIENT", Status: repo.On}); err != nil {
		t.Fatal(err)
	}
	cfg, err := env.SealConfig(sealed.Config{"url": "http://127.0.0.1:9/", "dlr_token": tManual}, sealed.ProviderAD(tenant, 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rp.CreateProvider(ctx, repo.Provider{ID: 1, TenantID: tenant, Name: "p", Type: "voicecom", ObjectType: repo.ObjectSMS, ConfigSealed: cfg, Status: repo.On}); err != nil {
		t.Fatal(err)
	}
	id := repo.NewID()
	if _, err := rp.CreateMessage(ctx, repo.Message{ID: id, TenantID: tenant, Actor: repo.ClientActor(1), Sid: 9999, Recipient: "359888000500", ProviderID: 1,
		StatusCode: 0, StatusMessage: "sms_provider_accepted"}); err != nil {
		t.Fatal(err)
	}
	running := apptest.Start(t, apptest.Options{DSN: db.AppDSN, KEK: kek})
	r := &replay{t: t, client: &http.Client{Timeout: 30 * time.Second}, names: map[string]string{}, values: map[string]string{}}
	plan := map[int]int{8: 40, 1: 30, 2: 30}
	type call struct {
		status int
		token  string
		path   string
	}
	var calls []call
	for status, n := range plan {
		for i := range n {
			calls = append(calls, call{status, tManual, []string{"/dlr", "/hermes/v1/sms/dlr"}[i%2]})
		}
	}
	for range 20 {
		calls = append(calls, call{16, strings.Repeat("x", 32), "/dlr"})
	}
	var wg sync.WaitGroup
	var bad sync.Map
	for _, c := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := r.do(running.Public, "GET", c.path+"?"+receiptQuery(id, c.status, 359888000500, c.token), "", nil)
			if err != nil || res.status != 200 || res.body != `"DLR_OK"` {
				bad.Store(c, res)
			}
		}()
	}
	wg.Wait()
	bad.Range(func(k, v any) bool { t.Errorf("not acknowledged: %+v %+v", k, v); return true })
	l, err := rp.ListReceipts(ctx, repo.TenantView(tenant), id)
	if err != nil {
		t.Fatal(err)
	}
	parts := map[int]int{}
	for _, d := range l.Items {
		parts[int(d.MessageStatus)] = d.PartsReceived
	}
	if fmt.Sprint(parts) != fmt.Sprint(plan) {
		t.Fatalf("aggregated parts %v, want %v", parts, plan)
	}
	m, err := rp.GetMessage(ctx, repo.TenantView(tenant), id)
	if err != nil || (m.StatusCode != 1 && m.StatusCode != 2) {
		t.Fatalf("message ended in %d %q: %v", m.StatusCode, m.StatusMessage, err)
	}
	for _, status := range []int{8, 1, 2, 0} {
		r.do(running.Public, "GET", "/dlr?"+receiptQuery(id, status, 359888000500, tManual), "", nil)
	}
	after, _ := rp.GetMessage(ctx, repo.TenantView(tenant), id)
	if after.StatusCode != m.StatusCode || after.StatusMessage != m.StatusMessage || after.DLRTs != m.DLRTs {
		t.Fatalf("late receipts replaced the terminal state: %d -> %d", m.StatusCode, after.StatusCode)
	}
}
