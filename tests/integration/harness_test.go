//go:build integration

package integration

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/repo"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/sealed"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/store"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/store/storetest"
)

// Seed identities (tests/fixtures/seed.sql).
const (
	TenantA         = "0b2f6a1e-4c55-4c8e-9d1a-000000000a0a"
	TenantB         = "0b2f6a1e-4c55-4c8e-9d1a-000000000b0b"
	Password        = "Passw0rd!"
	MessageA        = "00000000-0000-4000-8000-00000000000a"
	MessageOperator = "00000000-0000-4000-8000-00000000000c"
	MessageB        = "00000000-0000-4000-8000-00000000000b"
	ProviderA       = int64(1)
	ProviderAOff    = int64(2)
	ProviderB       = int64(3)
)

// DLRTokens are the seeded per-provider receipt tokens.
var DLRTokens = map[int64]string{ProviderA: strings.Repeat("a", 32), ProviderAOff: strings.Repeat("c", 32), ProviderB: strings.Repeat("b", 32)}

// CarrierToken is the seeded carrier API token of every provider (it must
// never appear in stored evidence, logs or responses).
const CarrierToken = "fixture-carrier-token-not-production"

var seedTenants = map[int64]string{ProviderA: TenantA, ProviderAOff: TenantA, ProviderB: TenantB}

// Env is a seeded database with a mock carrier and a callback receiver.
type Env struct {
	DB       *storetest.DB
	Repo     *repo.Postgres
	KEK      []byte
	Envelope *sealed.Envelope
	Carrier  *Carrier
	Receiver *Receiver
}

// Start seeds a fresh database and starts the mock carrier and receiver.
// Provider configurations point at the carrier and call back to callbackBase
// (the public listener under test; "" until one exists).
func Start(t *testing.T, callbackBase string) *Env {
	t.Helper()
	ctx := context.Background()
	db := storetest.Start(t)
	sql, err := os.ReadFile("../fixtures/seed.sql")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, db.OwnerDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	owner, err := store.Open(ctx, db.OwnerDSN, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if err := owner.SyncSequences(ctx); err != nil {
		t.Fatal(err)
	}
	e := &Env{DB: db, Repo: db.Repo(), KEK: []byte(strings.Repeat("k", 32)), Carrier: NewCarrier(t), Receiver: NewReceiver(t)}
	if e.Envelope, err = sealed.NewEnvelope(e.KEK); err != nil {
		t.Fatal(err)
	}
	e.SealProviders(t, callbackBase)
	return e
}

// ProviderConfig is the legacy Voicecom configuration of a seeded provider.
func (e *Env) ProviderConfig(id int64, callbackBase string) sealed.Config {
	sid := "9999"
	if id == ProviderB {
		sid = "8888"
	}
	return sealed.Config{"url": e.Carrier.URL + "/multichannel-api/sendmulti/", "sid": sid,
		"encoding": "utf-8", "priority": "2", "token": CarrierToken, "dlr_token": DLRTokens[id],
		"callback_url": callbackBase + "/dlr?dlr_token=" + DLRTokens[id]}
}

// SealProviders seals every seeded provider configuration with the harness KEK.
func (e *Env) SealProviders(t *testing.T, callbackBase string) {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), e.DB.OwnerDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	for id, tenant := range seedTenants {
		blob, err := e.Envelope.SealConfig(e.ProviderConfig(id, callbackBase), sealed.ProviderAD(tenant, id))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(context.Background(), "UPDATE sms_provider SET config_sealed = $1 WHERE id = $2", blob, id); err != nil {
			t.Fatal(err)
		}
	}
}

// Submission is one request the mock carrier received.
type Submission struct {
	Raw         string
	RequestID   string `json:"request_id"`
	To          uint64 `json:"to"`
	Sid         uint32 `json:"sid"`
	Token       string `json:"token"`
	CallbackURL string `json:"callback_url"`
	Sms         struct {
		Text        string `json:"text"`
		From        string `json:"from"`
		Encoding    string `json:"encoding"`
		Concatenate uint32 `json:"concatenate"`
	} `json:"sms"`
}

// Carrier modes.
const (
	Accept  = "accept"  // return_code 0, as the legacy mock
	Reject  = "reject"  // return_code 2001 (sms_invalid_sid)
	HTTP500 = "http500" // carrier failure
	Drop    = "drop"    // connection closed without a response (acceptance unknown)
	Hold    = "hold"    // waits for Release, then accepts
)

// Carrier mocks the Voicecom/LinkMobility multichannel API (the legacy
// cmd/test-linkmobility behaviour); it never forwards anything.
type Carrier struct {
	*httptest.Server
	mu      sync.Mutex
	subs    []Submission
	mode    string
	held    chan struct{}
	release chan struct{}
	client  *http.Client
}

