package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"

	"github.com/go-tangra/go-tangra/v4/listquery"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/audit"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/authz"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/repo"
)

// Error is a refusal with a reason from the closed vocabulary.
type Error struct {
	Status int
	Reason string
	Detail map[string]any
}

func (e *Error) Error() string { return e.Reason }

// With returns a copy carrying detail (parameter or field names and
// sanitized messages, never submitted values).
func (e *Error) With(kv ...any) *Error {
	d := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		d[kv[i].(string)] = kv[i+1]
	}
	return &Error{Status: e.Status, Reason: e.Reason, Detail: d}
}

// Refusals (api/openapi/sms-gw.yaml).
var (
	ErrNotFound       = &Error{Status: http.StatusNotFound, Reason: "not_found"}
	ErrMethod         = &Error{Status: http.StatusMethodNotAllowed, Reason: "method_not_allowed"}
	ErrMalformed      = &Error{Status: http.StatusBadRequest, Reason: "malformed_body"}
	ErrValidation     = &Error{Status: http.StatusBadRequest, Reason: "validation_failed"}
	ErrSendRejected   = &Error{Status: http.StatusBadRequest, Reason: "send_rejected"}
	ErrConflict       = &Error{Status: http.StatusConflict, Reason: "conflict"}
	ErrInUse          = &Error{Status: http.StatusConflict, Reason: "in_use"}
	ErrBodyTooLarge   = &Error{Status: http.StatusRequestEntityTooLarge, Reason: "body_too_large"}
	ErrCarrier        = &Error{Status: http.StatusBadGateway, Reason: "carrier_failed"}
	ErrUnavailable    = &Error{Status: http.StatusServiceUnavailable, Reason: "temporarily_unavailable"}
	ErrNotImplemented = &Error{Status: http.StatusNotImplemented, Reason: "not_implemented"}
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, e *Error) {
	body := map[string]any{"reason": e.Reason}
	if len(e.Detail) > 0 {
		body["detail"] = e.Detail
	}
	writeJSON(w, e.Status, body)
}

// fail answers err: an *Error verbatim, repository sentinels by meaning,
// anything else as 503 (details only in the log).
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	var e *Error
	var mbe *http.MaxBytesError
	switch {
	case errors.As(err, &e):
	case errors.As(err, &mbe):
		e = ErrBodyTooLarge
	case errors.Is(err, repo.ErrNotFound):
		e = ErrNotFound
	case errors.Is(err, repo.ErrConflict):
		e = ErrConflict
	case errors.Is(err, repo.ErrReference):
		e = ErrInUse
	case errors.Is(err, repo.ErrInvalid):
		e = ErrValidation
	default:
		s.log.ErrorContext(r.Context(), "management request failed", "path", r.URL.Path, "request_id", requestID(r), "err", err)
		e = ErrUnavailable
	}
	writeError(w, e)
}

var requestIDRE = regexp.MustCompile(`^[A-Za-z0-9:/._-]{1,128}$`)

// requestID is the gateway correlation id when it is a plain identifier.
func requestID(r *http.Request) string {
	if id := r.Header.Get("X-Request-Id"); requestIDRE.MatchString(id) {
		return id
	}
	return ""
}

// ServeHTTP runs the chain: security headers → route match → operator
// authentication and permission → bounded body → OpenAPI validation →
// handler with the operator in the context.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Cache-Control", "no-store")
	route, params, err := s.router.FindRoute(r)
	if err != nil {
		if errors.Is(err, routers.ErrMethodNotAllowed) {
			writeError(w, ErrMethod)
			return
		}
		writeError(w, ErrNotFound)
		return
	}
	op := s.ops[Route{r.Method, route.Path}]
	operator, err := s.cfg.Authz.Authenticate(r)
	if err == nil {
		err = s.cfg.Authz.Authorize(r.Context(), operator, op.permission)
	}
	if err != nil {
		authz.WriteError(w, err)
		return
	}
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, op.bodyLimit)
	}
	in := &openapi3filter.RequestValidationInput{Request: r, PathParams: params, Route: route,
		Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc, MultiError: false}}
	if err := openapi3filter.ValidateRequest(r.Context(), in); err != nil {
		writeError(w, validationError(err))
		return
	}
	s.mux.ServeHTTP(w, r.WithContext(authz.WithOperator(r.Context(), operator)))
}

// validationError names the failing parameter or body field, never its value.
func validationError(err error) *Error {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) || strings.Contains(err.Error(), "request body too large") {
		return ErrBodyTooLarge
	}
	var re *openapi3filter.RequestError
	if errors.As(err, &re) {
		if re.Parameter != nil {
			return ErrValidation.With("param", re.Parameter.Name)
		}
		var pe *openapi3filter.ParseError
		if errors.As(err, &pe) {
			return ErrMalformed
		}
		var se *openapi3.SchemaError
		if errors.As(err, &se) {
			if p := se.JSONPointer(); len(p) > 0 {
				return ErrValidation.With("field", strings.Join(p, "."))
			}
			return ErrValidation.With("field", "body")
		}
		if re.RequestBody != nil {
			return ErrMalformed
		}
	}
	return ErrValidation
}

// decode reads the (already validated) JSON body into v; an empty body
// leaves v unchanged.
func decode(r *http.Request, v any) error {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return ErrBodyTooLarge
		}
		return ErrMalformed
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return ErrMalformed
	}
	return nil
}

func operatorOf(r *http.Request) authz.Operator {
	op, _ := authz.FromContext(r.Context())
	return op
}

// idParam reads a validated numeric path parameter.
func idParam(r *http.Request, name string) int64 {
	n, _ := strconv.ParseInt(r.PathValue(name), 10, 64)
	return n
}

// page parses the list contract parameters against spec.
func page(r *http.Request, spec listquery.Spec) (repo.Page, listquery.Request, error) {
	q := r.URL.Query()
	req, err := listquery.Parse(q, spec)
	if err != nil {
		param := "page"
		var le *listquery.Error
		if errors.As(err, &le) {
			param = le.Param
		}
		return repo.Page{}, req, ErrValidation.With("param", param)
	}
	return repo.Page{Page: req.Page, Size: req.PageSize, Sort: req.Sort, Desc: req.Order == listquery.Desc, Search: strings.TrimSpace(q.Get("q"))}, req, nil
}

// listPage shapes a list response with the page actually served.
func listPage[T any](items []T, l repo.List[any], req listquery.Request) listquery.Page[T] {
	if l.Page > 0 {
		req.Page = l.Page
	}
	return listquery.NewPage(items, l.Total, req)
}

func meta[T any](l repo.List[T]) repo.List[any] { return repo.List[any]{Total: l.Total, Page: l.Page} }

// record writes a sanitized audit event for an operator action.
func (s *Server) record(r *http.Request, action, target string, id any, err error) {
	if s.cfg.Audit == nil {
		return
	}
	op := operatorOf(r)
	e := audit.Event{TenantID: op.TenantID, ActorKind: audit.ActorOperator, ActorID: op.UserID, Action: action, TargetType: target,
		Outcome: audit.Success, RequestID: requestID(r)}
	switch v := id.(type) {
	case int64:
		if v > 0 {
			e.TargetID = strconv.FormatInt(v, 10)
		}
	case string:
		e.TargetID = v
	}
	if err != nil {
		e.Outcome = audit.Failure
	}
	if err := s.cfg.Audit.Record(e); err != nil {
		s.log.Warn("audit event not recorded", "action", action)
	}
}
