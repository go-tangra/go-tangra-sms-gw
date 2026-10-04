// Package webhook delivers DLR events to per-client callback URLs out of
// the request path. Failures are retried with exponential backoff; the
// queue is best-effort and bounded — overflow drops the oldest event so
// a stalled customer endpoint cannot starve the gateway.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/tx7do/kratos-bootstrap/bootstrap"
)

// Event is one DLR delivery attempt. The fields are kept narrow so the
// dispatcher does not depend on the full proto/ent stack.
type Event struct {
	URL          string
	Secret       string
	MessageID    string
	Channel      string
	MessageStatus uint32
	StatusText   string
	Recipient    uint64
	From         string
	Timestamp    uint64
}

// Payload is the JSON body POSTed to the customer's callback URL. Field
// names mirror the public DLR query parameters so the same client-side
// code that handled inbound DLR webhooks against the legacy hermes can
// be repointed at the gateway's outbound feed.
type Payload struct {
	MessageID     string `json:"message_id"`
	Channel       string `json:"channel"`
	MessageStatus uint32 `json:"message_status"`
	StatusText    string `json:"status_text"`
	To            uint64 `json:"to"`
	From          string `json:"from"`
	Timestamp     uint64 `json:"timestamp"`
}

// Dispatcher is a single-process worker that drains the queue. The
// constructor wires it into the kratos lifecycle: Start spins workers,
// Stop drains and exits.
type Dispatcher struct {
	log     *log.Helper
	client  *http.Client
	queue   chan Event
	workers int
	stop    chan struct{}
	wg      sync.WaitGroup
}

const (
	queueCapacity   = 4096
	defaultWorkers  = 4
	maxAttempts     = 6
	httpTimeout     = 10 * time.Second
	signatureHeader = "X-SmsGw-Signature"
	timestampHeader = "X-SmsGw-Timestamp"
)

// NewDispatcher constructs and starts the dispatcher. Wire treats this
// as a singleton; the cleanup func returned to Wire performs the drain
// on shutdown.
func NewDispatcher(ctx *bootstrap.Context) (*Dispatcher, func(), error) {
	dialer := newSafeDialer()
	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: httpTimeout,
		ExpectContinueTimeout: 1 * time.Second,
	}
	d := &Dispatcher{
		log: ctx.NewLoggerHelper("sms-gw/webhook"),
		client: &http.Client{
			Timeout:       httpTimeout,
			Transport:     transport,
			CheckRedirect: noRedirect,
		},
		queue:   make(chan Event, queueCapacity),
		workers: defaultWorkers,
		stop:    make(chan struct{}),
	}
	for i := 0; i < d.workers; i++ {
		d.wg.Add(1)
		go d.worker()
	}
	return d, func() {
		close(d.stop)
		d.wg.Wait()
	}, nil
}

// Enqueue submits an event for asynchronous delivery. Returns immediately
// with a dropped=true result if the queue is full — callers may log but
// must not block on this. Empty URL is treated as a no-op (no callback
// configured).
func (d *Dispatcher) Enqueue(ev Event) (queued bool) {
	if ev.URL == "" {
		return false
	}
	select {
	case d.queue <- ev:
		return true
	default:
		d.log.Warnf("webhook queue full, dropping DLR for message_id=%s", ev.MessageID)
		return false
	}
}

func (d *Dispatcher) worker() {
	defer d.wg.Done()
	for {
		select {
		case <-d.stop:
			return
		case ev := <-d.queue:
			d.deliver(ev)
		}
	}
}

func (d *Dispatcher) deliver(ev Event) {
	body, err := json.Marshal(Payload{
		MessageID:     ev.MessageID,
		Channel:       ev.Channel,
		MessageStatus: ev.MessageStatus,
		StatusText:    ev.StatusText,
		To:            ev.Recipient,
		From:          ev.From,
		Timestamp:     ev.Timestamp,
	})
	if err != nil {
		d.log.Errorf("webhook marshal: %s", err.Error())
		return
	}
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			// 200ms, 400ms, 800ms, 1.6s, 3.2s — capped at maxAttempts.
			// The select makes the sleep interruptible by Stop so a
			// shutdown isn't held up by an in-flight backoff.
			select {
			case <-time.After(time.Duration(1<<attempt) * 100 * time.Millisecond):
			case <-d.stop:
				return
			}
		}
		if d.attempt(ev, body) {
			return
		}
		select {
		case <-d.stop:
			return
		default:
		}
	}
	d.log.Warnf("webhook delivery exhausted retries: url=%s message_id=%s", ev.URL, ev.MessageID)
}

func (d *Dispatcher) attempt(ev Event, body []byte) bool {
	ctx, cancel := context.WithTimeout(context.Background(), httpTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ev.URL, bytes.NewReader(body))
	if err != nil {
		d.log.Warnf("webhook build request: %s", err.Error())
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	if ev.Secret != "" {
		ts := time.Now().Unix()
		req.Header.Set(timestampHeader, formatInt(ts))
		req.Header.Set(signatureHeader, sign(ev.Secret, ts, body))
	}
	resp, err := d.client.Do(req)
	if err != nil {
		d.log.Warnf("webhook POST: %s", err.Error())
		return false
	}
	// Drain at most 64 KiB of the response body so a malicious or
	// misconfigured customer endpoint streaming megabytes can't tie
	// up a worker for the whole HTTP timeout. We discard the body —
	// nothing in the contract uses it.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	// 2xx-only counts as success. The CheckRedirect hook makes any 3xx
	// the FINAL response (we refuse to follow), so accepting 3xx here
	// would silently mark a redirect-to-internal-host as delivered.
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return true
	}
	d.log.Warnf("webhook delivery non-2xx: url=%s status=%d", ev.URL, resp.StatusCode)
	return false
}

// sign returns hex(HMAC-SHA256(secret, timestamp + "." + body)). Matches
// the convention used by Stripe-style webhook signing so customers can
// reuse off-the-shelf verifiers.
func sign(secret string, ts int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(formatInt(ts)))
	mac.Write([]byte{'.'})
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func formatInt(n int64) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	negative := n < 0
	if negative {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = digits[n%10]
		n /= 10
	}
	if negative {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