// NewCarrier starts an accepting carrier.
func NewCarrier(t *testing.T) *Carrier {
	c := &Carrier{mode: Accept, held: make(chan struct{}, 16), release: make(chan struct{}), client: &http.Client{Timeout: 10 * time.Second}}
	c.Server = httptest.NewServer(http.HandlerFunc(c.handle))
	t.Cleanup(c.Close)
	return c
}

// SetMode switches the response of later submissions.
func (c *Carrier) SetMode(m string) {
	c.mu.Lock()
	c.mode = m
	c.mu.Unlock()
}

// Held signals each submission that reached a held carrier.
func (c *Carrier) Held() <-chan struct{} { return c.held }

// Release lets every held submission answer.
func (c *Carrier) Release() { close(c.release) }

// Submissions returns what the carrier received.
func (c *Carrier) Submissions() []Submission {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Submission(nil), c.subs...)
}

func (c *Carrier) handle(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var s Submission
	if err := json.Unmarshal(raw, &s); err != nil {
		writeCarrier(w, 200, `{"return_code":3000,"return_message":"Invalid JSON input data"}`)
		return
	}
	s.Raw = string(raw)
	c.mu.Lock()
	c.subs = append(c.subs, s)
	mode := c.mode
	c.mu.Unlock()
	switch mode {
	case Reject:
		writeCarrier(w, 200, `{"return_code":2001,"return_message":"Invalid SID"}`)
	case HTTP500:
		writeCarrier(w, 500, `{"error":"carrier failure"}`)
	case Drop:
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
			}
		}
	case Hold:
		c.held <- struct{}{}
		<-c.release
		fallthrough
	default:
		writeCarrier(w, 200, `{"return_code":0,"return_message":"Message accepted","channels":{"sms":{"send_order":1,"message_parts":1}}}`)
	}
}

func writeCarrier(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, body+"\n")
}

// SendDLR calls the submission's callback URL the way the carrier does: a
// GET with the receipt merged into the configured query (dlr_token kept).
func (c *Carrier) SendDLR(t *testing.T, requestID string, status int, timestamp int64) (int, string) {
	t.Helper()
	var sub *Submission
	for _, s := range c.Submissions() {
		if s.RequestID == requestID {
			sub = &s
		}
	}
	if sub == nil {
		t.Fatalf("no submission for %s", requestID)
	}
	u, err := url.Parse(sub.CallbackURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("request_id", requestID)
	q.Set("channel", "sms")
	q.Set("sid", strconv.FormatUint(uint64(sub.Sid), 10))
	q.Set("message_status", strconv.Itoa(status))
	q.Set("to", strconv.FormatUint(sub.To, 10))
	q.Set("from", sub.Sms.From)
	q.Set("timestamp", strconv.FormatInt(timestamp, 10))
	u.RawQuery = q.Encode()
	resp, err := c.client.Get(u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// Delivery is one outbound callback the receiver got.
type Delivery struct {
	Path   string
	Header http.Header
	Body   []byte
	At     time.Time
}

// Receiver records client callbacks: /ok answers 204, /fail 500, /redirect 302.
type Receiver struct {
	*httptest.Server
	mu   sync.Mutex
	hits []Delivery
}

// NewReceiver starts a callback receiver.
func NewReceiver(t *testing.T) *Receiver {
	r := &Receiver{}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(req.Body, 1<<20))
		r.mu.Lock()
		r.hits = append(r.hits, Delivery{Path: req.URL.Path, Header: req.Header.Clone(), Body: body, At: time.Now()})
		r.mu.Unlock()
		switch {
		case strings.HasPrefix(req.URL.Path, "/fail"):
			w.WriteHeader(http.StatusInternalServerError)
		case strings.HasPrefix(req.URL.Path, "/redirect"):
			http.Redirect(w, req, "/ok/redirected", http.StatusFound)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(r.Close)
	return r
}

// Deliveries returns the callbacks whose path starts with prefix.
func (r *Receiver) Deliveries(prefix string) []Delivery {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Delivery
	for _, d := range r.hits {
		if strings.HasPrefix(d.Path, prefix) {
			out = append(out, d)
		}
	}
	return out
}

// Signed verifies the source callback signature: lower-case hex
// HMAC-SHA256 over timestamp + "." + body.
func Signed(ts, signature string, body []byte, secret string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	return hmac.Equal([]byte(hex.EncodeToString(mac.Sum(nil))), []byte(signature))
}

// Signed reports a delivery signed with secret.
func (d Delivery) Signed(secret string) bool {
	return Signed(d.Header.Get("X-Smsgw-Timestamp"), d.Header.Get("X-Smsgw-Signature"), d.Body, secret)
}
