// Package hermes holds what the public Hermes contract shares between its
// domain services and the wire adapter: the legacy error envelope values
// (HTTP code, reason, message) and the verified caller.
package hermes

import (
	"errors"
	"fmt"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/repo"
)

// Legacy reasons (sms_gw_error.proto, Kratos codec and unknown errors).
const (
	ReasonBadRequest      = "BAD_REQUEST"
	ReasonUnauthorized    = "UNAUTHORIZED"
	ReasonForbidden       = "FORBIDDEN"
	ReasonNotFound        = "RECORD_NOT_FOUND"
	ReasonTooManyRequests = "TOO_MANY_REQUESTS"
	ReasonInternal        = "INTERNAL_ERROR"
	ReasonUnavailable     = "DB_UNAVAILABLE"
	ReasonCodec           = "CODEC"
	ReasonUnknown         = ""
)

// Error is a public error envelope.
type Error struct {
	Code    int
	Reason  string
	Message string
}

func (e *Error) Error() string { return e.Message }

func newf(code int, reason, format string, args ...any) *Error {
	return &Error{Code: code, Reason: reason, Message: fmt.Sprintf(format, args...)}
}

// BadRequest is 400 BAD_REQUEST.
func BadRequest(format string, args ...any) *Error {
	return newf(400, ReasonBadRequest, format, args...)
}

// Unauthorized is 401 UNAUTHORIZED.
func Unauthorized(format string, args ...any) *Error {
	return newf(401, ReasonUnauthorized, format, args...)
}

// Forbidden is 403 FORBIDDEN.
func Forbidden(format string, args ...any) *Error { return newf(403, ReasonForbidden, format, args...) }

// NotFound is 404 RECORD_NOT_FOUND.
func NotFound(format string, args ...any) *Error { return newf(404, ReasonNotFound, format, args...) }

// TooManyRequests is 429 TOO_MANY_REQUESTS.
func TooManyRequests(format string, args ...any) *Error {
	return newf(429, ReasonTooManyRequests, format, args...)
}

// Internal is 500 INTERNAL_ERROR.
func Internal(format string, args ...any) *Error { return newf(500, ReasonInternal, format, args...) }

// Unavailable is 503 DB_UNAVAILABLE (storage failures; never their text).
func Unavailable(format string, args ...any) *Error {
	return newf(503, ReasonUnavailable, format, args...)
}

// Codec is 400 CODEC (request decoding).
func Codec(format string, args ...any) *Error { return newf(400, ReasonCodec, format, args...) }

// Carrier is the legacy unknown-error envelope a carrier failure produced:
// 500 with an empty reason. The message must already be sanitized.
func Carrier(message string) *Error {
	return &Error{Code: 500, Reason: ReasonUnknown, Message: message}
}

// From converts any error; one that is not an *Error becomes a storage
// failure without its text.
func From(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return Unavailable("storage unavailable")
}

// Authorities of Hermes clients.
const (
	AuthorityClient = "API_CLIENT"
	AuthorityViewer = "API_VIEWER"
)

// Caller is a verified Hermes client. Authority is the effective authority:
// the token claim when it agrees with the stored account, otherwise empty
// (unsupported, fails closed).
type Caller struct {
	Client    repo.APIClient
	Authority string
}

// CanSend reports a read/write client.
func (c Caller) CanSend() bool { return c.Authority == AuthorityClient }

// View is the caller's read scope; unsupported authorities (API_ADMIN or any
// other value) see nothing.
func (c Caller) View() (repo.View, bool) {
	switch c.Authority {
	case AuthorityClient, AuthorityViewer:
		return repo.ClientView(c.Client.TenantID, c.Client.ID), true
	}
	return repo.View{}, false
}
