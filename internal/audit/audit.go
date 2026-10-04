// Package audit records sanitized audit and Hermes login events. Actions,
// outcomes, actor kinds and login reasons are closed vocabularies; an event
// carries identifiers only, never request bodies, tokens, passwords or
// decrypted secrets. Writes are asynchronous and bounded: a full queue
// drops (and counts) rather than blocking a request.
package audit

import (
	"context"
	"errors"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/repo"
)

// Actions.
const (
	ProviderCreate      = "provider.create"
	ProviderUpdate      = "provider.update"
	ProviderDelete      = "provider.delete"
	TemplateCreate      = "template.create"
	TemplateUpdate      = "template.update"
	TemplateDelete      = "template.delete"
	ClientCreate        = "api_client.create"
	ClientUpdate        = "api_client.update"
	ClientDelete        = "api_client.delete"
	ClientResetPassword = "api_client.reset_password"
	BlockCreate         = "block.create"
	BlockUpdate         = "block.update"
	BlockDelete         = "block.delete"
	MessageSend         = "message.send"
	ClientLogout        = "api_client.logout"
	ReceiptRejected     = "receipt.rejected"
	ImportDryRun        = "import.dry_run"
	ImportApply         = "import.apply"
)

// Outcomes.
const (
	Success = "success"
	Denied  = "denied"
	Failure = "failure"
)

// Actor kinds.
const (
	ActorOperator  = "operator"
	ActorAPIClient = "api_client"
	ActorService   = "service"
	ActorSystem    = "system"
)

var (
	actions = map[string]bool{ProviderCreate: true, ProviderUpdate: true, ProviderDelete: true, TemplateCreate: true, TemplateUpdate: true,
		TemplateDelete: true, ClientCreate: true, ClientUpdate: true, ClientDelete: true, ClientResetPassword: true, BlockCreate: true,
		BlockUpdate: true, BlockDelete: true, MessageSend: true, ClientLogout: true, ReceiptRejected: true, ImportDryRun: true, ImportApply: true}
	outcomes   = map[string]bool{Success: true, Denied: true, Failure: true}
	actorKinds = map[string]bool{ActorOperator: true, ActorAPIClient: true, ActorService: true, ActorSystem: true}
	targets    = map[string]bool{"": true, "provider": true, "template": true, "api_client": true, "block": true, "message": true, "import": true}
	idRE       = regexp.MustCompile(`^[A-Za-z0-9:/._-]{0,128}$`)
)

// Errors.
var (
	ErrInvalid = errors.New("audit: event outside the closed vocabulary")
	ErrDropped = errors.New("audit: queue full, event dropped")
	ErrClosed  = errors.New("audit: writer closed")
)

// Event is one audit record before it is queued.
type Event struct {
	TenantID   string
	ActorKind  string
	ActorID    string
	Action     string
	TargetType string
	TargetID   string
	Outcome    string
	RequestID  string
}

// Validate checks the closed vocabularies and that identifiers look like
// identifiers (free text would be a way to smuggle content into the log).
func (e Event) Validate() error {
	if !actions[e.Action] || !outcomes[e.Outcome] || !actorKinds[e.ActorKind] || !targets[e.TargetType] ||
		!idRE.MatchString(e.ActorID) || !idRE.MatchString(e.TargetID) || !idRE.MatchString(e.RequestID) {
		return ErrInvalid
	}
	return nil
}

// Sink persists events (repo.Postgres).
type Sink interface {
	AddAudit(ctx context.Context, events ...repo.AuditEvent) error
}

// Writer queues events and writes them in batches.
type Writer struct {
	sink    Sink
	queue   chan repo.AuditEvent
	onError func(error)
	dropped atomic.Int64
	done    chan struct{}
	mu      sync.RWMutex
	closed  bool
	now     func() time.Time
}

// NewWriter starts the writer; onError receives write failures (never event content).
func NewWriter(sink Sink, size int, onError func(error)) *Writer {
	if size < 1 {
		size = 1024
	}
	if onError == nil {
		onError = func(error) {}
	}
	w := &Writer{sink: sink, queue: make(chan repo.AuditEvent, size), onError: onError, done: make(chan struct{}), now: time.Now}
	go w.run()
	return w
}

// Record validates and queues one event.
func (w *Writer) Record(e Event) error {
	if err := e.Validate(); err != nil {
		return err
	}
	ev := repo.AuditEvent{TenantID: e.TenantID, ActorKind: e.ActorKind, ActorID: e.ActorID, Action: e.Action, TargetType: e.TargetType,
		TargetID: e.TargetID, Outcome: e.Outcome, RequestID: e.RequestID, EventTime: w.now()}
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.closed {
		return ErrClosed
	}
	select {
	case w.queue <- ev:
		return nil
	default:
		w.dropped.Add(1)
		return ErrDropped
	}
}

// Dropped is the number of events lost to a full queue.
func (w *Writer) Dropped() int64 { return w.dropped.Load() }

func (w *Writer) run() {
	defer close(w.done)
	batch := make([]repo.AuditEvent, 0, 64)
	for ev := range w.queue {
		batch = append(batch[:0], ev)
	more:
		for len(batch) < cap(batch) {
			select {
			case next, ok := <-w.queue:
				if !ok {
					break more
				}
				batch = append(batch, next)
			default:
				break more
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := w.sink.AddAudit(ctx, batch...); err != nil {
			w.onError(err)
		}
		cancel()
	}
}

// Close stops accepting events and waits for queued ones to be written.
func (w *Writer) Close() {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	close(w.queue)
	w.mu.Unlock()
	<-w.done
}

// Login reasons, as the legacy service recorded them.
const (
	LoginOK                 = ""
	LoginMissingCredentials = "missing credentials"
	LoginUnknownUser        = "unknown user"
	LoginDisabled           = "account disabled"
	LoginWrongPassword      = "wrong password"
	LoginInternal           = "internal error"
)

var loginReasons = map[string]bool{LoginOK: true, LoginMissingCredentials: true, LoginUnknownUser: true, LoginDisabled: true,
	LoginWrongPassword: true, LoginInternal: true}

// Login builds a sanitized login record: the reason is a closed value (an
// unknown one becomes "internal error", so error text with secrets cannot
// leak), the supplied username, address and agent are length-bounded and
// nothing else of the request is kept. An unresolved account has no tenant.
func Login(tenant string, clientID int64, username, ip, userAgent, reason string) repo.LoginEvent {
	if !loginReasons[reason] {
		reason = LoginInternal
	}
	if tenant == "" || clientID <= 0 {
		tenant, clientID = "", 0
	}
	return repo.LoginEvent{TenantID: tenant, ClientID: clientID, Username: bound(username, 64), IP: bound(ip, 64),
		UserAgent: bound(userAgent, 512), Success: reason == LoginOK, ErrorMessage: reason, EventTime: time.Now()}
}

func bound(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		r = r[:n]
	}
	return string(r)
}
