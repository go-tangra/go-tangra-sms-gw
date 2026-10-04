package dlr

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/audit"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/repo"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/sealed"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/webhook"
)

const (
	tenantA = "0b2f6a1e-4c55-4c8e-9d1a-000000000a0a"
	tenantB = "0b2f6a1e-4c55-4c8e-9d1a-000000000b0b"
	msgA    = "00000000-0000-4000-8000-00000000000a"
	msgB    = "00000000-0000-4000-8000-00000000000b"
	msgOp   = "00000000-0000-4000-8000-00000000000c"
	msgOld  = "00000000-0000-4000-8000-00000000000d"
)

var (
	tokA = strings.Repeat("a", 32)
	tokB = strings.Repeat("b", 32)
)

type key struct {
	tenant string
	id     int64
}

type fakeStore struct {
	mu        sync.Mutex
	messages  map[string]repo.MessageRef
	providers map[key]repo.Provider
	clients   map[key]repo.APIClient
	applied   []repo.Receipt
	clientGet []key
}

func (f *fakeStore) ResolveMessage(_ context.Context, id string) (repo.MessageRef, error) {
	if m, ok := f.messages[id]; ok {
		return m, nil
	}
	return repo.MessageRef{}, repo.ErrNotFound
}

func (f *fakeStore) GetProvider(_ context.Context, tenant string, id int64) (repo.Provider, error) {
	if p, ok := f.providers[key{tenant, id}]; ok {
		return p, nil
	}
	return repo.Provider{}, repo.ErrNotFound
}

func (f *fakeStore) GetClient(_ context.Context, tenant string, id int64) (repo.APIClient, error) {
	f.mu.Lock()
	f.clientGet = append(f.clientGet, key{tenant, id})
	f.mu.Unlock()
	if c, ok := f.clients[key{tenant, id}]; ok {
		return c, nil
	}
	return repo.APIClient{}, repo.ErrNotFound
}

func (f *fakeStore) ApplyReceipt(_ context.Context, r repo.Receipt) (repo.ReceiptResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applied = append(f.applied, r)
	return repo.ReceiptResult{Receipt: r, Changed: true, Before: repo.Message{CreateTime: time.Now().Add(-3 * time.Second)}}, nil
}

type queue struct {
	mu     sync.Mutex
	events []webhook.Event
}

func (q *queue) Enqueue(e webhook.Event) bool {
	q.mu.Lock()
	q.events = append(q.events, e)
	q.mu.Unlock()
	return true
}

type recorder struct {
	mu       sync.Mutex
	received []uint32
	rejected []string
	latency  []string
	audits   []audit.Event
}

func (r *recorder) Receipt(s uint32) { r.mu.Lock(); r.received = append(r.received, s); r.mu.Unlock() }
func (r *recorder) ReceiptRejected(v string) {
	r.mu.Lock()
	r.rejected = append(r.rejected, v)
	r.mu.Unlock()
}
func (r *recorder) Record(e audit.Event) error {
	r.mu.Lock()
	r.audits = append(r.audits, e)
	r.mu.Unlock()
	return nil
}
func (r *recorder) DeliveryLatency(t string, _ time.Duration) {
	r.mu.Lock()
	r.latency = append(r.latency, t)
	r.mu.Unlock()
}

type fixture struct {
	store *fakeStore
	q     *queue
	rec   *recorder
	p     *Processor
	env   *sealed.Envelope
}

