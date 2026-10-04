package app

import (
	"context"
	"time"

	"github.com/go-tangra/go-tangra-sms-gw/v4/pkg/smsgwmanifest"
)

// Auth registration cadence: retry quickly until auth accepts, then refresh
// so tenants created later receive the roles and grants.
const (
	registerRetry  = 5 * time.Second
	registerPeriod = 5 * time.Minute
)

// SeedPermissions registers the module's permissions, roles and built-in
// grants with auth (auth.v1.Authorization/RegisterPermissions; idempotent).
func (a *App) SeedPermissions(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	conn, err := a.Freya.Client(ctx, a.Cfg.Gateway.AuthService)
	if err != nil {
		return err
	}
	_, err = smsgwmanifest.Registration().Register(ctx, conn, a.Log)
	return err
}

// seedLoop registers once the runtime is ready (retrying until auth
// accepts) and then every five minutes.
func (a *App) seedLoop(ctx context.Context) {
	if !a.awaitReady(ctx) {
		return
	}
	for {
		delay := registerPeriod
		if err := a.SeedPermissions(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			a.Log.Warn("auth registration unavailable; retrying", "err", err)
			delay = registerRetry
		}
		if !pause(ctx, delay) {
			return
		}
	}
}

// awaitReady waits until the service is ready (identity and database).
func (a *App) awaitReady(ctx context.Context) bool {
	for {
		if _, ok := a.Ready(ctx); ok {
			return true
		}
		if !pause(ctx, time.Second) {
			return false
		}
	}
}
