//go:build parity

package parity

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	. "github.com/go-tangra/go-tangra-sms-gw/v4/internal/parity"
)

// Seeded identities (seed.sql).
const (
	m101 = "00000000-0000-4000-8000-000000000101" // client_a, delivered, provider 1
	m102 = "00000000-0000-4000-8000-000000000102" // client_a, accepted, provider 1 (dlr_token)
	m103 = "00000000-0000-4000-8000-000000000103" // client_b, provider 4
	m104 = "00000000-0000-4000-8000-000000000104" // client_a, provider 4 (no dlr_token)
)

var jsonCT, formCT = JSONCT, FormCT

func bearer(v string) [2]string { return Bearer(v) }

func hdr(h ...[2]string) [][2]string { return h }

func post(path, body string, h ...[2]string) Request { return Post(path, body, h...) }

func get(path string, h ...[2]string) Request { return Get(path, h...) }

func method(m, path string, h ...[2]string) Request { return Method(m, path, h...) }

func php(h ...[2]string) [][2]string { return PHP(h...) }

func login(user string) string {
	return fmt.Sprintf(`{"username":%q,"password":%q}`, user, password)
}

func sendBody(to any, provider, template int, props string) string {
	if props == "" {
		props = "{}"
	}
	b, _ := json.Marshal(to)
	return fmt.Sprintf(`{"to":%s,"providerId":%d,"templateId":%d,"properties":%s,"sms":{"from":"Parity","encoding":"utf-8"}}`,
		b, provider, template, props)
}

// captureLogin stores the token pair of a successful login as tok:<k>/rt:<k>.
func captureLogin(k string) func(*Side, Response) {
	return func(s *Side, r Response) {
		m := r.JSON()
		if a, _ := m["access_token"].(string); a != "" {
			s.Set("tok:"+k, a)
		}
		if rt, _ := m["refresh_token"].(string); rt != "" {
			s.Set("rt:"+k, rt)
		}
	}
}

func captureMsg(k string) func(*Side, Response) {
	return func(s *Side, r Response) {
		if d, ok := r.JSON()["data"].(map[string]any); ok {
			if id, _ := d["id"].(string); id != "" {
				s.Set("msg:"+k, id)
			}
		}
	}
}

// forged returns tokens both services accept or refuse alike (they share
// the secret); lit: variables are never normalised away.
func forged() map[string]string {
	now := time.Now()
	past, future := now.Add(-3*time.Hour), now.Add(time.Hour)
	nbf := Claims("1", "client_a", "API_CLIENT", "access", now, now.Add(3*time.Hour))
	nbf["nbf"] = now.Add(time.Hour).Unix()
	return map[string]string{
		"lit:expired":         Forge(Claims("1", "client_a", "API_CLIENT", "access", past, now.Add(-time.Hour)), "HS256", jwtSecret),
		"lit:expiredRefresh":  Forge(Claims("1", "client_a", "API_CLIENT", "refresh", past, now.Add(-time.Hour)), "HS256", jwtSecret),
		"lit:badsig":          Forge(Claims("1", "client_a", "API_CLIENT", "access", now, future), "HS256", "another-secret-0123456789abcdef0123"),
		"lit:algnone":         Forge(Claims("1", "client_a", "API_CLIENT", "access", now, future), "none", ""),
		"lit:nokind":          Forge(Claims("1", "client_a", "API_CLIENT", "", now, future), "HS256", jwtSecret),
		"lit:futurenbf":       Forge(nbf, "HS256", jwtSecret),
		"lit:disabledAccess":  Forge(Claims("7", "disabled_d", "API_CLIENT", "access", now, future), "HS256", jwtSecret),
		"lit:disabledRefresh": Forge(Claims("7", "disabled_d", "API_CLIENT", "refresh", now, future), "HS256", jwtSecret),
		"lit:unknownAccess":   Forge(Claims("99", "ghost", "API_CLIENT", "access", now, future), "HS256", jwtSecret),
		"lit:unknownRefresh":  Forge(Claims("99", "ghost", "API_CLIENT", "refresh", now, future), "HS256", jwtSecret),
		"lit:bigsubRefresh":   Forge(Claims("4294967296", "client_a", "API_CLIENT", "refresh", now, future), "HS256", jwtSecret),
		"lit:textsubRefresh":  Forge(Claims("abc", "client_a", "API_CLIENT", "refresh", now, future), "HS256", jwtSecret),
		// A viewer account presenting an API_CLIENT claim, and the reverse.
		"lit:viewerAsClient": Forge(Claims("5", "viewer_v", "API_CLIENT", "access", now, future), "HS256", jwtSecret),
		"lit:clientAsViewer": Forge(Claims("1", "client_a", "API_VIEWER", "access", now, future), "HS256", jwtSecret),
	}
}

func dlrQuery(id string, status int, token string, extra ...string) string {
	q := url.Values{}
	q.Set("channel", "sms")
	q.Set("sid", "9999")
	q.Set("message_status", fmt.Sprint(status))
	q.Set("to", "359888000100")
	q.Set("from", "Parity")
	q.Set("timestamp", "1760000000")
	if token != "" {
		q.Set("dlr_token", token)
	}
	// request_id goes last and unescaped: it may be a {{variable}}.
	s := q.Encode() + "&request_id=" + id
	for _, e := range extra {
		s += "&" + e
	}
	return s
}

