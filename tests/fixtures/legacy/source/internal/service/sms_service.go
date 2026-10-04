// Package service contains the gRPC service implementations. Each
// service is thin: it validates inputs, calls one or more repos, and
// translates errors into the gateway's error envelope.
package service

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/go-kratos/kratos/v2/errors"
	"github.com/go-kratos/kratos/v2/log"
	"github.com/tx7do/kratos-bootstrap/bootstrap"

	smsgwpb "github.com/go-tangra/go-tangra-sms-gw/gen/go/sms_gw/service/v1"
	"github.com/go-tangra/go-tangra-sms-gw/internal/auth"
	"github.com/go-tangra/go-tangra-sms-gw/internal/ctxkeys"
	"github.com/go-tangra/go-tangra-sms-gw/internal/data"
	"github.com/go-tangra/go-tangra-sms-gw/internal/data/ent"
	"github.com/go-tangra/go-tangra-sms-gw/internal/metrics"
	"github.com/go-tangra/go-tangra-sms-gw/internal/provider"
	"github.com/go-tangra/go-tangra-sms-gw/internal/provider/voicecom"
	"github.com/go-tangra/go-tangra-sms-gw/internal/webhook"
	"github.com/google/uuid"
)

// SmsService implements smsgwpb.SmsServiceServer. It owns the orchestration
// logic for outbound SMS: provider lookup, template rendering, persistence,
// provider invocation, and DLR ingestion.
type SmsService struct {
	smsgwpb.UnimplementedSmsServiceServer

	log    *log.Helper
	sms    *data.SmsRepo
	pr     *data.ProviderRepo
	tr     *data.TemplateRepo
	br     *data.BlockRepo
	cr     *data.ApiClientRepo
	wh     *webhook.Dispatcher
	pcache *provider.Cache
	rp     recipientPolicy
}

func NewSmsService(
	ctx *bootstrap.Context,
	sms *data.SmsRepo,
	pr *data.ProviderRepo,
	tr *data.TemplateRepo,
	br *data.BlockRepo,
	cr *data.ApiClientRepo,
	wh *webhook.Dispatcher,
	pcache *provider.Cache,
) *SmsService {
	return &SmsService{
		log:    ctx.NewLoggerHelper("sms-gw/service/sms"),
		sms:    sms,
		pr:     pr,
		tr:     tr,
		br:     br,
		cr:     cr,
		wh:     wh,
		pcache: pcache,
		rp:     loadRecipientPolicy(),
	}
}

