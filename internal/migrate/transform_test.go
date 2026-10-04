package migrate

import (
	"bytes"
	"compress/gzip"
	"strings"
	"testing"
	"time"
)

func ptr[T any](v T) *T { return &v }

func gz(t *testing.T, s string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	_, _ = w.Write([]byte(s))
	_ = w.Close()
	return b.Bytes()
}

const carrierToken = "unit-carrier-token-not-production"
const dlrToken = "unitdlrtoken0123456789abcdef0123"

func snapshot(t *testing.T) *Snapshot {
	t0 := time.Date(2025, 1, 2, 3, 4, 5, 123456000, time.UTC)
	return &Snapshot{
		Clients: []LegacyClient{
			{ID: 1, CreateTime: &t0, UpdateTime: &t0, Username: "client_a", PasswordHash: "$2a$04$hash", Authority: "API_CLIENT", Status: "ON",
				CallbackURL: ptr("https://hooks.example.com/a"), CallbackSecret: ptr("cb-secret")},
			{ID: 4, CreateTime: &t0, UpdateTime: &t0, Username: "admin_x", PasswordHash: "$2a$04$hash", Authority: "API_ADMIN", Status: "ON"},
		},
		Providers: []LegacyProvider{
			{ID: 10, CreateTime: &t0, UpdateTime: &t0, Name: "main", Type: "voicecom", ObjectType: "OBJECT_TYPE_SMS", Status: "ON", RetentionDays: 30,
				Config: []byte(`{"url": "https://carrier.invalid/send", "token": "` + carrierToken + `", "dlr_token": "` + dlrToken + `", "callback_url": "https://gw.example.com/dlr?dlr_token=` + dlrToken + `"}`)},
			{ID: 12, CreateTime: &t0, UpdateTime: &t0, Name: "viber", Type: "carrier-x", ObjectType: "OBJECT_TYPE_VIBER", Status: "ON"},
		},
		Templates: []LegacyTemplate{{ID: 20, CreateTime: &t0, UpdateTime: &t0, Name: "hello", ObjectType: "OBJECT_TYPE_SMS", Templates: []byte(`{"body": "hi"}`), Status: "ON"}},
		Blocks: []LegacyBlock{
			{ID: 30, CreateBy: ptr(int64(5)), CreateTime: &t0, UpdateTime: &t0, Recipient: "359888000301", ProviderID: ptr(int64(10)), BlockType: "OBJECT_TYPE_SMS", Status: "ON"},
			{ID: 31, CreateTime: &t0, UpdateTime: &t0, DeleteTime: &t0, Recipient: "359888000302", ProviderID: ptr(int64(0)), BlockType: "OBJECT_TYPE_SMS", Status: "ON"},
		},
		Messages: []LegacyMessage{
			{ID: "00000000-0000-4000-8000-000000000101", CreateBy: ptr(int64(1)), CreateTime: &t0, UpdateTime: &t0, Recipient: "359888000100", Priority: 2,
				ProviderID: 10, TemplateID: 20, StatusCode: 1, Message: "hi", StatusMessage: ptr("sms_delivered"), Data: []byte(`{"sms": {}}`),
				RawRequest:  gz(t, `POST /send HTTP/1.1\r\n\r\n{"token":"`+carrierToken+`","callback_url":"https://gw.example.com/dlr?dlr_token=`+dlrToken+`"}`),
				RawResponse: []byte(`HTTP/1.1 200 OK` + "\r\n\r\n" + `{"return_code":0}`)},
			{ID: "00000000-0000-4000-8000-000000000103", CreateBy: ptr(int64(0)), CreateTime: &t0, UpdateTime: nil, Recipient: "359888000102", ProviderID: 10,
				TemplateID: 0, StatusCode: 0, Message: "admin"},
		},
		Receipts: []LegacyReceipt{{ID: 40, CreateTime: &t0, UpdateTime: &t0, Channel: "sms", MessageStatus: 1, Recipient: 359888000100, PartsReceived: 2,
			MessageID: ptr("00000000-0000-4000-8000-000000000101")}},
		Logins: []LegacyLogin{
			{ID: 50, ClientID: ptr(int64(1)), Username: "client_a", EventTime: t0, Success: true},
			{ID: 52, ClientID: ptr(int64(0)), Username: "nobody", EventTime: t0},
			{ID: 54, ClientID: ptr(int64(9)), Username: "removed", EventTime: t0},
		},
	}
}

