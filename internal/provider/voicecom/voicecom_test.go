package voicecom

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/provider"
)

const id = "6a8c0c8e-1b2d-4e5f-8a9b-0c1d2e3f4a5b"

type carrier struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []string
}

func (c *carrier) body(i int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if i >= len(c.bodies) {
		return ""
	}
	return c.bodies[i]
}

func (c *carrier) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.bodies)
}

func newCarrier(t *testing.T, h func(w http.ResponseWriter)) *carrier {
	c := &carrier{}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.bodies = append(c.bodies, string(b))
		c.mu.Unlock()
		if r.Header.Get("Content-Type") != "application/json; charset=utf-8" || r.Method != http.MethodPost {
			w.WriteHeader(400)
			return
		}
		h(w)
	}))
	t.Cleanup(c.Close)
	return c
}

func sender(t *testing.T, url string, extra map[string]string) provider.Sender {
	t.Helper()
	cfg := map[string]string{"url": url, "sid": "9999", "encoding": "utf-8", "priority": "2", "token": "carrier-token-value",
		"callback_url": "http://gw.example/dlr?dlr_token=" + strings.Repeat("b", 32)}
	for k, v := range extra {
		cfg[k] = v
	}
	s, err := provider.New(Type, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func reply(code int, body string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}
}

// The request body is the source carrier contract: field names, order and
// omissions as the legacy service sent them (send-roundtrip carrier
// submission); the configured priority is not sent.
func TestRequestShape(t *testing.T) {
	c := newCarrier(t, reply(200, `{"return_code":0,"return_message":"Message accepted","channels":{"sms":{"send_order":1,"message_parts":1}}}`))
	s := sender(t, c.URL+"/multichannel-api/sendmulti/", nil)
	out := s.Send(context.Background(), provider.Request{To: 359888000222, SMS: &provider.SMS{From: "Capture", Encoding: "utf-8"}}, id, "Hello Eve, code 1.")
	if out.Err != nil || out.Carrier == nil || out.Carrier.ReturnMessage != "Message accepted" || out.Carrier.Channels.SMS.MessageParts != 1 {
		t.Fatalf("%+v", out)
	}
	want := `{"sid":9999,"request_id":"` + id + `","to":359888000222,"token":"carrier-token-value","send_order":["sms"],` +
		`"callback_url":"http://gw.example/dlr?dlr_token=` + strings.Repeat("b", 32) + `","sms":{"text":"Hello Eve, code 1.","encoding":"utf-8","concatenate":2,"from":"Capture"}}`
	if c.body(0) != want {
		t.Fatalf("body\n got  %s\n want %s", c.body(0), want)
	}
	if !strings.Contains(string(out.RawRequest), want) || !strings.Contains(string(out.RawResponse), "Message accepted") {
		t.Fatal("raw evidence incomplete")
	}
	if s.Sid() != 9999 || s.Priority() != 2 {
		t.Fatal(s.Sid(), s.Priority())
	}
}

func TestRequestOptionsOverrideDefaults(t *testing.T) {
	c := newCarrier(t, reply(200, `{"return_code":0}`))
	s := sender(t, c.URL, map[string]string{"token": "", "concatenate": "5"})
	s.Send(context.Background(), provider.Request{To: 359888000104, SMS: &provider.SMS{From: "Capture", Encoding: "gsm-03-38", Concatenate: 3,
		Mccmnc: 28401, Validity: &provider.Validity{TTL: 60, Units: "minutes"}}}, id, "Здравей <no value>")
	want := `{"sid":9999,"request_id":"` + id + `","to":359888000104,"send_order":["sms"],"callback_url":"http://gw.example/dlr?dlr_token=` +
		strings.Repeat("b", 32) + `","sms":{"text":"Здравей \u003cno value\u003e","encoding":"gsm-03-38","concatenate":3,"from":"Capture","mccmnc":28401}}`
	if c.body(0) != want {
		t.Fatalf("body\n got  %s\n want %s", c.body(0), want)
	}
	s.Send(context.Background(), provider.Request{To: 359888000105}, id, "x")
	if !strings.Contains(c.body(1), `"sms":{"text":"x","encoding":"utf-8","concatenate":5}`) {
		t.Fatal(c.body(1))
	}
}

func TestFailuresAreSanitized(t *testing.T) {
	reject := newCarrier(t, reply(200, `{"return_code":2001,"return_message":"Invalid SID"}`))
	out := sender(t, reject.URL, nil).Send(context.Background(), provider.Request{To: 1}, id, "x")
	if out.Err != nil || out.Carrier.ReturnCode != 2001 || out.Carrier.Channels != nil {
		t.Fatalf("%+v", out)
	}
	failing := newCarrier(t, reply(500, `{"error":"carrier exploded"}`))
	out = sender(t, failing.URL, nil).Send(context.Background(), provider.Request{To: 1}, id, "x")
	if out.Err == nil || out.Err.Error() != "voicecom: http 500 Internal Server Error" || out.Carrier == nil || len(out.RawResponse) == 0 {
		t.Fatalf("%+v", out)
	}
	garbage := newCarrier(t, reply(200, `not json`))
	if out = sender(t, garbage.URL, nil).Send(context.Background(), provider.Request{To: 1}, id, "x"); out.Err == nil || out.Err.Error() != "voicecom: decode response" {
		t.Fatalf("%+v", out)
	}
	drop := newCarrier(t, func(w http.ResponseWriter) {
		conn, _, _ := w.(http.Hijacker).Hijack()
		_ = conn.(*net.TCPConn).SetLinger(0)
		_ = conn.Close()
	})
	out = sender(t, drop.URL+"/send?token=carrier-token-value", nil).Send(context.Background(), provider.Request{To: 1}, id, "x")
	if out.Err == nil || out.Err.Error() != "voicecom: endpoint error" || len(out.RawRequest) == 0 || drop.count() != 1 {
		t.Fatalf("%+v", out)
	}
	out = sender(t, drop.URL, map[string]string{"callback_url": ""}).Send(context.Background(), provider.Request{To: 1}, id, "x")
	if out.Err == nil || out.Err.Error() != "voicecom: callback_url not configured" || drop.count() != 1 {
		t.Fatalf("%+v", out)
	}
}

func TestConfigValidation(t *testing.T) {
	ok := map[string]string{"url": "https://bsms.example/send", "sid": "9999", "encoding": "utf-8"}
	if c, err := NewConfig(ok); err != nil || c.Priority != "2" {
		t.Fatal(err)
	}
	for k, v := range map[string]string{"sid": "", "url": "", "encoding": "latin1", "priority": "11", "concatenate": "x", "callback_url": "::"} {
		bad := map[string]string{}
		for kk, vv := range ok {
			bad[kk] = vv
		}
		bad[k] = v
		if _, err := NewConfig(bad); err == nil {
			t.Fatalf("%s=%q accepted", k, v)
		}
	}
	bad := map[string]string{"url": "https://bsms.example/send", "sid": "abc", "encoding": "utf-8"}
	if _, err := NewConfig(bad); err == nil {
		t.Fatal("non-numeric sid accepted")
	}
}

func TestStatusTable(t *testing.T) {
	for code, slug := range map[int32]string{-1: "sms_gw_accepted", 0: "sms_provider_accepted", 1: "sms_delivered", 2: "sms_delivery_failed",
		8: "sms_smsc_delivered", 16: "sms_rejected", 2001: "sms_invalid_sid", 4004: "sms_not_included_send_order", 77: ""} {
		if StatusText(code) != slug {
			t.Fatalf("%d: %q", code, StatusText(code))
		}
	}
	for code, terminal := range map[int32]bool{-1: false, 0: false, 8: false, 1: true, 2: true, 16: true, 1000: true, 9999: true} {
		if IsTerminal(code) != terminal {
			t.Fatalf("%d terminal=%v", code, IsTerminal(code))
		}
	}
}

func TestRegistry(t *testing.T) {
	m, ok := provider.MetaFor(Type)
	if !ok || len(m.Fields) != 10 || m.Fields[0].Key != "url" || strings.Join(m.Secrets(), ",") != "token,dlr_token" {
		t.Fatalf("%+v", m)
	}
	if _, err := provider.New("carrier-x", nil); err == nil || err.Error() != `provider "carrier-x" not registered` {
		t.Fatal(err)
	}
	vals := provider.SecretValues(Type, map[string]string{"token": "t0k3n", "dlr_token": "d1r", "sid": "9999"})
	if strings.Join(vals, ",") != "t0k3n,d1r" {
		t.Fatal(vals)
	}
	c := provider.NewCache()
	cfg := map[string]string{"url": "https://bsms.example/send", "sid": "9999", "encoding": "utf-8"}
	s1, _ := c.Get(1, Type, cfg)
	s2, _ := c.Get(1, Type, cfg)
	cfg["sid"] = "8888"
	s3, _ := c.Get(1, Type, cfg)
	c.Invalidate(1)
	s4, _ := c.Get(1, Type, cfg)
	if s1 != s2 || s3 == s2 || s3.Sid() != 8888 || s4 == s3 {
		t.Fatal("cache does not follow the configuration")
	}
}
