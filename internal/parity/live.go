package parity

import (
	"fmt"
	"net/url"
	"sort"
)

// LiveSteps is the read-only corpus for two running deployments. It never
// sends an SMS, logs in, posts a receipt (receipts mutate state) or logs out
// a token: only health, unknown paths, methods and redirects both reject
// without side effects, authentication failures, and, with a bearer token
// (variable lit:bearer, valid on both), /me, list variants and reads of
// message ids present on both instances. Data reads compare by shape, since
// the instances hold different traffic; errors compare exactly.
func LiveSteps(withBearer bool) []Step {
	s := []Step{
		{Name: "live-health", Req: Get("/health")},
		{Name: "live-health-head", Req: Method("HEAD", "/health")},
		{Name: "live-health-trailing-slash", Req: Get("/health/")},
		{Name: "live-health-unclean", Req: Get("//health")},
		{Name: "live-root", Req: Get("/")},
		{Name: "live-root-head", Req: Method("HEAD", "/")},
		{Name: "live-metrics", Req: Get("/metrics")},
		{Name: "live-unknown", Req: Get("/nope")},
		{Name: "live-unknown-hermes", Req: Get("/hermes/v1/unknown")},
		{Name: "live-login-get", Req: Get("/hermes/v1/login")},
		{Name: "live-logout-get", Req: Get("/hermes/v1/logout")},
		{Name: "live-me-post", Req: Post("/hermes/v1/me", "")},
		{Name: "live-sms-delete", Req: Method("DELETE", "/hermes/v1/sms")},
		{Name: "live-dlr-post", Req: Post("/dlr", "")},
		{Name: "live-dlr-head", Req: Method("HEAD", "/dlr")},
		{Name: "live-login-trailing-slash", Req: Post("/hermes/v1/login/", "")},
		{Name: "live-sms-trailing-slash", Req: Get("/hermes/v1/sms/")},
		{Name: "live-me-no-bearer", Req: Get("/hermes/v1/me")},
		{Name: "live-me-garbage-bearer", Req: Get("/hermes/v1/me", [2]string{"Authorization", "Bearer abc.def.ghi"})},
		{Name: "live-me-basic", Req: Get("/hermes/v1/me", [2]string{"Authorization", "Basic eDp5"})},
		{Name: "live-list-no-bearer", Req: Get("/hermes/v1/sms")},
		{Name: "live-get-no-bearer", Req: Get("/hermes/v1/sms/00000000-0000-4000-8000-000000000000")},
		{Name: "live-dlrs-no-bearer", Req: Get("/v1/sms/dlr/00000000-0000-4000-8000-000000000000")},
		{Name: "live-refresh-garbage", Req: Post("/hermes/v1/refresh_token", `{"refresh_token":"garbage"}`)},
		{Name: "live-refresh-empty", Req: Post("/hermes/v1/refresh_token", `{}`)},
		{Name: "live-refresh-no-content-type", Req: Request{Method: "POST", Path: "/hermes/v1/refresh_token", Body: `{}`}},
		{Name: "live-refresh-malformed", Req: Post("/hermes/v1/refresh_token", `{`)},
		{Name: "live-logout-no-token", Req: Post("/hermes/v1/logout", `{}`)},
	}
	if !withBearer {
		return s
	}
	b := [2]string{"Authorization", "Bearer {{lit:bearer}}"}
	ids := func(side *Side, r Response) {
		items, _ := r.JSON()["items"].([]any)
		var out []string
		for _, it := range items {
			if m, ok := it.(map[string]any); ok {
				if id, _ := m["id"].(string); id != "" {
					out = append(out, id)
				}
			}
		}
		side.Set("ids", fmt.Sprint(out))
		side.mu.Lock()
		side.idList = out
		side.mu.Unlock()
	}
	s = append(s,
		Step{Name: "live-me", Req: Get("/hermes/v1/me", b)},
		Step{Name: "live-me-accept-xml", Req: Get("/hermes/v1/me", b, [2]string{"Accept", "application/xml"})},
		Step{Name: "live-list-default", Req: Get("/hermes/v1/sms?pageSize=200", b), Capture: ids, Shape: true},
		Step{Name: "live-list-v1-alias", Req: Get("/v1/sms?pageSize=2", b), Shape: true},
		Step{Name: "live-list-page-2", Req: Get("/hermes/v1/sms?page=2&pageSize=2", b), Shape: true},
		Step{Name: "live-list-page-size-zero", Req: Get("/hermes/v1/sms?pageSize=0", b), Shape: true},
		Step{Name: "live-list-negative", Req: Get("/hermes/v1/sms?page=-1&pageSize=-1", b), Shape: true},
		Step{Name: "live-list-nopaging", Req: Get("/hermes/v1/sms?nopaging=true&pageSize=2", b), Shape: true},
		Step{Name: "live-list-query-status", Req: Get("/hermes/v1/sms?pageSize=2&query="+url.QueryEscape(`{"status":1}`), b), Shape: true},
		Step{Name: "live-list-query-recipient", Req: Get("/hermes/v1/sms?pageSize=2&query="+url.QueryEscape(`{"recipient":"359"}`), b), Shape: true},
		Step{Name: "live-list-query-malformed", Req: Get("/hermes/v1/sms?pageSize=2&query=%7Bx", b), Shape: true},
		Step{Name: "live-list-page-text", Req: Get("/hermes/v1/sms?page=abc", b)},
		Step{Name: "live-list-page-twice", Req: Get("/hermes/v1/sms?page=1&page=2", b)},
		Step{Name: "live-get-unknown", Req: Get("/hermes/v1/sms/00000000-0000-4000-8000-000000000000", b)},
		Step{Name: "live-get-malformed", Req: Get("/hermes/v1/sms/abc", b)},
		Step{Name: "live-dlrs-unknown", Req: Get("/hermes/v1/sms/dlr/00000000-0000-4000-8000-000000000000", b)},
		Step{Name: "live-v1-sms-dlr-no-id", Req: Get("/v1/sms/dlr", b)},
		Step{Name: "live-get-lowercase-bearer", Req: Get("/hermes/v1/sms/abc", [2]string{"Authorization", "bearer {{lit:bearer}}"})},
	)
	// Reads of messages both instances hold (imported before the split).
	shared := func(a, b *Side) {
		a.mu.Lock()
		seen := map[string]bool{}
		for _, id := range a.idList {
			seen[id] = true
		}
		a.mu.Unlock()
		b.mu.Lock()
		var common []string
		for _, id := range b.idList {
			if seen[id] {
				common = append(common, id)
			}
		}
		b.mu.Unlock()
		sort.Strings(common)
		for i := 0; i < 3 && i < len(common); i++ {
			k := fmt.Sprintf("lit:shared%d", i)
			a.Set(k, common[i])
			b.Set(k, common[i])
		}
	}
	for i := range 3 {
		v := fmt.Sprintf("{{lit:shared%d}}", i)
		st := Step{Name: fmt.Sprintf("live-get-shared-%d", i), Req: Get("/hermes/v1/sms/"+v, b), Shape: true}
		if i == 0 {
			st.Prepare = shared
		}
		s = append(s, st,
			Step{Name: fmt.Sprintf("live-get-shared-%d-alias", i), Req: Get("/v1/sms/"+v, b), Shape: true},
			Step{Name: fmt.Sprintf("live-dlrs-shared-%d", i), Req: Get("/hermes/v1/sms/dlr/"+v, b), Shape: true})
	}
	return s
}