func TestTransformPreservesAndMaps(t *testing.T) {
	r, f := Transform(snapshot(t), Options{PlatformActor: "legacy-admin"})
	if len(f.Errors) != 0 {
		t.Fatalf("errors: %+v", f.Errors)
	}
	if c := r.Counts(); c[EntClients] != 2 || c[EntProviders] != 2 || c[EntTemplates] != 1 || c[EntBlocks] != 2 || c[EntMessages] != 2 || c[EntReceipts] != 1 || c[EntLogins] != 3 {
		t.Fatalf("counts %v", c)
	}
	if r.Clients[0].ID != 1 || r.Clients[0].PasswordHash != "$2a$04$hash" || r.Clients[0].CallbackSecret != "cb-secret" {
		t.Fatalf("client %+v", r.Clients[0])
	}
	if r.Providers[0].Config["token"] != carrierToken || r.Providers[0].RetentionDays != 30 {
		t.Fatal("provider configuration must be carried in full (sealed on write)")
	}
	if r.Blocks[0].ProviderID == nil || *r.Blocks[0].ProviderID != 10 || r.Blocks[0].CreatedBy != "legacy:5" {
		t.Fatalf("block %+v", r.Blocks[0])
	}
	if r.Blocks[1].ProviderID != nil || r.Blocks[1].CreatedBy != "" {
		t.Fatalf("provider 0 must become NULL: %+v", r.Blocks[1])
	}
	m := r.Messages[0]
	if m.ActorKind != "api_client" || *m.APIClientID != 1 || *m.TemplateID != 20 || m.StatusCode != 1 || m.StatusMessage != "sms_delivered" {
		t.Fatalf("message %+v", m)
	}
	for _, ev := range [][]byte{m.RawRequest, m.RawResponse} {
		if bytes.Contains(ev, []byte(carrierToken)) || bytes.Contains(ev, []byte(dlrToken)) {
			t.Fatalf("evidence not scrubbed: %s", ev)
		}
	}
	if !bytes.Contains(m.RawRequest, []byte(`"token":"[REDACTED]"`)) || !bytes.Contains(m.RawRequest, []byte("dlr_token=[REDACTED]")) {
		t.Fatalf("evidence not decompressed/scrubbed: %s", m.RawRequest)
	}
	admin := r.Messages[1]
	if admin.ActorKind != "platform" || *admin.PlatformActor != "legacy-admin" || admin.APIClientID != nil || admin.TemplateID != nil {
		t.Fatalf("admin send %+v", admin)
	}
	if !admin.UpdateTime.Equal(admin.CreateTime) {
		t.Fatal("missing update_time must take create_time")
	}
	if !r.Logins[0].Resolved || r.Logins[1].Resolved || r.Logins[2].Resolved || r.Logins[2].ClientID != nil {
		t.Fatalf("logins %+v", r.Logins)
	}
	want := map[string]int{"zero_provider_blocks_to_null": 1, "zero_template_messages_to_null": 1, "admin_sends_to_platform_actor": 1,
		"evidence_decompressed": 1, "evidence_scrubbed": 1, "callback_secrets_sealed": 1, "provider_configs_sealed": 2, "unresolved_logins": 1,
		"logins_of_missing_clients_unresolved": 1, "soft_deleted_rows_imported": 1, "timestamps_defaulted": 1}
	for k, v := range want {
		if f.Transformations[k] != v {
			t.Errorf("transformation %s = %d, want %d (%v)", k, f.Transformations[k], v, f.Transformations)
		}
	}
	joined := strings.Join(f.Unsupported, "\n")
	for _, s := range []string{"API_ADMIN", "Viber", `"carrier-x"`} {
		if !strings.Contains(joined, s) {
			t.Errorf("unsupported data not reported: %s in %q", s, joined)
		}
	}
}

