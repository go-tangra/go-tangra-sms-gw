//go:build integration

package contract

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/app/apptest"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/repo"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/sealed"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/store"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/store/storetest"
)

// Capture constants of scripts/capture_legacy_runtime.py.
const (
	jwtSecret      = "capture-only-jwt-secret-0123456789abcdef"
	callbackSecret = "capture-callback-secret-not-production"
	carrierToken   = "capture-carrier-token-not-production"
	fixedTS        = 1700000000
	nilUUID        = "00000000-0000-0000-0000-000000000000"
	tenant         = "6b1d2a9e-3f00-4c1a-9d2e-00000000c0de"
)

var (
	tAuto, tManual, tSlow = strings.Repeat("a", 32), strings.Repeat("b", 32), strings.Repeat("c", 32)
	jwtRE                 = regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*`)
	uuidRE                = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	rfc3339RE             = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})`)
)

// fixture is one recorded legacy case.
type fixture struct {
	Case    string `json:"case"`
	Note    string `json:"note"`
	Request *struct {
		Method  string            `json:"method"`
		Path    string            `json:"path"`
		Headers map[string]string `json:"headers"`
		Body    *string           `json:"body"`
	} `json:"request"`
	Response *struct {
		Status      int    `json:"status"`
		ContentType string `json:"content_type"`
		Body        string `json:"body"`
	} `json:"response"`
	Observed map[string]json.RawMessage `json:"observed"`
}

func fixtureDir() string {
	return filepath.Join(apptest.Root(), "tests", "fixtures", "legacy", "runtime")
}

func loadFixture(t *testing.T, name string) fixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixtureDir(), name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var f fixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f fixture) observed(t *testing.T, key string, v any) {
	t.Helper()
	if err := json.Unmarshal(f.Observed[key], v); err != nil {
		t.Fatalf("%s observed %s: %v", f.Case, key, err)
	}
}

type result struct {
	status      int
	contentType string
	body        string
}

// replay drives the V4 public listener through the capture script's
// scenario, in its order, against a database seeded like the capture.
type replay struct {
	t        *testing.T
	db       *storetest.DB
	repo     *repo.Postgres
	env      *sealed.Envelope
	kek      []byte
	carrier  *captureCarrier
	receiver *captureReceiver
	running  *apptest.Running
	base     string
	client   *http.Client

	mu     sync.Mutex
	names  map[string]string // actual value -> placeholder
	values map[string]string // placeholder -> actual value
	got    map[string]result
	ids    map[string]int64
	tokens map[string]map[string]string
}

func (r *replay) name(value, label string) {
	if value == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.names[value]; ok {
		return
	}
	ph := "{{" + label + "}}"
	r.names[value] = ph
	if _, ok := r.values[ph]; !ok {
		r.values[ph] = value
	}
}

// normalize applies the capture normalizer to a V4 response.
func (r *replay) normalize(s string) string {
	r.mu.Lock()
	keys := make([]string, 0, len(r.names))
	for k := range r.names {
		keys = append(keys, k)
	}
	r.mu.Unlock()
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	for _, k := range keys {
		s = strings.ReplaceAll(s, k, r.names[k])
	}
	s = jwtRE.ReplaceAllString(s, "{{unregistered_jwt}}")
	s = uuidRE.ReplaceAllString(s, "{{unregistered_uuid}}")
	return rfc3339RE.ReplaceAllString(s, "{{rfc3339}}")
}

func (r *replay) substitute(s string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for ph, v := range r.values {
		s = strings.ReplaceAll(s, ph, v)
	}
	return strings.ReplaceAll(s, "{{unregistered_uuid}}", nilUUID)
}

func (r *replay) raw(method, path, body string, headers map[string]string) result {
	r.t.Helper()
	res, err := r.do(r.base, method, path, body, headers)
	if err != nil {
		r.t.Fatalf("%s %s: %v", method, path, err)
	}
	return res
}

// do is raw without failing the test (usable from other goroutines).
func (r *replay) do(base, method, path, body string, headers map[string]string) (result, error) {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, base+path, rd)
	if err != nil {
		return result{}, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if body != "" && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return result{}, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return result{resp.StatusCode, resp.Header.Get("Content-Type"), string(b)}, nil
}

func (r *replay) jsonBody(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		r.t.Fatal(err)
	}
	return string(b)
}

// call replays the request of a recorded case and keeps the response.
func (r *replay) call(name string) result {
	r.t.Helper()
	f := loadFixture(r.t, name)
	if f.Request == nil {
		r.t.Fatalf("%s has no request", name)
	}
	headers := map[string]string{}
	for k, v := range f.Request.Headers {
		headers[k] = r.substitute(v)
	}
	body := ""
	if f.Request.Body != nil {
		body = r.substitute(*f.Request.Body)
	}
	res := r.raw(f.Request.Method, r.substitute(f.Request.Path), body, headers)
	r.record(name, res)
	return res
}

