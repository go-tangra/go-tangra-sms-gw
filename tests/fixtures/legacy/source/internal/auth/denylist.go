package auth

import (
	"sync"
	"time"
)

// defaultMaxEntries caps the in-memory denylist. Without a cap an
// authenticated attacker can flood Logout to grow the map unboundedly
// (each entry lives until its natural JWT expiry — up to 7 days for a
// refresh token). At ~80 bytes per entry, 100 000 entries ≈ 8 MB of
// resident memory — a comfortable ceiling.
const defaultMaxEntries = 100_000

// Denylist holds revoked JWT IDs (jti) until their natural expiry time
// elapses. After expiry the entry is purged so the map size stays
// proportional to the number of currently-valid revocations.
//
// Single-process scope. Multi-replica deployments need a shared store
// (Redis SET with TTL); this is documented in CLAUDE.md as a v1
// limitation. The interface is intentionally narrow so swapping the
// backend later requires touching only this file.
type Denylist struct {
	mu      sync.RWMutex
	entries map[string]time.Time

	// maxEntries bounds the map. When exceeded, Revoke evicts the entry
	// with the earliest expiry (i.e. the soonest to be purged anyway).
	// Set in NewDenylist; mutated never thereafter.
	maxEntries int

	// Sweep cadence — purges expired entries to keep the map bounded.
	// Set in NewDenylist, read by the background goroutine.
	sweep    time.Duration
	stopOnce sync.Once
	stop     chan struct{}
}

// NewDenylist starts a background sweeper that purges expired JTIs
// every sweep interval. The returned cleanup func stops the sweeper
// (call it from a process-shutdown hook to release the goroutine).
func NewDenylist() (*Denylist, func()) {
	d := &Denylist{
		entries:    make(map[string]time.Time),
		maxEntries: defaultMaxEntries,
		sweep:      60 * time.Second,
		stop:       make(chan struct{}),
	}
	go d.run()
	return d, d.shutdown
}

// Revoke marks the JTI as revoked until expiresAt. Revoking a JTI more
// than once with a later expiry extends the entry; an earlier expiry
// is ignored (you can't shorten a revocation). When the map is at
// capacity an existing entry with an earlier expiry is evicted to make
// room — DoS via Logout-flooding cannot grow the map past maxEntries.
func (d *Denylist) Revoke(jti string, expiresAt time.Time) {
	if jti == "" || expiresAt.IsZero() {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if existing, ok := d.entries[jti]; ok {
		if expiresAt.After(existing) {
			d.entries[jti] = expiresAt
		}
		return
	}
	if len(d.entries) >= d.maxEntries {
		d.evictEarliestLocked()
	}
	d.entries[jti] = expiresAt
}

// evictEarliestLocked removes the entry with the soonest expiry. The
// caller must hold d.mu. O(N) over the map — acceptable because eviction
// is bounded by the flood rate, not by request volume; if we hit the cap
// we are already under attack and the audit log will reflect that.
func (d *Denylist) evictEarliestLocked() {
	var (
		oldestKey string
		oldestExp time.Time
	)
	for k, exp := range d.entries {
		// time.Time{} is the zero value (Jan 1 year 1 UTC), which is
		// always before any real expiry — the IsZero check is the
		// idiomatic "first iteration" sentinel.
		if oldestExp.IsZero() || exp.Before(oldestExp) {
			oldestKey = k
			oldestExp = exp
		}
	}
	if oldestKey != "" {
		delete(d.entries, oldestKey)
	}
}

// IsRevoked returns true when the JTI is on the denylist AND has not
// yet passed its expiry. Returns false for unknown or expired entries
// — the caller should still validate the JWT exp claim independently.
func (d *Denylist) IsRevoked(jti string) bool {
	if jti == "" {
		return false
	}
	d.mu.RLock()
	exp, ok := d.entries[jti]
	d.mu.RUnlock()
	if !ok {
		return false
	}
	return time.Now().Before(exp)
}

func (d *Denylist) run() {
	t := time.NewTicker(d.sweep)
	defer t.Stop()
	for {
		select {
		case <-d.stop:
			return
		case <-t.C:
			d.purgeExpired()
		}
	}
}

// purgeExpired collects expired keys under a read lock so the hot
// IsRevoked path is not blocked for the entire scan, then deletes
// under a brief write lock.
func (d *Denylist) purgeExpired() {
	now := time.Now()
	d.mu.RLock()
	expired := make([]string, 0, 32)
	for k, exp := range d.entries {
		if now.After(exp) {
			expired = append(expired, k)
		}
	}
	d.mu.RUnlock()
	if len(expired) == 0 {
		return
	}
	d.mu.Lock()
	for _, k := range expired {
		// Re-check the expiry — Revoke may have extended the entry
		// between our scan and the write lock.
		if exp, ok := d.entries[k]; ok && now.After(exp) {
			delete(d.entries, k)
		}
	}
	d.mu.Unlock()
}

func (d *Denylist) shutdown() { d.stopOnce.Do(func() { close(d.stop) }) }
