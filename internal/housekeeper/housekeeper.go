// Package housekeeper deletes messages (with their receipts) that have
// outlived their provider's retention window. Each tenant's provider is
// swept separately in bounded batches; retention_days 0 keeps messages
// forever. A pass never overlaps the next tick, one failing provider does
// not stop the others, and cancellation stops the pass between batches (the
// batch in flight rolls back).
package housekeeper

import (
	"context"
	"log/slog"
	"time"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/metrics"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/repo"
)

// Store is the retention persistence.
type Store interface {
	RetentionPolicies(ctx context.Context) ([]repo.RetentionPolicy, error)
	DeleteExpired(ctx context.Context, tenant string, providerID int64, cutoff time.Time, limit int) (int64, error)
}

// Config wires the worker.
type Config struct {
	Store    Store
	Interval time.Duration // between passes (retention.interval)
	Batch    int           // messages per delete transaction (default 1000)
	Metrics  *metrics.Metrics
	Log      *slog.Logger
	Now      func() time.Time
}

// Housekeeper is the retention worker.
type Housekeeper struct {
	c     Config
	delay time.Duration // before the first pass
}

// passTimeout bounds one pass.
const passTimeout = 10 * time.Minute

// New returns the worker; the first pass runs 30 s after Run starts.
func New(c Config) *Housekeeper {
	if c.Batch < 1 {
		c.Batch = 1000
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
	return &Housekeeper{c: c, delay: 30 * time.Second}
}

// Run sweeps shortly after start and then every interval until ctx ends.
func (h *Housekeeper) Run(ctx context.Context) {
	h.c.Log.Info("housekeeper started", "interval", h.c.Interval.String())
	wait := h.delay
	for {
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		h.Sweep(ctx)
		wait = h.c.Interval
	}
}

// Sweep runs one pass and returns the messages deleted and its outcome
// (ok, partial or error).
func (h *Housekeeper) Sweep(ctx context.Context) (int64, string) {
	ctx, cancel := context.WithTimeout(ctx, passTimeout)
	defer cancel()
	policies, err := h.c.Store.RetentionPolicies(ctx)
	if err != nil {
		h.c.Log.Error("housekeeper: retention policies unavailable", "err", err)
		h.c.Metrics.Housekeeping("error", nil)
		return 0, "error"
	}
	now := h.c.Now()
	deleted := map[string]int64{}
	var total int64
	swept, failed := 0, 0
	for _, p := range policies {
		if p.Days <= 0 {
			continue
		}
		swept++
		cutoff := now.AddDate(0, 0, -p.Days)
		n, err := h.purge(ctx, p, cutoff)
		total += n
		deleted[p.Type] += n
		if err != nil {
			failed++
			if ctx.Err() == nil {
				h.c.Log.Error("housekeeper: retention delete failed", "tenant", p.TenantID, "provider", p.ProviderID, "err", err)
			}
			continue
		}
		if n > 0 {
			h.c.Log.Info("housekeeper: expired messages deleted", "tenant", p.TenantID, "provider", p.ProviderID,
				"cutoff", cutoff.Format(time.RFC3339), "deleted", n)
		}
	}
	outcome := "ok"
	switch {
	case failed > 0 && failed == swept:
		outcome = "error"
	case failed > 0:
		outcome = "partial"
	}
	h.c.Metrics.Housekeeping(outcome, deleted)
	return total, outcome
}

func (h *Housekeeper) purge(ctx context.Context, p repo.RetentionPolicy, cutoff time.Time) (int64, error) {
	var total int64
	for {
		n, err := h.c.Store.DeleteExpired(ctx, p.TenantID, p.ProviderID, cutoff, h.c.Batch)
		total += n
		if err != nil || n < int64(h.c.Batch) {
			return total, err
		}
		if ctx.Err() != nil {
			return total, ctx.Err()
		}
	}
}
