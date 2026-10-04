package webhook

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type hit struct {
	path   string
	header http.Header
	body   string
	at     time.Time
}

type receiver struct {
	*httptest.Server
	mu   sync.Mutex
	hits []hit
}

func newReceiver(t *testing.T, handle func(w http.ResponseWriter, r *http.Request)) *receiver {
	rc := &receiver{}
	rc.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rc.mu.Lock()
		rc.hits = append(rc.hits, hit{r.URL.Path, r.Header.Clone(), string(b), time.Now()})
		rc.mu.Unlock()
		handle(w, r)
	}))
	t.Cleanup(rc.Close)
	return rc
}

func (rc *receiver) got(prefix string) []hit {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	var out []hit
	for _, h := range rc.hits {
		if strings.HasPrefix(h.path, prefix) {
			out = append(out, h)
		}
	}
	return out
}

func standard(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasPrefix(r.URL.Path, "/fail"):
		w.WriteHeader(500)
	case strings.HasPrefix(r.URL.Path, "/redirect"):
		http.Redirect(w, r, "/target", http.StatusFound)
	default:
		w.WriteHeader(204)
	}
}

type outcomes struct {
	mu sync.Mutex
	n  map[string]int
}

func (o *outcomes) Webhook(v string) { o.mu.Lock(); o.n[v]++; o.mu.Unlock() }
func (o *outcomes) count(v string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.n[v]
}

var local = Policy{AllowHTTP: true, AllowPrivate: true}

func start(t *testing.T, c Config) (*Dispatcher, *outcomes, context.CancelFunc, chan struct{}) {
	m := &outcomes{n: map[string]int{}}
	c.Metrics = m
	d := New(c)
	d.backoff = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return d, m, cancel, done
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func event(u, secret string) Event {
	return Event{URL: u, Secret: secret, MessageID: "6f1c0a52-8d4e-4b7a-9a51-0c3c1f6b2d10", Channel: "sms", MessageStatus: 1,
		StatusText: "sms_delivered", Recipient: 359888000210, From: "Capture", Timestamp: 1700000000}
}

func TestSignedPayloadIsTheSourceFormat(t *testing.T) {
	rc := newReceiver(t, standard)
	d, m, _, _ := start(t, Config{Policy: local})
	d.now = func() time.Time { return time.Unix(1750000000, 0) }
	if !d.Enqueue(event(rc.URL+"/ok/a", "capture-callback-secret")) {
		t.Fatal("not queued")
	}
	eventually(t, "delivery", func() bool { return m.count("delivered") == 1 })
	h := rc.got("/ok/a")[0]
	want := `{"message_id":"6f1c0a52-8d4e-4b7a-9a51-0c3c1f6b2d10","channel":"sms","message_status":1,"status_text":"sms_delivered","to":359888000210,"from":"Capture","timestamp":1700000000}`
	if h.body != want {
		t.Fatalf("body %s", h.body)
	}
	if h.header.Get("Content-Type") != "application/json" || h.header.Get("X-Smsgw-Timestamp") != "1750000000" ||
		h.header.Get("X-Smsgw-Signature") != Sign("capture-callback-secret", 1750000000, []byte(want)) {
		t.Fatalf("headers %v", h.header)
	}
	if len(h.header.Get("X-Smsgw-Signature")) != 64 {
		t.Fatal("signature is not 64 hex characters")
	}
	// Captured header set (webhook-signed, minus Host which the server strips).
	names := []string{}
	for k := range h.header {
		names = append(names, k)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"Accept-Encoding", "Content-Length", "Content-Type", "User-Agent", "X-Smsgw-Signature", "X-Smsgw-Timestamp"}) {
		t.Fatalf("header names %v", names)
	}
}

func TestSignatureVector(t *testing.T) {
	// python3: hmac.new(b"secret", b"1700000000.{}", hashlib.sha256).hexdigest()
	if got := Sign("secret", 1700000000, []byte("{}")); got != "b8569b78799ff9e3cbff0fc2d63a33a2b57f3282abd07c37ae5e8e7d79a5f163" {
		t.Fatalf("signature %q", got)
	}
}

func TestEmptySecretSendsNoSignature(t *testing.T) {
	rc := newReceiver(t, standard)
	d, m, _, _ := start(t, Config{Policy: local})
	d.Enqueue(event(rc.URL+"/ok/b", ""))
	eventually(t, "delivery", func() bool { return m.count("delivered") == 1 })
	h := rc.got("/ok/b")[0]
	if h.header.Get("X-Smsgw-Signature") != "" || h.header.Get("X-Smsgw-Timestamp") != "" {
		t.Fatalf("unsigned callback carries signature headers: %v", h.header)
	}
}

func TestNon2xxIsRetriedSixTimesWithDoublingBackoff(t *testing.T) {
	rc := newReceiver(t, standard)
	d, m, _, _ := start(t, Config{Policy: local})
	d.Enqueue(event(rc.URL+"/fail/f", "s"))
	eventually(t, "exhaustion", func() bool { return m.count("failed") == 1 })
	hits := rc.got("/fail")
	if len(hits) != MaxAttempts {
		t.Fatalf("%d attempts", len(hits))
	}
	for i := 1; i < len(hits); i++ {
		if gap, min := hits[i].at.Sub(hits[i-1].at), d.backoff<<(i-1); gap < min {
			t.Fatalf("gap %d is %v, want at least %v", i, gap, min)
		}
	}
	if New(Config{}).backoff != 200*time.Millisecond {
		t.Fatal("source backoff starts at 200ms")
	}
}

