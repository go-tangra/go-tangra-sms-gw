package app

import (
	"context"
	"crypto/tls"
	"net/http"
	"time"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/acme"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/auth"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/provider"
	_ "github.com/go-tangra/go-tangra-sms-gw/v4/internal/provider/voicecom" // registers the carrier type
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/publicapi"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/ratelimit"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/sms"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/trustedproxy"
)

// carrierTimeout keeps one carrier exchange within the public write timeout.
const carrierTimeout = 25 * time.Second

// warmDelay lets the listeners bind before certificates are prefetched.
const warmDelay = 3 * time.Second

// Public HTTPS states reported by readiness (never part of its verdict).
const (
	TLSDisabled    = "disabled"
	TLSStatic      = "static"
	TLSACME        = "acme"
	TLSUnavailable = "unavailable"
)

// buildPublic wires the Hermes domain services and binds the public
// listeners: plain HTTP always, HTTPS with a static keypair or ACME when
// configured (see publicTLS).
func (a *App) buildPublic() error {
	c := a.Cfg
	iss, err := auth.NewIssuer(a.JWTSecret.Reveal(), c.PublicAuth.AccessTTL(), c.PublicAuth.RefreshTTL())
	if err != nil {
		return err
	}
	proxy, err := trustedproxy.New(c.Public.TrustedProxies)
	if err != nil {
		return err
	}
	a.Senders = provider.NewCache()
	a.Auth = auth.NewService(a.Repo, iss, a.Audit, a.Metrics, a.Log.With("component", "hermes-auth"))
	a.SMS = sms.New(sms.Config{Store: a.Repo, Envelope: a.Envelope, Senders: a.Senders, Metrics: a.Metrics, Audit: a.Audit,
		Log: a.Log.With("component", "sms"), CarrierTimeout: carrierTimeout,
		Policy: sms.RecipientPolicy{MinDigits: c.Recipients.MinDigits, Allowed: c.Recipients.AllowedPrefixes, Blocked: c.Recipients.BlockedPrefixes}})
	a.Public = publicapi.New(publicapi.Config{Auth: a.Auth, SMS: a.SMS, Proxy: proxy, MaxBody: c.Public.MaxBodyBytes, Log: a.Log.With("component", "public"),
		Receipts: a.buildReceipts(proxy),
		Login:    ratelimit.New(c.RateLimits.LoginPerMinute, c.RateLimits.LoginBurst, 50000),
		Send:     ratelimit.New(c.RateLimits.SendPerMinute, c.RateLimits.SendBurst, 10000)})
	tlsCfg, challenge := a.publicTLS()
	if a.publicAddr, err = a.AddServer("public", c.Public.HTTPAddr, publicapi.WithChallenge(a.Public, challenge), nil); err != nil {
		return err
	}
	if tlsCfg != nil {
		if a.publicHTTPSAddr, err = a.AddServer("public-https", c.Public.HTTPSAddr, a.Public, tlsCfg); err != nil {
			a.tlsFailed("public https listener unavailable; plain HTTP still serving", err)
		}
	}
	return nil
}

// publicTLS resolves the public certificate source. Its keys are never the
// mesh identity. Every failure is logged and reported by readiness while
// the plain public listener, the mesh and the admin listener keep serving.
func (a *App) publicTLS() (*tls.Config, http.Handler) {
	c := a.Cfg
	a.publicTLSState = TLSDisabled
	switch {
	case c.ACME.Enabled:
		m, err := acme.New(c.ACME, a.Log.With("component", "acme"))
		if err != nil {
			a.tlsFailed("acme configured but unusable; public HTTPS disabled, plain HTTP still serving", err)
			return nil, nil
		}
		a.PublicCerts, a.publicTLSState = m, TLSACME
		if c.ACME.HTTPAddr != "" {
			if _, err := a.AddServer("acme-http", c.ACME.HTTPAddr, m.RedirectHandler(), nil); err != nil {
				a.Log.Warn("dedicated acme challenge listener disabled; challenges are still answered on the public listener", "err", err)
			}
		}
		if c.ACME.Prefetch {
			a.Go(func(ctx context.Context) {
				if pause(ctx, warmDelay) {
					m.Warm(ctx)
				}
			})
		}
		return m.TLSConfig(), m.ChallengeHandler()
	case c.Public.TLSEnabled():
		cfg, err := publicapi.LoadStaticTLS(c.Public.TLSCertFile, c.Public.TLSKeyFile)
		if err != nil {
			a.tlsFailed("public certificate unusable; public HTTPS disabled, plain HTTP still serving", err)
			return nil, nil
		}
		a.publicTLSState = TLSStatic
		return cfg, nil
	}
	return nil, nil
}

func (a *App) tlsFailed(msg string, err error) {
	a.publicTLSState = TLSUnavailable
	a.Log.Warn(msg, "err", err)
}

// PublicHTTPSAddr is the bound public HTTPS listener ("" when not serving).
func (a *App) PublicHTTPSAddr() string {
	if a.publicHTTPSAddr == nil {
		return ""
	}
	return a.publicHTTPSAddr.String()
}

// PublicAddr is the bound public Hermes listener address.
func (a *App) PublicAddr() string { return a.publicAddr.String() }