func TestTransformRequiresPlatformActorForAdminSends(t *testing.T) {
	_, f := Transform(snapshot(t), Options{})
	if len(f.Errors) != 1 || f.Errors[0].Field != "create_by" || f.Errors[0].ID != "00000000-0000-4000-8000-000000000103" {
		t.Fatalf("errors %+v", f.Errors)
	}
}

func TestTransformOrphans(t *testing.T) {
	s := snapshot(t)
	s.Receipts = append(s.Receipts, LegacyReceipt{ID: 41, Channel: "sms", MessageStatus: 2, PartsReceived: 1},
		LegacyReceipt{ID: 44, Channel: "sms", MessageStatus: 2, PartsReceived: 1, MessageID: ptr("00000000-0000-4000-8000-0000000009ff")})
	s.Messages = append(s.Messages, LegacyMessage{ID: "00000000-0000-4000-8000-000000000199", CreateBy: ptr(int64(1)), Recipient: "359", ProviderID: 77, Message: "x"})
	s.Blocks = append(s.Blocks, LegacyBlock{ID: 39, Recipient: "1", ProviderID: ptr(int64(78)), BlockType: "OBJECT_TYPE_SMS", Status: "ON"})
	_, f := Transform(s, Options{PlatformActor: "legacy-admin"})
	if len(f.Errors) != 4 || len(f.Excluded) != 0 {
		t.Fatalf("orphans must fail by default: %+v", f.Errors)
	}
	r, f := Transform(s, Options{PlatformActor: "legacy-admin", ExcludeOrphans: true})
	if len(f.Errors) != 0 || len(f.Excluded) != 4 || len(r.Receipts) != 1 || len(r.Messages) != 2 || len(r.Blocks) != 2 {
		t.Fatalf("excluded %+v errors %+v", f.Excluded, f.Errors)
	}
}

func TestTransformRejectsWhatV4CannotStore(t *testing.T) {
	s := snapshot(t)
	s.Clients[0].Username = "a b"
	s.Providers[0].Config = []byte(`{"priority": 2}`)
	s.Messages[0].Recipient = "+359"
	s.Messages[1].ID = "not-a-uuid"
	_, f := Transform(s, Options{PlatformActor: "legacy-admin"})
	fields := map[string]bool{}
	for _, e := range f.Errors {
		fields[e.Entity+"."+e.Field] = true
	}
	for _, k := range []string{"sms_api_client.username", "sms_provider.config", "sms_message.recipient", "sms_message.id"} {
		if !fields[k] {
			t.Errorf("missing error for %s: %+v", k, f.Errors)
		}
	}
}

func TestFingerprintIsContentBound(t *testing.T) {
	a, b := snapshot(t), snapshot(t)
	loc := time.FixedZone("x", 3600)
	t0 := b.Clients[0].CreateTime.In(loc)
	b.Clients[0].CreateTime = &t0
	if a.Fingerprint() != b.Fingerprint() || len(a.Fingerprint()) != 64 {
		t.Fatal("fingerprint must not depend on the time zone")
	}
	b.Messages[0].StatusCode = 2
	if a.Fingerprint() == b.Fingerprint() {
		t.Fatal("fingerprint must change with the content")
	}
}

func TestSourceConfigTenant(t *testing.T) {
	c := SourceConfig{Tenants: map[string]string{"fixture-tenant": "0b2f6a1e-4c55-4c8e-9d1a-000000000a0a"}}
	if id, err := c.Tenant("fixture-tenant"); err != nil || id != "0b2f6a1e-4c55-4c8e-9d1a-000000000a0a" {
		t.Fatal(id, err)
	}
	for _, bad := range []string{"", "unknown", "0B2F6A1E-4C55-4C8E-9D1A-000000000A0A"} {
		if _, err := c.Tenant(bad); err == nil {
			t.Fatalf("tenant %q accepted", bad)
		}
	}
}
