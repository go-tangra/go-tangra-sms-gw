package sms

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/hermes"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/provider"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/repo"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/sealed"
)

const (
	tenantA = "0b2f6a1e-4c55-4c8e-9d1a-000000000a0a"
	tenantB = "0b2f6a1e-4c55-4c8e-9d1a-000000000b0b"
	secret  = "carrier-secret-token-value"
)

// fakeCarrier is a registered provider type whose behaviour each test sets.
type fakeCarrier struct {
	mu    sync.Mutex
	calls []string
	reply func(ctx context.Context) provider.Response
}

var carrier = &fakeCarrier{}

func init() {
	provider.Register(provider.TypeMeta{Type: "fake", Fields: []provider.Field{{Key: "token", Type: "secret"}}},
		func(cfg map[string]string) (provider.Sender, error) { return fakeSender{cfg}, nil })
}

type fakeSender struct{ cfg map[string]string }

func (fakeSender) Type() string     { return "fake" }
func (fakeSender) Sid() uint32      { return 9999 }
func (fakeSender) Priority() uint32 { return 0 }
func (fakeSender) Send(ctx context.Context, _ provider.Request, id, text string) provider.Response {
	carrier.mu.Lock()
	carrier.calls = append(carrier.calls, id+"|"+text)
	reply := carrier.reply
	carrier.mu.Unlock()
	return reply(ctx)
}

func (c *fakeCarrier) reset(reply func(context.Context) provider.Response) {
	c.mu.Lock()
	c.calls, c.reply = nil, reply
	c.mu.Unlock()
}

func (c *fakeCarrier) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

func accepted(context.Context) provider.Response {
	return provider.Response{Carrier: &provider.CarrierResponse{ReturnCode: 0, ReturnMessage: "Message accepted",
		Channels: &provider.Channels{SMS: &provider.SMSChannel{SendOrder: 1, MessageParts: 1}}},
		RawRequest:  []byte(`POST / HTTP/1.1` + "\r\n\r\n" + `{"token":"` + secret + `","callback_url":"https://gw/dlr?dlr_token=dlrsecret0123"}`),
		RawResponse: []byte("HTTP/1.1 200 OK\r\n\r\n{}")}
}

// memStore is an in-memory Store with the repository's tenant semantics.
type memStore struct {
	mu         sync.Mutex
	providers  map[string]map[int64]repo.Provider
	templates  map[string]map[int64]repo.Template
	blocked    map[string]bool // tenant|provider|recipient
	blockErr   error
	messages   []repo.Message
	responses  []repo.ProviderResponse
	createFail error
}

func (m *memStore) GetProvider(_ context.Context, tenant string, id int64) (repo.Provider, error) {
	if p, ok := m.providers[tenant][id]; ok {
		return p, nil
	}
	return repo.Provider{}, repo.ErrNotFound
}

func (m *memStore) GetTemplate(_ context.Context, tenant string, id int64) (repo.Template, error) {
	if t, ok := m.templates[tenant][id]; ok {
		return t, nil
	}
	return repo.Template{}, repo.ErrNotFound
}

func (m *memStore) IsBlocked(_ context.Context, tenant string, providerID int64, recipient string) (bool, error) {
	if m.blockErr != nil {
		return false, m.blockErr
	}
	return m.blocked[tenant+"|"+recipient] || m.blocked[tenant+"|"+itoa(providerID)+"|"+recipient], nil
}

func (m *memStore) CreateMessage(_ context.Context, msg repo.Message) (repo.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.createFail != nil {
		return repo.Message{}, m.createFail
	}
	if _, ok := m.providers[msg.TenantID][msg.ProviderID]; !ok {
		return repo.Message{}, repo.ErrReference
	}
	m.messages = append(m.messages, msg)
	return msg, nil
}

func (m *memStore) SetProviderResponse(_ context.Context, tenant, id string, r repo.ProviderResponse) (repo.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, msg := range m.messages {
		if msg.TenantID == tenant && msg.ID == id {
			m.responses = append(m.responses, r)
			msg.RawRequest, msg.RawResponse = r.RawRequest, r.RawResponse
			if msg.StatusCode == -1 {
				msg.StatusCode, msg.StatusMessage = r.StatusCode, r.StatusMessage
			}
			m.messages[i] = msg
			return msg, nil
		}
	}
	return repo.Message{}, repo.ErrNotFound
}