func (r *replay) record(name string, res result) {
	r.mu.Lock()
	r.got[name] = res
	r.mu.Unlock()
}

func (r *replay) bearer(user string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + r.tokens[user]["access_token"]}
}

func (r *replay) login(user string) map[string]string {
	r.t.Helper()
	res := r.raw("POST", "/hermes/v1/login", r.jsonBody(map[string]string{"username": user, "password": "Passw0rd!", "grant_type": "password"}), nil)
	return r.tokenPair(res, user)
}

func (r *replay) tokenPair(res result, label string) map[string]string {
	r.t.Helper()
	var out map[string]string
	if res.status != 200 || json.Unmarshal([]byte(res.body), &out) != nil {
		r.t.Fatalf("login %s: %d %s", label, res.status, res.body)
	}
	r.name(out["access_token"], "access_token:"+label)
	r.name(out["refresh_token"], "refresh_token:"+label)
	return out
}

func (r *replay) forge(label string, claims jwt.MapClaims, secret string, alg jwt.SigningMethod) string {
	r.t.Helper()
	key := any([]byte(secret))
	if alg == jwt.SigningMethodNone {
		key = jwt.UnsafeAllowNoneSignatureType
	}
	tok, err := jwt.NewWithClaims(alg, claims).SignedString(key)
	if err != nil {
		r.t.Fatal(err)
	}
	r.name(tok, "forged_jwt:"+label)
	return tok
}

func (r *replay) sendBody(provider, template string, to uint64, props map[string]string) string {
	if props == nil {
		props = map[string]string{}
	}
	return r.jsonBody(map[string]any{"to": to, "providerId": r.ids["p:"+provider], "templateId": r.ids["t:"+template], "properties": props,
		"sms": map[string]string{"from": "Capture", "encoding": "utf-8"}})
}

func (r *replay) messageID(t *testing.T, recipient string) string {
	t.Helper()
	l, err := r.repo.ListMessages(context.Background(), repo.TenantView(tenant), repo.MessageFilter{Recipient: recipient}, repo.Page{})
	if err != nil || l.Total != 1 {
		t.Fatalf("message for %s: %+v %v", recipient, l, err)
	}
	return l.Items[0].ID
}

func (r *replay) dataID(t *testing.T, res result) string {
	t.Helper()
	var out struct{ Data struct{ ID string } }
	if json.Unmarshal([]byte(res.body), &out) != nil || out.Data.ID == "" {
		t.Fatalf("no message id in %d %s", res.status, res.body)
	}
	return out.Data.ID
}

