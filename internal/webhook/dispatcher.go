// Package webhook delivers receipt events to per-client callback URLs out
// of the request path, as the source dispatcher did: a bounded in-memory
// queue drained by a fixed worker pool, six attempts with 0.2–3.2 s backoff,
// only 2xx counts, redirects are never followed and the response drain is
// bounded. Delivery is best effort: a full queue drops the new event and
// queued events are lost on shutdown; nothing is persisted or retried later.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Source dispatcher constants.
const (
	DefaultQueue    = 4096
	DefaultWorkers  = 4
	MaxAttempts     = 6
	RequestTimeout  = 10 * time.Second
	SignatureHeader = "X-SmsGw-Signature" // sent in Go canonical form, X-Smsgw-Signature
	TimestampHeader = "X-SmsGw-Timestamp"
	maxDrain        = 64 << 10
)

// Event is one receipt to push to a client.
type Event struct {
	URL           string
	Secret        string
	MessageID     string
	Channel       string
	MessageStatus uint32
	StatusText    string
	Recipient     uint64
	From          string
	Timestamp     uint64
}

// Payload is the source callback body (standard JSON, numeric to/timestamp).
type Payload struct {
	MessageID     string `json:"message_id"`
	Channel       string `json:"channel"`
	MessageStatus uint32 `json:"message_status"`
	StatusText    string `json:"status_text"`
	To            uint64 `json:"to"`
	From          string `json:"from"`
	Timestamp     uint64 `json:"timestamp"`
}

// Metrics records final delivery outcomes (delivered, failed, dropped).
type Metrics interface{ Webhook(outcome string) }

// Config configures a dispatcher; zero values take the source defaults.
type Config struct {
	Workers   int
	QueueSize int
	Policy    Policy
	Resolver  Resolver // nil = net.DefaultResolver
	Metrics   Metrics
	Log       *slog.Logger
}

// Dispatcher queues and delivers events.
type Dispatcher struct {
	c       Config
	client  *http.Client
	queue   chan Event
	backoff time.Duration // first retry gap; doubles per attempt
	timeout time.Duration
	now     func() time.Time
}

// New builds a dispatcher; Run starts its workers.
func New(c Config) *Dispatcher {
	if c.Workers < 1 {
		c.Workers = DefaultWorkers
	}
	if c.QueueSize < 1 {
		c.QueueSize = DefaultQueue
	}
	if c.Resolver == nil {
		c.Resolver = net.DefaultResolver
	}
	if c.Log == nil {
		c.Log = slog.New(slog.DiscardHandler)
	}
	transport := &http.Transport{
		Proxy:                 nil, // an environment proxy would bypass the destination checks
		DialContext:           newSafeDialer(c.Resolver, c.Policy).DialContext,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: RequestTimeout,
		ExpectContinueTimeout: time.Second,
	}
	return &Dispatcher{c: c, queue: make(chan Event, c.QueueSize), backoff: 200 * time.Millisecond, timeout: RequestTimeout, now: time.Now,
		client: &http.Client{Timeout: RequestTimeout, Transport: transport, CheckRedirect: noRedirect}}
}

// Enqueue submits an event without blocking; it reports false when no
// callback is configured or the queue is full (the event is dropped).
func (d *Dispatcher) Enqueue(ev Event) bool {
	if ev.URL == "" {
		return false
	}
	select {
	case d.queue <- ev:
		return true
	default:
		d.c.Log.Warn("callback queue full; receipt callback dropped", "message_id", ev.MessageID)
		d.metric("dropped")
		return false
	}
}

// Run delivers until ctx is done; in-flight attempts and backoffs are
// cancelled and still-queued events are discarded.
func (d *Dispatcher) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for range d.c.Workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case ev := <-d.queue:
					d.deliver(ctx, ev)
				}
			}
		}()
	}
	wg.Wait()
}

func (d *Dispatcher) metric(outcome string) {
	if d.c.Metrics != nil {
		d.c.Metrics.Webhook(outcome)
	}
}

func (d *Dispatcher) deliver(ctx context.Context, ev Event) {
	defer func() {
		if v := recover(); v != nil {
			d.c.Log.Error("callback worker panic", "message_id", ev.MessageID)
		}
	}()
	if err := ValidateURL(ev.URL, d.c.Policy); err != nil {
		d.c.Log.Warn("receipt callback destination refused", "message_id", ev.MessageID, "err", err)
		d.metric("failed")
		return
	}
	body, err := json.Marshal(Payload{MessageID: ev.MessageID, Channel: ev.Channel, MessageStatus: ev.MessageStatus, StatusText: ev.StatusText,
		To: ev.Recipient, From: ev.From, Timestamp: ev.Timestamp})
	if err != nil {
		d.metric("failed")
		return
	}
	for attempt := range MaxAttempts {
		if attempt > 0 && !sleep(ctx, d.backoff<<(attempt-1)) {
			return
		}
		ok, err := d.attempt(ctx, ev, body)
		if ok {
			d.metric("delivered")
			return
		}
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, ErrBlocked) {
			d.c.Log.Warn("receipt callback destination refused", "message_id", ev.MessageID, "err", err)
			d.metric("failed")
			return
		}
	}
	d.c.Log.Warn("receipt callback retries exhausted", "message_id", ev.MessageID)
	d.metric("failed")
}

func (d *Dispatcher) attempt(ctx context.Context, ev Event, body []byte) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ev.URL, bytes.NewReader(body))
	if err != nil {
		return false, ErrBlocked
	}
	req.Header.Set("Content-Type", "application/json")
	if ev.Secret != "" {
		ts := d.now().Unix()
		req.Header.Set(TimestampHeader, strconv.FormatInt(ts, 10))
		req.Header.Set(SignatureHeader, Sign(ev.Secret, ts, body))
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return false, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrain))
	_ = resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return true, nil
	}
	d.c.Log.Warn("receipt callback answered non-2xx", "message_id", ev.MessageID, "status", resp.StatusCode)
	return false, nil
}

// Sign is the source signature: lower-case hex HMAC-SHA256 of
// timestamp + "." + body.
func Sign(secret string, ts int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(ts, 10)))
	mac.Write([]byte{'.'})
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
