// Package publicapi is the legacy Hermes wire adapter on the dedicated
// public listener: the source routes and aliases, codecs, error envelope,
// authentication and rate limits. Management routes are never mounted here.
package publicapi

import (
	"log/slog"
	"net/http"
	"path"
	"strconv"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/auth"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/hermes"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/provider"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/ratelimit"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/repo"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/sms"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/trustedproxy"
)

// Config wires the server.
type Config struct {
	Auth  *auth.Service
	SMS   *sms.Service
	Proxy *trustedproxy.Resolver
	Login *ratelimit.Limiter // per client address
	Send  *ratelimit.Limiter // per client
	// Receipts handles carrier receipts on the reserved GET /dlr and GET
	// /hermes/v1/sms/dlr routes; nil acknowledges them without effect.
	Receipts http.Handler
	MaxBody  int64
	// Style is the legacy build's JSON flavour (public.legacy_json_style).
	Style JSONStyle
	Log   *slog.Logger
}

// Server serves the public Hermes routes.
type Server struct{ c Config }

// New builds the server.
func New(c Config) *Server {
	if c.MaxBody <= 0 {
		c.MaxBody = 64 << 10
	}
	if c.Log == nil {
		c.Log = slog.New(slog.DiscardHandler)
	}
	return &Server{c: c}
}

// ServeHTTP routes exactly the source routes; anything else, including a
// known path with another method, is the source "404 page not found". As
// the source router (gorilla/mux under Kratos, StrictSlash) did, an unclean
// path is redirected to its clean form and a route with an extra trailing
// slash to the route, both 301.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if v := recover(); v != nil {
			s.c.Log.Error("public handler panic", "path", r.URL.Path)
			writeJSON(w, s.c.Style, 500, object{{"code", 500}, {"reason", "UNKNOWN"}, {"message", "unknown request error"}, {"metadata", object{}}})
		}
	}()
	p := r.URL.Path
	if clean := cleanPath(p); clean != p {
		u := *r.URL
		u.Path, u.RawPath = clean, ""
		w.Header().Set("Location", u.String())
		w.WriteHeader(http.StatusMovedPermanently)
		return
	}
	if h := s.route(r.Method, p); h != nil {
		h(w, r)
		return
	}
	if trimmed, ok := strings.CutSuffix(p, "/"); ok && trimmed != "" && s.route(r.Method, trimmed) != nil {
		u := *r.URL
		u.Path, u.RawPath = trimmed, ""
		http.Redirect(w, r, u.String(), http.StatusMovedPermanently)
		return
	}
	http.NotFound(w, r)
}

// route returns the handler of a source route, or nil.
func (s *Server) route(method, p string) http.HandlerFunc {
	get, post := method == http.MethodGet, method == http.MethodPost
	switch {
	case p == "/health" && get:
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("{\"status\":\"ok\"}\n"))
		}
	case p == "/hermes/v1/login" && post:
		return s.login
	case p == "/hermes/v1/refresh_token" && post:
		return s.refresh
	case p == "/hermes/v1/logout" && post:
		return s.logout
	case p == "/hermes/v1/me" && get:
		return s.me
	// Literal receipt routes come before the message id patterns.
	case (p == "/dlr" || p == "/hermes/v1/sms/dlr") && get:
		return s.receipt
	case (p == "/hermes/v1/sms" || p == "/v1/sms") && post:
		return s.send
	case (p == "/hermes/v1/sms" || p == "/v1/sms") && get:
		return s.list
	}
	if !get {
		return nil
	}
	if id, ok := segment(p, "/hermes/v1/sms/dlr/", "/v1/sms/dlr/"); ok {
		return func(w http.ResponseWriter, r *http.Request) { s.receipts(w, r, id) }
	}
	if id, ok := segment(p, "/hermes/v1/sms/", "/v1/sms/"); ok {
		return func(w http.ResponseWriter, r *http.Request) { s.get(w, r, id) }
	}
	return nil
}

// cleanPath is the source router's path cleaning: path.Clean keeping a
// trailing slash, "/" for an empty path.
func cleanPath(p string) string {
	if p == "" {
		return "/"
	}
	if p[0] != '/' {
		p = "/" + p
	}
	np := path.Clean(p)
	if p[len(p)-1] == '/' && np != "/" {
		np += "/"
	}
	return np
}

