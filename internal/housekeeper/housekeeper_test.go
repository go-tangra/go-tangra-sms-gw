package housekeeper

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/repo"
)

type call struct {
	tenant   string
	provider int64
	cutoff   time.Time
}

type fakeStore struct {
	mu       sync.Mutex
	policies []repo.RetentionPolicy
	pending  map[int64]int64 // provider -> expired messages left
	fail     map[int64]bool
	calls    []call
	listErr  error
	block    chan struct{}
}

func (f *fakeStore) RetentionPolicies(context.Context) ([]repo.RetentionPolicy, error) {
	return f.policies, f.listErr
}

func (f *fakeStore) DeleteExpired(ctx context.Context, tenant string, provider int64, cutoff time.Time, limit int) (int64, error) {
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{tenant, provider, cutoff})
	if f.fail[provider] {
		return 0, errors.New("boom")
	}
	n := min(f.pending[provider], int64(limit))
	f.pending[provider] -= n
	return n, nil
}

var now = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

func newHK(s Store) *Housekeeper {
	return New(Config{Store: s, Interval: time.Hour, Batch: 2, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return now }})
}

func TestSweepPerTenantProviderWithCutoffAndBatches(t *testing.T) {
	s := &fakeStore{policies: []repo.RetentionPolicy{
		{TenantID: "t-a", ProviderID: 1, Type: "voicecom", Days: 30},
		{TenantID: "t-b", ProviderID: 2, Type: "voicecom", Days: 1},
	}, pending: map[int64]int64{1: 5, 2: 0}}
	n, outcome := newHK(s).Sweep(context.Background())
	if n != 5 || outcome != "ok" {
		t.Fatalf("deleted %d outcome %s", n, outcome)
	}
	// provider 1: batches 2+2+1 then stops on the short batch; provider 2: one empty batch.
	if len(s.calls) != 4 {
		t.Fatalf("calls %+v", s.calls)
	}
	if s.calls[0].tenant != "t-a" || !s.calls[0].cutoff.Equal(now.AddDate(0, 0, -30)) || s.calls[3].tenant != "t-b" || !s.calls[3].cutoff.Equal(now.AddDate(0, 0, -1)) {
		t.Fatalf("cutoffs %+v", s.calls)
	}
}

func TestSweepZeroRetentionIsNeverSwept(t *testing.T) {
	s := &fakeStore{policies: []repo.RetentionPolicy{{TenantID: "t-a", ProviderID: 1, Days: 0}}, pending: map[int64]int64{1: 3}}
	if n, outcome := newHK(s).Sweep(context.Background()); n != 0 || outcome != "ok" || len(s.calls) != 0 {
		t.Fatalf("zero retention swept: %d %s %+v", n, outcome, s.calls)
	}
}

func TestSweepOneFailureDoesNotStopOthers(t *testing.T) {
	s := &fakeStore{policies: []repo.RetentionPolicy{{TenantID: "t-a", ProviderID: 1, Days: 1}, {TenantID: "t-a", ProviderID: 2, Days: 1}},
		pending: map[int64]int64{2: 1}, fail: map[int64]bool{1: true}}
	if n, outcome := newHK(s).Sweep(context.Background()); n != 1 || outcome != "partial" {
		t.Fatalf("%d %s", n, outcome)
	}
	s = &fakeStore{listErr: errors.New("down")}
	if _, outcome := newHK(s).Sweep(context.Background()); outcome != "error" {
		t.Fatal(outcome)
	}
}

func TestRunDrainsOnCancel(t *testing.T) {
	s := &fakeStore{policies: []repo.RetentionPolicy{{TenantID: "t-a", ProviderID: 1, Days: 1}}, pending: map[int64]int64{1: 1}, block: make(chan struct{})}
	h := newHK(s)
	h.delay = 0
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.Run(ctx); close(done) }()
	time.Sleep(50 * time.Millisecond) // the first sweep is now blocked in DeleteExpired
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}
