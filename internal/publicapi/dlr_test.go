package publicapi

import (
	"context"
	"io"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/dlr"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/ratelimit"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/trustedproxy"
)

type fakeProcessor struct {
	mu     sync.Mutex
	got    []dlr.Receipt
	reason string
}

func (p *fakeProcessor) Process(_ context.Context, r dlr.Receipt) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.got = append(p.got, r)
	return p.reason
}

type rejections []string

func (r *rejections) ReceiptRejected(v string) { *r = append(*r, v) }

func receiptServer(t *testing.T, p *fakeProcessor, limit *ratelimit.Limiter, rej *rejections) *Server {
	proxy, err := trustedproxy.New([]string{"192.0.2.1"})
	if err != nil {
		t.Fatal(err)
	}
	return New(Config{Proxy: proxy, Receipts: Receipts(ReceiptConfig{Processor: p, Proxy: proxy, Limit: limit, Metrics: rej})})
}

func get(s *Server, target, remote string, headers ...string) (int, string, string) {
	req := httptest.NewRequest("GET", target, nil)
	req.RemoteAddr = remote + ":40000"
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	b, _ := io.ReadAll(w.Result().Body)
	return w.Code, w.Header().Get("Content-Type"), string(b)
}

func TestReceiptBindsProtoNamesAndAlwaysAcknowledges(t *testing.T) {
	p, rej := &fakeProcessor{}, &rejections{}
	s := receiptServer(t, p, nil, rej)
	for _, path := range []string{"/dlr", "/hermes/v1/sms/dlr"} {
		code, ct, body := get(s, path+"?request_id=m-1&channel=sms&sid=-9&message_status=8&to=359888000220&from=Capture&timestamp=1700000000&dlr_token=tok",
			"192.0.2.1", "X-Real-IP", "198.51.100.9")
		if code != 200 || ct != "application/json" || body != `"DLR_OK"` {
			t.Fatalf("%s: %d %s %s", path, code, ct, body)
		}
	}
	want := dlr.Receipt{RequestID: "m-1", Channel: "sms", Sid: -9, MessageStatus: 8, To: 359888000220, From: "Capture", Timestamp: 1700000000,
		Token: "tok", RemoteAddr: "198.51.100.9"}
	if len(p.got) != 2 || p.got[0] != want || p.got[1] != want {
		t.Fatalf("bound %+v", p.got)
	}
	// Camel-case names are not bound (dlr-camel-case-params).
	get(s, "/dlr?requestId=m-1&messageStatus=16&dlr_token=tok", "203.0.113.5")
	if g := p.got[2]; g.RequestID != "" || g.MessageStatus != 0 || g.Token != "tok" || g.RemoteAddr != "203.0.113.5" {
		t.Fatalf("camel case bound: %+v", g)
	}
}

func TestMalformedReceiptIsNotProcessed(t *testing.T) {
	p, rej := &fakeProcessor{}, &rejections{}
	s := receiptServer(t, p, nil, rej)
	for _, q := range []string{"message_status=delivered", "message_status=-1", "sid=x", "to=1e3", "timestamp=1&timestamp=2", "message_status=4294967296"} {
		code, _, body := get(s, "/dlr?request_id=m-1&dlr_token=tok&"+q, "203.0.113.5")
		if code != 200 || body != `"DLR_OK"` {
			t.Fatalf("%s: %d %s", q, code, body)
		}
	}
	if len(p.got) != 0 || len(*rej) != 6 || (*rej)[0] != dlr.Malformed {
		t.Fatalf("malformed receipts processed: %v %v", p.got, *rej)
	}
}

func TestRejectedReceiptsAreBudgetedPerAddress(t *testing.T) {
	p, rej := &fakeProcessor{reason: dlr.BadToken}, &rejections{}
	s := receiptServer(t, p, ratelimit.New(0.0001, 3, 100), rej)
	for range 5 {
		if code, _, body := get(s, "/dlr?request_id=m-1&dlr_token=guess", "203.0.113.5"); code != 200 || body != `"DLR_OK"` {
			t.Fatalf("%d %s", code, body)
		}
	}
	// The processor records its own rejections; the handler only the limited ones.
	if len(p.got) != 3 || len(*rej) != 2 || (*rej)[0] != dlr.RateLimited {
		t.Fatalf("budget not applied: %d processed, %v", len(p.got), *rej)
	}
	get(s, "/dlr?request_id=m-1", "203.0.113.6")
	if len(p.got) != 4 {
		t.Fatal("another address was limited")
	}
	// Accepted receipts never consume the budget: a carrier is not throttled.
	p.reason = dlr.Accepted
	for range 50 {
		get(s, "/dlr?request_id=m-2", "198.51.100.20")
	}
	if len(p.got) != 54 {
		t.Fatalf("accepted receipts were limited: %d", len(p.got))
	}
}
