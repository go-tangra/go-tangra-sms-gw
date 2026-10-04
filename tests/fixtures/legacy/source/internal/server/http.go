package server

import (
	"crypto/tls"
	"io/fs"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/go-kratos/kratos/v2/middleware"
	"github.com/go-kratos/kratos/v2/middleware/logging"
	"github.com/go-kratos/kratos/v2/middleware/recovery"
	"github.com/go-kratos/kratos/v2/middleware/selector"
	"github.com/go-kratos/kratos/v2/transport"
	kratosHttp "github.com/go-kratos/kratos/v2/transport/http"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/tx7do/kratos-bootstrap/bootstrap"

	"github.com/go-tangra/go-tangra-sms-gw/cmd/server/assets"
	smsgwpb "github.com/go-tangra/go-tangra-sms-gw/gen/go/sms_gw/service/v1"
	"github.com/go-tangra/go-tangra-sms-gw/internal/acme"
	"github.com/go-tangra/go-tangra-sms-gw/internal/auth"
	"github.com/go-tangra/go-tangra-sms-gw/internal/ratelimit"
	"github.com/go-tangra/go-tangra-sms-gw/internal/service"
)

// PublicHTTPServers groups the public-facing HTTP listeners.
//
// Plain is always present and hosts the bound address from
// HTTP_PUBLIC_BIND (default `:9901`). The admin gateway reaches this
// listener over plain HTTP to reverse-proxy module assets, so this side
// must stay unencrypted regardless of TLS configuration.
//
// TLS is non-nil when the public certificate can be sourced, either
// from a static keypair (SMS_GW_TLS_CERT_PATH + SMS_GW_TLS_KEY_PATH) or
// from an ACME CA (SMS_GW_ACME_ENABLED). It binds SMS_GW_TLS_BIND
// (default `:9443`) and serves the same route set as Plain — it exists
// so external carriers / DLR callers can hit the gateway over HTTPS
// while the internal admin-gateway path keeps using HTTP.
//
// ACMEChallenge is the optional dedicated port-80 http-01 responder
// (SMS_GW_ACME_HTTP_BIND); ACMEWarmup prefetches certificates once the
// app is up. Both are nil unless ACME is enabled.
type PublicHTTPServers struct {
	Plain *kratosHttp.Server
	TLS   *kratosHttp.Server

	ACMEChallenge *acme.ChallengeServer
	ACMEWarmup    *acme.WarmupServer
}

// Servers returns the configured listeners in a shape suitable for
// passing to bootstrap.NewApp.
func (s *PublicHTTPServers) Servers() []transport.Server {
	out := []transport.Server{s.Plain}
	if s.TLS != nil {
		out = append(out, s.TLS)
	}
	// Typed nils would satisfy transport.Server while panicking on the
	// first method call, so check the concrete pointers.
	if s.ACMEChallenge != nil {
		out = append(out, s.ACMEChallenge)
	}
	if s.ACMEWarmup != nil {
		out = append(out, s.ACMEWarmup)
	}
	return out
}

