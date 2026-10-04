package app

import (
	"context"
	"time"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/housekeeper"
)

// buildWorkers attaches the maintenance workers. Like every worker they run
// on the lifecycle context: shutdown cancels them (a retention batch in
// flight rolls back) and Run waits until they have returned.
func (a *App) buildWorkers() {
	a.Go(a.purgeRevocations)
	a.Housekeeper = housekeeper.New(housekeeper.Config{Store: a.Repo, Interval: a.Cfg.Retention.Interval, Metrics: a.Metrics,
		Log: a.Log.With("component", "housekeeper")})
	a.Go(a.Housekeeper.Run)
}

// purgeRevocations drops expired Hermes token revocations hourly.
func (a *App) purgeRevocations(ctx context.Context) {
	for {
		if _, err := a.Repo.PurgeRevocations(ctx, time.Now()); err != nil && ctx.Err() == nil {
			a.Log.Warn("revocation purge failed; retrying next hour")
		}
		if !pause(ctx, time.Hour) {
			return
		}
	}
}