// carrierReceipt is the legacy mock carrier's automatic receipt: a GET of
// the submission's callback URL with the receipt merged into its query.
func (r *replay) carrierReceipt(t *testing.T, id string, status int, to uint64, ts int64) result {
	t.Helper()
	cb := r.carrier.callbackURL(id)
	if cb == "" {
		t.Fatalf("carrier has no submission for %s", id)
	}
	u, err := url.Parse(cb)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	for k, v := range map[string]string{"channel": "sms", "from": "Capture", "message_status": strconv.Itoa(status), "request_id": id, "sid": "9999",
		"timestamp": strconv.FormatInt(ts, 10), "to": strconv.FormatUint(to, 10)} {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()
	res, err := r.do("", "GET", u.String(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// ---------- seed ----------

func (r *replay) seed(t *testing.T, gw string) {
	ctx := context.Background()
	hash, err := bcrypt.GenerateFromPassword([]byte("Passw0rd!"), 4)
	if err != nil {
		t.Fatal(err)
	}
	rcv := r.receiver.URL + "/"
	clients := []struct {
		user, auth, status, url, secret string
	}{
		{"client_a", "API_CLIENT", "ON", rcv + "ok/a", callbackSecret},
		{"client_b", "API_CLIENT", "ON", rcv + "ok/b", ""},
		{"viewer_v", "API_VIEWER", "ON", "", ""},
		{"admin_x", "API_ADMIN", "ON", "", ""},
		{"disabled_d", "API_CLIENT", "OFF", "", ""},
		{"client_f", "API_CLIENT", "ON", rcv + "fail/f", callbackSecret},
		{"client_r", "API_CLIENT", "ON", rcv + "redirect/r", ""},
		{"client_l", "API_CLIENT", "ON", "", ""},
		{"client_s", "API_CLIENT", "ON", "", ""},
	}
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, c := range clients {
		id := int64(i + 1)
		sealedSecret, err := r.env.SealString(c.secret, sealed.CallbackAD(tenant, id))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.repo.CreateClient(ctx, repo.APIClient{ID: id, TenantID: tenant, Username: c.user, PasswordHash: string(hash),
			Email: c.user + "@example.invalid", Authority: c.auth, Status: c.status, CallbackURL: c.url, CallbackSecretSealed: sealedSecret,
			CreateTime: created, UpdateTime: created}); err != nil {
			t.Fatal(err)
		}
		r.ids["c:"+c.user] = id
	}
	mock := r.carrier.URL + "/multichannel-api/sendmulti/"
	base := func(extra map[string]string) sealed.Config {
		c := sealed.Config{"sid": "9999", "encoding": "utf-8", "priority": "2"}
		for k, v := range extra {
			c[k] = v
		}
		return c
	}
	providers := []struct {
		name, typ, obj, status string
		cfg                    sealed.Config
	}{
		{"mock-auto", "voicecom", repo.ObjectSMS, repo.On, base(map[string]string{"url": mock, "token": carrierToken, "callback_url": gw + "/dlr?dlr_token=" + tAuto, "dlr_token": tAuto})},
		{"mock-manual", "voicecom", repo.ObjectSMS, repo.On, base(map[string]string{"url": mock, "callback_url": gw + "/dlr?dlr_token=" + tManual, "dlr_token": tManual})},
		{"mock-legacy-no-token", "voicecom", repo.ObjectSMS, repo.On, base(map[string]string{"url": mock, "callback_url": gw + "/dlr"})},
		{"mock-off", "voicecom", repo.ObjectSMS, repo.Off, base(map[string]string{"url": mock, "callback_url": gw + "/dlr"})},
		{"mock-viber", "voicecom", repo.ObjectViber, repo.On, base(map[string]string{"url": mock, "callback_url": gw + "/dlr"})},
		{"fake-reject", "voicecom", repo.ObjectSMS, repo.On, base(map[string]string{"url": r.carrier.URL + "/reject", "callback_url": gw + "/dlr"})},
		{"fake-http500", "voicecom", repo.ObjectSMS, repo.On, base(map[string]string{"url": r.carrier.URL + "/http500", "callback_url": gw + "/dlr"})},
		{"fake-drop", "voicecom", repo.ObjectSMS, repo.On, base(map[string]string{"url": r.carrier.URL + "/drop", "callback_url": gw + "/dlr"})},
		{"fake-slow", "voicecom", repo.ObjectSMS, repo.On, base(map[string]string{"url": r.carrier.URL + "/slow", "callback_url": gw + "/dlr?dlr_token=" + tSlow, "dlr_token": tSlow})},
		{"unknown-type", "carrier-x", repo.ObjectSMS, repo.On, sealed.Config{"url": r.carrier.URL + "/reject"}},
		{"mock-no-callback", "voicecom", repo.ObjectSMS, repo.On, base(map[string]string{"url": mock})},
	}
	keys := []string{"auto", "manual", "legacy", "off", "viber", "reject", "http500", "drop", "slow", "unknown", "nocallback"}
	for i, p := range providers {
		id := int64(i + 1)
		blob, err := r.env.SealConfig(p.cfg, sealed.ProviderAD(tenant, id))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.repo.CreateProvider(ctx, repo.Provider{ID: id, TenantID: tenant, Name: p.name, Type: p.typ, ObjectType: p.obj,
			ConfigSealed: blob, ConfigPublic: sealed.Public(p.cfg, []string{"token", "dlr_token"}), Status: p.status, CreateTime: created, UpdateTime: created}); err != nil {
			t.Fatal(err)
		}
		r.ids["p:"+keys[i]] = id
	}
	templates := []struct {
		key, obj, status string
		body             map[string]string
	}{
		{"hello", repo.ObjectSMS, repo.On, map[string]string{"body": "Hello {{ .name }}, code {{ .code }}."}},
		{"static", repo.ObjectSMS, repo.On, map[string]string{"body": "ping"}},
		{"off", repo.ObjectSMS, repo.Off, map[string]string{"body": "off"}},
		{"viber", repo.ObjectViber, repo.On, map[string]string{"body": "viber"}},
		{"nobody", repo.ObjectSMS, repo.On, map[string]string{"subject": "no body fragment"}},
		{"badsyntax", repo.ObjectSMS, repo.On, map[string]string{"body": "Hello {{ .name"}},
		{"unicode", repo.ObjectSMS, repo.On, map[string]string{"body": "Здравей {{ .name }}"}},
	}
	for i, tp := range templates {
		id := int64(i + 1)
		if _, err := r.repo.CreateTemplate(ctx, repo.Template{ID: id, TenantID: tenant, Name: tp.key, ObjectType: tp.obj, Templates: tp.body,
			Status: tp.status, CreateTime: created, UpdateTime: created}); err != nil {
			t.Fatal(err)
		}
		r.ids["t:"+tp.key] = id
	}
	manual := r.ids["p:manual"]
	for _, b := range []struct {
		recipient string
		provider  *int64
		status    string
	}{{"359888000301", &manual, repo.On}, {"359888000302", nil, repo.On}, {"359888000303", &manual, repo.Off}} {
		if _, err := r.repo.CreateBlock(ctx, repo.Block{TenantID: tenant, Recipient: b.recipient, Description: "capture", ProviderID: b.provider,
			BlockType: repo.ObjectSMS, Status: b.status, CreateTime: created, UpdateTime: created}); err != nil {
			t.Fatal(err)
		}
	}
	owner, err := store.Open(ctx, r.db.OwnerDSN, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if err := owner.SyncSequences(ctx); err != nil {
		t.Fatal(err)
	}
	r.name(callbackSecret, "callback_secret")
	r.name(carrierToken, "carrier_token")
	r.name(tAuto, "dlr_token:auto")
	r.name(tManual, "dlr_token:manual")
	r.name(tSlow, "dlr_token:slow")
}

// captureCarrier is the capture's carrier set: the legacy mock's accepting
// endpoint plus the failure modes of the in-process fake.
type captureCarrier struct {
	*httptest.Server
	slowEntered chan struct{}
	slowRelease chan struct{}
	mu          sync.Mutex
	callbacks   map[string]string // request_id -> callback_url
}

func (c *captureCarrier) callbackURL(id string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.callbacks[id]
}

func newCaptureCarrier(t *testing.T) *captureCarrier {
	c := &captureCarrier{slowEntered: make(chan struct{}, 1), slowRelease: make(chan struct{}), callbacks: map[string]string{}}
	accept := `{"return_code":0,"return_message":"Message accepted","channels":{"sms":{"send_order":1,"message_parts":1}}}`
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw, _ := io.ReadAll(req.Body)
		var sub struct {
			RequestID   string `json:"request_id"`
			CallbackURL string `json:"callback_url"`
		}
		if json.Unmarshal(raw, &sub) == nil && sub.RequestID != "" {
			c.mu.Lock()
			c.callbacks[sub.RequestID] = sub.CallbackURL
			c.mu.Unlock()
		}
		reply := func(code int, body string) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_, _ = io.WriteString(w, body)
		}
		switch {
		case strings.HasPrefix(req.URL.Path, "/drop"):
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				_ = conn.(*net.TCPConn).SetLinger(0)
				_ = conn.Close()
			}
		case strings.HasPrefix(req.URL.Path, "/http500"):
			reply(500, `{"error":"carrier exploded"}`)
		case strings.HasPrefix(req.URL.Path, "/reject"):
			reply(200, `{"return_code":2001,"return_message":"Invalid SID"}`)
		case strings.HasPrefix(req.URL.Path, "/slow"):
			c.slowEntered <- struct{}{}
			<-c.slowRelease
			reply(200, accept)
		case req.URL.Path == "/multichannel-api/sendmulti/":
			reply(200, accept+"\n")
		default:
			reply(404, `{}`)
		}
	}))
	t.Cleanup(c.Close)
	return c
}

// captureReceiver is the capture's client callback receiver: /ok answers
// 204, /fail 500 and /redirect 302 to /ok.
type captureReceiver struct {
	*httptest.Server
	mu   sync.Mutex
	hits []delivery
}

type delivery struct {
	path   string
	header http.Header
	body   []byte
	at     time.Time
}

func newCaptureReceiver(t *testing.T) *captureReceiver {
	c := &captureReceiver{}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(req.Body, 1<<20))
		h := req.Header.Clone()
		h.Set("Host", req.Host)
		c.mu.Lock()
		c.hits = append(c.hits, delivery{req.URL.Path, h, body, time.Now()})
		c.mu.Unlock()
		switch {
		case strings.HasPrefix(req.URL.Path, "/fail"):
			w.WriteHeader(http.StatusInternalServerError)
		case strings.HasPrefix(req.URL.Path, "/redirect"):
			http.Redirect(w, req, "/ok", http.StatusFound)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(c.Close)
	return c
}

func (c *captureReceiver) deliveries(match func(path string) bool) []delivery {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []delivery
	for _, d := range c.hits {
		if match(d.path) {
			out = append(out, d)
		}
	}
	return out
}
