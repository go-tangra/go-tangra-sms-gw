//go:build parity

// Copied from the legacy repository (go-tangra-sms-gw v3.0.9, e2e/sms_e2e_test.go)
// unchanged except: the tests are subtests of TestLegacyE2EAgainstV4
// (legacy_e2e_harness_test.go) instead of top-level tests, so they run
// against V4 with the harness seeding V4's store.

package parity

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// All e2e tests share this provider+template+client triple per-run.
// Names carry a random suffix so concurrent runs don't collide.

// TestE2E_HappyPath: gateway → mock → DLR → status flips to delivered.
// Validates the entire pipeline: provider lookup, template render,
// HTTP POST to the mock, mock DLR loopback, gateway DLR validation
// (token match), and status update.
func e2eHappyPath(t *testing.T) {
	h := newHarness(t)
	suffix := randID(6)
	provID, _ := h.seedProvider("happy-" + suffix)
	tplID := h.seedTemplate("hello-"+suffix, "Hello {{ .name }}, code {{ .code }}.")
	_, pwd := h.seedAPIClient("happyclient_"+suffix, "API_CLIENT")
	token := h.login("happyclient_"+suffix, pwd)

	_, sent := h.send(token, map[string]any{
		"to":         359888100100,
		"providerId": provID,
		"templateId": tplID,
		"properties": map[string]string{"name": "Alice", "code": "8421"},
		"sms":        map[string]any{"from": "TEST", "encoding": "utf-8"},
	})
	if sent == nil {
		t.Fatal("send response was nil")
	}
	if sent.Data.ID == "" {
		t.Fatal("send response missing message id")
	}
	if sent.Data.Text != "Hello Alice, code 8421." {
		t.Fatalf("rendered text mismatch: %q", sent.Data.Text)
	}

	// Mock should have received the submission.
	entry := h.findInbox(sent.Data.ID, 2*time.Second)
	if entry.SubmitStatus != 0 {
		t.Fatalf("mock recorded non-success submit_status=%d", entry.SubmitStatus)
	}
	if entry.Decoded.To != 359888100100 {
		t.Fatalf("mock saw wrong recipient: %d", entry.Decoded.To)
	}

	// Within ~2s, the mock fires the configured DLR (status=1 by
	// default = sms_delivered). Gateway validates the dlr_token in the
	// callback URL and updates the message status.
	waitFor(t, 5*time.Second, func() bool {
		got := h.getSms(token, sent.Data.ID)
		return got.Data.Status == 1
	}, "message status to flip to 1 (delivered) after DLR")

	// And the DLR list should reflect the receipt.
	dlrs := h.listDlrs(token, sent.Data.ID)
	if dlrs.Total < 1 {
		t.Fatal("expected at least one DLR record")
	}
	found := false
	for _, d := range dlrs.Items {
		if d.MessageStatus == 1 {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected a delivered DLR (status=1) in list: %+v", dlrs.Items)
	}
}

// TestE2E_DLRSpoofRejected: a forged DLR with the wrong dlr_token is
// dropped on the gateway side — the message status stays at 0
// (sms_provider_accepted) even though we hit /dlr with a "delivered"
// status code. The gateway's response is still 200 DLR_OK to avoid
// tipping off the attacker via probing.
func e2eDLRSpoofRejected(t *testing.T) {
	h := newHarness(t)
	suffix := randID(6)
	provID, _ := h.seedProvider("spoof-" + suffix)
	tplID := h.seedTemplate("spoof-"+suffix, "ping")
	_, pwd := h.seedAPIClient("spoofclient_"+suffix, "API_CLIENT")
	token := h.login("spoofclient_"+suffix, pwd)

	_, sent := h.send(token, map[string]any{
		"to":         359888100200,
		"providerId": provID,
		"templateId": tplID,
		"properties": map[string]string{},
		"sms":        map[string]any{"from": "TEST", "encoding": "utf-8"},
	})

	// Wait for the legitimate DLR to settle so the spoof attempt
	// is unambiguous.
	waitFor(t, 5*time.Second, func() bool {
		return h.getSms(token, sent.Data.ID).Data.Status == 1
	}, "real DLR to land before spoof test")

	// Now attempt to override the status with a forged DLR using a
	// random token that is NOT the provider's dlr_token. The gateway
	// should silently drop it.
	q := url.Values{}
	q.Set("request_id", sent.Data.ID)
	q.Set("channel", "sms")
	q.Set("sid", "9999")
	q.Set("message_status", "16") // sms_rejected
	q.Set("to", "359888100200")
	q.Set("from", "TEST")
	q.Set("timestamp", strconv.FormatInt(time.Now().Unix(), 10))
	q.Set("dlr_token", "not-the-real-token-deadbeef00000000")
	status, body := h.rawDLR(q.Encode())
	// Kratos JSON-encodes the string body, so we get `"DLR_OK"`
	// (with surrounding quotes and a trailing newline). Substring
	// match keeps the test tolerant of encoder formatting.
	if status != 200 || !strings.Contains(body, "DLR_OK") {
		t.Fatalf("expected 200 with DLR_OK body, got %d %q", status, body)
	}

	// Status must remain 1 (delivered from the legitimate DLR), NOT
	// flipped to 16 by the spoof.
	time.Sleep(500 * time.Millisecond)
	got := h.getSms(token, sent.Data.ID)
	if got.Data.Status != 1 {
		t.Fatalf("spoofed DLR was applied: status=%d (expected 1)", got.Data.Status)
	}
}

// TestE2E_BlockedRecipient: a recipient on sms_block triggers a 400
// rejection at Send time and never reaches the mock.
func e2eBlockedRecipient(t *testing.T) {
	h := newHarness(t)
	suffix := randID(6)
	provID, _ := h.seedProvider("block-" + suffix)
	tplID := h.seedTemplate("block-"+suffix, "should not send")
	_, pwd := h.seedAPIClient("blockclient_"+suffix, "API_CLIENT")
	token := h.login("blockclient_"+suffix, pwd)

	const blocked = 359888100300
	h.seedBlock(provID, strconv.FormatUint(blocked, 10))

	resp, sent := h.send(token, map[string]any{
		"to":         blocked,
		"providerId": provID,
		"templateId": tplID,
		"properties": map[string]string{},
		"sms":        map[string]any{"from": "TEST", "encoding": "utf-8"},
	})
	if sent != nil {
		t.Fatalf("blocked send should not have returned 200: %+v", sent)
	}
	if resp.StatusCode != 400 {
		t.Fatalf("blocked send: expected 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Make sure NOTHING reached the mock for this recipient.
	time.Sleep(500 * time.Millisecond)
	for _, e := range h.inbox() {
		if e.Decoded.To == blocked {
			t.Fatal("blocked recipient leaked through to the mock")
		}
	}
}

// TestE2E_OwnershipScoping: client-A cannot read client-B's message
// even when the UUID is known. Defends against IDOR on /sms/{id}.
func e2eOwnershipScoping(t *testing.T) {
	h := newHarness(t)
	suffix := randID(6)
	provID, _ := h.seedProvider("scope-" + suffix)
	tplID := h.seedTemplate("scope-"+suffix, "scoped")

	_, pwdA := h.seedAPIClient("alice_"+suffix, "API_CLIENT")
	_, pwdB := h.seedAPIClient("bob_"+suffix, "API_CLIENT")
	tokA := h.login("alice_"+suffix, pwdA)
	tokB := h.login("bob_"+suffix, pwdB)

	// Alice sends a message.
	_, sent := h.send(tokA, map[string]any{
		"to":         359888100400,
		"providerId": provID,
		"templateId": tplID,
		"properties": map[string]string{},
		"sms":        map[string]any{"from": "TEST", "encoding": "utf-8"},
	})
	if sent == nil {
		t.Fatal("alice's send failed")
	}

	// Bob attempts to read it by UUID — must get 404.
	if got := h.statusGET(tokB, h.env.gwURL+"/hermes/v1/sms/"+sent.Data.ID); got != 404 {
		t.Fatalf("ownership check broken: bob got status %d (expected 404)", got)
	}
	// And bob cannot list its DLRs either.
	if got := h.statusGET(tokB, h.env.gwURL+"/hermes/v1/sms/dlr/"+sent.Data.ID); got != 404 {
		t.Fatalf("dlr ownership check broken: bob got status %d (expected 404)", got)
	}
}

// TestE2E_TwoStageDLR: the mock can be asked to fire two DLRs back-to-
// back via /debug/dlr — first a non-terminal status (8 = smsc_delivered)
// then the final (1 = delivered). Gateway must end at 1 and record
// both events in sms_dlr.
func e2eTwoStageDLR(t *testing.T) {
	h := newHarness(t)
	suffix := randID(6)
	provID, _ := h.seedProvider("twostage-" + suffix)
	tplID := h.seedTemplate("twostage-"+suffix, "two-stage test")
	_, pwd := h.seedAPIClient("twostage_"+suffix, "API_CLIENT")
	token := h.login("twostage_"+suffix, pwd)

	_, sent := h.send(token, map[string]any{
		"to":         359888100500,
		"providerId": provID,
		"templateId": tplID,
		"properties": map[string]string{},
		"sms":        map[string]any{"from": "TEST", "encoding": "utf-8"},
	})
	// Wait for the default DLR (status=1) to land first.
	waitFor(t, 5*time.Second, func() bool {
		return h.getSms(token, sent.Data.ID).Data.Status == 1
	}, "first DLR (delivered=1)")

	// Inject an additional non-terminal DLR (status=8). The gateway
	// MUST NOT downgrade — IsTerminal blocks the parent UPDATE because
	// status=1 is terminal. The DLR row itself is still recorded.
	h.triggerMockDLR(sent.Data.ID, 8)
	time.Sleep(500 * time.Millisecond)

	got := h.getSms(token, sent.Data.ID)
	if got.Data.Status != 1 {
		t.Fatalf("status was downgraded by late non-terminal DLR: %d", got.Data.Status)
	}

	dlrs := h.listDlrs(token, sent.Data.ID)
	if dlrs.Total < 2 {
		t.Fatalf("expected ≥2 DLR rows after two-stage, got %d", dlrs.Total)
	}
}

// TestE2E_DLRWithoutToken: a DLR arriving without ?dlr_token= is
// short-circuited (the R3-M14 fast path). It should not even reach
// the parent SMS lookup — but we can only assert the visible side
// effect: status stays unchanged.
func e2eDLRWithoutToken(t *testing.T) {
	h := newHarness(t)
	suffix := randID(6)
	provID, _ := h.seedProvider("notoken-" + suffix)
	tplID := h.seedTemplate("notoken-"+suffix, "no token")
	_, pwd := h.seedAPIClient("notoken_"+suffix, "API_CLIENT")
	token := h.login("notoken_"+suffix, pwd)

	_, sent := h.send(token, map[string]any{
		"to":         359888100600,
		"providerId": provID,
		"templateId": tplID,
		"properties": map[string]string{},
		"sms":        map[string]any{"from": "TEST", "encoding": "utf-8"},
	})
	// Real DLR settles to delivered.
	waitFor(t, 5*time.Second, func() bool {
		return h.getSms(token, sent.Data.ID).Data.Status == 1
	}, "real DLR settles")

	// Send a forged DLR with NO dlr_token. R3-M14 short-circuits.
	q := url.Values{}
	q.Set("request_id", sent.Data.ID)
	q.Set("channel", "sms")
	q.Set("sid", "9999")
	q.Set("message_status", "2") // failed
	q.Set("to", "359888100600")
	q.Set("from", "TEST")
	q.Set("timestamp", strconv.FormatInt(time.Now().Unix(), 10))
	status, _ := h.rawDLR(q.Encode())
	if status != 200 {
		t.Fatalf("no-token DLR returned %d (expected 200 DLR_OK)", status)
	}
	time.Sleep(300 * time.Millisecond)

	got := h.getSms(token, sent.Data.ID)
	if got.Data.Status != 1 {
		t.Fatalf("no-token forgery was applied: status=%d (expected 1)", got.Data.Status)
	}
}

// Reference net/http to suppress goimports — used inside statusGET (in harness.go).
var _ = http.MethodGet
