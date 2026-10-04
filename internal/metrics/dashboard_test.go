package metrics

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDashboardRunsOnlyPredefinedQueries(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Path+" "+r.URL.Query().Get("query")+" step="+r.URL.Query().Get("step"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/query_range" {
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"outcome":"ok"},"values":[[1700000000,"1.5"],[1700000060,"NaN"]]}]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"outcome":"ok"},"value":[1700000000,"3"]},{"metric":{},"value":[1700000000,"+Inf"]}]}}`))
	}))
	defer prom.Close()
	d, err := NewDashboard(prom.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	d.now = func() time.Time { return time.Unix(1700003600, 0) }

	res, err := d.Instant(context.Background(), "1h", nil)
	if err != nil || !res.Available || len(res.Results) != len(InstantQueries) {
		t.Fatalf("%+v %v", res, err)
	}
	if s := res.Results["send_by_outcome"]; len(s) != 2 || !s[0].HasValue || s[0].Value != 3 || s[0].Labels["outcome"] != "ok" || s[1].HasValue {
		t.Fatalf("%+v", s)
	}
	rr, err := d.Range(context.Background(), "15m", []string{"send_rate_by_outcome"})
	if err != nil || !rr.Available || rr.StepSeconds != 15 || rr.End-rr.Start != 900 {
		t.Fatalf("%+v %v", rr, err)
	}
	if s := rr.Results["send_rate_by_outcome"]; len(s) != 1 || len(s[0].Values) != 2 || *s[0].Values[0] != 1.5 || s[0].Values[1] != nil {
		t.Fatalf("%+v", s)
	}
	allowed := map[string]bool{}
	for _, w := range Windows {
		for _, q := range InstantQueries {
			allowed[q(w)] = true
		}
		for _, q := range RangeQueries {
			allowed[q(w)] = true
		}
	}
	for _, s := range seen {
		q := strings.SplitN(s, " ", 2)[1]
		q = q[:strings.LastIndex(q, " step=")]
		if !allowed[q] {
			t.Fatalf("unexpected query %q", q)
		}
	}
	if _, err := d.Instant(context.Background(), "30d", nil); !errors.Is(err, ErrUnknownWindow) {
		t.Fatal(err)
	}
	if _, err := d.Range(context.Background(), "1h", []string{"up"}); !errors.Is(err, ErrUnknownQuery) {
		t.Fatal(err)
	}
}

func TestDashboardUnavailable(t *testing.T) {
	var off *Dashboard
	if r, err := off.Instant(context.Background(), "1h", nil); err != nil || r.Available || r.Reason != NotConfigured {
		t.Fatalf("%+v %v", r, err)
	}
	if r, err := off.Range(context.Background(), "7d", nil); err != nil || r.Available || r.Reason != NotConfigured {
		t.Fatalf("%+v %v", r, err)
	}
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "secret upstream detail", http.StatusBadGateway)
	}))
	defer failing.Close()
	d, _ := NewDashboard(failing.URL)
	r, err := d.Instant(context.Background(), "6h", []string{"sends_per_sec"})
	if err != nil || r.Available || r.Reason != Unreachable || len(r.Results) != 0 {
		t.Fatalf("%+v %v", r, err)
	}
	failing.Close()
	if r, err := d.Range(context.Background(), "24h", nil); err != nil || r.Available || r.Reason != Unreachable {
		t.Fatalf("%+v %v", r, err)
	}
	for _, bad := range []string{"ftp://x", "http://user:pw@prom", "://"} {
		if _, err := NewDashboard(bad); err == nil {
			t.Fatalf("%s accepted", bad)
		}
	}
	if d, err := NewDashboard(""); d != nil || err != nil {
		t.Fatal("empty URL must disable monitoring")
	}
}
