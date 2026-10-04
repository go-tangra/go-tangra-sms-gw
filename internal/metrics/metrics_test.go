package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-tangra/go-tangra/v4/observe"
)

func TestLegacySeriesAndClosedLabels(t *testing.T) {
	om, err := observe.NewMetrics()
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(om.Meter("sms-gw"))
	if err != nil {
		t.Fatal(err)
	}
	m.Send("voicecom", "ok", 120*time.Millisecond)
	m.Send("Evil Type {tenant=x}", "359888000100", time.Second)
	m.Receipt(1)
	m.Receipt(77)
	m.ReceiptRejected("bad_token")
	m.ReceiptRejected("token=abc")
	m.DeliveryLatency("voicecom", 3*time.Second)
	m.Webhook("delivered")
	m.Login("invalid_credentials")
	m.Housekeeping("ok", map[string]int64{"voicecom": 3, "x": 0})
	rec := httptest.NewRecorder()
	om.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	out := rec.Body.String()
	for _, want := range []string{
		`sms_gw_send_total{outcome="ok",provider="voicecom"} 1`,
		`sms_gw_send_total{outcome="other",provider="other"} 1`,
		`sms_gw_send_duration_seconds_bucket{le="0.25",outcome="ok",provider="voicecom"} 1`,
		`sms_gw_dlr_received_total{status="1"} 1`,
		`sms_gw_dlr_received_total{status="other"} 1`,
		`sms_gw_dlr_rejected_total{reason="bad_token"} 1`,
		`sms_gw_dlr_rejected_total{reason="other"} 1`,
		`sms_gw_delivery_latency_seconds_count{provider="voicecom"} 1`,
		`sms_gw_webhook_delivery_total{outcome="delivered"} 1`,
		`sms_gw_auth_login_total{outcome="invalid_credentials"} 1`,
		`sms_gw_housekeeping_runs_total{outcome="ok"} 1`,
		`sms_gw_housekeeping_messages_deleted_total{provider="voicecom"} 3`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %s in\n%s", want, out)
		}
	}
	for _, leak := range []string{"359888000100", "token=abc", "tenant=x", `provider="x"`} {
		if strings.Contains(out, leak) {
			t.Fatalf("high-cardinality or secret label %q rendered", leak)
		}
	}
}

func TestNilMetricsIsNoop(t *testing.T) {
	var m *Metrics
	m.Send("voicecom", "ok", 0)
	m.Receipt(1)
	m.ReceiptRejected("bad_token")
	m.DeliveryLatency("voicecom", 0)
	m.Webhook("failed")
	m.Login("success")
	m.Housekeeping("ok", nil)
}