func (m *memStore) GetMessage(context.Context, repo.View, string) (repo.Message, error) {
	return repo.Message{}, repo.ErrNotFound
}

func (m *memStore) ListMessages(context.Context, repo.View, repo.MessageFilter, repo.Page) (repo.List[repo.Message], error) {
	return repo.List[repo.Message]{}, nil
}

func (m *memStore) ListReceipts(context.Context, repo.View, string) (repo.List[repo.Receipt], error) {
	return repo.List[repo.Receipt]{}, nil
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

type fixture struct {
	store *memStore
	svc   *Service
	env   *sealed.Envelope
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	env, err := sealed.NewEnvelope([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	seal := func(tenant string, id int64, cfg sealed.Config) []byte {
		b, err := env.SealConfig(cfg, sealed.ProviderAD(tenant, id))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	cfg := sealed.Config{"token": secret, "dlr_token": "dlrsecret0123"}
	st := &memStore{
		providers: map[string]map[int64]repo.Provider{
			tenantA: {
				1: {ID: 1, TenantID: tenantA, Name: "fake-a", Type: "fake", ObjectType: repo.ObjectSMS, Status: repo.On, ConfigSealed: seal(tenantA, 1, cfg)},
				2: {ID: 2, TenantID: tenantA, Name: "fake-off", Type: "fake", ObjectType: repo.ObjectSMS, Status: repo.Off, ConfigSealed: seal(tenantA, 2, cfg)},
				3: {ID: 3, TenantID: tenantA, Name: "fake-viber", Type: "fake", ObjectType: repo.ObjectViber, Status: repo.On, ConfigSealed: seal(tenantA, 3, cfg)},
				4: {ID: 4, TenantID: tenantA, Name: "nobody-knows", Type: "carrier-x", ObjectType: repo.ObjectSMS, Status: repo.On, ConfigSealed: seal(tenantA, 4, cfg)},
				// Sealed for another row: it must not open as provider 5.
				5: {ID: 5, TenantID: tenantA, Name: "moved", Type: "fake", ObjectType: repo.ObjectSMS, Status: repo.On, ConfigSealed: seal(tenantA, 1, cfg)},
			},
			tenantB: {9: {ID: 9, TenantID: tenantB, Name: "fake-b", Type: "fake", ObjectType: repo.ObjectSMS, Status: repo.On, ConfigSealed: seal(tenantB, 9, cfg)}},
		},
		templates: map[string]map[int64]repo.Template{
			tenantA: {
				1: {ID: 1, TenantID: tenantA, Name: "hello", ObjectType: repo.ObjectSMS, Status: repo.On, Templates: map[string]string{"body": "Hello {{ .name }}, code {{ .code }}."}},
				2: {ID: 2, TenantID: tenantA, Name: "off", ObjectType: repo.ObjectSMS, Status: repo.Off, Templates: map[string]string{"body": "off"}},
				3: {ID: 3, TenantID: tenantA, Name: "viber", ObjectType: repo.ObjectViber, Status: repo.On, Templates: map[string]string{"body": "v"}},
				4: {ID: 4, TenantID: tenantA, Name: "nobody", ObjectType: repo.ObjectSMS, Status: repo.On, Templates: map[string]string{"subject": "x"}},
				5: {ID: 5, TenantID: tenantA, Name: "badsyntax", ObjectType: repo.ObjectSMS, Status: repo.On, Templates: map[string]string{"body": "Hello {{ .name"}},
				6: {ID: 6, TenantID: tenantA, Name: "huge", ObjectType: repo.ObjectSMS, Status: repo.On, Templates: map[string]string{"body": `{{ printf "%999999999d" 1 }}`}},
			},
			tenantB: {7: {ID: 7, TenantID: tenantB, Name: "hello", ObjectType: repo.ObjectSMS, Status: repo.On, Templates: map[string]string{"body": "Hi"}}},
		},
		blocked: map[string]bool{tenantA + "|1|359888000301": true, tenantA + "|359888000302": true},
	}
	carrier.reset(accepted)
	return &fixture{store: st, env: env, svc: New(Config{Store: st, Envelope: env, Senders: provider.NewCache(), Policy: RecipientPolicy{MinDigits: 7}})}
}

func (f *fixture) send(req Request) (Result, error) {
	return f.svc.Send(context.Background(), tenantA, repo.ClientActor(1), req)
}

func ok() Request {
	return Request{To: 359888000100, ProviderID: 1, TemplateID: 1, Properties: map[string]string{"name": "Alice", "code": "8421"},
		SMS: &provider.SMS{From: "Capture", Encoding: "utf-8"}}
}

func wantErr(t *testing.T, err error, code int, reason, message string) {
	t.Helper()
	var e *hermes.Error
	if !errors.As(err, &e) || e.Code != code || e.Reason != reason || e.Message != message {
		t.Fatalf("error %#v, want %d %q %q", err, code, reason, message)
	}
}

func TestSendAccepted(t *testing.T) {
	f := newFixture(t)
	res, err := f.send(ok())
	if err != nil {
		t.Fatal(err)
	}
	if res.Message.StatusCode != 0 || res.Message.StatusMessage != "sms_provider_accepted" || res.Message.Text != "Hello Alice, code 8421." ||
		res.Carrier == nil || res.Carrier.ReturnMessage != "Message accepted" {
		t.Fatalf("result %+v", res)
	}
	if len(f.store.messages) != 1 || carrier.count() != 1 {
		t.Fatalf("messages %d carrier calls %d", len(f.store.messages), carrier.count())
	}
	m := f.store.messages[0]
	if m.TenantID != tenantA || m.Actor != repo.ClientActor(1) || m.Sid != 9999 || m.Priority != 2 || m.Recipient != "359888000100" ||
		m.ProviderID != 1 || m.TemplateID == nil || *m.TemplateID != 1 || len(m.ID) != 36 {
		t.Fatalf("persisted %+v", m)
	}
	var data map[string]any
	if err := json.Unmarshal(m.Data, &data); err != nil || data["sms"].(map[string]any)["from"] != "Capture" ||
		data["properties"].(map[string]any)["code"] != "8421" {
		t.Fatalf("data %s", m.Data)
	}
	// The carrier got the persisted UUID as request id before any response.
	if !strings.HasPrefix(carrier.calls[0], m.ID+"|") {
		t.Fatal("carrier request id is not the stored message id")
	}
	// Raw evidence is stored without the carrier token or the receipt token.
	r := f.store.responses[0]
	for _, s := range []string{secret, "dlrsecret0123"} {
		if strings.Contains(string(r.RawRequest), s) {
			t.Fatalf("credential %q in stored evidence: %s", s, r.RawRequest)
		}
	}
}

func TestSendRejectsBeforePersistOrCarrier(t *testing.T) {
	for _, c := range []struct {
		name            string
		mutate          func(*Request)
		code            int
		reason, message string
	}{
		{"provider zero", func(r *Request) { r.ProviderID = 0 }, 400, "BAD_REQUEST", "providerId is required"},
		{"recipient zero", func(r *Request) { r.To = 0 }, 400, "BAD_REQUEST", "recipient (to) is required"},
		{"recipient short", func(r *Request) { r.To = 123456 }, 400, "BAD_REQUEST", "recipient 123456 has an invalid length (expected 7-15 digits)"},
		{"recipient long", func(r *Request) { r.To = 18446744073709551615 }, 400, "BAD_REQUEST", "recipient 18446744073709551615 has an invalid length (expected 7-15 digits)"},
		{"provider missing", func(r *Request) { r.ProviderID = 99 }, 404, "RECORD_NOT_FOUND", "provider not found"},
		{"provider of another tenant", func(r *Request) { r.ProviderID = 9 }, 404, "RECORD_NOT_FOUND", "provider not found"},
		{"provider disabled", func(r *Request) { r.ProviderID = 2 }, 400, "BAD_REQUEST", "provider fake-off is inactive"},
		{"provider viber", func(r *Request) { r.ProviderID = 3 }, 400, "BAD_REQUEST", "provider not allowed for this service: OBJECT_TYPE_VIBER"},
		{"blocked for provider", func(r *Request) { r.To = 359888000301 }, 400, "BAD_REQUEST", "recipient 359888000301 is blocked for provider fake-a"},
		{"blocked for every provider", func(r *Request) { r.To = 359888000302 }, 400, "BAD_REQUEST", "recipient 359888000302 is blocked for provider fake-a"},
		// The block is checked before the template is rendered.
		{"blocked before render", func(r *Request) { r.To = 359888000302; r.TemplateID = 5 }, 400, "BAD_REQUEST", "recipient 359888000302 is blocked for provider fake-a"},
		{"template zero", func(r *Request) { r.TemplateID = 0 }, 400, "BAD_REQUEST", "templateId is required"},
		{"template missing", func(r *Request) { r.TemplateID = 99 }, 404, "RECORD_NOT_FOUND", "template not found"},
		{"template of another tenant", func(r *Request) { r.TemplateID = 7 }, 404, "RECORD_NOT_FOUND", "template not found"},
		{"template disabled", func(r *Request) { r.TemplateID = 2 }, 400, "BAD_REQUEST", "template off is inactive"},
		{"template viber", func(r *Request) { r.TemplateID = 3 }, 400, "BAD_REQUEST", "template not allowed for this service: OBJECT_TYPE_VIBER"},
		{"template without body", func(r *Request) { r.TemplateID = 4 }, 400, "BAD_REQUEST", `template missing fragment "body"`},
		{"template syntax", func(r *Request) { r.TemplateID = 5 }, 500, "INTERNAL_ERROR", "parse template: template: badsyntax:1: unclosed action"},
		{"template output bound", func(r *Request) { r.TemplateID = 6 }, 500, "INTERNAL_ERROR", "execute template: template output exceeds the limit"},
		{"too many properties", func(r *Request) {
			r.Properties = map[string]string{}
			for i := 0; i <= MaxProperties; i++ {
				r.Properties[itoa(int64(i))] = "x"
			}
		}, 400, "BAD_REQUEST", "too many template properties"},
		{"property too long", func(r *Request) { r.Properties = map[string]string{"name": strings.Repeat("x", MaxPropertyBytes+1)} }, 400, "BAD_REQUEST", "template property name is too long"},
		{"unknown provider type", func(r *Request) { r.ProviderID = 4 }, 500, "INTERNAL_ERROR", `provider init: provider "carrier-x" not registered`},
		{"sealed config of another row", func(r *Request) { r.ProviderID = 5 }, 500, "INTERNAL_ERROR", "provider init: configuration unavailable"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			req := ok()
			c.mutate(&req)
			_, err := f.send(req)
			wantErr(t, err, c.code, c.reason, c.message)
			if len(f.store.messages) != 0 || carrier.count() != 0 {
				t.Fatalf("rejected send persisted %d messages, %d carrier calls", len(f.store.messages), carrier.count())
			}
		})
	}
}

func TestRecipientPolicy(t *testing.T) {
	f := newFixture(t)
	f.svc.policy = RecipientPolicy{MinDigits: 7, Allowed: []string{"359"}, Blocked: []string{"3599"}}
	req := ok()
	req.To = 359900000000
	_, err := f.send(req)
	wantErr(t, err, 403, "FORBIDDEN", "destination 359900000000 is not permitted")
	req.To = 441234567890
	_, err = f.send(req)
	wantErr(t, err, 403, "FORBIDDEN", "destination 441234567890 is outside the permitted regions")
	if len(f.store.messages) != 0 || carrier.count() != 0 {
		t.Fatal("policy rejection persisted")
	}
}

func TestBlockCheckFailureFailsClosed(t *testing.T) {
	f := newFixture(t)
	f.store.blockErr = errors.New("connection reset")
	_, err := f.send(ok())
	wantErr(t, err, 503, "DB_UNAVAILABLE", "storage unavailable")
	if len(f.store.messages) != 0 || carrier.count() != 0 {
		t.Fatal("send continued without a block decision")
	}
}

func TestMissingPropertyRendersLegacyPlaceholder(t *testing.T) {
	f := newFixture(t)
	req := ok()
	req.Properties = map[string]string{"name": "Bob"}
	res, err := f.send(req)
	if err != nil || res.Message.Text != "Hello Bob, code <no value>." {
		t.Fatalf("%q %v", res.Message.Text, err)
	}
	req.Properties = nil
	if res, err = f.send(req); err != nil || res.Message.Text != "Hello <no value>, code <no value>." {
		t.Fatalf("%q %v", res.Message.Text, err)
	}
}

func TestCarrierOutcomes(t *testing.T) {
	for _, c := range []struct {
		name       string
		reply      provider.Response
		code       int32
		text       string
		errMessage string
	}{
		{"reject code", provider.Response{Carrier: &provider.CarrierResponse{ReturnCode: 2001, ReturnMessage: "Invalid SID"}}, 2001, "sms_invalid_sid", ""},
		{"unknown code keeps carrier text", provider.Response{Carrier: &provider.CarrierResponse{ReturnCode: 7777, ReturnMessage: "Odd"}}, 7777, "Odd", ""},
		{"http failure", provider.Response{Carrier: &provider.CarrierResponse{}, Err: errors.New("voicecom: http 500 Internal Server Error")}, 500,
			"voicecom: http 500 Internal Server Error", "voicecom: http 500 Internal Server Error"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			carrier.reset(func(context.Context) provider.Response { return c.reply })
			res, err := f.send(ok())
			if c.errMessage != "" {
				wantErr(t, err, 500, "", c.errMessage)
			} else if err != nil || res.Carrier == nil || res.Carrier.ReturnCode != c.code {
				t.Fatalf("%+v %v", res, err)
			}
			m := f.store.messages[0]
			if m.StatusCode != c.code || m.StatusMessage != c.text || carrier.count() != 1 {
				t.Fatalf("stored %d %q after %d calls", m.StatusCode, m.StatusMessage, carrier.count())
			}
		})
	}
}

// An uncertain carrier exchange (connection lost after submission) is
// recorded once and never resent; the client sees a sanitized error.
func TestUncertainCarrierResponseIsNotResent(t *testing.T) {
	f := newFixture(t)
	carrier.reset(func(context.Context) provider.Response {
		return provider.Response{Err: errors.New(`voicecom: endpoint error: Post "http://10.0.0.7:8080/send?token=` + secret + `": EOF`)}
	})
	_, err := f.send(ok())
	var e *hermes.Error
	if !errors.As(err, &e) || e.Code != 500 || e.Reason != "" || strings.Contains(e.Message, secret) || strings.Contains(e.Message, "10.0.0.7") {
		t.Fatalf("client error %#v", err)
	}
	if carrier.count() != 1 || len(f.store.messages) != 1 || len(f.store.responses) != 1 {
		t.Fatalf("carrier calls %d, messages %d", carrier.count(), len(f.store.messages))
	}
	if m := f.store.messages[0]; m.StatusCode != 500 || m.StatusMessage != e.Message {
		t.Fatalf("stored %d %q", m.StatusCode, m.StatusMessage)
	}
}

// A caller that goes away does not abort a submission already in flight:
// its outcome is still recorded.
func TestCarrierCallOutlivesTheRequest(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	carrier.reset(func(c context.Context) provider.Response {
		cancel()
		if c.Err() != nil {
			return provider.Response{Err: c.Err()}
		}
		return accepted(c)
	})
	if _, err := f.svc.Send(ctx, tenantA, repo.ClientActor(1), ok()); err != nil {
		t.Fatal(err)
	}
	if f.store.messages[0].StatusCode != 0 {
		t.Fatalf("status %d", f.store.messages[0].StatusCode)
	}
}

// A receipt that made the message terminal before the carrier answered is
// kept: the provider response does not overwrite it.
func TestProviderResponseKeepsEarlyTerminalStatus(t *testing.T) {
	f := newFixture(t)
	carrier.reset(func(c context.Context) provider.Response {
		f.store.mu.Lock()
		f.store.messages[0].StatusCode, f.store.messages[0].StatusMessage = 1, "sms_delivered"
		f.store.mu.Unlock()
		return accepted(c)
	})
	res, err := f.send(ok())
	if err != nil || res.Message.StatusCode != 1 || res.Message.StatusMessage != "sms_delivered" {
		t.Fatalf("%+v %v", res.Message, err)
	}
}

func TestStorageFailureBeforeCarrier(t *testing.T) {
	f := newFixture(t)
	f.store.createFail = errors.New("disk full at /var/lib/postgresql")
	_, err := f.send(ok())
	wantErr(t, err, 503, "DB_UNAVAILABLE", "storage unavailable")
	if carrier.count() != 0 {
		t.Fatal("carrier called without a stored message")
	}
}

func TestPlatformActorSend(t *testing.T) {
	f := newFixture(t)
	res, err := f.svc.Send(context.Background(), tenantA, repo.PlatformActor("4f0c0a52-1111-4222-8333-944455556666"), ok())
	if err != nil || res.Message.Actor.Kind != repo.ActorPlatform {
		t.Fatalf("%+v %v", res.Message.Actor, err)
	}
}