// segment returns the single non-empty path segment after one of prefixes.
func segment(p string, prefixes ...string) (string, bool) {
	for _, pre := range prefixes {
		if rest, ok := strings.CutPrefix(p, pre); ok && rest != "" && !strings.Contains(rest, "/") {
			return rest, true
		}
	}
	return "", false
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	m, err := decodeBody(r, "LoginRequest", s.c.MaxBody, s.c.Style)
	if err != nil {
		writeError(w, s.c.Style, err)
		return
	}
	ip := s.c.Proxy.ClientIP(r)
	if !s.c.Login.Allow(ip) {
		writeError(w, s.c.Style, hermes.TooManyRequests("rate limit exceeded for %s", ip))
		return
	}
	t, err := s.c.Auth.Login(r.Context(), str(m, "username"), str(m, "password"), ip, r.UserAgent())
	s.tokens(w, t, err)
}

func (s *Server) refresh(w http.ResponseWriter, r *http.Request) {
	m, err := decodeBody(r, "RefreshTokenRequest", s.c.MaxBody, s.c.Style)
	if err != nil {
		writeError(w, s.c.Style, err)
		return
	}
	t, err := s.c.Auth.Refresh(r.Context(), str(m, "refresh_token"))
	s.tokens(w, t, err)
}

func (s *Server) tokens(w http.ResponseWriter, t auth.Tokens, err error) {
	if err != nil {
		writeError(w, s.c.Style, err)
		return
	}
	writeJSON(w, s.c.Style, 200, object{{"access_token", t.Access}, {"refresh_token", t.Refresh}, {"token_type", "Bearer"}, {"expires_in", quoted(t.ExpiresIn)}})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if _, err := decodeBody(r, "LogoutRequest", s.c.MaxBody, s.c.Style); err != nil {
		writeError(w, s.c.Style, err)
		return
	}
	s.c.Auth.Logout(r.Context(), r.Header.Get("Authorization"), r.Header.Get("X-Refresh-Token"))
	writeJSON(w, s.c.Style, 200, object{})
}

func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (hermes.Caller, bool) {
	c, err := s.c.Auth.Authenticate(r.Context(), r.Header.Get("Authorization"))
	if err != nil {
		writeError(w, s.c.Style, err)
		return c, false
	}
	return c, true
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	c, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	abilities := []any{}
	for _, a := range auth.Abilities(c.Client.Authority) {
		abilities = append(abilities, object{{"action", a.Action}, {"subject", a.Subject}})
	}
	writeJSON(w, s.c.Style, 200, object{{"user", object{{"id", uint32(c.Client.ID)}, {"username", c.Client.Username}, {"email", c.Client.Email},
		{"authority", c.Client.Authority}}}, {"abilities", abilities}})
}

func (s *Server) send(w http.ResponseWriter, r *http.Request) {
	m, err := decodeBody(r, "SendSmsRequest", s.c.MaxBody, s.c.Style)
	if err != nil {
		writeError(w, s.c.Style, err)
		return
	}
	c, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	if key := "client:" + strconv.FormatInt(c.Client.ID, 10); !s.c.Send.Allow(key) {
		writeError(w, s.c.Style, hermes.TooManyRequests("rate limit exceeded for %s", key))
		return
	}
	if !c.CanSend() {
		writeError(w, s.c.Style, hermes.Forbidden("sending SMS requires API_CLIENT authority"))
		return
	}
	res, err := s.c.SMS.Send(r.Context(), c.Client.TenantID, repo.ClientActor(c.Client.ID), sendRequest(m))
	if err != nil {
		writeError(w, s.c.Style, err)
		return
	}
	// The source send response does not join the provider and client names.
	msg := res.Message
	msg.ProviderName, msg.APIClientUsername = "", ""
	writeJSON(w, s.c.Style, 200, object{{"data", messageJSON(msg)}, {"endpointResponse", carrierJSON(res.Carrier)}})
}

func sendRequest(m protoreflect.Message) sms.Request {
	req := sms.Request{To: u64v(m, "to"), ProviderID: u32(m, "provider_id"), TemplateID: u32(m, "template_id")}
	if props := m.Get(field(m, "properties")).Map(); props.Len() > 0 {
		req.Properties = make(map[string]string, props.Len())
		props.Range(func(k protoreflect.MapKey, v protoreflect.Value) bool {
			req.Properties[k.String()] = v.String()
			return true
		})
	}
	if has(m, "sms") {
		o := sub(m, "sms")
		req.SMS = &provider.SMS{From: str(o, "from"), Encoding: str(o, "encoding"), Concatenate: u32(o, "concatenate"), Mccmnc: u32(o, "mccmnc")}
		if has(o, "validity") {
			v := sub(o, "validity")
			req.SMS.Validity = &provider.Validity{TTL: u32(v, "ttl"), Units: str(v, "units")}
		}
	}
	return req
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	q, err := decodeQuery(r, "PagingRequest")
	if err != nil {
		writeError(w, s.c.Style, err)
		return
	}
	c, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	view, ok := c.View()
	if !ok {
		writeJSON(w, s.c.Style, 200, object{{"items", []any{}}, {"total", 0}})
		return
	}
	l, err := s.c.SMS.List(r.Context(), view, sms.ParseFilter(str(q, "query")), repo.Page{Page: int(i32(q, "page")), Size: int(i32(q, "page_size"))})
	if err != nil {
		writeError(w, s.c.Style, err)
		return
	}
	items := make([]any, 0, len(l.Items))
	for _, m := range l.Items {
		items = append(items, messageJSON(m))
	}
	writeJSON(w, s.c.Style, 200, object{{"items", items}, {"total", l.Total}})
}

