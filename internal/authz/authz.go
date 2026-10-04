// Package authz verifies platform operators for the management API. The
// gateway is a service caller, not proof of authority: every request carries
// the operator's bearer token, which is verified with the auth SDK; the
// tenant comes only from that token and every operation checks its
// permission with the auth service. Legacy identity headers are refused, and
// an unverifiable request fails closed (503 when verification itself is
// unavailable, never an allow).
package authz

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	authv1 "github.com/go-tangra/go-tangra-auth/sdk/v4/api/proto/auth/v1"
	"github.com/go-tangra/go-tangra-auth/sdk/v4/pkg/authclient"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Permissions of the management API (contracts/management-api.md).
const (
	ProvidersRead   = "providers:read"
	ProvidersManage = "providers:manage"
	TemplatesRead   = "templates:read"
	TemplatesManage = "templates:manage"
	ClientsRead     = "clients:read"
	ClientsManage   = "clients:manage"
	BlocksRead      = "blocks:read"
	BlocksManage    = "blocks:manage"
	MessagesRead    = "messages:read"
	MessagesSend    = "messages:send"
	DashboardRead   = "dashboard:read"
)

// AllPermissions lists every permission the module declares.
var AllPermissions = []string{ProvidersRead, ProvidersManage, TemplatesRead, TemplatesManage, ClientsRead, ClientsManage,
	BlocksRead, BlocksManage, MessagesRead, MessagesSend, DashboardRead}

// Errors, mapped to 401, 403 and 503.
var (
	ErrUnauthenticated = errors.New("authz: unauthenticated")
	ErrForbidden       = errors.New("authz: forbidden")
	ErrUnavailable     = errors.New("authz: verification unavailable")
)

// Verifier verifies a platform bearer token (authclient.Verifier).
type Verifier interface {
	Verify(ctx context.Context, token string) (authclient.Identity, error)
}

// Checker decides one permission for a user of a tenant.
type Checker interface {
	Has(ctx context.Context, tenant, user, permission string) (bool, error)
}

// Operator is a verified platform user acting in their token's tenant.
type Operator struct {
	TenantID  string
	UserID    string
	SessionID string
	Roles     []string
}

type operatorKey struct{}

// WithOperator stores the verified operator in ctx.
func WithOperator(ctx context.Context, op Operator) context.Context {
	return context.WithValue(ctx, operatorKey{}, op)
}

// FromContext returns the operator Require verified.
func FromContext(ctx context.Context) (Operator, bool) {
	op, ok := ctx.Value(operatorKey{}).(Operator)
	return op, ok
}

// Authz authenticates and authorizes management requests.
type Authz struct {
	verifier Verifier
	checker  Checker
}

// New combines a token verifier and a permission checker.
func New(v Verifier, c Checker) *Authz { return &Authz{verifier: v, checker: c} }

var tenantRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// legacyHeader names caller-supplied identity the legacy admin path trusted
// (x-md-global-*) and look-alikes; their presence is a forgery attempt.
func legacyHeader(h http.Header) bool {
	for k := range h {
		k = strings.ToLower(k)
		if strings.HasPrefix(k, "x-md-") || k == "x-tenant-id" || k == "x-user-id" || k == "x-roles" || k == "x-authority" {
			return true
		}
	}
	return false
}

// Authenticate verifies the request's bearer token.
func (a *Authz) Authenticate(r *http.Request) (Operator, error) {
	if legacyHeader(r.Header) {
		return Operator{}, ErrForbidden
	}
	if a == nil || a.verifier == nil {
		return Operator{}, ErrUnavailable
	}
	token := authclient.BearerToken(r.Header.Get("Authorization"))
	if token == "" {
		return Operator{}, ErrUnauthenticated
	}
	id, err := a.verifier.Verify(r.Context(), token)
	if errors.Is(err, authclient.ErrStale) {
		return Operator{}, ErrUnavailable
	}
	if err != nil || id.UserID == "" || !tenantRE.MatchString(id.TenantID) {
		return Operator{}, ErrUnauthenticated
	}
	return Operator{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID, Roles: append([]string(nil), id.Roles...)}, nil
}