// NewHTTPServer wires the public listener(s). The HTTP listener is
// mandatory; the HTTPS listener is opt-in via either a static keypair
// (SMS_GW_TLS_CERT_PATH / SMS_GW_TLS_KEY_PATH) or ACME
// (SMS_GW_ACME_ENABLED) and gets the SMS_GW_TLS_BIND port (default
// :9443). Both listeners share rate-limiter state — without that, an
// attacker could double their per-IP budget by hitting both ports.
func NewHTTPServer(
	ctx *bootstrap.Context,
	smsSvc *service.SmsService,
	authSvc *service.AuthenticationService,
	iss *auth.Issuer,
) *PublicHTTPServers {
	l := ctx.NewLoggerHelper("sms-gw/http")

	plainAddr := os.Getenv("HTTP_PUBLIC_BIND")
	if plainAddr == "" {
		plainAddr = "0.0.0.0:9901"
	}

	// Logout is whitelisted so a client whose access token has expired
	// can still revoke its still-valid refresh token. The handler
	// itself parses + validates whatever tokens are supplied (Bearer +
	// X-Refresh-Token headers) and silently no-ops on missing/invalid
	// — there's no security cost to accepting unauthenticated calls.
	// The DLR endpoints are also registered via raw r.GET handlers
	// (they need a custom 200 "DLR_OK" response that the proto-
	// generated handler won't produce). Those raw routes don't carry
	// a proto operation name, so the operation-only whitelist misses
	// them and they end up returning 401. Whitelisting by path covers
	// both the proto-registered route and the raw override.
	whitelist := auth.NewWhitelistWithPaths(
		[]string{
			smsgwpb.OperationAuthenticationServiceLogin,
			smsgwpb.OperationAuthenticationServiceRefreshToken,
			smsgwpb.OperationAuthenticationServiceLogout,
			smsgwpb.OperationSmsServiceDlr,
		},
		[]string{
			"/dlr",
			"/hermes/v1/sms/dlr",
		},
	)

	// Login: per-IP, defends against credential stuffing. 10 req/min
	// burst 5 means an attacker can try 5 passwords immediately, then
	// is throttled to 1 per 6 seconds — fast enough for legitimate
	// retypes, slow enough to make brute-force impractical. Operators
	// may tune via SMS_GW_LOGIN_RPM / SMS_GW_LOGIN_BURST (e2e tests
	// raise these so the suite can run without 429 stalls).
	loginLimiter := ratelimit.New(ratelimit.Config{
		RequestsPerSecond: envFloat("SMS_GW_LOGIN_RPM", 10) / 60.0,
		Burst:             envInt("SMS_GW_LOGIN_BURST", 5),
		MaxKeys:           50000,
	})
	loginGate := selector.Server(loginLimiter.Middleware(ratelimit.IPKey)).
		Match(ratelimit.MatchOps(smsgwpb.OperationAuthenticationServiceLogin)).
		Build()

	// Send: per-client, defends against billing-DoS from a single
	// compromised credential. 100 req/min sustained, burst 20 is the
	// default — operators should override per-customer when SLAs differ.
	sendLimiter := ratelimit.New(ratelimit.Config{
		RequestsPerSecond: envFloat("SMS_GW_SEND_RPM", 100) / 60.0,
		Burst:             envInt("SMS_GW_SEND_BURST", 20),
		MaxKeys:           10000,
	})
	sendGate := selector.Server(sendLimiter.Middleware(ratelimit.ClientIDKey)).
		Match(ratelimit.MatchOps(smsgwpb.OperationSmsServiceSend)).
		Build()

	// DLR: per-source-IP, defends against spoof floods. 500/min burst
	// 100 — DLRs from a real provider come in well under this.
	dlrLimiter := ratelimit.New(ratelimit.Config{
		RequestsPerSecond: envFloat("SMS_GW_DLR_RPM", 500) / 60.0,
		Burst:             envInt("SMS_GW_DLR_BURST", 100),
		MaxKeys:           50000,
	})
	dlrGate := selector.Server(dlrLimiter.Middleware(ratelimit.IPKey)).
		Match(ratelimit.MatchOps(smsgwpb.OperationSmsServiceDlr)).
		Build()

	authGate := selector.Server(authMiddleware(iss)).Match(whitelist).Build()

	mw := kratosHttp.Middleware(
		recovery.Recovery(),
		logging.Server(ctx.GetLogger()),
		protoValidator(),
		// Order: rate limits BEFORE auth so credential-stuffing floods
		// don't burn a bcrypt verify per attempt. Per-client send limit
		// comes AFTER auth (handled inside sendGate itself: ClientIDKey
		// falls back to IP when Identity is not set yet).
		loginGate,
		dlrGate,
		authGate,
		sendGate,
	)

	acmeLog := ctx.NewLoggerHelper("sms-gw/acme")
	src := resolveTLSSource(l, acmeLog, osGetenv)

	// The http-01 responder rides on the plain listener so the common
	// deployment — nginx/ingress mapping public :80 onto HTTP_PUBLIC_BIND
	// — needs no extra port. It is mounted ahead of the catch-all
	// frontend route inside buildPublicServer.
	var challengeHandler http.Handler
	if src != nil && src.ACME != nil {
		challengeHandler = src.ACME.ChallengeHandler()
	}

	plain := buildPublicServer(l, plainAddr, nil, mw, smsSvc, authSvc, challengeHandler)
	l.Infof("Public HTTP server listening on %s", plainAddr)

	servers := &PublicHTTPServers{Plain: plain}

	if src != nil {
		tlsAddr := os.Getenv("SMS_GW_TLS_BIND")
		if tlsAddr == "" {
			tlsAddr = "0.0.0.0:9443"
		}
		servers.TLS = buildPublicServer(l, tlsAddr, src.Config, mw, smsSvc, authSvc, nil)
		l.Infof("Public HTTPS server listening on %s (certificate source: %s)", tlsAddr, src.Kind)
	}

	if src != nil && src.ACME != nil {
		attachACMEServers(l, acmeLog, servers, src.ACME)
	}

	return servers
}

