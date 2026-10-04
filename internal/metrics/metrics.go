// Package metrics records the module's low-cardinality instruments on the
// Freya meter. Names render as the legacy Prometheus series
// (sms_gw_send_total, sms_gw_send_duration_seconds, ...) so existing
// dashboard queries keep working. Every label is a closed value: provider
// types, outcomes and receipt codes outside their sets become "other";
// tenants, clients, recipients and message ids are never labels. A nil
// *Metrics is a no-op.
package metrics

import (
	"context"
	"regexp"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Outcomes of a send (legacy values).
var sendOutcomes = set("ok", "forbidden", "validation_error", "block_check_error", "blocked", "provider_error", "rate_limited", "internal_error")

var (
	webhookOutcomes = set("delivered", "failed", "dropped")
	loginOutcomes   = set("success", "invalid_credentials", "disabled", "bad_request", "rate_limited", "error")
	runOutcomes     = set("ok", "error")
	rejectReasons   = set("bad_token", "unknown_message", "malformed", "rate_limited", "unavailable")
	providerTypeRE  = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)
	// Receipt codes of the Voicecom status table; anything else is "other".
	receiptCodes = map[uint32]bool{0: true, 1: true, 2: true, 8: true, 16: true, 1000: true, 1001: true, 1002: true, 2000: true, 2001: true,
		2008: true, 2009: true, 2010: true, 2012: true, 2015: true, 2016: true, 2017: true, 2018: true, 2019: true, 2020: true,
		3000: true, 3001: true, 3002: true, 4000: true, 4001: true, 4002: true, 4003: true, 4004: true}
)

func set(vs ...string) map[string]bool {
	m := make(map[string]bool, len(vs))
	for _, v := range vs {
		m[v] = true
	}
	return m
}

func closed(v string, allowed map[string]bool) string {
	if allowed[v] {
		return v
	}
	return "other"
}

// ProviderLabel bounds a provider type label.
func ProviderLabel(t string) string {
	if providerTypeRE.MatchString(t) {
		return t
	}
	return "other"
}

// Metrics holds the instruments.
type Metrics struct {
	send            metric.Int64Counter
	sendDuration    metric.Float64Histogram
	dlr             metric.Int64Counter
	dlrRejected     metric.Int64Counter
	deliveryLatency metric.Float64Histogram
	webhook         metric.Int64Counter
	login           metric.Int64Counter
	housekeeping    metric.Int64Counter
	deleted         metric.Int64Counter
}

// New creates the instruments on meter.
func New(meter metric.Meter) (*Metrics, error) {
	m := &Metrics{}
	var err error
	counter := func(name, desc string) metric.Int64Counter {
		if err != nil {
			return nil
		}
		var c metric.Int64Counter
		c, err = meter.Int64Counter(name, metric.WithDescription(desc))
		return c
	}
	histogram := func(name, desc string, bounds ...float64) metric.Float64Histogram {
		if err != nil {
			return nil
		}
		var h metric.Float64Histogram
		h, err = meter.Float64Histogram(name, metric.WithDescription(desc), metric.WithUnit("s"), metric.WithExplicitBucketBoundaries(bounds...))
		return h
	}
	m.send = counter("sms_gw.send", "Total Send attempts grouped by provider and outcome.")
	m.sendDuration = histogram("sms_gw.send.duration", "Wall-clock duration of Send.", 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30)
	m.dlr = counter("sms_gw.dlr.received", "Inbound DLR webhooks grouped by reported message status.")
	m.dlrRejected = counter("sms_gw.dlr.rejected", "Inbound DLR webhooks acknowledged without effect, by reason.")
	m.deliveryLatency = histogram("sms_gw.delivery.latency", "End-to-end delivery latency: time from Send accept to status=1 DLR.", 1, 2, 5, 10, 30, 60, 120, 300, 600, 1800)
	m.webhook = counter("sms_gw.webhook.delivery", "Outbound DLR webhook delivery attempts.")
	m.login = counter("sms_gw.auth.login", "Login attempts grouped by outcome.")
	m.housekeeping = counter("sms_gw.housekeeping.runs", "Housekeeping ticks grouped by overall outcome.")
	m.deleted = counter("sms_gw.housekeeping.messages_deleted", "SMS messages deleted by retention sweeps, per provider type.")
	if err != nil {
		return nil, err
	}
	return m, nil
}

func attrs(kv ...string) metric.MeasurementOption {
	a := make([]attribute.KeyValue, 0, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		a = append(a, attribute.String(kv[i], kv[i+1]))
	}
	return metric.WithAttributes(a...)
}

// Send records one send attempt.
func (m *Metrics) Send(providerType, outcome string, d time.Duration) {
	if m == nil {
		return
	}
	o := attrs("provider", ProviderLabel(providerType), "outcome", closed(outcome, sendOutcomes))
	m.send.Add(context.Background(), 1, o)
	m.sendDuration.Record(context.Background(), d.Seconds(), o)
}

// Receipt records an inbound receipt by reported status.
func (m *Metrics) Receipt(status uint32) {
	if m == nil {
		return
	}
	label := "other"
	if receiptCodes[status] {
		label = strconv.FormatUint(uint64(status), 10)
	}
	m.dlr.Add(context.Background(), 1, attrs("status", label))
}

// ReceiptRejected records a receipt acknowledged without effect.
func (m *Metrics) ReceiptRejected(reason string) {
	if m == nil {
		return
	}
	m.dlrRejected.Add(context.Background(), 1, attrs("reason", closed(reason, rejectReasons)))
}

// DeliveryLatency records send-to-delivered time.
func (m *Metrics) DeliveryLatency(providerType string, d time.Duration) {
	if m == nil {
		return
	}
	m.deliveryLatency.Record(context.Background(), d.Seconds(), attrs("provider", ProviderLabel(providerType)))
}

// Webhook records an outbound callback outcome.
func (m *Metrics) Webhook(outcome string) {
	if m == nil {
		return
	}
	m.webhook.Add(context.Background(), 1, attrs("outcome", closed(outcome, webhookOutcomes)))
}

// Login records a Hermes login outcome.
func (m *Metrics) Login(outcome string) {
	if m == nil {
		return
	}
	m.login.Add(context.Background(), 1, attrs("outcome", closed(outcome, loginOutcomes)))
}

// Housekeeping records one retention pass and its deletions by provider type.
func (m *Metrics) Housekeeping(outcome string, deleted map[string]int64) {
	if m == nil {
		return
	}
	m.housekeeping.Add(context.Background(), 1, attrs("outcome", closed(outcome, runOutcomes)))
	for t, n := range deleted {
		if n > 0 {
			m.deleted.Add(context.Background(), n, attrs("provider", ProviderLabel(t)))
		}
	}
}