// Authorize requires every permission for the operator in their tenant.
func (a *Authz) Authorize(ctx context.Context, op Operator, permissions ...string) error {
	if len(permissions) == 0 {
		return ErrForbidden
	}
	if a == nil || a.checker == nil {
		return ErrUnavailable
	}
	for _, p := range permissions {
		ok, err := a.checker.Has(ctx, op.TenantID, op.UserID, p)
		if err != nil {
			return ErrUnavailable
		}
		if !ok {
			return ErrForbidden
		}
	}
	return nil
}

// Require wraps next: authenticate, authorize every permission, then serve
// with the operator in the request context.
func (a *Authz) Require(next http.Handler, permissions ...string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		op, err := a.Authenticate(r)
		if err == nil {
			err = a.Authorize(r.Context(), op, permissions...)
		}
		if err != nil {
			WriteError(w, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithOperator(r.Context(), op)))
	})
}

// WriteError answers an authz refusal: {"reason": ...} with 401, 403 or 503.
func WriteError(w http.ResponseWriter, err error) {
	code, reason := http.StatusServiceUnavailable, "temporarily_unavailable"
	switch {
	case errors.Is(err, ErrUnauthenticated):
		code, reason = http.StatusUnauthorized, "unauthenticated"
		w.Header().Set("WWW-Authenticate", `Bearer realm="tangra"`)
	case errors.Is(err, ErrForbidden):
		code, reason = http.StatusForbidden, "forbidden"
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"reason": reason})
}

// AuthChecker asks the auth service (auth.v1.Authorization/Check). Refusals
// of the request are denials; transport failures and timeouts are errors,
// never a deny or an allow.
type AuthChecker struct {
	Client  authv1.AuthorizationClient
	Timeout time.Duration
}

// Has implements Checker.
func (c AuthChecker) Has(ctx context.Context, tenant, user, permission string) (bool, error) {
	resource, action, ok := strings.Cut(permission, ":")
	if !ok || resource == "" || action == "" {
		return false, nil
	}
	if c.Client == nil {
		return false, ErrUnavailable
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := c.Client.Check(ctx, &authv1.CheckRequest{TenantId: tenant, UserId: user, Resource: resource, Action: action})
	switch status.Code(err) {
	case codes.OK:
		return res.GetAllowed(), nil
	case codes.InvalidArgument, codes.NotFound, codes.PermissionDenied:
		return false, nil
	}
	return false, ErrUnavailable
}

// Cache remembers allow and deny decisions per tenant, user and permission
// for a short TTL; errors are never cached. Revoked sessions are rejected
// by token verification before a decision is consulted, so the TTL only
// bounds how long a role change in auth takes to apply here.
type Cache struct {
	next Checker
	ttl  time.Duration
	max  int
	now  func() time.Time
	mu   sync.Mutex
	m    map[cacheKey]cacheEntry
}

type cacheKey struct{ tenant, user, permission string }

type cacheEntry struct {
	allowed bool
	until   time.Time
}

// NewCache wraps next with at most max cached decisions.
func NewCache(next Checker, ttl time.Duration, max int) *Cache {
	return &Cache{next: next, ttl: ttl, max: max, now: time.Now, m: map[cacheKey]cacheEntry{}}
}

// Has implements Checker.
func (c *Cache) Has(ctx context.Context, tenant, user, permission string) (bool, error) {
	k := cacheKey{tenant, user, permission}
	c.mu.Lock()
	v, ok := c.m[k]
	c.mu.Unlock()
	if ok && c.now().Before(v.until) {
		return v.allowed, nil
	}
	allowed, err := c.next.Has(ctx, tenant, user, permission)
	if err != nil {
		return false, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) >= c.max {
		now := c.now()
		for key, e := range c.m {
			if !now.Before(e.until) {
				delete(c.m, key)
			}
		}
		for key := range c.m {
			if len(c.m) < c.max {
				break
			}
			delete(c.m, key)
		}
	}
	c.m[k] = cacheEntry{allowed, c.now().Add(c.ttl)}
	return allowed, nil
}
