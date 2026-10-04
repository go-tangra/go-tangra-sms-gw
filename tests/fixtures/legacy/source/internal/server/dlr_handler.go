package server

import (
	"context"

	"github.com/go-kratos/kratos/v2/log"
	kratosHttp "github.com/go-kratos/kratos/v2/transport/http"

	smsgwpb "github.com/go-tangra/go-tangra-sms-gw/gen/go/sms_gw/service/v1"
	"github.com/go-tangra/go-tangra-sms-gw/internal/ctxkeys"
	"github.com/go-tangra/go-tangra-sms-gw/internal/service"
	"github.com/go-tangra/go-tangra-sms-gw/internal/trustedproxy"
)

// DlrHandler returns a Kratos HTTP handler that mirrors havi/hermes's
// DLR webhook: GET /dlr binds query parameters into DlrRequest, calls
// SmsService.Dlr, and ALWAYS returns HTTP 200 with body "DLR_OK"
// regardless of internal errors. Third-party providers retry on any
// non-2xx response, so swallowing failures here prevents endless
// duplicate DLRs from masking bugs.
//
// The handler also threads two values through context for the service:
//   - ctxkeys.ClientIP : extracted source IP for sms_dlr.remote_address
//   - ctxkeys.DlrToken : ?dlr_token= query value, validated downstream
//     against the provider's stored secret to defeat UUID-guessing
//     spoof attempts on this unauthenticated endpoint.
//
// The IP is resolved through trustedproxy so X-Real-IP / X-Forwarded-For
// are honored only when the request came from a configured trusted
// proxy CIDR.
func DlrHandler(svc *service.SmsService, l *log.Helper) func(kratosHttp.Context) error {
	return func(ctx kratosHttp.Context) error {
		var in smsgwpb.DlrRequest
		if err := ctx.BindQuery(&in); err != nil {
			l.Warnf("dlr bind query: %s", err.Error())
			return ctx.Result(200, "DLR_OK")
		}
		if err := ctx.BindVars(&in); err != nil {
			l.Warnf("dlr bind vars: %s", err.Error())
			return ctx.Result(200, "DLR_OK")
		}

		ip := trustedproxy.ResolveIP(ctx)
		dlrToken := ctx.Query().Get("dlr_token")
		newCtx := context.WithValue(ctx.Request().Context(), ctxkeys.ClientIP{}, ip)
		newCtx = context.WithValue(newCtx, ctxkeys.DlrToken{}, dlrToken)
		req := ctx.Request().Clone(newCtx)
		ctx.Reset(ctx.Response(), req)

		if _, err := svc.Dlr(ctx, &in); err != nil {
			l.Warnf("dlr handler: %s", err.Error())
		}
		return ctx.Result(200, "DLR_OK")
	}
}
