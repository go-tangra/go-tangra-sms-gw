package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/hermes"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/metrics"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/provider"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/repo"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/sealed"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/sms"
)

// Actor is who sent a message.
type Actor struct {
	Kind              string `json:"kind"`
	APIClientID       int64  `json:"api_client_id,omitempty"`
	APIClientUsername string `json:"api_client_username,omitempty"`
	UserID            string `json:"user_id,omitempty"`
}

// Message is the message DTO (lists and send results).
type Message struct {
	ID            string    `json:"id"`
	Recipient     string    `json:"recipient"`
	ProviderID    int64     `json:"provider_id"`
	ProviderName  string    `json:"provider_name"`
	TemplateID    *int64    `json:"template_id"`
	Sid           int64     `json:"sid"`
	Priority      int       `json:"priority"`
	Text          string    `json:"text"`
	StatusCode    int32     `json:"status_code"`
	StatusMessage string    `json:"status_message"`
	Actor         Actor     `json:"actor"`
	DLRTs         int64     `json:"dlr_ts"`
	RemoteAddress string    `json:"remote_address,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// MessageDetail adds the send data and sanitized carrier evidence.
type MessageDetail struct {
	Message
	Data     json.RawMessage   `json:"data,omitempty"`
	Evidence map[string]string `json:"evidence,omitempty"`
}

// Receipt is the delivery receipt DTO.
type Receipt struct {
	ID            int64     `json:"id"`
	Status        uint32    `json:"status"`
	StatusText    string    `json:"status_text"`
	Channel       string    `json:"channel"`
	Sid           int64     `json:"sid"`
	Recipient     string    `json:"recipient"`
	Sender        string    `json:"sender"`
	Timestamp     int64     `json:"timestamp"`
	PartsReceived int       `json:"parts_received"`
	RemoteAddress string    `json:"remote_address,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func messageDTO(m repo.Message) Message {
	d := Message{ID: m.ID, Recipient: m.Recipient, ProviderID: m.ProviderID, ProviderName: m.ProviderName, TemplateID: m.TemplateID, Sid: m.Sid,
		Priority: m.Priority, Text: m.Text, StatusCode: m.StatusCode, StatusMessage: m.StatusMessage, DLRTs: m.DLRTs, RemoteAddress: m.RemoteAddress,
		CreatedAt: m.CreateTime, UpdatedAt: m.UpdateTime, Actor: Actor{Kind: m.Actor.Kind}}
	if m.TemplateID != nil && *m.TemplateID == 0 {
		d.TemplateID = nil
	}
	if m.Actor.Kind == repo.ActorPlatform {
		d.Actor.UserID = m.Actor.Platform
	} else {
		d.Actor.APIClientID, d.Actor.APIClientUsername = m.Actor.APIClientID, m.APIClientUsername
	}
	return d
}

func evidence(raw []byte) string {
	return strings.ToValidUTF8(string(sealed.Evidence(raw)), "�")
}

func (s *Server) listMessages(w http.ResponseWriter, r *http.Request) {
	pg, req, err := page(r, repo.MessageList)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	q := r.URL.Query()
	f := repo.MessageFilter{Recipient: q.Get("recipient"), APIClientUsername: q.Get("api_client_username")}
	if v := q.Get("sid"); v != "" {
		n, _ := strconv.ParseInt(v, 10, 64)
		f.Sid = &n
	}
	if v := q.Get("status"); v != "" {
		n, _ := strconv.ParseInt(v, 10, 32)
		st := int32(n)
		f.Status = &st
	}
	if v := q.Get("provider_id"); v != "" {
		n, _ := strconv.ParseInt(v, 10, 64)
		f.ProviderID = &n
	}
	l, err := s.cfg.Store.ListMessages(r.Context(), repo.TenantView(operatorOf(r).TenantID), f, pg)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	items := make([]Message, 0, len(l.Items))
	for _, m := range l.Items {
		items = append(items, messageDTO(m))
	}
	writeJSON(w, http.StatusOK, listPage(items, meta(l), req))
}