func newFixture(t *testing.T) *fixture {
	env, err := sealed.NewEnvelope([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	seal := func(tenant string, id int64, c sealed.Config) []byte {
		b, err := env.SealConfig(c, sealed.ProviderAD(tenant, id))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	secret, _ := env.SealString("callback-secret-a", sealed.CallbackAD(tenantA, 1))
	// Provider 1 exists in both tenants with different receipt tokens; the
	// tenant always comes from the stored message.
	s := &fakeStore{
		messages: map[string]repo.MessageRef{
			msgA:   {ID: msgA, TenantID: tenantA, ProviderID: 1, APIClientID: 1},
			msgB:   {ID: msgB, TenantID: tenantB, ProviderID: 1, APIClientID: 6},
			msgOp:  {ID: msgOp, TenantID: tenantA, ProviderID: 1},
			msgOld: {ID: msgOld, TenantID: tenantA, ProviderID: 2, APIClientID: 2},
		},
		providers: map[key]repo.Provider{
			{tenantA, 1}: {ID: 1, TenantID: tenantA, Type: "voicecom", ConfigSealed: seal(tenantA, 1, sealed.Config{"dlr_token": tokA, "token": "carrier"})},
			{tenantA, 2}: {ID: 2, TenantID: tenantA, Type: "voicecom", ConfigSealed: seal(tenantA, 2, sealed.Config{"url": "http://carrier"})},
			{tenantB, 1}: {ID: 1, TenantID: tenantB, Type: "voicecom", ConfigSealed: seal(tenantB, 1, sealed.Config{"dlr_token": tokB})},
		},
		clients: map[key]repo.APIClient{
			{tenantA, 1}: {ID: 1, TenantID: tenantA, CallbackURL: "https://hooks.example.com/a", CallbackSecretSealed: secret},
			{tenantA, 2}: {ID: 2, TenantID: tenantA, CallbackURL: "https://hooks.example.com/old"},
			{tenantB, 6}: {ID: 6, TenantID: tenantB},
		},
	}
	f := &fixture{store: s, q: &queue{}, rec: &recorder{}, env: env}
	f.p = New(Config{Store: s, Envelope: env, Callbacks: f.q, Metrics: f.rec, Audit: f.rec})
	return f
}

func receipt(id, token string, status uint32) Receipt {
	return Receipt{RequestID: id, Channel: "sms", Sid: 9999, MessageStatus: status, To: 359888000100, From: "Fixture", Timestamp: 1700000000,
		Token: token, RemoteAddr: "198.51.100.7"}
}

func TestValidReceiptIsAppliedInTheMessageTenantAndCalledBack(t *testing.T) {
	f := newFixture(t)
	if r := f.p.Process(context.Background(), receipt(msgA, tokA, 1)); r != Accepted {
		t.Fatalf("rejected: %s", r)
	}
	if len(f.store.applied) != 1 {
		t.Fatal("receipt not applied")
	}
	got := f.store.applied[0]
	if got.TenantID != tenantA || got.MessageID != msgA || got.StatusText != "sms_delivered" || got.MessageStatus != 1 || got.Sid != 9999 ||
		got.Recipient != 359888000100 || got.Sender != "Fixture" || got.Timestamp != 1700000000 || got.RemoteAddress != "198.51.100.7" || got.Channel != "sms" {
		t.Fatalf("applied %+v", got)
	}
	want := webhook.Event{URL: "https://hooks.example.com/a", Secret: "callback-secret-a", MessageID: msgA, Channel: "sms", MessageStatus: 1,
		StatusText: "sms_delivered", Recipient: 359888000100, From: "Fixture", Timestamp: 1700000000}
	if len(f.q.events) != 1 || f.q.events[0] != want {
		t.Fatalf("callbacks %+v", f.q.events)
	}
	if len(f.rec.latency) != 1 || f.rec.latency[0] != "voicecom" || len(f.rec.received) != 1 || len(f.rec.rejected) != 0 {
		t.Fatalf("metrics %+v", f.rec)
	}
}

func TestOtherTenantUsesItsOwnToken(t *testing.T) {
	f := newFixture(t)
	if r := f.p.Process(context.Background(), receipt(msgB, tokA, 8)); r != BadToken {
		t.Fatalf("tenant A token accepted for a tenant B message: %q", r)
	}
	if r := f.p.Process(context.Background(), receipt(msgB, tokB, 8)); r != Accepted {
		t.Fatalf("own token rejected: %q", r)
	}
	if f.store.applied[0].TenantID != tenantB || len(f.store.clientGet) != 1 || f.store.clientGet[0] != (key{tenantB, 6}) {
		t.Fatalf("tenant not taken from the message: %+v %+v", f.store.applied, f.store.clientGet)
	}
	if len(f.q.events) != 0 {
		t.Fatal("client without a callback URL was called back")
	}
}

func TestRejectedReceiptsChangeNothing(t *testing.T) {
	for name, c := range map[string]struct {
		in     Receipt
		reason string
	}{
		"wrong token":         {receipt(msgA, strings.Repeat("x", 32), 16), BadToken},
		"missing token":       {receipt(msgA, "", 16), BadToken},
		"token with suffix":   {receipt(msgA, tokA+"junk", 16), BadToken},
		"token prefix":        {receipt(msgA, tokA[:31], 16), BadToken},
		"unknown message":     {receipt("00000000-0000-0000-0000-000000000000", tokA, 1), UnknownMessage},
		"empty request id":    {receipt("", tokA, 1), UnknownMessage},
		"not a uuid":          {receipt("'; DROP TABLE sms_dlr; --", tokA, 1), UnknownMessage},
		"status out of range": {receipt(msgA, tokA, 1<<31), Malformed},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			if r := f.p.Process(context.Background(), c.in); r != c.reason {
				t.Fatalf("reason %q, want %q", r, c.reason)
			}
			if len(f.store.applied) != 0 || len(f.store.clientGet) != 0 || len(f.q.events) != 0 {
				t.Fatal("rejected receipt had an effect")
			}
			if len(f.rec.rejected) != 1 || f.rec.rejected[0] != c.reason {
				t.Fatalf("rejection metric %v", f.rec.rejected)
			}
			if c.reason == BadToken {
				a := f.rec.audits
				if len(a) != 1 || a[0].TenantID != tenantA || a[0].Action != audit.ReceiptRejected || a[0].TargetID != msgA || a[0].Outcome != audit.Denied {
					t.Fatalf("audit %+v", a)
				}
				if a[0].Validate() != nil {
					t.Fatal("audit event outside the vocabulary")
				}
			}
		})
	}
}

func TestProviderWithoutTokenAcceptsAny(t *testing.T) {
	f := newFixture(t)
	for _, tok := range []string{"anything", ""} {
		if r := f.p.Process(context.Background(), receipt(msgOld, tok, 1)); r != Accepted {
			t.Fatalf("legacy provider rejected %q: %s", tok, r)
		}
	}
	if len(f.q.events) != 2 || f.q.events[0].Secret != "" || f.q.events[0].URL != "https://hooks.example.com/old" {
		t.Fatalf("unsigned callback %+v", f.q.events)
	}
}

func TestOperatorMessagesHaveNoCallback(t *testing.T) {
	f := newFixture(t)
	if r := f.p.Process(context.Background(), receipt(msgOp, tokA, 8)); r != Accepted {
		t.Fatal(r)
	}
	if len(f.store.applied) != 1 || len(f.store.clientGet) != 0 || len(f.q.events) != 0 {
		t.Fatal("operator message called back")
	}
}

func TestUnknownStatusIsStoredWithEmptyText(t *testing.T) {
	f := newFixture(t)
	f.p.Process(context.Background(), receipt(msgA, tokA, 77))
	if f.store.applied[0].StatusText != "" || f.q.events[0].StatusText != "" || len(f.rec.latency) != 0 {
		t.Fatalf("unknown code %+v", f.store.applied[0])
	}
}

func TestTamperedProviderConfigFailsClosed(t *testing.T) {
	f := newFixture(t)
	p := f.store.providers[key{tenantA, 1}]
	p.ConfigSealed = f.store.providers[key{tenantB, 1}].ConfigSealed // sealed for another tenant
	f.store.providers[key{tenantA, 1}] = p
	if r := f.p.Process(context.Background(), receipt(msgA, tokB, 1)); r != Unavailable {
		t.Fatalf("reason %q", r)
	}
	if len(f.store.applied) != 0 {
		t.Fatal("receipt applied under an unreadable provider")
	}
}

func TestTokenComparison(t *testing.T) {
	if !tokenMatches("", "x") || !tokenMatches("abc", "abc") || tokenMatches("abc", "abcd") || tokenMatches("abc", "ab") || tokenMatches("abc", "") {
		t.Fatal("token comparison")
	}
}