// corpus is the request corpus, in order. Steps depend on earlier captures.
func corpus() []Step {
	var c []Step
	add := func(group, name string, r Request, capture ...func(*Side, Response)) {
		st := Step{Name: name, Group: group, Req: r}
		if len(capture) > 0 {
			st.Capture = capture[0]
		}
		c = append(c, st)
	}

	// ---- surface ----
	g := "surface"
	add(g, "health", get("/health"))
	add(g, "health-head", method("HEAD", "/health"))
	add(g, "health-options", method("OPTIONS", "/health"))
	add(g, "health-post", post("/health", "{}"))
	add(g, "health-trailing-slash", get("/health/"))
	add(g, "root", get("/"))
	add(g, "root-head", method("HEAD", "/"))
	add(g, "root-post", post("/", "{}"))
	add(g, "index-html", get("/index.html"))
	add(g, "metrics", get("/metrics"))
	add(g, "metrics-sub", get("/metrics/x"))
	add(g, "favicon", get("/favicon.ico"))
	add(g, "unknown-path", get("/nope"))
	add(g, "unknown-path-post", post("/nope", "{}"))
	add(g, "unknown-hermes", get("/hermes"))
	add(g, "unknown-hermes-v1", get("/hermes/v1/"))
	add(g, "unknown-hermes-sub", get("/hermes/v1/unknown"))
	add(g, "login-get", get("/hermes/v1/login"))
	add(g, "login-put", Request{Method: "PUT", Path: "/hermes/v1/login", Header: hdr(jsonCT), Body: login("client_a")})
	add(g, "login-delete", method("DELETE", "/hermes/v1/login"))
	add(g, "login-head", method("HEAD", "/hermes/v1/login"))
	add(g, "login-options-preflight", method("OPTIONS", "/hermes/v1/login", [2]string{"Origin", "https://example.com"},
		[2]string{"Access-Control-Request-Method", "POST"}, [2]string{"Access-Control-Request-Headers", "content-type"}))
	add(g, "login-trailing-slash", post("/hermes/v1/login/", login("client_a")))
	add(g, "login-double-slash", post("//hermes/v1/login", login("client_a")))
	add(g, "login-dot-segment", post("/hermes/v1/x/../login", login("client_a")))
	add(g, "login-uppercase-path", post("/Hermes/v1/login", login("client_a")))
	add(g, "login-query-string", post("/hermes/v1/login?x=1", login("client_a")))
	add(g, "me-trailing-slash", get("/hermes/v1/me/"))
	add(g, "sms-delete", method("DELETE", "/hermes/v1/sms"))
	add(g, "sms-put", Request{Method: "PUT", Path: "/hermes/v1/sms", Header: hdr(jsonCT), Body: "{}"})
	add(g, "v1-sms-patch", method("PATCH", "/v1/sms"))
	add(g, "sms-trailing-slash-post", post("/hermes/v1/sms/", sendBody(359888000100, 4, 2, "")))
	add(g, "sms-trailing-slash-get", get("/hermes/v1/sms/"))
	add(g, "sms-id-post", post("/hermes/v1/sms/"+m101, "{}"))
	add(g, "sms-id-trailing-slash", get("/hermes/v1/sms/"+m101+"/"))
	add(g, "sms-id-extra-segment", get("/hermes/v1/sms/"+m101+"/x"))
	add(g, "dlr-trailing-slash", get("/dlr/?"+dlrQuery(m102, 8, dlrTokenA)))
	add(g, "dlr-head", method("HEAD", "/dlr"))
	add(g, "dlr-post", post("/dlr?"+dlrQuery(m102, 8, dlrTokenA), "{}"))
	add(g, "dlr-post-hermes", post("/hermes/v1/sms/dlr", ""))
	add(g, "dlrs-no-id", get("/hermes/v1/sms/dlr/"))
	add(g, "v1-sms-dlr-no-id", get("/v1/sms/dlr"))
	add(g, "hermes-v1-dlr", get("/hermes/v1/dlr"))
	add(g, "logout-get", get("/hermes/v1/logout"))
	add(g, "refresh-get", get("/hermes/v1/refresh_token"))
	add(g, "me-post", post("/hermes/v1/me", "{}"))

	// ---- login ----
	g = "login"
	add(g, "login-json", post("/hermes/v1/login", login("client_a")), captureLogin("a"))
	add(g, "login-php-sdk", Request{Method: "POST", Path: "/hermes/v1/login", Header: php(),
		Body: `{"username":"client_a","password":"Passw0rd!","grant_type":"password"}`}, captureLogin("aphp"))
	add(g, "login-php-sdk-client-credentials", Request{Method: "POST", Path: "/hermes/v1/login", Header: php(),
		Body: `{"username":"client_b","password":"Passw0rd!","grant_type":"password","client_id":"x","client_secret":"y"}`}, captureLogin("b"))
	add(g, "login-client-c", post("/hermes/v1/login", login("client_c")), captureLogin("c"))
	add(g, "login-viewer", post("/hermes/v1/login", login("viewer_v")), captureLogin("v"))
	add(g, "login-admin", post("/hermes/v1/login", login("admin_x")), captureLogin("x"))
	add(g, "login-form", post("/hermes/v1/login", "username=client_a&password=Passw0rd%21&grant_type=password", formCT), captureLogin("aform"))
	add(g, "login-form-charset", post("/hermes/v1/login", "username=client_a&password=Passw0rd%21", [2]string{"Content-Type", "application/x-www-form-urlencoded; charset=UTF-8"}))
	add(g, "login-json-charset", post("/hermes/v1/login", login("client_a"), [2]string{"Content-Type", "application/json; charset=utf-8"}))
	add(g, "login-no-content-type", Request{Method: "POST", Path: "/hermes/v1/login", Body: login("client_a")})
	add(g, "login-ct-uppercase", post("/hermes/v1/login", login("client_a"), [2]string{"Content-Type", "application/JSON"}))
	add(g, "login-ct-space-before-params", post("/hermes/v1/login", login("client_a"), [2]string{"Content-Type", "application/json ; charset=utf-8"}))
	add(g, "login-ct-vendor-json", post("/hermes/v1/login", login("client_a"), [2]string{"Content-Type", "application/vnd.api+json"}))
	add(g, "login-ct-text-json", post("/hermes/v1/login", login("client_a"), [2]string{"Content-Type", "text/json"}))
	add(g, "login-ct-yaml", post("/hermes/v1/login", "username: client_a\npassword: Passw0rd!\n", [2]string{"Content-Type", "application/yaml"}))
	add(g, "login-form-nested-unknown", post("/hermes/v1/login", "username=client_a&password=Passw0rd%21&a.b.c=1&x[y]=2", formCT))
	add(g, "login-form-empty-password", post("/hermes/v1/login", "username=client_a&password=", formCT))
	add(g, "login-text-plain", post("/hermes/v1/login", login("client_a"), [2]string{"Content-Type", "text/plain"}))
	add(g, "login-xml-content-type", post("/hermes/v1/login", "<LoginRequest><username>client_a</username></LoginRequest>", [2]string{"Content-Type", "application/xml"}))
	add(g, "login-accept-xml", post("/hermes/v1/login", login("client_a"), [2]string{"Accept", "application/xml"}))
	add(g, "login-accept-html", post("/hermes/v1/login", login("client_a"), [2]string{"Accept", "text/html"}))
	add(g, "login-accept-any", post("/hermes/v1/login", login("client_a"), [2]string{"Accept", "*/*"}))
	add(g, "login-accept-xml-error", post("/hermes/v1/login", `{"username":"client_a","password":"wrong"}`, [2]string{"Accept", "application/xml"}))
	add(g, "login-empty-body", post("/hermes/v1/login", "", jsonCT))
	add(g, "login-empty-object", post("/hermes/v1/login", "{}"))
	add(g, "login-missing-password", post("/hermes/v1/login", `{"username":"client_a"}`))
	add(g, "login-missing-username", post("/hermes/v1/login", `{"password":"Passw0rd!"}`))
	add(g, "login-empty-username", post("/hermes/v1/login", `{"username":"","password":"Passw0rd!"}`))
	add(g, "login-wrong-password", post("/hermes/v1/login", `{"username":"client_a","password":"wrong"}`))
	add(g, "login-unknown-user", post("/hermes/v1/login", `{"username":"nobody_here","password":"Passw0rd!"}`))
	add(g, "login-unknown-user-invalid-name", post("/hermes/v1/login", `{"username":"no-such user!","password":"Passw0rd!"}`))
	add(g, "login-disabled", post("/hermes/v1/login", login("disabled_d")))
	add(g, "login-disabled-wrong-password", post("/hermes/v1/login", `{"username":"disabled_d","password":"wrong"}`))
	add(g, "login-username-case", post("/hermes/v1/login", login("Client_A")))
	add(g, "login-username-space", post("/hermes/v1/login", login(" client_a")))
	add(g, "login-long-username", post("/hermes/v1/login", fmt.Sprintf(`{"username":%q,"password":"x"}`, strings.Repeat("u", 1000))))
	add(g, "login-malformed-json", post("/hermes/v1/login", `{"username":`))
	add(g, "login-json-array", post("/hermes/v1/login", `[]`))
	add(g, "login-json-null", post("/hermes/v1/login", `null`))
	add(g, "login-username-number", post("/hermes/v1/login", `{"username":123,"password":"Passw0rd!"}`))
	add(g, "login-unknown-field", post("/hermes/v1/login", `{"username":"client_a","password":"Passw0rd!","foo":{"bar":1}}`))
	add(g, "login-duplicate-field", post("/hermes/v1/login", `{"username":"nobody","username":"client_a","password":"Passw0rd!"}`))
	add(g, "login-trailing-garbage", post("/hermes/v1/login", login("client_a")+`x`))
	add(g, "login-grant-refresh", post("/hermes/v1/login", `{"username":"client_a","password":"Passw0rd!","grant_type":"refresh_token"}`))
	add(g, "login-query-params-only", post("/hermes/v1/login?username=client_a&password=Passw0rd%21", ""))

	// ---- refresh ----
	g = "refresh"
	add(g, "refresh-json", post("/hermes/v1/refresh_token", `{"refresh_token":"{{rt:a}}"}`), captureLogin("a2"))
	add(g, "refresh-php-sdk", Request{Method: "POST", Path: "/hermes/v1/refresh_token", Header: php(),
		Body: `{"refresh_token":"{{rt:aphp}}","grant_type":"refresh_token"}`}, captureLogin("aphp2"))
	add(g, "refresh-form", post("/hermes/v1/refresh_token", "refresh_token={{rt:aform}}", formCT))
	add(g, "refresh-old-token-again", post("/hermes/v1/refresh_token", `{"refresh_token":"{{rt:a}}"}`))
	add(g, "refresh-camel-case", post("/hermes/v1/refresh_token", `{"refreshToken":"{{rt:a}}"}`))
	add(g, "refresh-access-as-refresh", post("/hermes/v1/refresh_token", `{"refresh_token":"{{tok:a}}"}`))
	add(g, "refresh-garbage", post("/hermes/v1/refresh_token", `{"refresh_token":"garbage"}`))
	add(g, "refresh-empty", post("/hermes/v1/refresh_token", `{}`))
	add(g, "refresh-no-body", post("/hermes/v1/refresh_token", ""))
	add(g, "refresh-header-only", post("/hermes/v1/refresh_token", "{}", [2]string{"Authorization", "Bearer {{rt:a}}"}))
	add(g, "refresh-empty-body-json", post("/hermes/v1/refresh_token", "", jsonCT))
	add(g, "refresh-expired", post("/hermes/v1/refresh_token", `{"refresh_token":"{{lit:expiredRefresh}}"}`))
	add(g, "refresh-disabled-account", post("/hermes/v1/refresh_token", `{"refresh_token":"{{lit:disabledRefresh}}"}`))
	add(g, "refresh-unknown-client", post("/hermes/v1/refresh_token", `{"refresh_token":"{{lit:unknownRefresh}}"}`))
	add(g, "refresh-subject-overflow", post("/hermes/v1/refresh_token", `{"refresh_token":"{{lit:bigsubRefresh}}"}`))
	add(g, "refresh-subject-text", post("/hermes/v1/refresh_token", `{"refresh_token":"{{lit:textsubRefresh}}"}`))
	add(g, "refresh-alg-none", post("/hermes/v1/refresh_token", `{"refresh_token":"{{lit:algnone}}"}`))
	add(g, "refresh-malformed-json", post("/hermes/v1/refresh_token", `{`))

	// ---- me ----
	g = "me"
	add(g, "me-client", get("/hermes/v1/me", bearer("tok:a")))
	add(g, "me-client-refreshed-token", get("/hermes/v1/me", bearer("tok:a2")))
	add(g, "me-viewer", get("/hermes/v1/me", bearer("tok:v")))
	add(g, "me-admin", get("/hermes/v1/me", bearer("tok:x")))
	add(g, "me-client-b", get("/hermes/v1/me", bearer("tok:b")))
	add(g, "me-no-bearer", get("/hermes/v1/me"))
	add(g, "me-lowercase-bearer", get("/hermes/v1/me", [2]string{"Authorization", "bearer {{tok:a}}"}))
	add(g, "me-bearer-no-space", get("/hermes/v1/me", [2]string{"Authorization", "Bearer{{tok:a}}"}))
	add(g, "me-bearer-double-space", get("/hermes/v1/me", [2]string{"Authorization", "Bearer  {{tok:a}}"}))
	add(g, "me-bearer-empty", get("/hermes/v1/me", [2]string{"Authorization", "Bearer "}))
	add(g, "me-basic-scheme", get("/hermes/v1/me", [2]string{"Authorization", "Basic Y2xpZW50X2E6UGFzc3cwcmQh"}))
	add(g, "me-refresh-as-access", get("/hermes/v1/me", bearer("rt:a")))
	add(g, "me-expired", get("/hermes/v1/me", bearer("lit:expired")))
	add(g, "me-bad-signature", get("/hermes/v1/me", bearer("lit:badsig")))
	add(g, "me-alg-none", get("/hermes/v1/me", bearer("lit:algnone")))
	add(g, "me-no-kind", get("/hermes/v1/me", bearer("lit:nokind")))
	add(g, "me-future-nbf", get("/hermes/v1/me", bearer("lit:futurenbf")))
	add(g, "me-disabled-account", get("/hermes/v1/me", bearer("lit:disabledAccess")))
	add(g, "me-unknown-client", get("/hermes/v1/me", bearer("lit:unknownAccess")))
	add(g, "me-garbage-token", get("/hermes/v1/me", [2]string{"Authorization", "Bearer abc.def.ghi"}))
	add(g, "me-viewer-claiming-client", get("/hermes/v1/me", bearer("lit:viewerAsClient")))
	add(g, "me-accept-xml", get("/hermes/v1/me", bearer("tok:a"), [2]string{"Accept", "application/xml"}))

	// ---- send ----
	g = "send"
	add(g, "send-happy-token-provider", post("/hermes/v1/sms", sendBody(359888000110, 1, 1, `{"name":"Ana","code":"42"}`), bearer("tok:a")), captureMsg("s1"))
	add(g, "send-happy-prod-provider", post("/hermes/v1/sms", sendBody(359888000111, 4, 2, ""), bearer("tok:a")), captureMsg("s2"))
	add(g, "send-happy-v1-alias", post("/v1/sms", sendBody(359888000112, 4, 13, `{"code":"1234"}`), bearer("tok:a")), captureMsg("s3"))
	add(g, "send-happy-client-b", post("/hermes/v1/sms", sendBody(359888000113, 4, 2, ""), bearer("tok:b")), captureMsg("b1"))
	add(g, "send-php-sdk", Request{Method: "POST", Path: "/hermes/v1/sms", Header: php(bearer("tok:aphp")),
		Body: `{"to":359888000114,"providerId":4,"templateId":13,"properties":{"code":"5678"},"sms":{"from":"Jobs.bg"}}`}, captureMsg("php1"))
	add(g, "send-php-sdk-escaped-unicode", Request{Method: "POST", Path: "/hermes/v1/sms", Header: php(bearer("tok:aphp")),
		Body: `{"to":359888000115,"providerId":4,"templateId":7,"properties":{"name":"Иван \/ <x>"},"sms":{"from":"Cars.bg"}}`}, captureMsg("php2"))
	add(g, "send-php-sdk-empty-properties-array", Request{Method: "POST", Path: "/hermes/v1/sms", Header: php(bearer("tok:aphp")),
		Body: `{"to":359888000116,"providerId":4,"templateId":2,"properties":[],"sms":{"from":"Homes.bg"}}`})
	add(g, "send-php-sdk-sms-text", Request{Method: "POST", Path: "/hermes/v1/sms", Header: php(bearer("tok:aphp")),
		Body: `{"to":359888000117,"providerId":4,"templateId":2,"sms":{"from":"Test","text":"hi"}}`})
	add(g, "send-php-sdk-int-property", Request{Method: "POST", Path: "/hermes/v1/sms", Header: php(bearer("tok:aphp")),
		Body: `{"to":359888000118,"providerId":4,"templateId":13,"properties":{"code":1234},"sms":{"from":"Jobs.bg"}}`})
	add(g, "send-php-sdk-bool-property", Request{Method: "POST", Path: "/hermes/v1/sms", Header: php(bearer("tok:aphp")),
		Body: `{"to":359888000118,"providerId":4,"templateId":13,"properties":{"code":true},"sms":{"from":"Jobs.bg"}}`})
	add(g, "send-php-sdk-null-property", Request{Method: "POST", Path: "/hermes/v1/sms", Header: php(bearer("tok:aphp")),
		Body: `{"to":359888000118,"providerId":4,"templateId":13,"properties":{"code":null},"sms":{"from":"Jobs.bg"}}`})
	add(g, "send-php-sdk-only-to", Request{Method: "POST", Path: "/hermes/v1/sms", Header: php(bearer("tok:aphp")), Body: `{"to":1}`})
	add(g, "send-to-string", post("/hermes/v1/sms", sendBody("359888000119", 4, 2, ""), bearer("tok:a")))
	add(g, "send-to-plus-string", post("/hermes/v1/sms", sendBody("+359888000119", 4, 2, ""), bearer("tok:a")))
	add(g, "send-to-leading-zero-string", post("/hermes/v1/sms", sendBody("0888000119", 4, 2, ""), bearer("tok:a")))
	add(g, "send-to-negative", post("/hermes/v1/sms", sendBody(-359888000119, 4, 2, ""), bearer("tok:a")))
	add(g, "send-to-float", post("/hermes/v1/sms", `{"to":3.59888000119e11,"providerId":4,"templateId":2}`, bearer("tok:a")))
	add(g, "send-to-fraction", post("/hermes/v1/sms", `{"to":359888000119.5,"providerId":4,"templateId":2}`, bearer("tok:a")))
	add(g, "send-to-zero", post("/hermes/v1/sms", sendBody(0, 4, 2, ""), bearer("tok:a")))
	add(g, "send-to-missing", post("/hermes/v1/sms", `{"providerId":4,"templateId":2}`, bearer("tok:a")))
	add(g, "send-to-short", post("/hermes/v1/sms", sendBody(123456, 4, 2, ""), bearer("tok:a")))
	add(g, "send-to-min-length", post("/hermes/v1/sms", sendBody(1234567, 4, 2, ""), bearer("tok:a")))
	add(g, "send-to-long", post("/hermes/v1/sms", sendBody(int64(1234567890123456), 4, 2, ""), bearer("tok:a")))
	add(g, "send-to-overflow", post("/hermes/v1/sms", `{"to":18446744073709551616,"providerId":4,"templateId":2}`, bearer("tok:a")))
	add(g, "send-to-text", post("/hermes/v1/sms", `{"to":"abc","providerId":4,"templateId":2}`, bearer("tok:a")))
	add(g, "send-snake-case-ids", post("/hermes/v1/sms", `{"to":359888000120,"provider_id":4,"template_id":2}`, bearer("tok:a")))
	add(g, "send-ids-as-strings", post("/hermes/v1/sms", `{"to":359888000121,"providerId":"4","templateId":"2"}`, bearer("tok:a")))
	add(g, "send-both-id-spellings", post("/hermes/v1/sms", `{"to":359888000121,"providerId":4,"provider_id":1,"templateId":2}`, bearer("tok:a")))
	add(g, "send-all-zero", post("/hermes/v1/sms", `{}`, bearer("tok:a")))
	add(g, "send-provider-zero", post("/hermes/v1/sms", sendBody(359888000122, 0, 2, ""), bearer("tok:a")))
	add(g, "send-provider-zero-to-zero", post("/hermes/v1/sms", sendBody(0, 0, 0, ""), bearer("tok:a")))
	add(g, "send-provider-unknown", post("/hermes/v1/sms", sendBody(359888000122, 999, 2, ""), bearer("tok:a")))
	add(g, "send-provider-unknown-template-zero", post("/hermes/v1/sms", sendBody(359888000122, 999, 0, ""), bearer("tok:a")))
	add(g, "send-provider-off", post("/hermes/v1/sms", sendBody(359888000122, 5, 2, ""), bearer("tok:a")))
	add(g, "send-provider-viber", post("/hermes/v1/sms", sendBody(359888000122, 12, 2, ""), bearer("tok:a")))
	add(g, "send-template-zero", post("/hermes/v1/sms", sendBody(359888000122, 4, 0, ""), bearer("tok:a")))
	add(g, "send-template-unknown", post("/hermes/v1/sms", sendBody(359888000122, 4, 999, ""), bearer("tok:a")))
	add(g, "send-template-off", post("/hermes/v1/sms", sendBody(359888000122, 4, 3, ""), bearer("tok:a")))
	add(g, "send-template-viber", post("/hermes/v1/sms", sendBody(359888000122, 4, 4, ""), bearer("tok:a")))
	add(g, "send-template-no-body", post("/hermes/v1/sms", sendBody(359888000122, 4, 5, ""), bearer("tok:a")))
	add(g, "send-template-bad-syntax", post("/hermes/v1/sms", sendBody(359888000122, 4, 6, `{"name":"x"}`), bearer("tok:a")))
	add(g, "send-template-missing-vars", post("/hermes/v1/sms", sendBody(359888000123, 4, 1, ""), bearer("tok:a")))
	add(g, "send-blocked-for-provider", post("/hermes/v1/sms", sendBody(359888000301, 1, 2, ""), bearer("tok:a")))
	add(g, "send-blocked-other-provider-ok", post("/hermes/v1/sms", sendBody(359888000301, 4, 2, ""), bearer("tok:a")))
	add(g, "send-blocked-global", post("/hermes/v1/sms", sendBody(359888000302, 4, 2, ""), bearer("tok:a")))
	add(g, "send-blocked-disabled-block", post("/hermes/v1/sms", sendBody(359888000303, 1, 2, ""), bearer("tok:a")))
	add(g, "send-blocked-template-zero", post("/hermes/v1/sms", sendBody(359888000302, 4, 0, ""), bearer("tok:a")))
	add(g, "send-carrier-reject", post("/hermes/v1/sms", sendBody(359888000124, 10, 2, ""), bearer("tok:a")), captureMsg("rej"))
	add(g, "send-carrier-http500", post("/hermes/v1/sms", sendBody(359888000125, 11, 2, ""), bearer("tok:a")))
	add(g, "send-viewer", post("/hermes/v1/sms", sendBody(359888000126, 4, 2, ""), bearer("tok:v")))
	add(g, "send-viewer-invalid-body", post("/hermes/v1/sms", `{`, bearer("tok:v")))
	add(g, "send-admin", post("/hermes/v1/sms", sendBody(359888000126, 4, 2, ""), bearer("tok:x")))
	add(g, "send-client-claiming-viewer", post("/hermes/v1/sms", sendBody(359888000126, 4, 2, ""), bearer("lit:clientAsViewer")))
	add(g, "send-no-bearer", post("/hermes/v1/sms", sendBody(359888000126, 4, 2, "")))
	add(g, "send-no-bearer-invalid-body", post("/hermes/v1/sms", `{`))
	add(g, "send-lowercase-bearer", post("/hermes/v1/sms", sendBody(359888000126, 4, 2, ""), [2]string{"Authorization", "bearer {{tok:a}}"}))
	add(g, "send-expired", post("/hermes/v1/sms", sendBody(359888000126, 4, 2, ""), bearer("lit:expired")))
	add(g, "send-refresh-token", post("/hermes/v1/sms", sendBody(359888000126, 4, 2, ""), bearer("rt:a")))
	add(g, "send-large-body-under-limit", post("/hermes/v1/sms", sendBody(359888000128, 4, 13, `{"code":"1","pad":"`+strings.Repeat("x", 3<<10)+`"}`), bearer("tok:a")))
	add(g, "send-unknown-fields", post("/hermes/v1/sms", `{"to":359888000129,"providerId":4,"templateId":2,"text":"x","defer":"later","foo":{"a":[1]}}`, bearer("tok:a")))
	add(g, "send-form-encoded", post("/hermes/v1/sms", "to=359888000130&providerId=4&templateId=13&properties%5Bcode%5D=1", formCT, bearer("tok:a")))
	add(g, "send-form-properties-dot", post("/hermes/v1/sms", "to=359888000135&providerId=4&templateId=13&properties.code=7", formCT, bearer("tok:a")))
	add(g, "send-form-sms-nested", post("/hermes/v1/sms", "to=359888000136&providerId=4&templateId=2&sms.from=Form&sms.validity.ttl=5", formCT, bearer("tok:a")))
	add(g, "send-form-sms-leaf", post("/hermes/v1/sms", "to=359888000137&providerId=4&templateId=2&sms=x", formCT, bearer("tok:a")))
	add(g, "send-form-bad-to", post("/hermes/v1/sms", "to=abc&providerId=4&templateId=2", formCT, bearer("tok:a")))
	add(g, "send-form-two-to", post("/hermes/v1/sms", "to=1&to=2&providerId=4&templateId=2", formCT, bearer("tok:a")))
	add(g, "send-form-bad-map-path", post("/hermes/v1/sms", "to=359888000138&providerId=4&templateId=2&properties.a.b=1", formCT, bearer("tok:a")))
	add(g, "send-form-empty-to", post("/hermes/v1/sms", "to=&providerId=4&templateId=2", formCT, bearer("tok:a")))
	add(g, "send-form-snake", post("/hermes/v1/sms", "to=359888000131&provider_id=4&template_id=2", formCT, bearer("tok:a")))
	add(g, "send-malformed-json", post("/hermes/v1/sms", `{"to":`, bearer("tok:a")))
	add(g, "send-empty-body", post("/hermes/v1/sms", "", jsonCT, bearer("tok:a")))
	add(g, "send-properties-null", post("/hermes/v1/sms", `{"to":359888000132,"providerId":4,"templateId":2,"properties":null,"sms":null}`, bearer("tok:a")))
	add(g, "send-sms-fields", post("/hermes/v1/sms", `{"to":359888000133,"providerId":4,"templateId":2,"sms":{"from":"X","encoding":"ucs2","concatenate":1,"validity":{"ttl":60,"units":"m"},"mccmnc":28401}}`, bearer("tok:a")))
	add(g, "send-sms-wrong-type", post("/hermes/v1/sms", `{"to":359888000133,"providerId":4,"templateId":2,"sms":"x"}`, bearer("tok:a")))
	add(g, "send-accept-xml", post("/hermes/v1/sms", sendBody(359888000134, 4, 2, ""), bearer("tok:a"), [2]string{"Accept", "application/xml"}))
	add(g, "send-viewer-provider-zero", post("/hermes/v1/sms", sendBody(359888000122, 0, 2, ""), bearer("tok:v")))

	// ---- PHP SDK token renewal: 401 on send -> refresh -> resend ----
	g = "php-flow"
	add(g, "php-send-expired-token", Request{Method: "POST", Path: "/hermes/v1/sms", Header: php([2]string{"Authorization", "Bearer {{lit:expired}}"}),
		Body: `{"to":359888000140,"providerId":4,"templateId":13,"properties":{"code":"1"}}`})
	add(g, "php-refresh", Request{Method: "POST", Path: "/hermes/v1/refresh_token", Header: php(),
		Body: `{"refresh_token":"{{rt:aphp2}}","grant_type":"refresh_token"}`}, captureLogin("aphp3"))
	add(g, "php-resend", Request{Method: "POST", Path: "/hermes/v1/sms", Header: php(bearer("tok:aphp3")),
		Body: `{"to":359888000140,"providerId":4,"templateId":13,"properties":{"code":"1"}}`})

	// ---- get / receipts ----
	g = "read"
	for _, alias := range []string{"/hermes/v1/sms/", "/v1/sms/"} {
		p := strings.Trim(strings.ReplaceAll(alias, "/", "-"), "-")
		add(g, p+"-get-seeded", get(alias+m101, bearer("tok:a")))
		add(g, p+"-get-new", get(alias+"{{msg:s1}}", bearer("tok:a")))
		add(g, p+"-dlrs-seeded", get(strings.Replace(alias, "sms/", "sms/dlr/", 1)+m101, bearer("tok:a")))
	}
	add(g, "get-carrier-rejected", get("/hermes/v1/sms/{{msg:rej}}", bearer("tok:a")))
	add(g, "get-other-owner-seeded", get("/hermes/v1/sms/"+m103, bearer("tok:a")))
	add(g, "get-other-owner-new", get("/hermes/v1/sms/{{msg:b1}}", bearer("tok:a")))
	add(g, "get-own-by-b", get("/hermes/v1/sms/{{msg:b1}}", bearer("tok:b")))
	add(g, "get-viewer", get("/hermes/v1/sms/"+m101, bearer("tok:v")))
	add(g, "get-admin", get("/hermes/v1/sms/"+m101, bearer("tok:x")))
	add(g, "get-unknown-v4-uuid", get("/hermes/v1/sms/00000000-0000-4000-8000-0000000009ff", bearer("tok:a")))
	add(g, "get-unknown-v7-uuid", get("/hermes/v1/sms/01890000-0000-7000-8000-0000000009ff", bearer("tok:a")))
	add(g, "get-uppercase-id", get("/hermes/v1/sms/"+strings.ToUpper(m101), bearer("tok:a")))
	add(g, "get-malformed-id", get("/hermes/v1/sms/abc", bearer("tok:a")))
	add(g, "get-numeric-id", get("/hermes/v1/sms/123", bearer("tok:a")))
	add(g, "get-braced-id", get("/hermes/v1/sms/{"+m101+"}", bearer("tok:a")))
	add(g, "get-no-bearer", get("/hermes/v1/sms/"+m101))
	add(g, "get-expired", get("/hermes/v1/sms/"+m101, bearer("lit:expired")))
	add(g, "dlrs-other-owner", get("/hermes/v1/sms/dlr/"+m103, bearer("tok:a")))
	add(g, "dlrs-unknown", get("/hermes/v1/sms/dlr/00000000-0000-4000-8000-0000000009ff", bearer("tok:a")))
	add(g, "dlrs-malformed-id", get("/hermes/v1/sms/dlr/abc", bearer("tok:a")))
	add(g, "dlrs-new-empty", get("/hermes/v1/sms/dlr/{{msg:s2}}", bearer("tok:a")))
	add(g, "dlrs-viewer", get("/hermes/v1/sms/dlr/"+m101, bearer("tok:v")))
	add(g, "dlrs-admin", get("/hermes/v1/sms/dlr/"+m101, bearer("tok:x")))
	add(g, "dlrs-no-bearer", get("/hermes/v1/sms/dlr/"+m101))
	add(g, "dlrs-paging-params", get("/hermes/v1/sms/dlr/"+m101+"?page=1&pageSize=1", bearer("tok:a")))

	// ---- carrier receipts ----
	g = "dlr"
	add(g, "dlr-new-intermediate", get("/dlr?"+dlrQuery("{{msg:s1}}", 8, dlrTokenA)))
	add(g, "dlr-new-after-intermediate-get", get("/hermes/v1/sms/{{msg:s1}}", bearer("tok:a")))
	add(g, "dlr-new-duplicate", get("/hermes/v1/sms/dlr?"+dlrQuery("{{msg:s1}}", 8, dlrTokenA)))
	add(g, "dlr-new-bad-token", get("/dlr?"+dlrQuery("{{msg:s1}}", 2, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")))
	add(g, "dlr-new-missing-token", get("/dlr?"+dlrQuery("{{msg:s1}}", 2, "")))
	add(g, "dlr-new-terminal", get("/dlr?"+dlrQuery("{{msg:s1}}", 1, dlrTokenA)))
	add(g, "dlr-new-after-terminal", get("/dlr?"+dlrQuery("{{msg:s1}}", 8, dlrTokenA)))
	add(g, "dlr-new-state", get("/hermes/v1/sms/{{msg:s1}}", bearer("tok:a")))
	add(g, "dlr-new-receipts", get("/hermes/v1/sms/dlr/{{msg:s1}}", bearer("tok:a")))
	add(g, "dlr-prod-provider-no-token", get("/hermes/v1/sms/dlr?"+dlrQuery("{{msg:s2}}", 1, "")))
	add(g, "dlr-prod-provider-any-token", get("/hermes/v1/sms/dlr?"+dlrQuery("{{msg:s3}}", 2, "whatever")))
	add(g, "dlr-prod-provider-state", get("/hermes/v1/sms?query=%7B%22recipient%22%3A%2235988800011%22%7D", bearer("tok:a")))
	add(g, "dlr-imported-with-token", get("/dlr?"+dlrQuery(m102, 1, dlrTokenA)))
	add(g, "dlr-imported-no-token-provider", get("/hermes/v1/sms/dlr?"+dlrQuery(m104, 16, "")))
	add(g, "dlr-imported-state", get("/hermes/v1/sms/dlr/"+m102, bearer("tok:a")))
	add(g, "dlr-imported-state-104", get("/hermes/v1/sms/"+m104, bearer("tok:a")))
	add(g, "dlr-unknown-request-id", get("/dlr?"+dlrQuery("00000000-0000-4000-8000-0000000009ff", 1, dlrTokenA)))
	add(g, "dlr-malformed-request-id", get("/dlr?"+dlrQuery("not-a-uuid", 1, dlrTokenA)))
	add(g, "dlr-empty-request-id", get("/dlr?"+dlrQuery("", 1, dlrTokenA)))
	add(g, "dlr-no-params", get("/dlr"))
	add(g, "dlr-no-params-hermes", get("/hermes/v1/sms/dlr"))
	add(g, "dlr-s2-before-malformed", get("/hermes/v1/sms/dlr/{{msg:s2}}", bearer("tok:a")))
	add(g, "dlr-camel-case", get("/dlr?requestId={{msg:s2}}&messageStatus=2&dlr_token=x"))
	add(g, "dlr-s2-after-camel-case", get("/hermes/v1/sms/dlr/{{msg:s2}}", bearer("tok:a")))
	add(g, "dlr-s2-message-after-camel-case", get("/hermes/v1/sms/{{msg:s2}}", bearer("tok:a")))
	add(g, "dlr-malformed-status", get("/dlr?request_id={{msg:s2}}&message_status=abc"))
	add(g, "dlr-negative-status", get("/dlr?request_id={{msg:s2}}&message_status=-1"))
	add(g, "dlr-malformed-to", get("/dlr?request_id={{msg:s2}}&message_status=8&to=abc"))
	add(g, "dlr-malformed-timestamp", get("/dlr?request_id={{msg:s2}}&message_status=8&timestamp=1.5"))
	add(g, "dlr-malformed-sid", get("/dlr?request_id={{msg:s2}}&message_status=8&sid=x"))
	add(g, "dlr-s2-after-malformed", get("/hermes/v1/sms/dlr/{{msg:s2}}", bearer("tok:a")))
	add(g, "dlr-unknown-status-code", get("/dlr?"+dlrQuery("{{msg:php1}}", 77, "")))
	add(g, "dlr-with-bearer", get("/dlr?"+dlrQuery("{{msg:php1}}", 8, ""), bearer("tok:a")))
	add(g, "dlr-forwarded-for", get("/dlr?"+dlrQuery("{{msg:php1}}", 0, ""), [2]string{"X-Forwarded-For", "203.0.113.9, 10.0.0.1"}))
	add(g, "dlr-real-ip", get("/dlr?"+dlrQuery("{{msg:php1}}", 2, ""), [2]string{"X-Real-IP", "203.0.113.10"}))
	add(g, "dlr-php1-receipts", get("/hermes/v1/sms/dlr/{{msg:php1}}", bearer("tok:aphp")))
	add(g, "dlr-duplicate-param", get("/dlr?request_id={{msg:php2}}&request_id=x&message_status=8"))
	add(g, "dlr-accept-xml", get("/dlr?request_id=x", [2]string{"Accept", "application/xml"}))
	add(g, "dlr-two-request-ids", get("/dlr?request_id={{msg:php2}}&request_id={{msg:php2}}&message_status=8&dlr_token=x"))
	add(g, "dlr-empty-status", get("/dlr?request_id={{msg:php2}}&message_status=&to="))
	add(g, "dlr-path-id", get("/dlr/{{msg:php2}}"))

	// ---- list ----
	g = "list"
	add(g, "list-default", get("/hermes/v1/sms", bearer("tok:a")))
	add(g, "list-v1-alias", get("/v1/sms?pageSize=3", bearer("tok:a")))
	add(g, "list-page-2", get("/hermes/v1/sms?page=2&pageSize=3", bearer("tok:a")))
	add(g, "list-page-beyond", get("/hermes/v1/sms?page=99&pageSize=3", bearer("tok:a")))
	add(g, "list-page-size-zero", get("/hermes/v1/sms?pageSize=0", bearer("tok:a")))
	add(g, "list-page-size-negative", get("/hermes/v1/sms?page=-1&pageSize=-5", bearer("tok:a")))
	add(g, "list-page-size-huge", get("/hermes/v1/sms?pageSize=100000", bearer("tok:a")))
	add(g, "list-page-text", get("/hermes/v1/sms?page=abc", bearer("tok:a")))
	add(g, "list-page-size-overflow", get("/hermes/v1/sms?pageSize=99999999999", bearer("tok:a")))
	add(g, "list-nopaging", get("/hermes/v1/sms?nopaging=true&pageSize=2", bearer("tok:a")))
	add(g, "list-nopaging-text", get("/hermes/v1/sms?nopaging=yes", bearer("tok:a")))
	add(g, "list-order-by", get("/hermes/v1/sms?orderBy=-createTime&orderBy=id&pageSize=2", bearer("tok:a")))
	add(g, "list-query-recipient", get("/hermes/v1/sms?query="+url.QueryEscape(`{"recipient":"3598880001"}`), bearer("tok:a")))
	add(g, "list-query-status", get("/hermes/v1/sms?query="+url.QueryEscape(`{"status":1}`), bearer("tok:a")))
	add(g, "list-query-status-zero", get("/hermes/v1/sms?query="+url.QueryEscape(`{"status":0}`)+"&pageSize=3", bearer("tok:a")))
	add(g, "list-query-sid", get("/hermes/v1/sms?query="+url.QueryEscape(`{"sid":9999}`), bearer("tok:a")))
	add(g, "list-query-username", get("/hermes/v1/sms?query="+url.QueryEscape(`{"api_client_username":"client_b"}`), bearer("tok:a")))
	add(g, "list-query-malformed", get("/hermes/v1/sms?query=%7Bnot-json&pageSize=2", bearer("tok:a")))
	add(g, "list-query-wrong-type", get("/hermes/v1/sms?query="+url.QueryEscape(`{"status":"1"}`)+"&pageSize=2", bearer("tok:a")))
	add(g, "list-or-param", get("/hermes/v1/sms?or="+url.QueryEscape(`{"recipient":"1"}`)+"&pageSize=2", bearer("tok:a")))
	add(g, "list-client-b", get("/hermes/v1/sms", bearer("tok:b")))
	add(g, "list-viewer", get("/hermes/v1/sms", bearer("tok:v")))
	add(g, "list-admin", get("/hermes/v1/sms?pageSize=2", bearer("tok:x")))
	add(g, "list-no-bearer", get("/hermes/v1/sms"))
	add(g, "list-page-empty", get("/hermes/v1/sms?page=&pageSize=2", bearer("tok:a")))
	add(g, "list-page-twice", get("/hermes/v1/sms?page=1&page=2", bearer("tok:a")))
	add(g, "list-nested-unknown", get("/hermes/v1/sms?a.b=1&pageSize=1", bearer("tok:a")))
	add(g, "list-unknown-param", get("/hermes/v1/sms?foo=bar&pageSize=1", bearer("tok:a")))
	add(g, "list-camel-snake-page-size", get("/hermes/v1/sms?page_size=1", bearer("tok:a")))

	// ---- logout ----
	g = "logout"
	add(g, "logout-no-content-type", Request{Method: "POST", Path: "/hermes/v1/logout", Body: "{}"})
	add(g, "logout-no-tokens", post("/hermes/v1/logout", "{}"))
	add(g, "logout-empty-json", post("/hermes/v1/logout", "{}"))
	add(g, "logout-malformed-json", post("/hermes/v1/logout", "{"))
	add(g, "logout-form", post("/hermes/v1/logout", "a=b", formCT))
	add(g, "logout-php-style", Request{Method: "POST", Path: "/hermes/v1/logout", Header: php(bearer("tok:aphp3"))})
	add(g, "logout-garbage-bearer", post("/hermes/v1/logout", "{}", [2]string{"Authorization", "Bearer garbage"}))
	add(g, "logout-expired-bearer", post("/hermes/v1/logout", "{}", bearer("lit:expired")))
	add(g, "logout-bearer-b", post("/hermes/v1/logout", "{}", bearer("tok:b")))
	add(g, "logout-me-after", get("/hermes/v1/me", bearer("tok:b")))
	add(g, "logout-send-after", post("/hermes/v1/sms", sendBody(359888000150, 4, 2, ""), bearer("tok:b")))
	add(g, "logout-refresh-after-access-revoked", post("/hermes/v1/refresh_token", `{"refresh_token":"{{rt:b}}"}`))
	add(g, "logout-refresh-header", post("/hermes/v1/logout", "{}", [2]string{"X-Refresh-Token", "{{rt:a2}}"}))
	add(g, "logout-refresh-after-refresh-revoked", post("/hermes/v1/refresh_token", `{"refresh_token":"{{rt:a2}}"}`))
	add(g, "logout-me-after-refresh-revoked", get("/hermes/v1/me", bearer("tok:a2")))
	add(g, "logout-me-other-pair-still-valid", get("/hermes/v1/me", bearer("tok:a")))
	add(g, "logout-lowercase-bearer", post("/hermes/v1/logout", "{}", [2]string{"Authorization", "bearer {{tok:aform}}"}))
	add(g, "logout-lowercase-bearer-me", get("/hermes/v1/me", bearer("tok:aform")))
	add(g, "logout-refresh-as-bearer", post("/hermes/v1/logout", "{}", bearer("rt:aform")))
	add(g, "logout-refresh-as-bearer-me", get("/hermes/v1/me", bearer("tok:aform")))
	add(g, "logout-both", post("/hermes/v1/logout", "{}", bearer("tok:c"), [2]string{"X-Refresh-Token", "{{rt:c}}"}))
	add(g, "logout-both-me", get("/hermes/v1/me", bearer("tok:c")))
	// ---- sends V4 deliberately refuses (documented): last, so the state
	// they create on v3 cannot change the earlier reads ----
	g = "divergent"
	add(g, "send-viewer-claiming-client", post("/hermes/v1/sms", sendBody(359888000126, 4, 2, ""), bearer("lit:viewerAsClient")))
	add(g, "send-disabled-account-token", post("/hermes/v1/sms", sendBody(359888000127, 4, 2, ""), bearer("lit:disabledAccess")))
	add(g, "send-unknown-client-token", post("/hermes/v1/sms", sendBody(359888000127, 4, 2, ""), bearer("lit:unknownAccess")))
	add(g, "send-for-token-prefix", post("/hermes/v1/sms", sendBody(359888000170, 1, 2, ""), bearer("tok:a")), captureMsg("s4"))
	add(g, "dlr-token-prefix", get("/dlr?"+dlrQuery("{{msg:s4}}", 2, dlrTokenA+"extra")))
	add(g, "dlr-token-prefix-state", get("/hermes/v1/sms/{{msg:s4}}", bearer("tok:a")))
	add(g, "send-oversized-body", post("/hermes/v1/sms", sendBody(359888000128, 4, 13, `{"code":"1","pad":"`+strings.Repeat("x", 100<<10)+`"}`), bearer("tok:a")))
	return c
}

// floods are the rate-limit exchanges of a profile: concurrent bursts whose
// first 429 is compared (the exact count depends on timing and is reported).
func floods(p profile) []Step {
	login := func(s *Side) (Response, string) {
		return Flood(s, 2*p.loginBurst+40, 16, Request{Method: "POST", Path: "/hermes/v1/login", Header: hdr(jsonCT, [2]string{"X-Real-IP", "203.0.113.77"}),
			Body: `{"username":"client_a","password":"wrong"}`})
	}
	loginDirect := func(s *Side) (Response, string) {
		return Flood(s, 2*p.loginBurst+40, 16, Request{Method: "POST", Path: "/hermes/v1/login", Header: hdr(jsonCT),
			Body: `{"username":"client_a","password":"wrong"}`})
	}
	send := func(s *Side) (Response, string) {
		return Flood(s, 2*p.sendBurst+60, 32, Request{Method: "POST", Path: "/hermes/v1/sms", Header: hdr(jsonCT, bearer("tok:c2")),
			Body: sendBody(359888000160, 0, 2, "")})
	}
	out := []Step{
		{Name: "ratelimit-login-" + p.name, Group: "ratelimit", Custom: login},
		{Name: "ratelimit-send-" + p.name, Group: "ratelimit", Custom: send},
		{Name: "ratelimit-send-other-client-" + p.name, Group: "ratelimit", Req: post("/hermes/v1/sms", sendBody(359888000161, 0, 2, ""), bearer("tok:b2"))},
		{Name: "ratelimit-login-other-ip-" + p.name, Group: "ratelimit", Req: post("/hermes/v1/login", `{"username":"client_a","password":"wrong"}`, [2]string{"X-Real-IP", "203.0.113.78"})},
		{Name: "ratelimit-login-direct-" + p.name, Group: "ratelimit", Custom: loginDirect},
		{Name: "ratelimit-refresh-unlimited-" + p.name, Group: "ratelimit", Req: post("/hermes/v1/refresh_token", `{"refresh_token":"{{rt:b2}}"}`)},
		{Name: "ratelimit-send-invalid-body-" + p.name, Group: "ratelimit", Req: post("/hermes/v1/sms", `{`, bearer("tok:c2"))},
		{Name: "ratelimit-send-viewer-" + p.name, Group: "ratelimit", Req: post("/hermes/v1/sms", sendBody(359888000161, 4, 2, ""), bearer("tok:v2"))},
	}
	return out
}
