package publicapi

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/dlr"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/ratelimit"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/trustedproxy"
)

// ReceiptProcessor applies one carrier receipt and returns its rejection
// reason ("" when accepted).
type ReceiptProcessor interface {
	Process(ctx context.Context, r dlr.Receipt) string
}

// ReceiptConfig wires the carrier receipt routes.
type ReceiptConfig struct {
	Processor ReceiptProcessor
	Proxy     *trustedproxy.Resolver
	// Limit budgets rejected receipts per client address; an address that
	// exhausted it is acknowledged without processing until it refills.
	// Accepted receipts never consume it.
	Limit   *ratelimit.Limiter
	Metrics interface{ ReceiptRejected(reason string) }
	Log     *slog.Logger
}

type receiptHandler struct{ c ReceiptConfig }

// Receipts is the source receipt handler for GET /dlr and GET
// /hermes/v1/sms/dlr: the query binds the proto field names of DlrRequest
// plus dlr_token, and the answer is always 200 "DLR_OK" whatever happened,
// so a carrier learns nothing from it and never retries.
func Receipts(c ReceiptConfig) http.Handler {
	if c.Log == nil {
		c.Log = slog.New(slog.DiscardHandler)
	}
	return &receiptHandler{c: c}
}

func (h *receiptHandler) reject(reason string) {
	if h.c.Metrics != nil {
		h.c.Metrics.ReceiptRejected(reason)
	}
}

func (h *receiptHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer WriteReceiptAck(w)
	ip := h.c.Proxy.ClientIP(r)
	if h.c.Limit != nil && !h.c.Limit.Available(ip) {
		h.reject(dlr.RateLimited)
		return
	}
	q, err := decodeQuery(r, "DlrRequest")
	if err != nil {
		h.c.Log.Warn("receipt query not bound; acknowledged without effect")
		h.reject(dlr.Malformed)
		h.charge(ip)
		return
	}
	reason := h.c.Processor.Process(r.Context(), dlr.Receipt{RequestID: str(q, "request_id"), Channel: str(q, "channel"), Sid: i32(q, "sid"),
		MessageStatus: u32(q, "message_status"), To: u64v(q, "to"), From: str(q, "from"), Timestamp: u64v(q, "timestamp"),
		Token: r.URL.Query().Get("dlr_token"), RemoteAddr: ip})
	if reason != dlr.Accepted && reason != dlr.Unavailable {
		h.charge(ip)
	}
}

func (h *receiptHandler) charge(ip string) {
	if h.c.Limit != nil {
		h.c.Limit.Allow(ip)
	}
}
