//go:build parity

package parity

// The legacy end-to-end suite (go-tangra-sms-gw v3.0.9 e2e/) against V4.
// The legacy harness seeded the legacy tables over SQL; this one seeds V4's
// store (sealed provider configuration, tenant rows) with the same values
// and is otherwise the legacy harness: the HTTP and mock helpers below the
// marker are copied unchanged, and the tests (legacy_e2e_test.go) are the
// legacy tests. The legacy mock carrier delivers its receipts to V4.

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/repo"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/sealed"
)

// TestLegacyE2EAgainstV4 runs the legacy e2e tests against V4 with the
// legacy mock carrier delivering receipts (300 ms after each submission).
func TestLegacyE2EAgainstV4(t *testing.T) {
	repoDir := os.Getenv("SMSGW_PARITY_V3_SRC")
	if repoDir == "" {
		repoDir = filepath.Join(repoRoot(), "..", "go-tangra-sms-gw")
	}
	if !fileExists(filepath.Join(repoDir, "cmd", "server")) {
		t.Skipf("legacy checkout not found at %s (SMSGW_PARITY_V3_SRC)", repoDir)
	}
	ref := os.Getenv("SMSGW_PARITY_V3_REF")
	if ref == "" {
		ref = "v3.0.9"
	}
	s := &stack{t: t, dir: t.TempDir()}
	bin := pgBin(t)
	s.build(repoDir, ref)
	s.pg4 = startPG(t, bin, filepath.Join(s.dir, "pg4"))
	s.pg4.exec(t, "postgres", "CREATE DATABASE sms_gw", "CREATE ROLE smsgw_app LOGIN PASSWORD 'app' NOBYPASSRLS")
	s.startCarriers("--dlr-delay=300ms")
	s.prepareV4()
	gw := s.startV4(profile{name: "e2e", loginRPM: 1e6, loginBurst: 100000, sendRPM: 1e6, sendBurst: 100000})
	e2eCur = &e2eStack{s: s, env: e2eEnv{gwURL: gw, mockURL: s.carrier, mockInnerURL: s.carrier}}
	defer func() { e2eCur = nil }()
	for _, c := range []struct {
		name string
		f    func(*testing.T)
	}{
		{"HappyPath", e2eHappyPath}, {"DLRSpoofRejected", e2eDLRSpoofRejected}, {"BlockedRecipient", e2eBlockedRecipient},
		{"OwnershipScoping", e2eOwnershipScoping}, {"TwoStageDLR", e2eTwoStageDLR}, {"DLRWithoutToken", e2eDLRWithoutToken},
	} {
		t.Run(c.name, c.f)
	}
}

type e2eStack struct {
	s   *stack
	env e2eEnv
}

var e2eCur *e2eStack

// e2eEnv carries the resolved endpoints (legacy harness shape).
type e2eEnv struct {
	gwURL        string
	mockURL      string
	mockInnerURL string
}

type harness struct {
	t   *testing.T
	env e2eEnv
	hc  *http.Client
	st  *e2eStack
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	if e2eCur == nil {
		t.Skip("run through TestLegacyE2EAgainstV4")
	}
	return &harness{t: t, env: e2eCur.env, hc: &http.Client{Timeout: 10 * time.Second}, st: e2eCur}
}

func testCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// ---------- seed helpers (V4 store, same values as the legacy SQL) ----------

func (h *harness) seedProvider(name string) (providerID uint32, dlrToken string) {
	h.t.Helper()
	tok, err := randomHex(16)
	if err != nil {
		h.t.Fatalf("rand: %v", err)
	}
	cfg := sealed.Config{
		"url":          h.env.mockInnerURL + "/multichannel-api/sendmulti/",
		"sid":          "9999",
		"encoding":     "utf-8",
		"priority":     "2",
		"callback_url": h.env.gwURL + "/dlr?dlr_token=" + tok,
		"dlr_token":    tok,
	}
	a := h.st.s.v4
	p, err := a.Repo.CreateProviderSealed(testCtx(h.t), repo.Provider{TenantID: tenant, Name: name, Type: "voicecom", ObjectType: repo.ObjectSMS, Status: repo.On},
		func(id int64) ([]byte, error) { return a.Envelope.SealConfig(cfg, sealed.ProviderAD(tenant, id)) })
	if err != nil {
		h.t.Fatalf("seed provider: %v", err)
	}
	return uint32(p.ID), tok
}