var errNoSMS = hermes.NotFound("sms not found")

func (s *Server) get(w http.ResponseWriter, r *http.Request, id string) {
	c, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	view, ok := c.View()
	if !ok {
		writeError(w, s.c.Style, errNoSMS)
		return
	}
	m, err := s.c.SMS.Get(r.Context(), view, id)
	if err != nil {
		writeError(w, s.c.Style, err)
		return
	}
	writeJSON(w, s.c.Style, 200, object{{"data", messageJSON(m)}})
}

func (s *Server) receipts(w http.ResponseWriter, r *http.Request, id string) {
	c, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	view, ok := c.View()
	if !ok {
		writeError(w, s.c.Style, errNoSMS)
		return
	}
	l, err := s.c.SMS.Receipts(r.Context(), view, id)
	if err != nil {
		writeError(w, s.c.Style, err)
		return
	}
	items := make([]any, 0, len(l.Items))
	for _, d := range l.Items {
		items = append(items, receiptJSON(d))
	}
	writeJSON(w, s.c.Style, 200, object{{"items", items}, {"total", l.Total}})
}

// receipt is the reserved carrier receipt route: always the source
// acknowledgement, the JSON string "DLR_OK".
func (s *Server) receipt(w http.ResponseWriter, r *http.Request) {
	if s.c.Receipts != nil {
		s.c.Receipts.ServeHTTP(w, r)
		return
	}
	WriteReceiptAck(w)
}

// WriteReceiptAck writes the source receipt acknowledgement.
func WriteReceiptAck(w http.ResponseWriter) { writeJSON(w, StyleSpaced, 200, "DLR_OK") }

// messageJSON is the source Message: the legacy mapper never filled sms,
// raw evidence or providerId, and the status is the unsigned code.
func messageJSON(m repo.Message) object {
	to, _ := strconv.ParseUint(m.Recipient, 10, 64)
	return object{{"id", m.ID}, {"owner", uint32(m.Actor.APIClientID)}, {"sid", uint32(m.Sid)}, {"to", quotedU(to)}, {"priority", uint32(m.Priority)},
		{"defer", m.Defer}, {"sms", nil}, {"text", m.Text}, {"status", uint32(m.StatusCode)}, {"statusMessage", m.StatusMessage}, {"rawResponse", ""},
		{"rawRequest", ""}, {"userName", m.UserName}, {"providerId", uint32(0)}, {"providerName", m.ProviderName}, {"apiClientUsername", m.APIClientUsername},
		{"createTime", timestamp(m.CreateTime)}, {"updateTime", timestamp(m.UpdateTime)}}
}

func carrierJSON(c *provider.CarrierResponse) any {
	if c == nil {
		return nil
	}
	var channels any
	if ch := c.Channels; ch != nil {
		var smsCh, viber any
		if ch.SMS != nil {
			smsCh = object{{"sendOrder", ch.SMS.SendOrder}, {"messageParts", ch.SMS.MessageParts}}
		}
		if ch.Viber != nil {
			viber = object{{"sendOrder", ch.Viber.SendOrder}}
		}
		channels = object{{"sms", smsCh}, {"viber", viber}}
	}
	return object{{"returnCode", c.ReturnCode}, {"returnMessage", c.ReturnMessage}, {"channels", channels}}
}

// receiptJSON is the source Dlr, including the remoteAddrerss spelling.
func receiptJSON(d repo.Receipt) object {
	return object{{"id", quotedU(uint64(d.ID))}, {"requestId", d.MessageID}, {"channel", d.Channel}, {"sid", int32(d.Sid)},
		{"messageStatus", d.MessageStatus}, {"to", quotedU(d.Recipient)}, {"from", d.Sender}, {"timestamp", quotedU(uint64(d.Timestamp))},
		{"remoteAddrerss", d.RemoteAddress}, {"statusText", d.StatusText}, {"partsReceived", uint32(d.PartsReceived)},
		{"createTime", timestamp(d.CreateTime)}, {"updateTime", timestamp(d.UpdateTime)}}
}