// attachACMEServers adds the optional dedicated http-01 listener and the
// boot-time certificate prefetch to the server set.
//
// A failure to bind the dedicated port is a warning, not a boot failure:
// the challenge handler on the plain listener is still there, so a
// deployment that fronts this process with a proxy remains fully
// functional.
func attachACMEServers(l, acmeLog *log.Helper, servers *PublicHTTPServers, mgr *acme.Manager) {
	cfg := mgr.Config()

	if cfg.HTTPBind != "" {
		cs, err := acme.NewChallengeServer(cfg.HTTPBind, mgr.RedirectHandler(), acmeLog)
		if err != nil {
			l.Warnf(
				"Dedicated ACME http-01 listener disabled (%v) — challenges are still answered on %s of the public HTTP listener",
				err, acme.ChallengePath,
			)
		} else {
			servers.ACMEChallenge = cs
		}
	}

	if cfg.Prefetch {
		servers.ACMEWarmup = acme.NewWarmupServer(mgr, acmeLog)
	}
}

// buildPublicServer constructs one Kratos HTTP server (TLS optional) and
// mounts the full public route set on it: DLR overrides, proto-generated
// SMS + Authentication handlers, health, Prometheus, embedded frontend.
func buildPublicServer(
	l *log.Helper,
	addr string,
	tlsCfg *tls.Config,
	mw kratosHttp.ServerOption,
	smsSvc *service.SmsService,
	authSvc *service.AuthenticationService,
	acmeChallenge http.Handler,
) *kratosHttp.Server {
	opts := []kratosHttp.ServerOption{
		kratosHttp.Address(addr),
		kratosHttp.Timeout(30 * time.Second),
		mw,
	}
	if tlsCfg != nil {
		opts = append(opts, kratosHttp.TLSConfig(tlsCfg))
	}
	srv := kratosHttp.NewServer(opts...)

	// Custom DLR handlers FIRST so they win route matching against the
	// proto-generated /hermes/v1/sms/{id} template. Kratos's HTTP router
	// (gorilla/mux underneath) matches in registration order, so if we
	// register the proto routes first, /hermes/v1/sms/dlr gets eaten by
	// SmsService.Get with id="dlr" and returns 404 RECORD_NOT_FOUND for
	// every carrier callback.
	//
	// /dlr is the legacy hermes path that customers have configured into
	// their Voicecom (and successor) provider callbacks.
	// /hermes/v1/sms/dlr is the same handler under the proto path so we
	// always reply with "DLR_OK" instead of an empty JSON.
	dlrH := DlrHandler(smsSvc, l)
	r := srv.Route("/")
	r.GET("/dlr", dlrH)
	r.GET("/hermes/v1/sms/dlr", dlrH)

	// ACME http-01. Registered before the proto routes and the catch-all
	// frontend handler so nothing else claims the prefix, and via
	// HandlePrefix so it bypasses the auth middleware — the CA sends no
	// credentials, and a 401 here means no certificate.
	if acmeChallenge != nil {
		srv.HandlePrefix(acme.ChallengePath, acmeChallenge)
		l.Infof("ACME http-01 challenge route mounted at %s on %s", acme.ChallengePath, addr)
	}

	smsgwpb.RegisterSmsServiceHTTPServer(srv, smsSvc)
	smsgwpb.RegisterAuthenticationServiceHTTPServer(srv, authSvc)

	r.GET("/health", func(c kratosHttp.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
	})

	// Prometheus metrics. MUST stay unauthenticated (Prometheus scrapes
	// don't carry user credentials). Restrict at the network layer if
	// needed. HandlePrefix bypasses the auth middleware entirely so we
	// don't have to whitelist a non-RPC operation name.
	srv.HandlePrefix("/metrics", promhttp.Handler())

	if fsys, err := fs.Sub(assets.FrontendDist, "frontend-dist"); err == nil {
		srv.HandlePrefix("/", http.FileServer(http.FS(fsys)))
	} else {
		l.Warnf("Failed to load embedded frontend assets: %v", err)
	}

	return srv
}

// authMiddleware materializes auth.Server in middleware shape. Inlined
// rather than calling auth.Server directly so we can keep the selector
// match function adjacent to the operation list above for readability.
func authMiddleware(iss *auth.Issuer) middleware.Middleware {
	return auth.Server(iss)
}

// envFloat parses a positive float env var, returning fallback when
// unset / unparseable / non-positive. Used to make the rate-limiter
// sizing operator-tunable without rebuilding the binary.
func envFloat(key string, fallback float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f <= 0 {
		return fallback
	}
	return f
}

func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}