func (h *harness) seedTemplate(name, body string) uint32 {
	h.t.Helper()
	tp, err := h.st.s.v4.Repo.CreateTemplate(testCtx(h.t), repo.Template{TenantID: tenant, Name: name, ObjectType: repo.ObjectSMS,
		Templates: map[string]string{"body": body}, Status: repo.On})
	if err != nil {
		h.t.Fatalf("seed template: %v", err)
	}
	return uint32(tp.ID)
}

func (h *harness) seedAPIClient(username, authority string) (uint32, string) {
	h.t.Helper()
	pwd := "test-pwd-" + randID(8)
	hash, err := bcrypt.GenerateFromPassword([]byte(pwd), bcrypt.DefaultCost)
	if err != nil {
		h.t.Fatalf("bcrypt: %v", err)
	}
	c, err := h.st.s.v4.Repo.CreateClient(testCtx(h.t), repo.APIClient{TenantID: tenant, Username: username, PasswordHash: string(hash),
		Authority: authority, Status: repo.On})
	if err != nil {
		h.t.Fatalf("seed api_client: %v", err)
	}
	return uint32(c.ID), pwd
}

func (h *harness) seedBlock(providerID uint32, recipient string) {
	h.t.Helper()
	pid := int64(providerID)
	if _, err := h.st.s.v4.Repo.CreateBlock(testCtx(h.t), repo.Block{TenantID: tenant, Recipient: recipient, Description: "e2e block",
		ProviderID: &pid, BlockType: repo.ObjectSMS, Status: repo.On}); err != nil {
		h.t.Fatalf("seed block: %v", err)
	}
}

// ---------- copied unchanged from the legacy e2e/harness.go ----------

// ---------- public HTTP API helpers ----------

type loginResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	// gRPC-gateway encodes int64 as a JSON string. We don't actually
	// use this value, but RawMessage avoids the unmarshal failure.
	ExpiresIn json.RawMessage `json:"expires_in"`
}

func (h *harness) login(username, password string) string {
	h.t.Helper()
	body, _ := json.Marshal(map[string]string{
		"username":   username,
		"password":   password,
		"grant_type": "password",
	})
	req, _ := http.NewRequest("POST", h.env.gwURL+"/hermes/v1/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.hc.Do(req)
	if err != nil {
		h.t.Fatalf("login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		h.t.Fatalf("login: status %d body %s", resp.StatusCode, string(b))
	}
	var lr loginResponse
	if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
		h.t.Fatalf("decode login: %v", err)
	}
	if lr.AccessToken == "" {
		h.t.Fatal("login returned empty access_token")
	}
	return lr.AccessToken
}

type sendSmsResponse struct {
	Data struct {
		ID            string `json:"id"`
		Owner         uint32 `json:"owner"`
		Status        uint32 `json:"status"`
		StatusMessage string `json:"status_message"`
		Text          string `json:"text"`
	} `json:"data"`
	EndpointResponse map[string]any `json:"endpoint_response"`
}

func (h *harness) send(token string, body map[string]any) (*http.Response, *sendSmsResponse) {
	h.t.Helper()
	bs, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", h.env.gwURL+"/hermes/v1/sms", bytes.NewReader(bs))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := h.hc.Do(req)
	if err != nil {
		h.t.Fatalf("send: %v", err)
	}
	if resp.StatusCode != 200 {
		// Return the response so callers can inspect non-2xx codes.
		return resp, nil
	}
	defer resp.Body.Close()
	var out sendSmsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		h.t.Fatalf("decode send response: %v", err)
	}
	return resp, &out
}

type smsGetResponse struct {
	Data struct {
		ID            string `json:"id"`
		Status        uint32 `json:"status"`
		StatusMessage string `json:"status_message"`
	} `json:"data"`
}

