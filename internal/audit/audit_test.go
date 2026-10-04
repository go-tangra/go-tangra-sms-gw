package audit

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/repo"
)

const tenant = "0b2f6a1e-4c55-4c8e-9d1a-000000000a0a"

type sink struct {
	mu      sync.Mutex
	events  []repo.AuditEvent
	batches int
	block   chan struct{}
	err     error
}

func (s *sink) AddAudit(_ context.Context, events ...repo.AuditEvent) error {
	if s.block != nil {
		<-s.block
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, events...)
	s.batches++
	return s.err
}

func valid() Event {
	return Event{TenantID: tenant, ActorKind: ActorOperator, ActorID: "4f0c0a52-1111-4222-8333-944455556666", Action: ProviderUpdate,
		TargetType: "provider", TargetID: "3", Outcome: Success, RequestID: "req-1"}
}

func TestValidateClosedVocabulary(t *testing.T) {
	if err := valid().Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Event){
		"free action":        func(e *Event) { e.Action = "provider.update with body" },
		"unknown action":     func(e *Event) { e.Action = "provider.export" },
		"unknown outcome":    func(e *Event) { e.Outcome = "ok" },
		"unknown actor":      func(e *Event) { e.ActorKind = "admin" },
		"unknown target":     func(e *Event) { e.TargetType = "secret" },
		"content in target":  func(e *Event) { e.TargetID = "Hello Alice, code 8421." },
		"token as actor":     func(e *Event) { e.ActorID = "Bearer eyJhbGciOi.x.y" },
		"long request id":    func(e *Event) { e.RequestID = strings.Repeat("a", 129) },
		"json in request id": func(e *Event) { e.RequestID = `{"password":"x"}` },
	} {
		e := valid()
		mutate(&e)
		if err := e.Validate(); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestWriterBatchesAndDrains(t *testing.T) {
	s := &sink{}
	w := NewWriter(s, 100, nil)
	for i := 0; i < 50; i++ {
		if err := w.Record(valid()); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Record(Event{Action: "x"}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	w.Close()
	w.Close()
	if len(s.events) != 50 || s.events[0].EventTime.IsZero() || s.events[0].TargetID != "3" {
		t.Fatalf("written %d", len(s.events))
	}
	if err := w.Record(valid()); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}

func TestWriterDropsWhenFull(t *testing.T) {
	s := &sink{block: make(chan struct{}), err: errors.New("db down")}
	var failures int
	var mu sync.Mutex
	w := NewWriter(s, 2, func(error) { mu.Lock(); failures++; mu.Unlock() })
	var dropped int
	for i := 0; i < 20; i++ {
		if errors.Is(w.Record(valid()), ErrDropped) {
			dropped++
		}
	}
	if dropped == 0 || w.Dropped() != int64(dropped) {
		t.Fatalf("dropped %d counted %d", dropped, w.Dropped())
	}
	close(s.block)
	w.Close()
	if failures == 0 {
		t.Fatal("write failure not reported")
	}
}

func TestLoginIsSanitized(t *testing.T) {
	e := Login(tenant, 7, strings.Repeat("u", 100), "192.0.2.1", strings.Repeat("a", 600), "pq: password authentication failed for user smsgw_app")
	if e.ErrorMessage != LoginInternal || e.Success || len(e.Username) != 64 || len(e.UserAgent) != 512 {
		t.Fatalf("%+v", e)
	}
	ok := Login(tenant, 7, "client_a", "192.0.2.1", "", LoginOK)
	if !ok.Success || ok.ErrorMessage != "" || ok.TenantID != tenant || ok.ClientID != 7 {
		t.Fatalf("%+v", ok)
	}
	unknown := Login(tenant, 0, "nobody", "192.0.2.1", "", LoginUnknownUser)
	if unknown.TenantID != "" || unknown.ClientID != 0 || unknown.ErrorMessage != "unknown user" {
		t.Fatalf("%+v", unknown)
	}
}
