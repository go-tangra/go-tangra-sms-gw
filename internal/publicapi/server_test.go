package publicapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/auth"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/provider"
	_ "github.com/go-tangra/go-tangra-sms-gw/v4/internal/provider/voicecom"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/ratelimit"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/repo"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/sealed"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/sms"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/trustedproxy"
)

const tenant = "0b2f6a1e-4c55-4c8e-9d1a-000000000a0a"

// mem is an in-memory store for the auth and sms services.
type mem struct {
	mu       sync.Mutex
	clients  map[int64]repo.APIClient
	provider repo.Provider
	template repo.Template
	messages []repo.Message
}

func (m *mem) ClientByUsername(_ context.Context, u string) (repo.APIClient, error) {
	for _, c := range m.clients {
		if c.Username == u {
			return c, nil
		}
	}
	return repo.APIClient{}, repo.ErrNotFound
}
func (m *mem) ClientByID(_ context.Context, id int64) (repo.APIClient, error) {
	if c, ok := m.clients[id]; ok {
		return c, nil
	}
	return repo.APIClient{}, repo.ErrNotFound
}
func (m *mem) RecordLogin(context.Context, string, int64, string, time.Time) error { return nil }
func (m *mem) AddLogin(context.Context, repo.LoginEvent) error                     { return nil }
func (m *mem) Revoke(context.Context, repo.Revocation) error                       { return nil }
func (m *mem) IsRevoked(context.Context, string, string, time.Time) (bool, error)  { return false, nil }
func (m *mem) GetProvider(_ context.Context, t string, id int64) (repo.Provider, error) {
	if t == tenant && id == m.provider.ID {
		return m.provider, nil
	}
	return repo.Provider{}, repo.ErrNotFound
}
func (m *mem) GetTemplate(_ context.Context, t string, id int64) (repo.Template, error) {
	if t == tenant && id == m.template.ID {
		return m.template, nil
	}
	return repo.Template{}, repo.ErrNotFound
}
func (m *mem) IsBlocked(context.Context, string, int64, string) (bool, error) { return false, nil }
func (m *mem) CreateMessage(_ context.Context, msg repo.Message) (repo.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	msg.CreateTime, msg.UpdateTime = time.Date(2026, 10, 4, 12, 0, 0, 123456000, time.UTC), time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	m.messages = append(m.messages, msg)
	return msg, nil
}
func (m *mem) SetProviderResponse(_ context.Context, _, id string, r repo.ProviderResponse) (repo.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.messages {
		if m.messages[i].ID == id {
			m.messages[i].StatusCode, m.messages[i].StatusMessage = r.StatusCode, r.StatusMessage
			m.messages[i].ProviderName, m.messages[i].APIClientUsername = "joined", "joined"
			return m.messages[i], nil
		}
	}
	return repo.Message{}, repo.ErrNotFound
}
func (m *mem) GetMessage(_ context.Context, v repo.View, id string) (repo.Message, error) {
	for _, msg := range m.messages {
		if msg.ID == id && msg.TenantID == v.Tenant() && (v.Client() == 0 || msg.Actor.APIClientID == v.Client()) {
			return msg, nil
		}
	}
	return repo.Message{}, repo.ErrNotFound
}
func (m *mem) ListMessages(_ context.Context, v repo.View, _ repo.MessageFilter, _ repo.Page) (repo.List[repo.Message], error) {
	out := repo.List[repo.Message]{Items: []repo.Message{}}
	for _, msg := range m.messages {
		if msg.Actor.APIClientID == v.Client() {
			out.Items = append(out.Items, msg)
		}
	}
	out.Total = len(out.Items)
	return out, nil
}
func (m *mem) ListReceipts(ctx context.Context, v repo.View, id string) (repo.List[repo.Receipt], error) {
	if _, err := m.GetMessage(ctx, v, id); err != nil {
		return repo.List[repo.Receipt]{}, err
	}
	return repo.List[repo.Receipt]{Items: []repo.Receipt{{ID: 7, MessageID: id, Channel: "sms", Sid: 9999, MessageStatus: 1, Recipient: 359888000100,
		Sender: "X", Timestamp: 1700000000, RemoteAddress: "192.0.2.1", StatusText: "sms_delivered", PartsReceived: 2,
		CreateTime: time.Unix(1700000000, 0), UpdateTime: time.Unix(1700000000, 5e8)}}, Total: 1}, nil
}

type fixture struct {
	srv     http.Handler
	store   *mem
	carrier *httptest.Server
}

