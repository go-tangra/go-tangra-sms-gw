package app

import (
	"time"

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

// buildPublic wires the Hermes domain services and binds the public
// listener (plain HTTP; static TLS and ACME are attached by T048).
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
		Login: ratelimit.New(c.RateLimits.LoginPerMinute, c.RateLimits.LoginBurst, 50000),
		Send:  ratelimit.New(c.RateLimits.SendPerMinute, c.RateLimits.SendBurst, 10000)})
	if c.Public.TLSEnabled() || c.ACME.Enabled {
		a.Log.Warn("public HTTPS is configured but not served yet; only the plain public listener is active")
	}
	a.publicAddr, err = a.AddServer("public", c.Public.HTTPAddr, a.Public, nil)
	return err
}

// PublicAddr is the bound public Hermes listener address.
func (a *App) PublicAddr() string { return a.publicAddr.String() }