func TestRedirectIsNotFollowed(t *testing.T) {
	rc := newReceiver(t, standard)
	d, m, _, _ := start(t, Config{Policy: local})
	d.Enqueue(event(rc.URL+"/redirect/r", ""))
	eventually(t, "exhaustion", func() bool { return m.count("failed") == 1 })
	if n := len(rc.got("/redirect")); n != MaxAttempts {
		t.Fatalf("%d attempts", n)
	}
	if n := len(rc.got("/target")); n != 0 {
		t.Fatalf("redirect followed %d times", n)
	}
}

func TestQueueOverflowDropsTheNewEvent(t *testing.T) {
	m := &outcomes{n: map[string]int{}}
	d := New(Config{QueueSize: 1, Policy: local, Metrics: m})
	if d.Enqueue(Event{}) {
		t.Fatal("event without a callback URL was queued")
	}
	if !d.Enqueue(event("http://127.0.0.1:9/a", "")) {
		t.Fatal("first event not queued")
	}
	if d.Enqueue(event("http://127.0.0.1:9/b", "")) {
		t.Fatal("overflow event was queued")
	}
	if m.count("dropped") != 1 || len(d.queue) != 1 || (<-d.queue).URL != "http://127.0.0.1:9/a" {
		t.Fatal("overflow did not drop the new event")
	}
}

type fakeResolver map[string][]string

func (f fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	var out []netip.Addr
	for _, s := range f[host] {
		out = append(out, netip.MustParseAddr(s))
	}
	if out == nil {
		return nil, errors.New("no such host")
	}
	return out, nil
}

func TestPrivateDestinationsAreBlockedWithoutRetry(t *testing.T) {
	rc := newReceiver(t, standard)
	port := mustURL(t, rc.URL).Port()
	res := fakeResolver{"rebind.example": {"127.0.0.1"}, "split.example": {"93.184.216.34", "10.1.2.3"}, "public.example": {"127.0.0.1"}}
	d, m, _, _ := start(t, Config{Policy: Policy{AllowHTTP: true}, Resolver: res})
	for _, u := range []string{rc.URL + "/literal", "http://rebind.example:" + port + "/dns", "http://split.example:" + port + "/split",
		"http://localhost:" + port + "/name", "http://[::ffff:127.0.0.1]:" + port + "/mapped"} {
		d.Enqueue(event(u, "s"))
	}
	eventually(t, "refusals", func() bool { return m.count("failed") == 5 })
	if n := len(rc.got("/")); n != 0 {
		t.Fatalf("blocked destinations were reached %d times", n)
	}
	// A name that passes resolution is still checked at connect time.
	if err := blockAtConnect("tcp", "10.0.0.7:443", nil); !errors.Is(err, ErrBlocked) {
		t.Fatalf("connect-time check: %v", err)
	}
	if err := blockAtConnect("tcp", "93.184.216.34:443", nil); err != nil {
		t.Fatalf("public address refused at connect: %v", err)
	}
}

func TestValidateURL(t *testing.T) {
	strict := Policy{}
	for raw, ok := range map[string]bool{
		"https://hooks.example.com/dlr?x=1": true,
		"https://93.184.216.34/dlr":         true,
		"http://hooks.example.com/dlr":      false,
		"ftp://hooks.example.com/dlr":       false,
		"https://user:pw@hooks.example.com": false,
		"https://localhost/dlr":             false,
		"https://a.localhost/dlr":           false,
		"https://10.0.0.1/dlr":              false,
		"https://169.254.169.254/latest":    false,
		"https://[::1]/dlr":                 false,
		"https://[fd00::1]/dlr":             false,
		"https://100.64.0.1/dlr":            false,
		"https://0.0.0.0/dlr":               false,
		"https:///dlr":                      false,
		"/relative":                         false,
		"":                                  false,
	} {
		if err := ValidateURL(raw, strict); (err == nil) != ok {
			t.Errorf("ValidateURL(%q) = %v", raw, err)
		}
	}
	if ValidateURL("http://127.0.0.1:8080/x", local) != nil {
		t.Error("development policy refused a local receiver")
	}
	if ValidateURL("http://127.0.0.1:8080/x", Policy{AllowPrivate: true}) == nil {
		t.Error("plain http allowed without allow_http")
	}
}

func TestShutdownCancelsInFlightAndBackoff(t *testing.T) {
	entered := make(chan struct{}, 8)
	rc := newReceiver(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/hang") {
			entered <- struct{}{}
			<-r.Context().Done()
			return
		}
		w.WriteHeader(500)
	})
	d, m, cancel, done := start(t, Config{Policy: local, Workers: 2})
	d.backoff = time.Hour
	d.Enqueue(event(rc.URL+"/hang", ""))
	d.Enqueue(event(rc.URL+"/fail", ""))
	<-entered
	eventually(t, "first failure", func() bool { return len(rc.got("/fail")) == 1 })
	began := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatcher did not stop")
	}
	if time.Since(began) > time.Second || m.count("delivered")+m.count("failed") != 0 {
		t.Fatal("shutdown waited for delivery or recorded an outcome")
	}
}

func mustURL(t *testing.T, s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