// Send orchestrates the full outbound flow:
//  1. Resolve and validate the provider
//  2. Optionally render the template body
//  3. Persist the SMS row in the initial sms_gw_accepted state
//  4. Hand off to the provider implementation
//  5. Persist the resulting raw_request/raw_response and final status
//
// On provider failure the SMS row is still updated so admins can debug
// from the message detail view.
func (s *SmsService) Send(ctx context.Context, req *smsgwpb.SendSmsRequest) (*smsgwpb.SendSmsResponse, error) {
	start := time.Now()
	providerType := "unknown"
	outcome := "ok"
	defer func() {
		metrics.SmsSentTotal.WithLabelValues(providerType, outcome).Inc()
		metrics.SmsSendDurationSeconds.WithLabelValues(providerType, outcome).Observe(time.Since(start).Seconds())
	}()

	// Authorization: sending is a write operation. On the public path a
	// caller carries an auth.Identity and MUST be an API_CLIENT — a
	// read-only API_VIEWER token may not send. Admin gRPC calls (mTLS
	// from the portal gateway) carry no Identity and are allowed through.
	caller := auth.FromContext(ctx)
	if caller != nil && !caller.IsClient() {
		outcome = "forbidden"
		return nil, smsgwpb.ErrorForbidden("sending SMS requires API_CLIENT authority")
	}

	if req.ProviderId == 0 {
		outcome = "validation_error"
		return nil, smsgwpb.ErrorBadRequest("providerId is required")
	}

	// Recipient policy: reject structurally invalid numbers and
	// destinations outside the configured allow/deny prefix policy before
	// any provider work. Primary defense against SMS-pumping / toll fraud.
	if err := s.rp.validate(req.To); err != nil {
		outcome = "validation_error"
		return nil, err
	}
	p, err := s.pr.Get(ctx, req.ProviderId)
	if err != nil {
		outcome = "validation_error"
		return nil, err
	}
	providerType = p.Type
	if p.ObjectType != smsgwpb.ObjectType_OBJECT_TYPE_SMS {
		outcome = "validation_error"
		return nil, smsgwpb.ErrorBadRequest("provider not allowed for this service: %s", p.ObjectType.String())
	}
	if p.Status == "OFF" {
		outcome = "validation_error"
		return nil, smsgwpb.ErrorBadRequest("provider %s is inactive", p.Name)
	}

	// Block list check. A non-zero match aborts the send before any
	// template rendering or persistence so blocked recipients leave no
	// row behind.
	blocked, err := s.br.IsBlocked(ctx, p.Id, fmtRecipient(req.To))
	if err != nil {
		// Fail closed: if we cannot determine block status we refuse the
		// send rather than risk delivering to a blocked recipient.
		outcome = "block_check_error"
		return nil, err
	}
	if blocked {
		outcome = "blocked"
		return nil, smsgwpb.ErrorBadRequest("recipient %d is blocked for provider %s", req.To, p.Name)
	}

	// Body rendering. When a template_id is supplied it is required to
	// exist and contain a "body" fragment; otherwise we error rather
	// than send an empty SMS. Without a template_id, the caller is
	// responsible for providing a non-empty Sms.From + properties (the
	// body itself comes from the template — there is no inline-text
	// path in the legacy hermes contract).
	if req.TemplateId == 0 {
		return nil, smsgwpb.ErrorBadRequest("templateId is required")
	}
	tpl, err := s.tr.Get(ctx, req.TemplateId)
	if err != nil {
		return nil, err
	}
	if tpl.Status == "OFF" {
		return nil, smsgwpb.ErrorBadRequest("template %s is inactive", tpl.Name)
	}
	if tpl.ObjectType != smsgwpb.ObjectType_OBJECT_TYPE_SMS {
		return nil, smsgwpb.ErrorBadRequest("template not allowed for this service: %s", tpl.ObjectType.String())
	}
	rendered, err := s.tr.Render(tpl, "body", req.Properties)
	if err != nil {
		return nil, err
	}

	sender, err := s.pcache.Get(p.Id, p.Type, p.Config)
	if err != nil {
		return nil, smsgwpb.ErrorInternalError("provider init: %s", err.Error())
	}

	var owner uint32
	if caller != nil {
		owner = caller.ClientID
	}

	// Persist BEFORE calling the provider so the DLR webhook can find a
	// row even if the provider responds (or DLRs in) before our HTTP
	// response unwinds.
	priority := sender.Priority()
	if priority == 0 {
		priority = 2 // sane default for providers that have no priority concept
	}
	created, err := s.sms.Create(ctx, req, sender.Sid(), priority, rendered, owner, "")
	if err != nil {
		return nil, err
	}

	uid, err := uuid.Parse(created.Data.Id)
	if err != nil {
		return nil, smsgwpb.ErrorInternalError("invalid generated id")
	}

	out := sender.Send(ctx, req, uid, rendered)

	// Translate the provider Response into a status code + message.
	// Prefer the canonical slug from voicecom.StatusCodes over the
	// carrier's free-text message so the persisted status_message
	// matches what the DLR pipeline writes (e.g. "sms_provider_accepted"
	// instead of the carrier-localised "Message accepted"). Only
	// falls back to the carrier's text for unknown codes — keeps the
	// admin UI consistent across Send vs DLR rows.
	statusCode := int32(0)
	statusMsg := "sms_provider_accepted"
	if out.Response != nil {
		statusCode = out.Response.ReturnCode
		if slug, ok := voicecom.StatusCodes[statusCode]; ok {
			statusMsg = slug
		} else {
			statusMsg = out.Response.ReturnMessage
		}
	}
	if out.Error != nil {
		outcome = "provider_error"
		// Persist the failure but continue to surface the error to the caller.
		ke := errors.FromError(out.Error)
		if _, uerr := s.sms.UpdateResponse(ctx, created.Data.Id, out.RawResponse, out.RawRequest, int32(ke.Code), ke.Message); uerr != nil {
			s.log.Errorf("persist provider failure: %s", uerr.Error())
		}
		return nil, out.Error
	}

	updated, err := s.sms.UpdateResponse(ctx, created.Data.Id, out.RawResponse, out.RawRequest, statusCode, statusMsg)
	if err != nil {
		s.log.Errorf("persist provider success: %s", err.Error())
		// Fall through with the in-memory data so callers still get a useful response.
		updated = created
	}
	updated.EndpointResponse = out.Response
	return updated, nil
}