func (s *Server) getMessage(w http.ResponseWriter, r *http.Request) {
	m, err := s.cfg.Store.GetMessage(r.Context(), repo.TenantView(operatorOf(r).TenantID), r.PathValue("message_id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	d := MessageDetail{Message: messageDTO(m), Data: m.Data}
	if len(m.RawRequest) > 0 || len(m.RawResponse) > 0 {
		d.Evidence = map[string]string{"request": evidence(m.RawRequest), "response": evidence(m.RawResponse)}
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) listReceipts(w http.ResponseWriter, r *http.Request) {
	l, err := s.cfg.Store.ListReceipts(r.Context(), repo.TenantView(operatorOf(r).TenantID), r.PathValue("message_id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	items := make([]Receipt, 0, len(l.Items))
	for _, d := range l.Items {
		items = append(items, Receipt{ID: d.ID, Status: d.MessageStatus, StatusText: d.StatusText, Channel: d.Channel, Sid: d.Sid,
			Recipient: strconv.FormatUint(d.Recipient, 10), Sender: d.Sender, Timestamp: d.Timestamp, PartsReceived: d.PartsReceived,
			RemoteAddress: d.RemoteAddress, CreatedAt: d.CreateTime, UpdatedAt: d.UpdateTime})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": l.Total})
}

// sendMessage runs the public send pipeline with the verified operator as
// a separate platform actor (no client callback follows).
func (s *Server) sendMessage(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ProviderID uint32            `json:"provider_id"`
		TemplateID uint32            `json:"template_id"`
		To         string            `json:"to"`
		Properties map[string]string `json:"properties"`
		SMS        *struct {
			From        string `json:"from"`
			Encoding    string `json:"encoding"`
			Concatenate uint32 `json:"concatenate"`
		} `json:"sms"`
	}
	if err := decode(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	to, err := strconv.ParseUint(in.To, 10, 64)
	if err != nil {
		s.fail(w, r, ErrValidation.With("field", "to"))
		return
	}
	if err := checkProperties(in.Properties); err != nil {
		s.fail(w, r, err)
		return
	}
	req := sms.Request{To: to, ProviderID: in.ProviderID, TemplateID: in.TemplateID, Properties: in.Properties}
	if in.SMS != nil {
		req.SMS = &provider.SMS{From: in.SMS.From, Encoding: in.SMS.Encoding, Concatenate: in.SMS.Concatenate}
	}
	op := operatorOf(r)
	res, err := s.cfg.SMS.Send(r.Context(), op.TenantID, repo.PlatformActor(op.UserID), req)
	if err != nil {
		s.fail(w, r, sendError(err))
		return
	}
	out := map[string]any{"message": messageDTO(res.Message), "carrier": nil}
	if c := res.Carrier; c != nil {
		out["carrier"] = map[string]any{"return_code": c.ReturnCode, "return_message": c.ReturnMessage}
	}
	writeJSON(w, http.StatusCreated, out)
}

// sendError maps a domain refusal: carrier failures (already sanitized,
// recorded and never resent) are 502, storage outages 503, everything else
// a rejected send with the domain's message.
func sendError(err error) error {
	var e *hermes.Error
	if !errors.As(err, &e) {
		return err
	}
	switch {
	case e.Code == http.StatusInternalServerError && e.Reason == hermes.ReasonUnknown:
		return ErrCarrier.With("message", e.Message)
	case e.Code == http.StatusServiceUnavailable:
		return ErrUnavailable
	}
	return ErrSendRejected.With("message", e.Message)
}

// ---------- dashboard ----------

type dashboardQuery struct {
	Window  string   `json:"window"`
	Queries []string `json:"queries"`
}

func dashboardError(err error) error {
	switch {
	case errors.Is(err, metrics.ErrUnknownWindow):
		return ErrValidation.With("field", "window")
	case errors.Is(err, metrics.ErrUnknownQuery):
		return ErrValidation.With("field", "queries")
	}
	return err
}

func (s *Server) dashboardInstant(w http.ResponseWriter, r *http.Request) {
	var in dashboardQuery
	if err := decode(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	res, err := s.cfg.Dashboard.Instant(r.Context(), in.Window, in.Queries)
	if err != nil {
		s.fail(w, r, dashboardError(err))
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) dashboardRange(w http.ResponseWriter, r *http.Request) {
	var in dashboardQuery
	if err := decode(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	res, err := s.cfg.Dashboard.Range(r.Context(), in.Window, in.Queries)
	if err != nil {
		s.fail(w, r, dashboardError(err))
		return
	}
	writeJSON(w, http.StatusOK, res)
}
