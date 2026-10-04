//go:build integration

package contract

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/app/apptest"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/config"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/repo"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/sealed"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/store/storetest"
)

// TestLegacyReplay drives the V4 public listener through the legacy capture
// scenario (scripts/capture_legacy_runtime.py) in its order, with the same
// accounts, carriers, templates and blocks, and compares every recorded
// response byte for byte after the capture's normalization. The only
// differences accepted are the deliberate V4 changes listed in v4Changes,
// each citing docs/compatibility.md; cases owned by later stories are
// listed in deferred. Every captured case is accounted for.
func TestLegacyReplay(t *testing.T) {
	db := storetest.Start(t)
	r := &replay{t: t, db: db, repo: db.Repo(), kek: []byte(strings.Repeat("k", 32)), carrier: newCaptureCarrier(t),
		client: &http.Client{Timeout: 60 * time.Second}, names: map[string]string{}, values: map[string]string{},
		got: map[string]result{}, ids: map[string]int64{}, tokens: map[string]map[string]string{}}
	var err error
	if r.env, err = sealed.NewEnvelope(r.kek); err != nil {
		t.Fatal(err)
	}
	relaxed := func(c *config.Config) {
		c.RateLimits = config.RateLimits{LoginPerMinute: 1e6, LoginBurst: 100000, SendPerMinute: 1e6, SendBurst: 100000, DLRPerMinute: 1e6, DLRBurst: 100000}
	}
	r.running = apptest.Start(t, apptest.Options{DSN: db.AppDSN, KEK: r.kek, JWTSecret: jwtSecret, Configure: relaxed})
	r.base = r.running.Public
	r.seed(t, r.base)
	r.names[nilUUID] = nilUUID

	r.surface(t)
	r.auth(t)
	r.sends(t)
	r.carrierFailures(t)
	r.earlyReceipt(t)
	r.reads(t)
	r.receipts(t)
	r.logout(t)
	r.restart(t)

	// The capture names every message it did not name otherwise by its
	// position in creation order.
	l, err := r.repo.ListMessages(context.Background(), repo.TenantView(tenant), repo.MessageFilter{Oldest: true}, repo.Page{Size: 500})
	if err != nil {
		t.Fatal(err)
	}
	for n, m := range l.Items {
		r.name(m.ID, "msg:seq"+itoa(n+1))
	}
	r.compare(t)
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func (r *replay) surface(t *testing.T) {
	for _, c := range []string{"surface-health", "surface-unknown-route", "surface-wrong-method"} {
		r.call(c)
	}
	r.record("surface-root", r.raw("GET", "/", "", nil))
	r.record("surface-metrics", r.raw("GET", "/metrics", "", nil))
}

func (r *replay) auth(t *testing.T) {
	r.tokens["client_a"] = r.tokenPair(r.call("login-json"), "client_a")
	r.checkClaims(t)
	r.tokenPair(r.call("login-form"), "client_b(form)")
	for _, c := range []string{"login-missing-password", "login-empty-body", "login-unknown-user", "login-wrong-password", "login-disabled",
		"login-malformed-json", "login-without-grant-type"} {
		r.call(c)
	}
	r.checkLoginLog(t)
	for _, u := range []string{"client_b", "viewer_v", "admin_x", "client_f", "client_r"} {
		r.tokens[u] = r.login(u)
	}
	for _, c := range []string{"me-client", "me-viewer", "me-admin", "me-missing-token", "me-non-bearer-scheme", "me-lowercase-bearer",
		"me-garbage-token", "me-refresh-token-as-access"} {
		r.call(c)
	}
	now := time.Now().Unix()
	base := func(over jwt.MapClaims) jwt.MapClaims {
		c := jwt.MapClaims{"authority": "API_CLIENT", "kind": "access", "username": "client_a", "sub": "1", "iss": "sms-gw", "jti": strings.Repeat("f", 32),
			"iat": now, "nbf": now, "exp": now + 7200}
		for k, v := range over {
			if v == nil {
				delete(c, k)
			} else {
				c[k] = v
			}
		}
		return c
	}
	hs := jwt.SigningMethodHS256
	r.forge("me-expired-access", base(jwt.MapClaims{"iat": now - 7300, "nbf": now - 7300, "exp": now - 100}), jwtSecret, hs)
	r.forge("me-not-yet-valid", base(jwt.MapClaims{"nbf": now + 3600}), jwtSecret, hs)
	r.forge("me-wrong-secret", base(nil), "another-secret-another-secret-123", hs)
	r.forge("me-alg-none", base(nil), "", jwt.SigningMethodNone)
	r.forge("me-missing-kind", base(jwt.MapClaims{"kind": nil}), jwtSecret, hs)
	r.forge("me-unknown-client-id", base(jwt.MapClaims{"sub": "999999"}), jwtSecret, hs)
	r.forge("me-non-numeric-sub", base(jwt.MapClaims{"sub": "abc"}), jwtSecret, hs)
	for _, c := range []string{"me-expired-access", "me-not-yet-valid", "me-wrong-secret", "me-alg-none", "me-missing-kind", "me-unknown-client-id", "me-non-numeric-sub"} {
		r.call(c)
	}
	r.forge("authority-ROOT", base(jwt.MapClaims{"authority": "ROOT"}), jwtSecret, hs)
	r.call("me-unknown-authority")

	r.tokenPair(r.call("refresh-json"), "client_a(refreshed)")
	r.checkRefreshRotation(t)
	r.tokenPair(r.call("refresh-form"), "client_b(refresh-form)")
	for _, c := range []string{"refresh-with-access-token", "refresh-missing", "refresh-garbage"} {
		r.call(c)
	}
	refresh := func(over jwt.MapClaims) jwt.MapClaims { over["kind"] = "refresh"; return base(over) }
	r.forge("expired-refresh", refresh(jwt.MapClaims{"iat": now - 700000, "nbf": now - 700000, "exp": now - 10}), jwtSecret, hs)
	r.call("refresh-expired")
	r.forge("refresh-disabled", refresh(jwt.MapClaims{"sub": "5", "username": "disabled_d", "exp": now + 600}), jwtSecret, hs)
	r.call("refresh-disabled-account")
	r.forge("refresh-unknown-client", refresh(jwt.MapClaims{"sub": "999999", "exp": now + 600}), jwtSecret, hs)
	r.call("refresh-unknown-client")
	r.forge("refresh-overflow-sub", refresh(jwt.MapClaims{"sub": "99999999999999999999", "exp": now + 600}), jwtSecret, hs)
	r.call("refresh-overflow-subject")
	r.call("refresh-stale-authority-claim")
	r.checkStaleAuthority(t)
}

func claimsOf(t *testing.T, tok string) (map[string]any, map[string]any) {
	t.Helper()
	parts := strings.Split(tok, ".")
	var h, c map[string]any
	for i, dst := range []*map[string]any{&h, &c} {
		b, err := base64.RawURLEncoding.DecodeString(parts[i])
		if err != nil || json.Unmarshal(b, dst) != nil {
			t.Fatalf("token part %d: %v", i, err)
		}
	}
	return h, c
}

func (r *replay) checkClaims(t *testing.T) {
	f := loadFixture(t, "jwt-claims")
	var want map[string]struct {
		Header map[string]any `json:"header"`
		Claims map[string]any `json:"claims"`
	}
	raw, _ := json.Marshal(map[string]json.RawMessage{"access": f.Observed["access"], "refresh": f.Observed["refresh"]})
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	var jti string
	for kind, w := range want {
		h, c := claimsOf(t, r.tokens["client_a"][kind+"_token"])
		if jti == "" {
			jti = c["jti"].(string)
			r.name(jti, "jti:client_a")
		}
		got := map[string]any{"exp_minus_iat": c["exp"].(float64) - c["iat"].(float64), "nbf_equals_iat": c["nbf"] == c["iat"]}
		for k, v := range c {
			switch k {
			case "exp", "iat", "nbf":
				got[k] = "{{unix_seconds}}"
			case "jti":
				got[k] = r.normalize(v.(string))
			default:
				got[k] = v
			}
		}
		if g, _ := json.Marshal(got); string(g) != string(must(json.Marshal(w.Claims))) {
			t.Errorf("jwt-claims %s: %s, legacy %s", kind, g, must(json.Marshal(w.Claims)))
		}
		if g, _ := json.Marshal(h); string(g) != string(must(json.Marshal(w.Header))) {
			t.Errorf("jwt-claims %s header: %s", kind, g)
		}
	}
	var sameJTI bool
	var hexLen int
	f.observed(t, "same_jti", &sameJTI)
	f.observed(t, "jti_hex_len", &hexLen)
	_, rc := claimsOf(t, r.tokens["client_a"]["refresh_token"])
	if (rc["jti"] == jti) != sameJTI || len(jti) != hexLen {
		t.Errorf("jwt-claims jti pairing")
	}
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}

func (r *replay) checkLoginLog(t *testing.T) {
	f := loadFixture(t, "login-log-rows")
	type row struct {
		ClientID     int64  `json:"client_id"`
		Username     string `json:"username"`
		Success      bool   `json:"success"`
		ErrorMessage string `json:"error_message"`
		LoginIP      string `json:"login_ip"`
	}
	var want []row
	f.observed(t, "rows", &want)
	conn := r.owner(t)
	defer conn.Close(context.Background())
	rows, err := conn.Query(context.Background(), "SELECT COALESCE(client_id, 0), username, success, error_message, login_ip FROM sms_login_log ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	var got []row
	for rows.Next() {
		var x row
		if err := rows.Scan(&x.ClientID, &x.Username, &x.Success, &x.ErrorMessage, &x.LoginIP); err != nil {
			t.Fatal(err)
		}
		got = append(got, x)
	}
	if string(must(json.Marshal(got))) != string(must(json.Marshal(want))) {
		t.Errorf("login-log-rows:\n got  %+v\n want %+v", got, want)
	}
	type last struct {
		Username    string `json:"username"`
		LastLoginIP string `json:"last_login_ip"`
		HasTime     bool   `json:"has_time"`
	}
	var wantLast, gotLast []last
	f.observed(t, "last_login", &wantLast)
	rows, err = conn.Query(context.Background(), "SELECT username, last_login_ip, last_login_time IS NOT NULL FROM sms_api_client ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var x last
		if err := rows.Scan(&x.Username, &x.LastLoginIP, &x.HasTime); err != nil {
			t.Fatal(err)
		}
		gotLast = append(gotLast, x)
	}
	if string(must(json.Marshal(gotLast))) != string(must(json.Marshal(wantLast))) {
		t.Errorf("login-log-rows last_login:\n got  %+v\n want %+v", gotLast, wantLast)
	}
}

func (r *replay) owner(t *testing.T) *pgx.Conn {
	conn, err := pgx.Connect(context.Background(), r.db.OwnerDSN)
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

func (r *replay) checkRefreshRotation(t *testing.T) {
	f := loadFixture(t, "refresh-json")
	var differs bool
	var oldStatus int
	f.observed(t, "new_jti_differs", &differs)
	f.observed(t, "old_refresh_still_usable", &oldStatus)
	var out map[string]string
	_ = json.Unmarshal([]byte(r.got["refresh-json"].body), &out)
	_, nc := claimsOf(t, out["access_token"])
	_, oc := claimsOf(t, r.tokens["client_a"]["access_token"])
	if (nc["jti"] != oc["jti"]) != differs {
		t.Error("refresh-json: jti rotation differs")
	}
	if res := r.raw("POST", "/hermes/v1/refresh_token", r.jsonBody(map[string]string{"refresh_token": r.tokens["client_a"]["refresh_token"]}), nil); res.status != oldStatus {
		t.Errorf("refresh-json: old refresh token %d, legacy %d", res.status, oldStatus)
	}
}

// V4 change (docs/compatibility.md, refresh authority): refresh issues the
// account's current authority instead of copying the presented claim.
func (r *replay) checkStaleAuthority(t *testing.T) {
	ctx := context.Background()
	set := func(auth string) {
		c, err := r.repo.GetClient(ctx, tenant, r.ids["c:viewer_v"])
		if err != nil {
			t.Fatal(err)
		}
		c.Authority = auth
		if _, err := r.repo.UpdateClient(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	set("API_CLIENT")
	res := r.raw("POST", "/hermes/v1/refresh_token", r.jsonBody(map[string]string{"refresh_token": r.tokens["viewer_v"]["refresh_token"]}), nil)
	var out map[string]string
	_ = json.Unmarshal([]byte(res.body), &out)
	_, c := claimsOf(t, out["access_token"])
	var legacy string
	loadFixture(t, "refresh-stale-authority-claim").observed(t, "authority_after_db_change_to_API_CLIENT", &legacy)
	if legacy != "API_VIEWER" || c["authority"] != "API_CLIENT" {
		t.Errorf("refresh-stale-authority-claim: V4 issued %v (legacy copied %s)", c["authority"], legacy)
	}
	set("API_VIEWER")
}

func (r *replay) sends(t *testing.T) {
	ctx := context.Background()
	named := func(c, label string) {
		if res := r.call(c); res.status == 200 {
			r.name(r.dataID(t, res), "msg:"+label)
		}
	}
	named("send-hermes", "send-hermes")
	r.checkMessageRow(t, "send-hermes", "db_row", r.values["{{msg:send-hermes}}"])
	named("send-v1-alias", "send-v1-alias")
	named("send-to-as-string", "send-to-as-string")
	named("send-proto-field-names", "send-proto-field-names")
	named("send-all-sms-options", "send-all-sms-options")
	r.checkMessageRow(t, "send-all-sms-options", "db_row", r.values["{{msg:send-all-sms-options}}"])
	named("send-missing-template-variable", "send-missing-template-variable")
	named("send-unknown-field", "send-unknown-field")
	named("send-no-carrier-callback", "send-no-carrier-callback")
	before, _ := r.repo.ListMessages(ctx, repo.TenantView(tenant), repo.MessageFilter{}, repo.Page{})
	for _, c := range []string{"send-viewer-forbidden", "send-admin-authority", "send-unknown-authority", "send-no-token", "send-provider-zero",
		"send-provider-missing", "send-provider-off", "send-provider-viber", "send-template-zero", "send-template-missing", "send-template-off",
		"send-template-viber", "send-template-no-body", "send-template-bad-syntax", "send-to-zero", "send-to-short", "send-to-too-long",
		"send-to-negative", "send-blocked-provider", "send-blocked-global", "send-unknown-provider-type", "send-malformed-json"} {
		r.call(c)
	}
	after, _ := r.repo.ListMessages(ctx, repo.TenantView(tenant), repo.MessageFilter{}, repo.Page{})
	var created int
	loadFixture(t, "send-rejections-persisted").observed(t, "rows_created", &created)
	if after.Total-before.Total != created {
		t.Errorf("send-rejections-persisted: %d rows, legacy %d", after.Total-before.Total, created)
	}
	named("send-block-disabled-not-enforced", "send-block-disabled")
	res := r.call("send-other-client")
	r.name(r.dataID(t, res), "msg:client_b")
}

// checkMessageRow compares the stored row with the legacy row. Raw carrier
// evidence is scrubbed in V4 (docs/compatibility.md, raw carrier evidence):
// it must keep the exchange but carry neither the carrier token nor the
// receipt token.
func (r *replay) checkMessageRow(t *testing.T, c, key, id string) {
	var want struct {
		CreateBy      int64           `json:"create_by"`
		Sid           int64           `json:"sid"`
		Recipient     string          `json:"recipient"`
		Priority      int             `json:"priority"`
		ProviderID    int64           `json:"provider_id"`
		TemplateID    int64           `json:"template_id"`
		Data          json.RawMessage `json:"data"`
		StatusCode    int32           `json:"status_code"`
		Message       string          `json:"message"`
		StatusMessage string          `json:"status_message"`
		RawRequest    *string         `json:"raw_request"`
	}
	loadFixture(t, c).observed(t, key, &want)
	m, err := r.repo.GetMessage(context.Background(), repo.TenantView(tenant), id)
	if err != nil {
		t.Fatal(err)
	}
	var wantData, gotData any
	_ = json.Unmarshal(want.Data, &wantData)
	_ = json.Unmarshal(m.Data, &gotData)
	if m.Actor.APIClientID != want.CreateBy || m.Sid != want.Sid || m.Recipient != want.Recipient || m.Priority != want.Priority ||
		m.ProviderID != want.ProviderID || m.TemplateID == nil || *m.TemplateID != want.TemplateID || m.Text != want.Message ||
		string(must(json.Marshal(gotData))) != string(must(json.Marshal(wantData))) {
		t.Errorf("%s %s: stored %+v data %s", c, key, m, m.Data)
	}
	if r.normalize(r.sanitizedMessage(want.StatusMessage)) != r.normalize(m.StatusMessage) || m.StatusCode != want.StatusCode {
		t.Errorf("%s %s: status %d %q, legacy %d %q", c, key, m.StatusCode, m.StatusMessage, want.StatusCode, want.StatusMessage)
	}
	if want.RawRequest != nil {
		raw := string(m.RawRequest)
		for _, secret := range []string{carrierToken, tAuto, tManual, tSlow} {
			if strings.Contains(raw, secret) {
				t.Errorf("%s: stored evidence contains a credential", c)
			}
		}
		body := (*want.RawRequest)[strings.Index(*want.RawRequest, "\r\n\r\n")+4:]
		i := strings.Index(raw, "\r\n\r\n")
		if got := portRE.ReplaceAllString(r.normalize(raw[i+4:]), "127.0.0.1:{{port}}"); i < 0 || got != scrubbedLegacy(body) {
			t.Errorf("%s: carrier request body\n got  %s\n want %s", c, got, scrubbedLegacy(body))
		}
	}
}

var portRE = regexp.MustCompile(`127\.0\.0\.1:\d+`)

// scrubbedLegacy is the legacy carrier body with the V4 evidence redaction.
func scrubbedLegacy(s string) string {
	s = strings.ReplaceAll(s, `"token":"{{carrier_token}}"`, `"token":"[REDACTED]"`)
	return regexp.MustCompile(`dlr_token=\{\{dlr_token:[a-z]+\}\}`).ReplaceAllString(s, "dlr_token=[REDACTED]")
}

// sanitizedMessage maps a legacy carrier error text to the V4 text: the
// transport error no longer names the carrier URL (docs/compatibility.md,
// leaked carrier errors).
func (r *replay) sanitizedMessage(s string) string {
	if strings.HasPrefix(s, "voicecom: endpoint error:") {
		return "voicecom: endpoint error"
	}
	return s
}

func (r *replay) carrierFailures(t *testing.T) {
	for _, c := range []struct {
		name string
		to   string
	}{{"send-carrier-reject", "359888000201"}, {"send-carrier-http500", "359888000202"}, {"send-carrier-connection-drop", "359888000203"}} {
		r.call(c.name)
		id := r.messageID(t, c.to)
		r.name(id, "msg:"+c.name)
		r.checkMessageRow(t, c.name, "db_row", id)
		f := loadFixture(t, c.name)
		var getAfter struct {
			Status int    `json:"status"`
			Body   string `json:"body"`
		}
		f.observed(t, "get_after", &getAfter)
		res := r.raw("GET", "/hermes/v1/sms/"+id, "", r.bearer("client_a"))
		want := strings.Replace(getAfter.Body, `voicecom: endpoint error: Post \"http://127.0.0.1:{{port}}/drop\": EOF`, "voicecom: endpoint error", 1)
		if res.status != getAfter.Status || r.normalize(res.body) != want {
			t.Errorf("%s get_after: %d %s\n want %s", c.name, res.status, r.normalize(res.body), want)
		}
	}
}

func (r *replay) earlyReceipt(t *testing.T) {
	body := r.sendBody("slow", "static", 359888000210, nil)
	done := make(chan result, 1)
	base, auth := r.base, r.bearer("client_a")
	go func() {
		res, err := r.do(base, "POST", "/hermes/v1/sms", body, auth)
		if err != nil {
			res = result{body: err.Error()}
		}
		done <- res
	}()
	select {
	case <-r.carrier.slowEntered:
	case <-time.After(15 * time.Second):
		t.Fatal("slow carrier not reached")
	}
	id := r.messageID(t, "359888000210")
	r.name(id, "msg:slow-carrier")
	r.call("get-while-carrier-pending")
	r.call("list-while-carrier-pending")
	r.receipt(t, id, 1, 359888000210, fixedTS) // dlr-before-carrier-response
	close(r.carrier.slowRelease)
	res := <-done
	r.record("send-carrier-response-after-early-dlr", res)
	m, err := r.repo.GetMessage(context.Background(), repo.TenantView(tenant), id)
	if err != nil || m.StatusCode != 1 || m.StatusMessage != "sms_delivered" || m.DLRTs != fixedTS || len(m.RawResponse) == 0 {
		t.Errorf("send-carrier-response-after-early-dlr: terminal receipt state lost: %d %q %v", m.StatusCode, m.StatusMessage, err)
	}
}

func (r *replay) reads(t *testing.T) {
	for _, c := range []string{"get-own", "get-own-v1-alias", "get-foreign", "get-viewer-foreign", "get-admin-any", "get-unknown-authority",
		"get-nonexistent", "get-not-a-uuid", "get-no-token", "list-default", "list-v1-alias", "list-page-size", "list-page-size-snake",
		"list-page-beyond", "list-negative-paging", "list-huge-page-size", "list-nopaging", "list-order-by", "list-or-query",
		"list-filter-recipient-prefix", "list-filter-status", "list-filter-status-zero", "list-filter-sid",
		"list-filter-api-client-username-foreign", "list-filter-api-client-username-unknown", "list-filter-malformed",
		"list-filter-sql-text", "list-viewer-empty", "list-admin-all", "list-admin-filter-username", "list-unknown-authority",
		"list-no-token", "list-bad-page-type"} {
		r.call(c)
	}
}

func (r *replay) receipts(t *testing.T) {
	r.raw("POST", "/hermes/v1/sms", r.sendBody("manual", "static", 359888000220, nil), r.bearer("client_a"))
	id := r.messageID(t, "359888000220")
	r.name(id, "msg:receipts")
	r.call("dlrs-none-yet")
	// dlr-intermediate, dlr-duplicate-intermediate, dlr-alias-path-terminal,
	// dlr-after-terminal and dlr-unknown-status-code; the rejected receipts
	// in between change nothing.
	for _, x := range []struct {
		status uint32
		ts     int64
	}{{8, fixedTS}, {8, fixedTS + 5}, {1, fixedTS + 10}, {2, fixedTS + 20}, {77, fixedTS}} {
		r.receipt(t, id, x.status, 359888000220, x.ts)
	}
	r.call("dlr-post-method")
	for _, c := range []string{"dlrs-own", "dlrs-own-v1-alias", "dlrs-foreign", "dlrs-admin", "dlrs-nonexistent", "dlrs-no-token"} {
		r.call(c)
	}
	r.raw("POST", "/hermes/v1/sms", r.sendBody("legacy", "static", 359888000221, nil), r.bearer("client_a"))
	lid := r.messageID(t, "359888000221")
	r.name(lid, "msg:legacy-provider")
	r.receipt(t, lid, 1, 359888000221, fixedTS) // dlr-provider-without-token-accepts-any
	res := r.call("send-roundtrip")
	rid := r.dataID(t, res)
	r.name(rid, "msg:roundtrip")
	ts := time.Now().Unix()
	r.receipt(t, rid, 1, 359888000222, ts) // the legacy mock's automatic receipt
	r.name(itoa(int(ts)), "unix_seconds:carrier_dlr")
	r.call("dlrs-roundtrip")
	r.call("get-roundtrip")
}

func (r *replay) logout(t *testing.T) {
	r.call("logout-no-token")
	r.call("logout-garbage-token")
	r.tokens["client_l"] = r.login("client_l")
	now := time.Now().Unix()
	r.forge("expired-access-client_l", jwt.MapClaims{"authority": "API_CLIENT", "kind": "access", "username": "client_l", "sub": "8", "iss": "sms-gw",
		"jti": strings.Repeat("e", 32), "iat": now - 9000, "nbf": now - 9000, "exp": now - 10}, jwtSecret, jwt.SigningMethodHS256)
	r.call("logout-expired-access-with-refresh")
	f := loadFixture(t, "logout-expired-access-with-refresh")
	var refreshAfter, accessAfter int
	f.observed(t, "refresh_after", &refreshAfter)
	f.observed(t, "access_after_shared_jti", &accessAfter)
	if res := r.raw("POST", "/hermes/v1/refresh_token", r.jsonBody(map[string]string{"refresh_token": r.tokens["client_l"]["refresh_token"]}), nil); res.status != refreshAfter {
		t.Errorf("logout-expired-access-with-refresh: refresh after %d, legacy %d", res.status, refreshAfter)
	}
	if res := r.raw("GET", "/hermes/v1/me", "", r.bearer("client_l")); res.status != accessAfter {
		t.Errorf("logout-expired-access-with-refresh: access after %d, legacy %d", res.status, accessAfter)
	}
	r.tokens["client_s"] = r.login("client_s")
	for _, c := range []string{"logout-access-and-refresh", "me-after-logout", "refresh-after-logout", "logout-get-method"} {
		r.call(c)
	}
}

// restart replays the second legacy instance (default limits) on the same
// database and secret.
func (r *replay) restart(t *testing.T) {
	r.running.Stop()
	// Default bursts (login 5 per address, send 20 per client); the refill
	// is slowed so the observed statuses do not depend on test speed.
	defaults := config.Default().RateLimits
	if defaults.LoginBurst != 5 || defaults.SendBurst != 20 || defaults.LoginPerMinute != 10 || defaults.SendPerMinute != 100 {
		t.Fatalf("default rate limits changed: %+v", defaults)
	}
	r.running = apptest.Start(t, apptest.Options{DSN: r.db.AppDSN, KEK: r.kek, JWTSecret: jwtSecret, Configure: func(c *config.Config) {
		c.RateLimits.LoginPerMinute, c.RateLimits.SendPerMinute = 0.001, 0.001
	}})
	r.base = r.running.Public
	r.call("restart-revoked-access-accepted")
	r.call("restart-old-tokens-still-valid")
	var statuses []int
	for range 7 {
		res := r.raw("POST", "/hermes/v1/login", r.jsonBody(map[string]string{"username": "client_a", "password": "Passw0rd!", "grant_type": "password"}), nil)
		statuses = append(statuses, res.status)
		if res.status == 429 {
			r.record("login-rate-limited", res)
		}
	}
	r.checkStatuses(t, "login-rate-limited", statuses)
	statuses = nil
	for i := range 22 {
		res := r.raw("POST", "/hermes/v1/sms", r.sendBody("manual", "static", uint64(359888000250+i), nil), r.bearer("client_a"))
		statuses = append(statuses, res.status)
		if _, ok := r.got["send-rate-limited"]; res.status == 429 && !ok {
			r.record("send-rate-limited", res)
		}
	}
	r.checkStatuses(t, "send-rate-limited", statuses)
	var other int
	loadFixture(t, "send-rate-limited").observed(t, "other_client_not_limited", &other)
	if res := r.raw("POST", "/hermes/v1/sms", r.sendBody("manual", "static", 359888000299, nil), r.bearer("client_b")); res.status != other {
		t.Errorf("send-rate-limited: other client %d", res.status)
	}
}

func (r *replay) checkStatuses(t *testing.T, c string, got []int) {
	var want []int
	loadFixture(t, c).observed(t, "statuses_in_order", &want)
	if string(must(json.Marshal(got))) != string(must(json.Marshal(want))) {
		t.Errorf("%s: statuses %v, legacy %v", c, got, want)
	}
}

// ---------- comparison ----------

// deferred are captured cases another story replays.
var deferred = map[string]string{
	"dlr-100-concurrent-same-status":         "US2 receipt ingestion (T024/T026)",
	"dlr-after-terminal":                     "US2 receipt ingestion (T024/T027); its effect is applied through the repository above",
	"dlr-alias-path-terminal":                "US2 receipt ingestion (T024/T027); its effect is applied through the repository above",
	"dlr-before-carrier-response":            "US2 receipt ingestion (T024/T027); its effect is applied through the repository above",
	"dlr-camel-case-params":                  "US2 receipt ingestion (T024/T027)",
	"dlr-duplicate-intermediate":             "US2 receipt ingestion (T024/T027); its effect is applied through the repository above",
	"dlr-empty-request-id":                   "US2 receipt ingestion (T024/T027)",
	"dlr-intermediate":                       "US2 receipt ingestion (T024/T027); its effect is applied through the repository above",
	"dlr-malformed-status":                   "US2 receipt ingestion (T024/T027)",
	"dlr-missing-token":                      "US2 receipt ingestion (T024/T027)",
	"dlr-provider-without-token-accepts-any": "US2 receipt ingestion (T024/T027); its effect is applied through the repository above",
	"dlr-unknown-request-id":                 "US2 receipt ingestion (T024/T027)",
	"dlr-unknown-status-code":                "US2 receipt ingestion (T024/T027); its effect is applied through the repository above",
	"dlr-wrong-token":                        "US2 receipt ingestion (T024/T027)",
	"webhook-redirect-not-followed":          "US2 outbound callbacks (T025/T028)",
	"webhook-retry-on-500":                   "US2 outbound callbacks (T025/T028)",
	"webhook-signed":                         "US2 outbound callbacks (T025/T028)",
	"webhook-unsigned":                       "US2 outbound callbacks (T025/T028)",
}

// observedOnly are captured observations without a response of their own;
// the scenario checks them where they occur.
var observedOnly = map[string]bool{"jwt-claims": true, "login-log-rows": true, "send-rejections-persisted": true}

type expectation struct {
	status      int
	contentType string
	body        string
}

const jsonCT = "application/json"

var (
	notFoundPage = expectation{404, "text/plain; charset=utf-8", "404 page not found\n"}
	smsNotFound  = expectation{404, jsonCT, `{"code":404, "reason":"RECORD_NOT_FOUND", "message":"sms not found", "metadata":{}}`}
	emptyList    = expectation{200, jsonCT, `{"items":[], "total":0}`}
)

// v4Changes are the deliberate differences from the legacy recording, each
// documented in docs/compatibility.md. want derives the V4 response from the
// legacy one.
var v4Changes = map[string]struct {
	why  string
	want func(legacy expectation) expectation
}{
	"surface-root":               {"the embedded admin UI is not served on the public listener (compatibility.md: /metrics and the embedded admin UI)", fixed(notFoundPage)},
	"surface-metrics":            {"/metrics is on the admin listener, not the public one (compatibility.md: /metrics and the embedded admin UI)", fixed(notFoundPage)},
	"get-admin-any":              {"API_ADMIN fails closed on the public API (compatibility.md: API_ADMIN cross-client reads)", fixed(smsNotFound)},
	"dlrs-admin":                 {"API_ADMIN fails closed on the public API (compatibility.md: API_ADMIN cross-client reads)", fixed(smsNotFound)},
	"list-admin-all":             {"API_ADMIN fails closed on the public API (compatibility.md: API_ADMIN cross-client reads)", fixed(emptyList)},
	"list-admin-filter-username": {"API_ADMIN fails closed on the public API (compatibility.md: API_ADMIN cross-client reads)", fixed(emptyList)},
	"restart-revoked-access-accepted": {"logout revocations persist across restarts (compatibility.md: in-memory revocation)",
		fixed(expectation{401, jsonCT, `{"code":401, "reason":"UNAUTHORIZED", "message":"invalid token", "metadata":{}}`})},
	"send-carrier-connection-drop": {"carrier transport errors no longer name the carrier URL (compatibility.md: leaked carrier errors)",
		func(l expectation) expectation { l.body = sanitizeDrop(l.body); return l }},
	"send-carrier-response-after-early-dlr": {"a receipt that arrived before the carrier answered keeps its terminal state (compatibility.md: early receipt overwrite)",
		func(l expectation) expectation {
			l.body = strings.Replace(l.body, `"status":0, "statusMessage":"sms_provider_accepted"`, `"status":1, "statusMessage":"sms_delivered"`, 1)
			return l
		}},
}

// slowListed are the list cases that show the early-receipt message after
// the legacy overwrite and the dropped-connection message with the legacy
// carrier error text; V4 shows the kept terminal state
// (compatibility.md: early receipt overwrite) and the sanitized text
// (compatibility.md: leaked carrier errors).
var slowListed = []string{"list-default", "list-v1-alias", "list-negative-paging", "list-huge-page-size", "list-nopaging", "list-order-by",
	"list-or-query", "list-filter-malformed", "list-filter-sid"}

const (
	slowLegacy = `"text":"ping", "status":0, "statusMessage":"sms_provider_accepted", "rawResponse":"", "rawRequest":"", "userName":"", "providerId":0, "providerName":"fake-slow"`
	slowV4     = `"text":"ping", "status":1, "statusMessage":"sms_delivered", "rawResponse":"", "rawRequest":"", "userName":"", "providerId":0, "providerName":"fake-slow"`
	dropLegacy = `voicecom: endpoint error: Post \"http://127.0.0.1:{{port}}/drop\": EOF`
)

func sanitizeDrop(body string) string {
	if !strings.Contains(body, dropLegacy) {
		panic("recording no longer shows the legacy carrier transport error")
	}
	return strings.ReplaceAll(body, dropLegacy, "voicecom: endpoint error")
}

func init() {
	for _, c := range slowListed {
		v4Changes[c] = struct {
			why  string
			want func(expectation) expectation
		}{"early receipt terminal state kept; carrier transport error sanitized (compatibility.md)", func(l expectation) expectation {
			if !strings.Contains(l.body, slowLegacy) {
				panic(c + ": recording no longer shows the overwritten early receipt")
			}
			l.body = sanitizeDrop(strings.Replace(l.body, slowLegacy, slowV4, 1))
			return l
		}}
	}
}

func fixed(e expectation) func(expectation) expectation {
	return func(expectation) expectation { return e }
}

func (r *replay) compare(t *testing.T) {
	var manifest struct{ Cases []string }
	raw, err := os.ReadFile(fixtureDir() + "/manifest.json")
	if err != nil || json.Unmarshal(raw, &manifest) != nil || len(manifest.Cases) == 0 {
		t.Fatalf("manifest: %v", err)
	}
	for _, c := range manifest.Cases {
		if deferred[c] != "" || observedOnly[c] {
			continue
		}
		if c == "list-filter-status-zero" {
			r.compareStatusZero(t)
			continue
		}
		got, ok := r.got[c]
		if !ok {
			t.Errorf("%s: captured case was not replayed", c)
			continue
		}
		f := loadFixture(t, c)
		want := expectation{f.Response.Status, f.Response.ContentType, f.Response.Body}
		if change, ok := v4Changes[c]; ok {
			want = change.want(want)
		}
		if body := r.normalize(got.body); got.status != want.status || got.contentType != want.contentType || body != want.body {
			t.Errorf("%s:\n got  %d %s %s\n want %d %s %s", c, got.status, got.contentType, body, want.status, want.contentType, want.body)
		}
	}
	for c := range r.got {
		if !contains(manifest.Cases, c) {
			t.Errorf("%s: replayed case is not in the manifest", c)
		}
	}
	for c := range v4Changes {
		if !contains(manifest.Cases, c) {
			t.Errorf("%s: documented change for a case that is not captured", c)
		}
	}
	t.Logf("%d captured cases: %d replayed (%d with documented V4 changes), %d checked as observations, %d deferred",
		len(manifest.Cases), len(r.got), len(v4Changes)+1, len(observedOnly), len(deferred))
}

// compareStatusZero: the early-receipt message is terminal in V4, so the
// status-0 filter no longer lists it (compatibility.md: early receipt
// overwrite); the remaining legacy items keep their order.
func (r *replay) compareStatusZero(t *testing.T) {
	type list struct {
		Items []json.RawMessage `json:"items"`
		Total int               `json:"total"`
	}
	f := loadFixture(t, "list-filter-status-zero")
	var legacy, got list
	got0 := r.got["list-filter-status-zero"]
	if json.Unmarshal([]byte(f.Response.Body), &legacy) != nil || json.Unmarshal([]byte(r.normalize(got0.body)), &got) != nil || got0.status != 200 {
		t.Fatalf("list-filter-status-zero: %d %s", got0.status, got0.body)
	}
	if !strings.Contains(string(legacy.Items[0]), `"{{msg:slow-carrier}}"`) || got.Total != legacy.Total-1 {
		t.Fatalf("list-filter-status-zero: total %d, legacy %d", got.Total, legacy.Total)
	}
	for i, item := range legacy.Items[1:] {
		if i >= len(got.Items) || string(got.Items[i]) != string(item) {
			t.Errorf("list-filter-status-zero item %d:\n got  %s\n want %s", i, got.Items[min(i, len(got.Items)-1)], item)
		}
	}
}

func contains(list []string, s string) bool {
	i := sort.SearchStrings(list, s)
	return i < len(list) && list[i] == s
}
