package sealed

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const tenant = "0b2f6a1e-4c55-4c8e-9d1a-000000000a0a"

func envelope(t *testing.T) *Envelope {
	t.Helper()
	e, err := NewEnvelope(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestSealBindsTenantAndRow(t *testing.T) {
	e := envelope(t)
	cfg := Config{"url": "https://carrier.example/send", "token": "carrier-token-123", "dlr_token": strings.Repeat("a", 32)}
	blob, err := e.SealConfig(cfg, ProviderAD(tenant, 3))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(blob, []byte("carrier-token-123")) {
		t.Fatal("plaintext in sealed value")
	}
	got, err := e.OpenConfig(blob, ProviderAD(tenant, 3))
	if err != nil || got["token"] != "carrier-token-123" {
		t.Fatal(got, err)
	}
	for name, ad := range map[string][]byte{
		"other row":    ProviderAD(tenant, 4),
		"other tenant": ProviderAD("0b2f6a1e-4c55-4c8e-9d1a-000000000b0b", 3),
		"callback":     CallbackAD(tenant, 3),
	} {
		if _, err := e.OpenConfig(blob, ad); !errors.Is(err, ErrTampered) {
			t.Fatalf("%s opened: %v", name, err)
		}
	}
	blob[len(blob)-1] ^= 1
	if _, err := e.Open(blob, ProviderAD(tenant, 3)); !errors.Is(err, ErrTampered) {
		t.Fatal("tampered ciphertext opened")
	}
	other, _ := NewEnvelope(bytes.Repeat([]byte{8}, 32))
	blob[len(blob)-1] ^= 1
	if _, err := other.Open(blob, ProviderAD(tenant, 3)); !errors.Is(err, ErrTampered) {
		t.Fatal("opened with another KEK")
	}
	if _, err := e.Seal(make([]byte, MaxBytes+1), nil); !errors.Is(err, ErrTooLarge) {
		t.Fatal(err)
	}
	if _, err := e.Open([]byte("short"), nil); !errors.Is(err, ErrTampered) {
		t.Fatal(err)
	}
}

func TestSealString(t *testing.T) {
	e := envelope(t)
	if b, err := e.SealString("", CallbackAD(tenant, 1)); b != nil || err != nil {
		t.Fatal("empty secret stored")
	}
	b, _ := e.SealString("hook-secret", CallbackAD(tenant, 1))
	if s, err := e.OpenString(b, CallbackAD(tenant, 1)); s != "hook-secret" || err != nil {
		t.Fatal(s, err)
	}
	if s, err := e.OpenString(nil, CallbackAD(tenant, 1)); s != "" || err != nil {
		t.Fatal(s, err)
	}
}

func TestLoadKEK(t *testing.T) {
	dir := t.TempDir()
	key := bytes.Repeat([]byte{1}, 32)
	b64 := filepath.Join(dir, "b64")
	raw := filepath.Join(dir, "raw")
	bad := filepath.Join(dir, "bad")
	_ = os.WriteFile(b64, []byte(base64.StdEncoding.EncodeToString(key)+"\n"), 0o600)
	_ = os.WriteFile(raw, key, 0o600)
	_ = os.WriteFile(bad, []byte("short"), 0o600)
	for _, p := range []string{b64, raw} {
		if k, err := LoadKEK("file", p, ""); err != nil || !bytes.Equal(k, key) {
			t.Fatal(p, err)
		}
	}
	if _, err := LoadKEK("file", bad, ""); !errors.Is(err, ErrKEK) {
		t.Fatal(err)
	}
	if _, err := LoadKEK("file", filepath.Join(dir, "missing"), ""); err == nil {
		t.Fatal("missing file accepted")
	}
	t.Setenv("SMSGW_TEST_KEK", base64.RawStdEncoding.EncodeToString(key))
	if k, err := LoadKEK("env", "", "SMSGW_TEST_KEK"); err != nil || !bytes.Equal(k, key) {
		t.Fatal(err)
	}
	if _, err := LoadKEK("env", "", "SMSGW_TEST_KEK_UNSET"); err == nil {
		t.Fatal("empty env accepted")
	}
	if _, err := LoadKEK("inline", "", ""); err == nil {
		t.Fatal("unknown source accepted")
	}
	if _, err := NewEnvelope(key[:16]); !errors.Is(err, ErrKEK) {
		t.Fatal(err)
	}
}

func TestRedactMergePublic(t *testing.T) {
	secret := []string{"token", "dlr_token"}
	stored := Config{"url": "https://c", "token": "t1", "dlr_token": "d1"}
	if r := Redact(Config{"url": "u", "token": "t", "dlr_token": ""}, secret); r["token"] != Marker || r["url"] != "u" {
		t.Fatal(r)
	} else if _, ok := r["dlr_token"]; ok {
		t.Fatal("empty secret shown")
	}
	if p := Public(stored, secret); p["token"] != nil || p["dlr_token"] != nil || p["url"] != "https://c" {
		t.Fatal(p)
	}
	m := Merge(stored, Config{"url": "https://new", "token": Marker}, secret)
	if m["url"] != "https://new" || m["token"] != "t1" || m["dlr_token"] != "d1" {
		t.Fatalf("marker/omitted must keep stored: %v", m)
	}
	m = Merge(stored, Config{"url": "u", "token": "", "dlr_token": "d2"}, secret)
	if _, ok := m["token"]; ok || m["dlr_token"] != "d2" {
		t.Fatalf("clear/replace: %v", m)
	}
}

// A legacy carrier exchange as recorded by the runtime capture.
const legacyDump = "POST /multichannel-api/sendmulti/ HTTP/1.1\r\nHost: carrier.example\r\nAuthorization: Basic dXNlcjpwYXNz\r\n" +
	"Content-Type: application/json; charset=utf-8\r\n\r\n{\"sid\":9999,\"request_id\":\"1b4e\",\"to\":359888000222," +
	"\"token\":\"carrier-token-123\",\"callback_url\":\"https://gw.example/dlr?dlr_token=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\"," +
	"\"sms\":{\"text\":\"Hello\",\"from\":\"Capture\"}}"

func TestEvidenceKeepsNamesDropsValues(t *testing.T) {
	out := string(Evidence([]byte(legacyDump), "carrier-token-123", strings.Repeat("a", 32)))
	for _, leak := range []string{"carrier-token-123", "dXNlcjpwYXNz", strings.Repeat("a", 32)} {
		if strings.Contains(out, leak) {
			t.Fatalf("leaked %q in %s", leak, out)
		}
	}
	for _, kept := range []string{`"token":"[REDACTED]"`, "Authorization: [REDACTED]", "dlr_token=[REDACTED]", `"text":"Hello"`, `"to":359888000222`} {
		if !strings.Contains(out, kept) {
			t.Fatalf("missing %q in %s", kept, out)
		}
	}
	// Patterns alone (no known secret) still remove credentials.
	if out := string(Evidence([]byte(legacyDump))); strings.Contains(out, "carrier-token-123") || strings.Contains(out, strings.Repeat("a", 32)) {
		t.Fatal(out)
	}
	if Evidence(nil) != nil {
		t.Fatal("nil evidence changed")
	}
}

func TestScrubAndURL(t *testing.T) {
	got := Scrub("voicecom: endpoint error: Post \"https://c.example/send?token=abc\": token carrier-token-123\nsecond line", "carrier-token-123")
	if strings.Contains(got, "carrier-token-123") || strings.Contains(got, "abc") || strings.Contains(got, "second line") {
		t.Fatal(got)
	}
	if len(Scrub(strings.Repeat("x", 2000))) != 512 {
		t.Fatal("not bounded")
	}
	u := URL("https://user:pw@gw.example/dlr?dlr_token=abcdef&channel=sms")
	if strings.Contains(u, "pw@") || strings.Contains(u, "abcdef") || !strings.Contains(u, "channel=sms") {
		t.Fatal(u)
	}
	if URL("https://gw.example/dlr?x=1") != "https://gw.example/dlr?x=1" || URL("") != "" || URL("http://[::1") != Redacted {
		t.Fatal("URL redaction changed safe input")
	}
}

func TestSecretNeverPrints(t *testing.T) {
	s := Secret("hunter22-very-secret")
	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("x", "s", s)
	j, _ := json.Marshal(struct{ S Secret }{s})
	for _, out := range []string{fmt.Sprint(s), fmt.Sprintf("%v %+v %#v %s", s, s, s, s), logs.String(), string(j)} {
		if strings.Contains(out, "hunter22") {
			t.Fatalf("secret printed: %s", out)
		}
	}
	if s.Reveal() != "hunter22-very-secret" || !s.Equal("hunter22-very-secret") {
		t.Fatal("secret lost")
	}
}

func TestReplaceAttr(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{ReplaceAttr: ReplaceAttr}))
	log.Info("request", "password", "pw-123456", "Authorization", "Bearer abc.def.ghi", "body", `{"to":1}`, "raw_request", "POST /",
		"callback_url", "https://h/dlr?dlr_token=tok-987654", "status", 200, "username", "client_a")
	out := logs.String()
	for _, leak := range []string{"pw-123456", "abc.def.ghi", `{\"to\":1}`, "POST /", "tok-987654"} {
		if strings.Contains(out, leak) {
			t.Fatalf("leaked %q: %s", leak, out)
		}
	}
	if !strings.Contains(out, `"status":200`) || !strings.Contains(out, `"username":"client_a"`) {
		t.Fatal(out)
	}
}
