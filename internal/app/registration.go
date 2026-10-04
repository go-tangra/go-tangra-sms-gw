package app

import (
	"context"
	"time"

	"github.com/go-tangra/go-tangra-portal/sdk/v4/pkg/gatewayclient"

	"github.com/go-tangra/go-tangra-sms-gw/v4/pkg/smsgwmanifest"
)

// register keeps the gateway lease for the manifest while the service is
// ready, advertising the actual mesh HTTP endpoint. Losing readiness ends
// the lease (graceful deregistration) until the service recovers; the
// lifecycle context ends it on shutdown.
func (a *App) register(ctx context.Context) {
	man, err := smsgwmanifest.Manifest()
	if err != nil {
		a.Log.Error("gateway manifest invalid; not registering", "err", err)
		return
	}
	for a.awaitReady(ctx) {
		ep, err := a.Freya.HTTP().Endpoint()
		if err != nil {
			a.Log.Warn("gateway registration: no http endpoint yet")
			if !pause(ctx, 2*time.Second) {
				return
			}
			continue
		}
		attempt, cancel := context.WithTimeout(ctx, 5*time.Second)
		conn, err := a.Freya.Client(attempt, a.Cfg.Gateway.Service)
		cancel()
		if err != nil {
			a.Log.Warn("gateway connection unavailable; retrying", "err", err)
			if !pause(ctx, 2*time.Second) {
				return
			}
			continue
		}
		client, err := gatewayclient.New(conn, gatewayclient.Options{Manifest: man, HTTPURL: "https://" + ep.Host, Logger: a.Log,
			OnState: func(s gatewayclient.State) {
				a.Log.Info("gateway lease", "registered", s.Registered, "lease", s.LeaseID, "err", s.Err)
			}})
		if err != nil {
			a.Log.Error("gateway client", "err", err)
			return
		}
		lease, stop := context.WithCancel(ctx)
		go func() {
			defer stop()
			for pause(lease, 3*time.Second) {
				if _, ok := a.Ready(lease); !ok {
					a.Log.Warn("not ready; releasing the gateway lease")
					return
				}
			}
		}()
		if err := client.Run(lease); err != nil && ctx.Err() == nil {
			a.Log.Warn("gateway registration ended", "err", err)
		}
		stop()
		if !pause(ctx, 2*time.Second) {
			return
		}
	}
}
