package auth

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/audit"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/hermes"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/metrics"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/repo"
)

// Store is the persistence the Hermes authentication needs.
type Store interface {
	ClientByUsername(ctx context.Context, username string) (repo.APIClient, error)
	ClientByID(ctx context.Context, id int64) (repo.APIClient, error)
	RecordLogin(ctx context.Context, tenant string, id int64, ip string, at time.Time) error
	AddLogin(ctx context.Context, e repo.LoginEvent) error
	Revoke(ctx context.Context, r repo.Revocation) error
	IsRevoked(ctx context.Context, tenant, jti string, now time.Time) (bool, error)
}

// Auditor records sanitized audit events.
type Auditor interface {
	Record(audit.Event) error
}

// Service implements the Hermes authentication operations.
type Service struct {
	store   Store
	iss     *Issuer
	audit   Auditor
	metrics *metrics.Metrics
	log     *slog.Logger
	now     func() time.Time
}

// NewService wires the operations; audit, metrics and log may be nil.
func NewService(st Store, iss *Issuer, a Auditor, m *metrics.Metrics, log *slog.Logger) *Service {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Service{store: st, iss: iss, audit: a, metrics: m, log: log, now: time.Now}
}

// Tokens is a login or refresh result.
type Tokens struct {
	Access    string
	Refresh   string
	ExpiresIn int64
}

var loginOutcome = map[string]string{audit.LoginOK: "success", audit.LoginMissingCredentials: "bad_request", audit.LoginUnknownUser: "invalid_credentials",
	audit.LoginWrongPassword: "invalid_credentials", audit.LoginDisabled: "disabled", audit.LoginInternal: "error"}

var (
	errCredentials   = hermes.Unauthorized("invalid credentials")
	errDisabled      = hermes.Unauthorized("account disabled")
	errNoClient      = hermes.NotFound("api client not found")
	errMissingFields = hermes.BadRequest("username and password are required")
)

func (s *Service) storage(err error) error {
	s.log.Error("hermes auth storage failure", "err", err)
	return hermes.Unavailable("storage unavailable")
}

// Login checks a password and issues a token pair. Unknown usernames and
// wrong passwords share one answer and take the same time.
func (s *Service) Login(ctx context.Context, username, password, ip, userAgent string) (Tokens, error) {
	record := func(c repo.APIClient, reason string) {
		if err := s.store.AddLogin(ctx, audit.Login(c.TenantID, c.ID, username, ip, userAgent, reason)); err != nil {
			s.log.Warn("login record failed", "err", err)
		}
		s.metrics.Login(loginOutcome[reason])
	}
	if username == "" || password == "" {
		record(repo.APIClient{}, audit.LoginMissingCredentials)
		return Tokens{}, errMissingFields
	}
	c, err := s.store.ClientByUsername(ctx, username)
	switch {
	case errors.Is(err, repo.ErrNotFound):
		burnPasswordCheck(password)
		record(repo.APIClient{}, audit.LoginUnknownUser)
		return Tokens{}, errCredentials
	case err != nil:
		record(repo.APIClient{}, audit.LoginInternal)
		return Tokens{}, s.storage(err)
	}
	if c.Status == repo.Off {
		record(c, audit.LoginDisabled)
		return Tokens{}, errDisabled
	}
	if VerifyPassword(c.PasswordHash, password) != nil {
		record(c, audit.LoginWrongPassword)
		return Tokens{}, errCredentials
	}
	t, err := s.issue(c)
	if err != nil {
		record(c, audit.LoginInternal)
		return Tokens{}, err
	}
	if err := s.store.RecordLogin(ctx, c.TenantID, c.ID, ip, s.now()); err != nil {
		s.log.Warn("last login update failed", "err", err)
	}
	record(c, audit.LoginOK)
	return t, nil
}

func (s *Service) issue(c repo.APIClient) (Tokens, error) {
	access, refresh, err := s.iss.IssuePair(c.ID, c.Username, c.Authority)
	if err != nil {
		return Tokens{}, hermes.Internal("issue token failed")
	}
	return Tokens{Access: access, Refresh: refresh, ExpiresIn: s.iss.AccessTTLSeconds()}, nil
}