func (s *SmsService) List(ctx context.Context, req *smsgwpb.PagingRequest) (*smsgwpb.ListSmsResponse, error) {
	page := int32(0)
	if req.Page != nil {
		page = *req.Page
	}
	size := int32(0)
	if req.PageSize != nil {
		size = *req.PageSize
	}
	// Any public-API caller (non-nil identity) sees only their own
	// messages — not just API_CLIENT. API_VIEWER on the public path is
	// scoped per-account, not "see everyone's data". Admin gRPC calls
	// arrive with no auth.Identity and pass through unscoped.
	owner := publicCallerOwner(ctx)
	filter := parseListFilter(req.Query)
	return s.sms.List(ctx, page, size, owner, filter)
}

// parseListFilter decodes the JSON-encoded havi-style query field into
// the structured filter the repo understands. An empty/invalid query
// yields a zero-value filter (no filtering applied) — the admin UI
// passes a partial object so we tolerate missing keys.
func parseListFilter(q *string) data.SmsListFilter {
	if q == nil || strings.TrimSpace(*q) == "" {
		return data.SmsListFilter{}
	}
	var raw struct {
		Recipient         string `json:"recipient"`
		Sid               *int32 `json:"sid"`
		ApiClientUsername string `json:"api_client_username"`
		Status            *int32 `json:"status"`
	}
	if err := json.Unmarshal([]byte(*q), &raw); err != nil {
		// Malformed query is treated as no filter rather than 400 —
		// keeps wire-compat with hermes which silently ignored bad
		// query payloads.
		return data.SmsListFilter{}
	}
	out := data.SmsListFilter{
		Recipient:         strings.TrimSpace(raw.Recipient),
		ApiClientUsername: strings.TrimSpace(raw.ApiClientUsername),
		StatusCode:        raw.Status,
	}
	if raw.Sid != nil && *raw.Sid > 0 {
		out.Sid = uint32(*raw.Sid)
	}
	return out
}