// getSms reads back the message by id; useful for asserting status
// transitions after DLRs arrive.
func (h *harness) getSms(token, id string) *smsGetResponse {
	h.t.Helper()
	req, _ := http.NewRequest("GET", h.env.gwURL+"/hermes/v1/sms/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := h.hc.Do(req)
	if err != nil {
		h.t.Fatalf("getSms: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		h.t.Fatalf("getSms: status %d body %s", resp.StatusCode, string(b))
	}
	var out smsGetResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		h.t.Fatalf("decode getSms: %v", err)
	}
	return &out
}

// dlrsResponse mirrors smsgwpb.ListDlrResponse. `id` is uint64 in the
// proto but gRPC-gateway encodes it as a JSON string — use json.Number
// to accept either form. The tests don't actually inspect the id; we
// only need Total + the per-item MessageStatus.
type dlrsResponse struct {
	Items []struct {
		ID            json.Number `json:"id"`
		MessageStatus uint32      `json:"messageStatus"`
		StatusText    string      `json:"statusText"`
	} `json:"items"`
	Total int32 `json:"total"`
}

func (h *harness) listDlrs(token, smsID string) *dlrsResponse {
	h.t.Helper()
	req, _ := http.NewRequest("GET", h.env.gwURL+"/hermes/v1/sms/dlr/"+smsID, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := h.hc.Do(req)
	if err != nil {
		h.t.Fatalf("listDlrs: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		h.t.Fatalf("listDlrs: status %d body %s", resp.StatusCode, string(b))
	}
	var out dlrsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		h.t.Fatalf("decode listDlrs: %v", err)
	}
	return &out
}

// statusGET issues an authenticated GET and returns just the status
// code. Used by tests that only care about authorization outcomes
// (ownership scoping, etc.) rather than the response body.
func (h *harness) statusGET(token, url string) int {
	h.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := h.hc.Do(req)
	if err != nil {
		h.t.Fatalf("statusGET %s: %v", url, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// rawDLR sends a DLR straight at the gateway's /dlr endpoint
// (bypassing the mock). Used to verify spoof-rejection behavior.
func (h *harness) rawDLR(query string) (status int, body string) {
	h.t.Helper()
	resp, err := h.hc.Get(h.env.gwURL + "/dlr?" + query)
	if err != nil {
		h.t.Fatalf("raw dlr: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// ---------- mock helpers ----------

type inboxEntry struct {
	Decoded struct {
		RequestID   string `json:"request_id"`
		To          uint64 `json:"to"`
		CallbackURL string `json:"callback_url"`
	} `json:"decoded"`
	SubmitStatus int `json:"submit_status"`
}

// inbox returns every submission the mock has accepted. Each test
// scenario filters by request_id (the SMS UUID we generated) to find
// its own row.
func (h *harness) inbox() []inboxEntry {
	h.t.Helper()
	resp, err := h.hc.Get(h.env.mockURL + "/debug/inbox")
	if err != nil {
		h.t.Fatalf("inbox: %v", err)
	}
	defer resp.Body.Close()
	var out []inboxEntry
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		h.t.Fatalf("decode inbox: %v", err)
	}
	return out
}

// triggerMockDLR asks the mock to fire an additional DLR with a
// specific status against an already-accepted submission. Lets a test
// re-send terminal-state DLRs without re-running the whole flow.
func (h *harness) triggerMockDLR(requestID string, status int) {
	h.t.Helper()
	body, _ := json.Marshal(map[string]any{"request_id": requestID, "status": status})
	resp, err := h.hc.Post(h.env.mockURL+"/debug/dlr", "application/json", bytes.NewReader(body))
	if err != nil {
		h.t.Fatalf("triggerMockDLR: %v", err)
	}
	_ = resp.Body.Close()
}

// ---------- assertions ----------

// waitFor polls until predicate returns true or timeout. Used for
// async-completion assertions (DLR roundtrip, status transitions).
func waitFor(t *testing.T, timeout time.Duration, predicate func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for: %s", msg)
}

// ---------- tiny utilities ----------

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := readRand(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// readRand is var so tests can inject deterministic randomness if needed.
var readRand = func(b []byte) (int, error) {
	for i := range b {
		b[i] = byte(rand.IntN(256))
	}
	return len(b), nil
}

// randID returns a short alphanumeric suffix for unique entity names.
func randID(n int) string {
	const alpha = "abcdefghijklmnopqrstuvwxyz0123456789"
	var sb strings.Builder
	for i := 0; i < n; i++ {
		sb.WriteByte(alpha[rand.IntN(len(alpha))])
	}
	return sb.String()
}

// findInbox returns the mock-inbox entry whose request_id matches.
// Polls because the mock's POST → goroutine → inbox-append is async.
func (h *harness) findInbox(requestID string, timeout time.Duration) *inboxEntry {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, e := range h.inbox() {
			if e.Decoded.RequestID == requestID {
				return &e
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	h.t.Fatalf("inbox entry for request_id=%s not found within %s", requestID, timeout)
	return nil
}

// fmtErr is a tiny shim used by harness helpers to log full HTTP body
// content alongside a status code. Kept here so changes to error
// formatting stay co-located with the helpers.
func fmtErr(resp *http.Response) string {
	b, _ := io.ReadAll(resp.Body)
	return fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(b))
}