func newFixture(t *testing.T, sendBurst int) *fixture {
	t.Helper()
	carrier := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"return_code":0,"return_message":"Message accepted","channels":{"sms":{"send_order":1,"message_parts":1}}}`)
	}))
	t.Cleanup(carrier.Close)
	env, _ := sealed.NewEnvelope(bytes.Repeat([]byte("k"), 32))
	cfg, _ := env.SealConfig(sealed.Config{"url": carrier.URL, "sid": "9999", "encoding": "utf-8", "callback_url": "http://gw/dlr"}, sealed.ProviderAD(tenant, 2))
	hash, _ := bcrypt.GenerateFromPassword([]byte("Passw0rd!"), 4)
	st := &mem{clients: map[int64]repo.APIClient{}, provider: repo.Provider{ID: 2, TenantID: tenant, Name: "mock", Type: "voicecom", ObjectType: repo.ObjectSMS,
		Status: repo.On, ConfigSealed: cfg}, template: repo.Template{ID: 2, TenantID: tenant, Name: "static", ObjectType: repo.ObjectSMS, Status: repo.On,
		Templates: map[string]string{"body": "ping <{{ .n }}>"}}}
	for i, c := range []struct{ user, auth string }{{"client_a", "API_CLIENT"}, {"viewer_v", "API_VIEWER"}, {"client_b", "API_CLIENT"}} {
		st.clients[int64(i+1)] = repo.APIClient{ID: int64(i + 1), TenantID: tenant, Username: c.user, Email: c.user + "@example.invalid",
			PasswordHash: string(hash), Authority: c.auth, Status: repo.On}
	}
	iss, _ := auth.NewIssuer(strings.Repeat("s", 32), 0, 0)
	proxy, _ := trustedproxy.New([]string{"10.0.0.0/8"})
	srv := New(Config{Auth: auth.NewService(st, iss, nil, nil, nil), SMS: sms.New(sms.Config{Store: st, Envelope: env}), Proxy: proxy,
		Login: ratelimit.New(10, 3, 0), Send: ratelimit.New(10, sendBurst, 0), MaxBody: 1024})
	return &fixture{srv: srv, store: st, carrier: carrier}
}

type response struct {
	code int
	ct   string
	body string
}

func (f *fixture) do(method, path, body string, headers ...string) response {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "192.0.2.10:5000"
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	w := httptest.NewRecorder()
	f.srv.ServeHTTP(w, req)
	return response{w.Code, w.Header().Get("Content-Type"), w.Body.String()}
}

func (f *fixture) token(t *testing.T, user string) string {
	t.Helper()
	r := f.do("POST", "/hermes/v1/login", `{"username":"`+user+`","password":"Passw0rd!"}`)
	var out map[string]string
	if r.code != 200 || json.Unmarshal([]byte(r.body), &out) != nil {
		t.Fatalf("login %s: %+v", user, r)
	}
	return "Bearer " + out["access_token"]
}

func errBody(code int, reason, message string) string {
	return `{"code":` + itoa(code) + `, "reason":"` + reason + `", "message":"` + message + `", "metadata":{}}`
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestRouting(t *testing.T) {
	f := newFixture(t, 20)
	notFound := response{404, "text/plain; charset=utf-8", "404 page not found\n"}
	for _, c := range []struct {
		method, path string
		want         response
	}{
		{"GET", "/health", response{200, "application/json", "{\"status\":\"ok\"}\n"}},
		{"GET", "/dlr", response{200, "application/json", `"DLR_OK"`}},
		{"GET", "/hermes/v1/sms/dlr?request_id=x", response{200, "application/json", `"DLR_OK"`}},
		{"POST", "/dlr", notFound},
		{"GET", "/hermes/v1/logout", notFound},
		{"DELETE", "/hermes/v1/sms", notFound},
		{"HEAD", "/hermes/v1/me", notFound},
		{"GET", "/hermes/v1/unknown", notFound},
		{"GET", "/hermes/v1/sms/a/b", notFound},
		{"GET", "/hermes/v1/sms/", notFound},
		{"GET", "/v1/sms/dlr/", notFound},
		{"GET", "/v1/login", notFound},
		{"GET", "/metrics", notFound},
		{"GET", "/", notFound},
		{"GET", "/api/sms-gw/v1/providers", notFound},
		{"GET", "/hermes/v1/me", response{401, "application/json", errBody(401, "UNAUTHORIZED", "missing bearer token")}},
		{"GET", "/v1/sms/x", response{401, "application/json", errBody(401, "UNAUTHORIZED", "missing bearer token")}},
		{"GET", "/hermes/v1/sms/dlr/x", response{401, "application/json", errBody(401, "UNAUTHORIZED", "missing bearer token")}},
	} {
		if got := f.do(c.method, c.path, ""); got != c.want {
			t.Fatalf("%s %s: %+v", c.method, c.path, got)
		}
	}
}

func TestDecodeErrorsKeepSourceText(t *testing.T) {
	f := newFixture(t, 20)
	a := f.token(t, "client_a")
	for _, c := range []struct {
		method, path, body, ctype, token, want string
		code                                   int
	}{
		// Bodies are decoded before authentication, as in the source.
		{"POST", "/hermes/v1/sms", `{"to":`, "application/json", "", errBody(400, "CODEC", "body unmarshal proto:\u00a0unexpected EOF"), 400},
		{"POST", "/hermes/v1/sms", `{"to": -1, "providerId": 2}`, "application/json", a,
			errBody(400, "CODEC", "body unmarshal proto:\u00a0(line 1:8): invalid value for uint64 field to: -1"), 400},
		{"POST", "/hermes/v1/login", `{"username":`, "", "", errBody(400, "CODEC", "body unmarshal proto:\u00a0unexpected EOF"), 400},
		{"GET", "/hermes/v1/sms?page=abc", "", "", "", errBody(400, "CODEC", `parsing field \"page\": strconv.ParseInt: parsing \"abc\": invalid syntax`), 400},
		{"GET", "/hermes/v1/sms?page_size=1x", "", "", a, errBody(400, "CODEC", `parsing field \"page_size\": strconv.ParseInt: parsing \"1x\": invalid syntax`), 400},
		{"GET", "/hermes/v1/sms?nopaging=maybe", "", "", a, errBody(400, "CODEC", `parsing field \"no_paging\": strconv.ParseBool: parsing \"maybe\": invalid syntax`), 400},
		{"GET", "/hermes/v1/sms?page=1&page=2", "", "", a, errBody(400, "CODEC", `too many values for field \"page\": 1, 2`), 400},
		{"POST", "/hermes/v1/login", strings.Repeat(" ", 2048), "", "", errBody(400, "BAD_REQUEST", "request body too large"), 400},
	} {
		r := f.do(c.method, c.path, c.body, "Content-Type", c.ctype, "Authorization", c.token)
		if r.code != c.code || r.body != c.want {
			t.Fatalf("%s %s %q:\n got  %d %s\n want %s", c.method, c.path, c.body, r.code, r.body, c.want)
		}
	}
}

