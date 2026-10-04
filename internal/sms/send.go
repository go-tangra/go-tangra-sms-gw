// Package sms is the gateway domain: the validated send pipeline and the
// tenant/owner-scoped message and receipt queries, shared by the public
// Hermes adapter and management.
package sms

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/audit"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/hermes"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/metrics"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/provider"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/provider/voicecom"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/render"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/repo"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/sealed"
)

// Bounds on request properties.
const (
	MaxProperties    = 64
	MaxPropertyBytes = 4096
	maxMSISDNDigits  = 15
)

// Store is the persistence the pipeline uses; every call is tenant-scoped.
type Store interface {
	GetProvider(ctx context.Context, tenant string, id int64) (repo.Provider, error)
	GetTemplate(ctx context.Context, tenant string, id int64) (repo.Template, error)
	IsBlocked(ctx context.Context, tenant string, providerID int64, recipient string) (bool, error)
	CreateMessage(ctx context.Context, m repo.Message) (repo.Message, error)
	SetProviderResponse(ctx context.Context, tenant, id string, r repo.ProviderResponse) (repo.Message, error)
	GetMessage(ctx context.Context, v repo.View, id string) (repo.Message, error)
	ListMessages(ctx context.Context, v repo.View, f repo.MessageFilter, p repo.Page) (repo.List[repo.Message], error)
	ListReceipts(ctx context.Context, v repo.View, messageID string) (repo.List[repo.Receipt], error)
}

// Auditor records sanitized audit events.
type Auditor interface {
	Record(audit.Event) error
}

// RecipientPolicy is the destination check applied before anything else is
// looked up (SMS pumping / toll-fraud defence).
type RecipientPolicy struct {
	MinDigits int
	Allowed   []string
	Blocked   []string
}

func (p RecipientPolicy) check(to uint64) error {
	if to == 0 {
		return hermes.BadRequest("recipient (to) is required")
	}
	num := strconv.FormatUint(to, 10)
	if len(num) < p.MinDigits || len(num) > maxMSISDNDigits {
		return hermes.BadRequest("recipient %s has an invalid length (expected %d-%d digits)", num, p.MinDigits, maxMSISDNDigits)
	}
	for _, pre := range p.Blocked {
		if strings.HasPrefix(num, pre) {
			return hermes.Forbidden("destination %s is not permitted", num)
		}
	}
	if len(p.Allowed) == 0 {
		return nil
	}
	for _, pre := range p.Allowed {
		if strings.HasPrefix(num, pre) {
			return nil
		}
	}
	return hermes.Forbidden("destination %s is outside the permitted regions", num)
}

// Config wires a Service.
type Config struct {
	Store          Store
	Envelope       *sealed.Envelope
	Senders        *provider.Cache
	Policy         RecipientPolicy
	Metrics        *metrics.Metrics
	Audit          Auditor
	Log            *slog.Logger
	CarrierTimeout time.Duration // bounds one carrier exchange (default 60s)
}

// Service runs sends and queries.
type Service struct {
	store   Store
	env     *sealed.Envelope
	senders *provider.Cache
	policy  RecipientPolicy
	metrics *metrics.Metrics
	audit   Auditor
	log     *slog.Logger
	timeout time.Duration
}

// New builds the service.
func New(c Config) *Service {
	if c.Senders == nil {
		c.Senders = provider.NewCache()
	}
	if c.Log == nil {
		c.Log = slog.New(slog.DiscardHandler)
	}
	if c.CarrierTimeout <= 0 {
		c.CarrierTimeout = 60 * time.Second
	}
	if c.Policy.MinDigits < 1 {
		c.Policy.MinDigits = 7
	}
	return &Service{store: c.Store, env: c.Envelope, senders: c.Senders, policy: c.Policy, metrics: c.Metrics, audit: c.Audit, log: c.Log, timeout: c.CarrierTimeout}
}

// Request is one send.
type Request struct {
	To         uint64
	ProviderID uint32
	TemplateID uint32
	Properties map[string]string
	SMS        *provider.SMS
}

// Result is the stored message after the carrier exchange and the carrier
// envelope (nil when there was none).
type Result struct {
	Message repo.Message
	Carrier *provider.CarrierResponse
}

func (s *Service) storage(err error) error {
	s.log.Error("sms storage failure", "err", err)
	return hermes.Unavailable("storage unavailable")
}