func (s *SmsService) Get(ctx context.Context, req *smsgwpb.GetSmsMessageRequest) (*smsgwpb.GetSmsMessageResponse, error) {
	if req.Id == "" {
		return nil, smsgwpb.ErrorBadRequest("id is required")
	}
	res, err := s.sms.Get(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	if owner := publicCallerOwner(ctx); owner != 0 && res.Data.Owner != owner {
		return nil, smsgwpb.ErrorRecordNotFound("sms not found")
	}
	return res, nil
}

// publicCallerOwner returns the owner-id any results should be scoped to.
// Returns 0 for admin gRPC calls (no public identity) — callers MUST
// treat 0 as "no scoping" rather than "owner==0".
func publicCallerOwner(ctx context.Context) uint32 {
	id := auth.FromContext(ctx)
	if id == nil {
		return 0
	}
	switch id.Authority {
	case "API_CLIENT", "API_VIEWER":
		return id.ClientID
	case "API_ADMIN":
		// Admins of the public API can see all records on the public
		// path. The admin gRPC path is a separate trust boundary.
		return 0
	default:
		// Unknown authority — fail closed. Treats the caller as if they
		// own nothing, returning empty results / not-found.
		return ^uint32(0)
	}
}

// Dlr is invoked by the third-party provider via the public HTTP wrapper
// that always returns 200 "DLR_OK". This handler returns errors for
// observability only; the wrapper swallows them.
//
// After persisting, looks up the originating client's outbound webhook
// configuration and enqueues a delivery. A missing or empty callback URL
// is a no-op.
func (s *SmsService) Dlr(ctx context.Context, req *smsgwpb.DlrRequest) (*smsgwpb.DlrResponse, error) {
	metrics.DlrReceivedTotal.WithLabelValues(strconv.Itoa(int(req.MessageStatus))).Inc()

	supplied, _ := ctx.Value(ctxkeys.DlrToken{}).(string)
	remoteAddr, _ := ctx.Value(ctxkeys.ClientIP{}).(string)

	// Voicecom (LINK mChannel JSON API §6, p18) calls back as
	//   https://{callback_url}?channel=…&sid=…&message_status=…
	// — a literal `?` is appended to whatever URL was configured, so
	// any pre-existing query string we put there (including
	// `?dlr_token=…`) is overwritten. There is no place in the carrier
	// spec for an extra auth token on the DLR side.
	//
	// Anti-spoofing falls back to validateDlrToken's existing logic:
	//   - if the SMS row has no stored dlr_token (legacy / no token
	//     in the provider config) → accept the DLR (LoadParentForDlr
	//     still requires a real request_id to match a row we sent).
	//   - if the SMS row HAS a token AND the request supplies one,
	//     constant-time compare. Mismatch = drop.
	//   - if the SMS row HAS a token but the request supplies none,
	//     mismatch = drop (this is the only path where the legacy
	//     defense still applies — for non-Voicecom providers that DO
	//     preserve the query string).

	// Single read of the parent — feeds both validation and persistence.
	parent, found, err := s.sms.LoadParentForDlr(ctx, req.RequestId)
	if err != nil {
		// DB error — drop silently (the wrapper still returns 200 DLR_OK).
		s.log.Warnf("dlr parent load failed for %s: %s", req.RequestId, err.Error())
		return &smsgwpb.DlrResponse{}, nil
	}
	if !s.validateDlrToken(ctx, parent, found, supplied) {
		// Anti-probe: log and return success without persisting. If
		// we returned an error, the wrapping handler would still reply
		// "DLR_OK" — but a probing attacker would notice the side
		// effect (or absence of one) by polling /sms/dlr/{id}.
		s.log.Warnf("dlr token mismatch for message_id=%s; dropping", req.RequestId)
		return &smsgwpb.DlrResponse{}, nil
	}
	if !found {
		// Spoofed UUID. Token compare already ran inside
		// validateDlrToken to flatten the timing oracle; nothing to
		// persist.
		return &smsgwpb.DlrResponse{}, nil
	}

	parent, err = s.sms.PersistDlr(ctx, parent, req, remoteAddr)
	if err != nil {
		return nil, err
	}
	if parent != nil && parent.CreateBy != nil && *parent.CreateBy != 0 && s.wh != nil {
		target := s.cr.GetCallbackTarget(ctx, *parent.CreateBy)
		if target.URL != "" {
			queued := s.wh.Enqueue(webhook.Event{
				URL:           target.URL,
				Secret:        target.Secret,
				MessageID:     parent.ID,
				Channel:       req.Channel,
				MessageStatus: req.MessageStatus,
				StatusText:    voicecomStatusText(int32(req.MessageStatus)),
				Recipient:     req.To,
				From:          req.From,
				Timestamp:     req.Timestamp,
			})
			if !queued {
				metrics.WebhookDeliveryTotal.WithLabelValues("dropped").Inc()
			}
		}
	}
	return &smsgwpb.DlrResponse{}, nil
}

// voicecomStatusText is a small wrapper over the voicecom map kept in
// service-layer scope so we don't pull provider/voicecom into the
// webhook event helper. Synced manually with the canonical map.
func voicecomStatusText(code int32) string {
	return statusTextLookup(code)
}

// dlrTokenWidth is the byte length of an auto-generated dlr_token.
// providerRepo.Create generates tokens via randomHex(16), so the hex
// representation is always 32 characters. Both arms of validateDlrToken
// pad supplied to this width before comparing — subtle.ConstantTimeCompare
// returns immediately on length mismatch, which would otherwise leak
// the expected token length to an attacker probing the unauthenticated
// DLR endpoint with varying-length values.
const dlrTokenWidth = 32

// dummyDlrToken is the comparison target when the message_id doesn't
// resolve. Same width as a real token so the constant-time compare on
// the spoofed-UUID branch takes the same wall-clock time as a real-UUID
// branch with a wrong token.
const dummyDlrToken = "00000000000000000000000000000000"

// validateDlrToken decides whether to persist the inbound DLR. The
// parent (already loaded) carries provider_id; we look up that
// provider's stored dlr_token and constant-time-compare it against
// the supplied value. Decision matrix:
//
//	parent == nil (spoofed UUID)             → run dummy compare, true
//	GetDlrToken DB error                     → false (fail closed)
//	provider has no dlr_token (legacy)       → true  (no enforcement)
//	supplied == stored (constant-time)       → true
//	mismatch                                 → false
//
// The dummy-compare branch on `!found` flattens the timing oracle that
// would otherwise let an attacker distinguish "real UUID, wrong token"
// from "fake UUID" by measuring response time. Both branches pad
// supplied to dlrTokenWidth so the compare cost is independent of the
// supplied length.
func (s *SmsService) validateDlrToken(ctx context.Context, parent *ent.SmsMessage, found bool, supplied string) bool {
	suppliedFixed := padOrTruncate(supplied, dlrTokenWidth)
	if !found || parent == nil {
		_ = subtle.ConstantTimeCompare(suppliedFixed, []byte(dummyDlrToken))
		return true
	}
	expected, ok := s.pr.GetDlrToken(ctx, parent.ProviderID)
	if !ok {
		return false
	}
	if expected == "" {
		// Legacy provider — still pay the compare cost so the timing
		// distinction between "no enforcement" and "enforcement with
		// wrong token" is invisible to a probing caller.
		_ = subtle.ConstantTimeCompare(suppliedFixed, []byte(dummyDlrToken))
		return true
	}
	return subtle.ConstantTimeCompare(suppliedFixed, padOrTruncate(expected, dlrTokenWidth)) == 1
}

// padOrTruncate returns a byte slice of exactly width bytes — left-pads
// with NUL when the input is shorter, truncates when longer. The fixed
// width is what makes subtle.ConstantTimeCompare actually constant-time:
// when the two slices differ in length the function short-circuits.
func padOrTruncate(s string, width int) []byte {
	out := make([]byte, width)
	copy(out, s)
	return out
}

func (s *SmsService) Dlrs(ctx context.Context, req *smsgwpb.ListDlrRequest) (*smsgwpb.ListDlrResponse, error) {
	if req.Id == "" {
		return nil, smsgwpb.ErrorBadRequest("id is required")
	}
	// Resolve the parent SMS first so we can apply the same ownership
	// guard Get uses. Without this, any authenticated caller could
	// enumerate DLRs for other clients' messages just by knowing the
	// UUID.
	if owner := publicCallerOwner(ctx); owner != 0 {
		parent, err := s.sms.Get(ctx, req.Id)
		if err != nil {
			return nil, err
		}
		if parent.Data.Owner != owner {
			return nil, smsgwpb.ErrorRecordNotFound("sms not found")
		}
	}
	return s.sms.Dlrs(ctx, req.Id)
}

// fmtRecipient stringifies the wire-level uint64 recipient. Centralized
// so the block-list check uses exactly the same encoding the SMS row was
// stored under.
func fmtRecipient(to uint64) string { return strconv.FormatUint(to, 10) }