func TestLoginCodecsAndLimit(t *testing.T) {
	f := newFixture(t, 20)
	form := f.do("POST", "/hermes/v1/login", "username=client_a&password=Passw0rd%21&grant_type=password", "Content-Type", "application/x-www-form-urlencoded")
	if form.code != 200 || !strings.HasSuffix(form.body, `", "token_type":"Bearer", "expires_in":"7200"}`) || !strings.HasPrefix(form.body, `{"access_token":"ey`) {
		t.Fatalf("%+v", form)
	}
	// The source accepts proto and JSON names and ignores unknown fields.
	if r := f.do("POST", "/hermes/v1/login", `{"username":"client_a","password":"Passw0rd!","extra":{"x":[1]}}`); r.code != 200 {
		t.Fatalf("%+v", r)
	}
	if r := f.do("POST", "/hermes/v1/login", `{"username":"client_a","password":"x"}`); r.body != errBody(401, "UNAUTHORIZED", "invalid credentials") {
		t.Fatalf("%+v", r)
	}
	// Login burst 3 per client address: the fourth attempt is limited.
	if r := f.do("POST", "/hermes/v1/login", `{"username":"client_a","password":"x"}`); r.code != 429 || r.body != errBody(429, "TOO_MANY_REQUESTS", "rate limit exceeded for 192.0.2.10") {
		t.Fatalf("%+v", r)
	}
}

