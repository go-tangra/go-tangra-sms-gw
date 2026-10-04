// Package dlr ingests carrier delivery receipts: the message (and with it
// the tenant) is resolved from the stored record, the receipt secret of the
// message's provider is checked in constant time before anything changes,
// the receipt is aggregated and applied atomically, and only after the
// commit is the owning client's callback queued.
package dlr

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"log/slog"
	"math"
	"strconv"
	"time"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/audit"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/provider/voicecom"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/repo"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/sealed"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/webhook"
)

// TokenKey is the provider configuration key holding the receipt secret.
const TokenKey = "dlr_token"

// Receipt is one inbound carrier receipt (the legacy DlrRequest plus the
// supplied dlr_token and the resolved client address).
type Receipt struct {
	RequestID     string
	Channel       string
	Sid           int32
	MessageStatus uint32
	To            uint64
	From          string
	Timestamp     uint64
	Token         string
	RemoteAddr    string
}

// Rejection reasons (closed metric vocabulary).
const (
	Accepted       = ""
	BadToken       = "bad_token"
	UnknownMessage = "unknown_message"
	Malformed      = "malformed"
	Unavailable    = "unavailable"
	RateLimited    = "rate_limited"
)

// Store is the persistence receipts need.
type Store interface {
	// ResolveMessage is the named cross-tenant lookup by message id.
	ResolveMessage(ctx context.Context, id string) (repo.MessageRef, error)
	GetProvider(ctx context.Context, tenant string, id int64) (repo.Provider, error)
	GetClient(ctx context.Context, tenant string, id int64) (repo.APIClient, error)
	ApplyReceipt(ctx context.Context, r repo.Receipt) (repo.ReceiptResult, error)
}

// Dispatcher queues client callbacks.
type Dispatcher interface{ Enqueue(webhook.Event) bool }

// Metrics records receipt telemetry.
type Metrics interface {
	Receipt(status uint32)
	ReceiptRejected(reason string)
	DeliveryLatency(providerType string, d time.Duration)
}

// Auditor records sanitized audit events.
type Auditor interface{ Record(audit.Event) error }

// Config wires a processor.
type Config struct {
	Store     Store
	Envelope  *sealed.Envelope
	Callbacks Dispatcher // nil = no callbacks
	Metrics   Metrics
	Audit     Auditor
	Log       *slog.Logger
}

// Processor applies receipts.
type Processor struct {
	c   Config
	now func() time.Time
}

// New builds a processor.
func New(c Config) *Processor {
	if c.Log == nil {
		c.Log = slog.New(slog.DiscardHandler)
	}
	return &Processor{c: c, now: time.Now}
}

// dummy is compared against when there is nothing to compare, so every
// path pays the same comparison.
var dummy = sha256.Sum256([]byte("sms-gw:no-receipt-token"))

// tokenMatches compares digests, so neither the length nor a prefix of the
// stored secret is observable; an empty stored secret accepts any value
// (legacy providers without a receipt token).
func tokenMatches(stored, supplied string) bool {
	s := sha256.Sum256([]byte(supplied))
	if stored == "" {
		subtle.ConstantTimeCompare(s[:], dummy[:])
		return true
	}
	want := sha256.Sum256([]byte(stored))
	return subtle.ConstantTimeCompare(s[:], want[:]) == 1
}

// Process applies one receipt and returns the rejection reason ("" when
// accepted). The caller acknowledges the carrier the same way either way.
func (p *Processor) Process(ctx context.Context, in Receipt) string {
	if p.c.Metrics != nil {
		p.c.Metrics.Receipt(in.MessageStatus)
	}
	reason := p.process(ctx, in)
	if reason != Accepted && p.c.Metrics != nil {
		p.c.Metrics.ReceiptRejected(reason)
	}
	return reason
}

func (p *Processor) process(ctx context.Context, in Receipt) string {
	ref, err := p.c.Store.ResolveMessage(ctx, in.RequestID)
	if errors.Is(err, repo.ErrNotFound) {
		tokenMatches(string(dummy[:]), in.Token)
		return UnknownMessage
	}
	if err != nil {
		p.c.Log.Warn("receipt message lookup failed", "err", err)
		return Unavailable
	}
	prov, err := p.c.Store.GetProvider(ctx, ref.TenantID, ref.ProviderID)
	if err != nil {
		p.c.Log.Warn("receipt provider lookup failed", "tenant", ref.TenantID, "message_id", ref.ID, "err", err)
		return Unavailable
	}
	cfg, err := p.c.Envelope.OpenConfig(prov.ConfigSealed, sealed.ProviderAD(ref.TenantID, prov.ID))
	if err != nil {
		p.c.Log.Error("receipt provider configuration does not open", "tenant", ref.TenantID, "provider_id", prov.ID)
		return Unavailable
	}
	if !tokenMatches(cfg[TokenKey], in.Token) {
		p.c.Log.Warn("receipt token mismatch; dropped", "tenant", ref.TenantID, "message_id", ref.ID)
		if p.c.Audit != nil {
			_ = p.c.Audit.Record(audit.Event{TenantID: ref.TenantID, ActorKind: audit.ActorService, ActorID: "carrier", Action: audit.ReceiptRejected,
				TargetType: "message", TargetID: ref.ID, Outcome: audit.Denied})
		}
		return BadToken
	}
	if in.MessageStatus > math.MaxInt32 || in.Timestamp > math.MaxInt64 || in.To > math.MaxInt64 {
		return Malformed
	}
	text := voicecom.StatusText(int32(in.MessageStatus))
	res, err := p.c.Store.ApplyReceipt(ctx, repo.Receipt{TenantID: ref.TenantID, MessageID: ref.ID, Channel: in.Channel, Sid: int64(in.Sid),
		StatusText: text, MessageStatus: in.MessageStatus, Recipient: in.To, Sender: in.From, Timestamp: int64(in.Timestamp), RemoteAddress: in.RemoteAddr})
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return UnknownMessage
		}
		p.c.Log.Warn("receipt not stored", "tenant", ref.TenantID, "message_id", ref.ID, "err", err)
		return Unavailable
	}
	if res.Changed && in.MessageStatus == 1 && p.c.Metrics != nil {
		p.c.Metrics.DeliveryLatency(prov.Type, p.now().Sub(res.Before.CreateTime))
	}
	p.callback(ctx, ref, in, text)
	return Accepted
}

// callback queues the owning client's callback for an accepted receipt
// (duplicates and late receipts included, as in the source). The target is
// read in the message's tenant; operator-sent messages have none.
func (p *Processor) callback(ctx context.Context, ref repo.MessageRef, in Receipt, text string) {
	if p.c.Callbacks == nil || ref.APIClientID == 0 {
		return
	}
	c, err := p.c.Store.GetClient(ctx, ref.TenantID, ref.APIClientID)
	if err != nil || c.CallbackURL == "" {
		return
	}
	secret, err := p.c.Envelope.OpenString(c.CallbackSecretSealed, sealed.CallbackAD(ref.TenantID, c.ID))
	if err != nil {
		p.c.Log.Error("callback secret does not open; callback skipped", "tenant", ref.TenantID, "client_id", strconv.FormatInt(c.ID, 10))
		return
	}
	p.c.Callbacks.Enqueue(webhook.Event{URL: c.CallbackURL, Secret: secret, MessageID: ref.ID, Channel: in.Channel, MessageStatus: in.MessageStatus,
		StatusText: text, Recipient: in.To, From: in.From, Timestamp: in.Timestamp})
}
