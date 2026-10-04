package app

import (
	"net/http"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/dlr"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/publicapi"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/ratelimit"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/trustedproxy"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/webhook"
)

// buildReceipts wires carrier receipt ingestion and the client callback
// dispatcher. Callbacks are queued only after a receipt commits; the
// dispatcher runs with the lifecycle context, so shutdown cancels in-flight
// attempts and discards what is still queued (best effort, as the source).
func (a *App) buildReceipts(proxy *trustedproxy.Resolver) http.Handler {
	c := a.Cfg
	a.Callbacks = webhook.New(webhook.Config{Workers: c.Webhook.Workers, QueueSize: c.Webhook.QueueSize, Metrics: a.Metrics,
		Policy: webhook.Policy{AllowHTTP: c.Webhook.AllowHTTP, AllowPrivate: c.Webhook.AllowPrivate}, Log: a.Log.With("component", "callbacks")})
	a.Go(a.Callbacks.Run)
	a.Receipts = dlr.New(dlr.Config{Store: a.Repo, Envelope: a.Envelope, Callbacks: a.Callbacks, Metrics: a.Metrics, Audit: a.Audit,
		Log: a.Log.With("component", "receipts")})
	return publicapi.Receipts(publicapi.ReceiptConfig{Processor: a.Receipts, Proxy: proxy, Metrics: a.Metrics, Log: a.Log.With("component", "receipts"),
		Limit: ratelimit.New(c.RateLimits.DLRPerMinute, c.RateLimits.DLRBurst, 50000)})
}