// Send validates the request in the source order (provider, recipient
// policy, provider state, blocks, template, rendering, sender), records the
// message in its initial state, submits it once and stores the carrier
// outcome. Nothing is stored or submitted for a rejected request; a lost or
// failed carrier exchange is recorded and never retried. The provider
// response only changes a message that no receipt has moved yet.
func (s *Service) Send(ctx context.Context, tenant string, actor repo.Actor, req Request) (res Result, err error) {
	start, ptype, outcome := time.Now(), "unknown", "ok"
	defer func() { s.metrics.Send(ptype, outcome, time.Since(start)) }()
	reject := func(o string, e error) (Result, error) { outcome = o; return Result{}, e }

	if req.ProviderID == 0 {
		return reject("validation_error", hermes.BadRequest("providerId is required"))
	}
	if err := s.policy.check(req.To); err != nil {
		return reject("validation_error", err)
	}
	p, err := s.store.GetProvider(ctx, tenant, int64(req.ProviderID))
	if errors.Is(err, repo.ErrNotFound) {
		return reject("validation_error", hermes.NotFound("provider not found"))
	} else if err != nil {
		return reject("internal_error", s.storage(err))
	}
	ptype = p.Type
	if p.ObjectType != repo.ObjectSMS {
		return reject("validation_error", hermes.BadRequest("provider not allowed for this service: %s", p.ObjectType))
	}
	if p.Status == repo.Off {
		return reject("validation_error", hermes.BadRequest("provider %s is inactive", p.Name))
	}
	recipient := strconv.FormatUint(req.To, 10)
	blocked, err := s.store.IsBlocked(ctx, tenant, p.ID, recipient)
	if err != nil {
		return reject("block_check_error", s.storage(err))
	}
	if blocked {
		return reject("blocked", hermes.BadRequest("recipient %d is blocked for provider %s", req.To, p.Name))
	}
	text, err := s.render(ctx, tenant, req)
	if err != nil {
		return reject("validation_error", err)
	}
	cfg, err := s.env.OpenConfig(p.ConfigSealed, sealed.ProviderAD(tenant, p.ID))
	if err != nil {
		return reject("internal_error", hermes.Internal("provider init: configuration unavailable"))
	}
	sender, err := s.senders.Get(p.ID, p.Type, cfg)
	if err != nil {
		return reject("internal_error", hermes.Internal("provider init: %s", sealed.Scrub(err.Error(), provider.SecretValues(p.Type, cfg)...)))
	}

	priority := int(sender.Priority())
	if priority == 0 {
		priority = 2
	}
	tid := int64(req.TemplateID)
	msg := repo.Message{ID: repo.NewID(), TenantID: tenant, Actor: actor, Sid: int64(sender.Sid()), Recipient: recipient, Priority: priority,
		ProviderID: p.ID, TemplateID: &tid, Text: text, StatusCode: -1, StatusMessage: voicecom.StatusText(-1)}
	if req.SMS != nil {
		msg.Data, _ = json.Marshal(map[string]any{"sms": req.SMS, "properties": properties(req.Properties)})
	}
	created, err := s.store.CreateMessage(ctx, msg)
	if err != nil {
		return reject("internal_error", s.storage(err))
	}

	// The carrier exchange is not cut short by the caller going away: once
	// submitted, its outcome must be recorded.
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.timeout)
	out := sender.Send(cctx, provider.Request{To: req.To, SMS: req.SMS, Properties: req.Properties}, created.ID, text)
	cancel()

	secrets := provider.SecretValues(p.Type, cfg)
	resp := repo.ProviderResponse{RawRequest: sealed.Evidence(out.RawRequest, secrets...), RawResponse: sealed.Evidence(out.RawResponse, secrets...),
		StatusCode: 0, StatusMessage: voicecom.StatusText(0)}
	var failure error
	switch {
	case out.Err != nil:
		outcome = "provider_error"
		text := urlRE.ReplaceAllString(sealed.Scrub(out.Err.Error(), secrets...), sealed.Redacted)
		resp.StatusCode, resp.StatusMessage = 500, text
		failure = hermes.Carrier(text)
	case out.Carrier != nil:
		resp.StatusCode = out.Carrier.ReturnCode
		if slug := voicecom.StatusText(resp.StatusCode); slug != "" {
			resp.StatusMessage = slug
		} else {
			resp.StatusMessage = sealed.Scrub(out.Carrier.ReturnMessage, secrets...)
		}
	}
	ctx = context.WithoutCancel(ctx)
	updated, err := s.store.SetProviderResponse(ctx, tenant, created.ID, resp)
	if err != nil {
		s.log.Error("carrier outcome not stored", "message_id", created.ID, "err", err)
		updated = created
	}
	s.record(tenant, actor, created.ID, failure == nil)
	if failure != nil {
		return Result{}, failure
	}
	return Result{Message: updated, Carrier: out.Carrier}, nil
}

// urlRE finds URLs in carrier error text: clients never learn carrier
// endpoints or internal addresses.
var urlRE = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s"'<>]+`)

func properties(p map[string]string) map[string]string {
	if p == nil {
		return map[string]string{}
	}
	return p
}

func (s *Service) render(ctx context.Context, tenant string, req Request) (string, error) {
	if req.TemplateID == 0 {
		return "", hermes.BadRequest("templateId is required")
	}
	t, err := s.store.GetTemplate(ctx, tenant, int64(req.TemplateID))
	if errors.Is(err, repo.ErrNotFound) {
		return "", hermes.NotFound("template not found")
	} else if err != nil {
		return "", s.storage(err)
	}
	if t.Status == repo.Off {
		return "", hermes.BadRequest("template %s is inactive", t.Name)
	}
	if t.ObjectType != repo.ObjectSMS {
		return "", hermes.BadRequest("template not allowed for this service: %s", t.ObjectType)
	}
	if len(req.Properties) > MaxProperties {
		return "", hermes.BadRequest("too many template properties")
	}
	for k, v := range req.Properties {
		if len(k) > MaxPropertyBytes || len(v) > MaxPropertyBytes {
			return "", hermes.BadRequest("template property %s is too long", truncate(k, 64))
		}
	}
	body, ok := t.Templates["body"]
	if !ok {
		return "", hermes.BadRequest(`template missing fragment "body"`)
	}
	text, err := render.Render(t.Name, body, req.Properties)
	if err != nil {
		return "", hermes.Internal("%s", err.Error())
	}
	return text, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func (s *Service) record(tenant string, actor repo.Actor, id string, ok bool) {
	if s.audit == nil {
		return
	}
	e := audit.Event{TenantID: tenant, Action: audit.MessageSend, TargetType: "message", TargetID: id, Outcome: audit.Success}
	if !ok {
		e.Outcome = audit.Failure
	}
	if actor.Kind == repo.ActorPlatform {
		e.ActorKind, e.ActorID = audit.ActorOperator, actor.Platform
	} else {
		e.ActorKind, e.ActorID = audit.ActorAPIClient, strconv.FormatInt(actor.APIClientID, 10)
	}
	_ = s.audit.Record(e)
}