func TestSendAndRead(t *testing.T) {
	f := newFixture(t, 3)
	a, v, b := f.token(t, "client_a"), f.token(t, "viewer_v"), f.token(t, "client_b")
	body := `{"to": "359888000100", "provider_id": 2, "templateId": 2, "properties": {"n": "<&>"}, "sms": {"from": "X", "validity": {"ttl": 60}}, "text": "ignored"}`
	r := f.do("POST", "/v1/sms", body, "Authorization", a)
	if r.code != 200 || r.ct != "application/json" {
		t.Fatalf("%+v", r)
	}
	m := f.store.messages[0]
	want := `{"data":{"id":"` + m.ID + `", "owner":1, "sid":9999, "to":"359888000100", "priority":2, "defer":"", "sms":null, "text":"ping <<&>>", ` +
		`"status":0, "statusMessage":"sms_provider_accepted", "rawResponse":"", "rawRequest":"", "userName":"", "providerId":0, "providerName":"", ` +
		`"apiClientUsername":"", "createTime":"2026-10-04T12:00:00.123456Z", "updateTime":"2026-10-04T12:00:00Z"}, ` +
		`"endpointResponse":{"returnCode":0, "returnMessage":"Message accepted", "channels":{"sms":{"sendOrder":1, "messageParts":1}, "viber":null}}}`
	if r.body != want {
		t.Fatalf("send\n got  %s\n want %s", r.body, want)
	}
	var data map[string]any
	if json.Unmarshal(m.Data, &data) != nil || data["sms"].(map[string]any)["validity"].(map[string]any)["ttl"] != float64(60) {
		t.Fatalf("stored data %s", m.Data)
	}
	if r := f.do("POST", "/hermes/v1/sms", body, "Authorization", v); r.body != errBody(403, "FORBIDDEN", "sending SMS requires API_CLIENT authority") {
		t.Fatalf("%+v", r)
	}
	// Per-client send limit (burst 3): client_a has used one.
	for range 2 {
		f.do("POST", "/hermes/v1/sms", body, "Authorization", a)
	}
	if r := f.do("POST", "/hermes/v1/sms", body, "Authorization", a); r.body != errBody(429, "TOO_MANY_REQUESTS", "rate limit exceeded for client:1") {
		t.Fatalf("%+v", r)
	}
	if r := f.do("POST", "/hermes/v1/sms", body, "Authorization", b); r.code != 200 {
		t.Fatalf("other client limited: %+v", r)
	}
	if r := f.do("GET", "/hermes/v1/sms/"+m.ID, "", "Authorization", b); r.body != errBody(404, "RECORD_NOT_FOUND", "sms not found") {
		t.Fatalf("foreign get %+v", r)
	}
	if r := f.do("GET", "/hermes/v1/sms/"+m.ID, "", "Authorization", a); r.code != 200 || !strings.Contains(r.body, `"providerName":"joined"`) {
		t.Fatalf("own get %+v", r)
	}
	if r := f.do("GET", "/hermes/v1/sms", "", "Authorization", v); r.body != `{"items":[], "total":0}` {
		t.Fatalf("viewer list %+v", r)
	}
	r = f.do("GET", "/v1/sms/dlr/"+m.ID, "", "Authorization", a)
	wantDLR := `{"items":[{"id":"7", "requestId":"` + m.ID + `", "channel":"sms", "sid":9999, "messageStatus":1, "to":"359888000100", "from":"X", ` +
		`"timestamp":"1700000000", "remoteAddrerss":"192.0.2.1", "statusText":"sms_delivered", "partsReceived":2, "createTime":"2023-11-14T22:13:20Z", ` +
		`"updateTime":"2023-11-14T22:13:20.500Z"}], "total":1}`
	if r.body != wantDLR {
		t.Fatalf("receipts\n got  %s\n want %s", r.body, wantDLR)
	}
	if r := f.do("GET", "/hermes/v1/sms/dlr/"+m.ID, "", "Authorization", b); r.code != 404 {
		t.Fatalf("foreign receipts %+v", r)
	}
	if r := f.do("GET", "/hermes/v1/me", "", "Authorization", v); r.body != `{"user":{"id":2, "username":"viewer_v", "email":"viewer_v@example.invalid", "authority":"API_VIEWER"}, `+
		`"abilities":[{"action":"list", "subject":"sms"}, {"action":"get", "subject":"sms"}, {"action":"get", "subject":"dlr"}]}` {
		t.Fatalf("me %+v", r)
	}
	if r := f.do("POST", "/hermes/v1/logout", `{"id": 1}`); r.body != `{}` || r.code != 200 {
		t.Fatalf("logout %+v", r)
	}
}

func TestEncoding(t *testing.T) {
	var b bytes.Buffer
	encode(&b, object{{"s", "a\"b\\c\n\t\x01<>&é😀"}, {"n", nil}, {"q", quotedU(18446744073709551615)}, {"i", quoted(-5)},
		{"u", uint32(4294967295)}, {"l", []any{}}, {"o", object{}}, {"t", timestamp(time.Date(2026, 1, 2, 3, 4, 5, 1, time.FixedZone("x", 3600)))}})
	want := `{"s":"a\"b\\c\n\t\u0001<>&é😀", "n":null, "q":"18446744073709551615", "i":"-5", "u":4294967295, "l":[], "o":{}, "t":"2026-01-02T02:04:05.000000001Z"}`
	if b.String() != want {
		t.Fatalf("\n got  %s\n want %s", b.String(), want)
	}
	// The source unsigned status: the initial -1 is 4294967295 on the wire.
	if !strings.Contains(string(mustJSON(messageJSON(repo.Message{StatusCode: -1, Recipient: "1"}))), `"status":4294967295`) {
		t.Fatal("status not unsigned")
	}
	if carrierJSON(nil) != nil || string(mustJSON(carrierJSON(&provider.CarrierResponse{ReturnCode: 2001, ReturnMessage: "Invalid SID"}))) !=
		`{"returnCode":2001, "returnMessage":"Invalid SID", "channels":null}` {
		t.Fatal("carrier envelope")
	}
}

func mustJSON(v any) []byte {
	var b bytes.Buffer
	encode(&b, v)
	return b.Bytes()
}