// Refresh issues a new pair for a valid, unrevoked refresh token of an
// enabled account. The presented token stays valid (no rotation, as in the
// source); the new pair carries the account's current authority.
func (s *Service) Refresh(ctx context.Context, raw string) (Tokens, error) {
	claims, err := s.iss.ParseKind(raw, KindRefresh)
	if err != nil {
		return Tokens{}, err
	}
	id, err := strconv.ParseUint(claims.Subject, 10, 32)
	if err != nil {
		return Tokens{}, hermes.Unauthorized("invalid subject")
	}
	c, err := s.client(ctx, int64(id))
	if err != nil {
		return Tokens{}, err
	}
	if err := s.checkRevoked(ctx, c, claims); err != nil {
		return Tokens{}, err
	}
	if c.Status == repo.Off {
		return Tokens{}, errDisabled
	}
	return s.issue(c)
}

func (s *Service) client(ctx context.Context, id int64) (repo.APIClient, error) {
	if id <= 0 {
		return repo.APIClient{}, errNoClient
	}
	c, err := s.store.ClientByID(ctx, id)
	if errors.Is(err, repo.ErrNotFound) {
		return c, errNoClient
	}
	if err != nil {
		return c, s.storage(err)
	}
	return c, nil
}

func (s *Service) checkRevoked(ctx context.Context, c repo.APIClient, claims *Claims) error {
	revoked, err := s.store.IsRevoked(ctx, c.TenantID, claims.ID, s.now())
	if err != nil {
		return s.storage(err)
	}
	if revoked {
		return ErrInvalidToken
	}
	return nil
}

// Authenticate verifies an Authorization header carrying an access token
// and resolves the caller from the stored account: a revoked token, a
// removed account or a disabled one is refused. The effective authority is
// the token claim only while it equals the account's authority.
func (s *Service) Authenticate(ctx context.Context, header string) (hermes.Caller, error) {
	if header == "" {
		return hermes.Caller{}, ErrMissingToken
	}
	raw, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		return hermes.Caller{}, ErrInvalidToken
	}
	claims, err := s.iss.ParseKind(raw, KindAccess)
	if err != nil {
		return hermes.Caller{}, err
	}
	id, _ := claims.ClientID()
	c, err := s.client(ctx, id)
	if err != nil {
		return hermes.Caller{}, err
	}
	if err := s.checkRevoked(ctx, c, claims); err != nil {
		return hermes.Caller{}, err
	}
	if c.Status == repo.Off {
		return hermes.Caller{}, errDisabled
	}
	caller := hermes.Caller{Client: c}
	if claims.Authority == c.Authority {
		caller.Authority = c.Authority
	}
	return caller, nil
}

// Logout revokes every valid token it is given (Authorization bearer and
// X-Refresh-Token), expired access tokens excepted; it never fails. The
// pair shares one id, so revoking either revokes both.
func (s *Service) Logout(ctx context.Context, authorization, refresh string) {
	if raw, ok := strings.CutPrefix(authorization, "Bearer "); ok {
		s.revoke(ctx, raw)
	}
	if refresh != "" {
		s.revoke(ctx, refresh)
	}
}

func (s *Service) revoke(ctx context.Context, raw string) {
	claims, err := s.iss.Parse(raw)
	if err != nil || claims.ExpiresAt == nil || claims.ID == "" {
		return
	}
	id, _ := claims.ClientID()
	c, err := s.client(ctx, id)
	if err != nil {
		return
	}
	if err := s.store.Revoke(ctx, repo.Revocation{JTI: claims.ID, TenantID: c.TenantID, ClientID: c.ID, ExpiresAt: claims.ExpiresAt.Time}); err != nil {
		s.log.Warn("token revocation failed", "err", err)
		return
	}
	if s.audit != nil {
		_ = s.audit.Record(audit.Event{TenantID: c.TenantID, ActorKind: audit.ActorAPIClient, ActorID: strconv.FormatInt(c.ID, 10),
			Action: audit.ClientLogout, TargetType: "api_client", TargetID: strconv.FormatInt(c.ID, 10), Outcome: audit.Success})
	}
}

// Ability is one source front-end ability.
type Ability struct{ Action, Subject string }

// Abilities is the static list of an authority (none for unsupported ones).
func Abilities(authority string) []Ability {
	switch authority {
	case hermes.AuthorityClient:
		return []Ability{{"create", "sms"}, {"list", "sms"}, {"get", "sms"}, {"get", "dlr"}}
	case hermes.AuthorityViewer:
		return []Ability{{"list", "sms"}, {"get", "sms"}, {"get", "dlr"}}
	}
	return []Ability{}
}
