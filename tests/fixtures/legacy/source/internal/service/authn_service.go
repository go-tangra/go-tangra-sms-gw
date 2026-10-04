package service

import (
	"context"
	"strconv"
	"strings"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/go-kratos/kratos/v2/transport"
	"github.com/tx7do/kratos-bootstrap/bootstrap"
	"google.golang.org/protobuf/types/known/emptypb"

	smsgwpb "github.com/go-tangra/go-tangra-sms-gw/gen/go/sms_gw/service/v1"
	"github.com/go-tangra/go-tangra-sms-gw/internal/auth"
	"github.com/go-tangra/go-tangra-sms-gw/internal/data"
	"github.com/go-tangra/go-tangra-sms-gw/internal/trustedproxy"
)

// requestMeta returns the caller's IP (via trustedproxy so X-Forwarded-For
// is honored only from a configured trusted proxy CIDR) and User-Agent.
// Returns ("", "") when called from a non-HTTP context.
func requestMeta(ctx context.Context) (ip, ua string) {
	ip = trustedproxy.ResolveIP(ctx)
	if tr, ok := transport.FromServerContext(ctx); ok {
		ua = tr.RequestHeader().Get("User-Agent")
	}
	return ip, ua
}

// AuthenticationService implements havi-format password+refresh login
// for SMS API clients. Tokens issued here pass auth.Server() middleware
// on the public HTTP server.
type AuthenticationService struct {
	smsgwpb.UnimplementedAuthenticationServiceServer

	log *log.Helper
	cr  *data.ApiClientRepo
	ll  *data.LoginLogRepo
	iss *auth.Issuer
}

func NewAuthenticationService(ctx *bootstrap.Context, cr *data.ApiClientRepo, ll *data.LoginLogRepo, iss *auth.Issuer) *AuthenticationService {
	return &AuthenticationService{
		log: ctx.NewLoggerHelper("sms-gw/service/authn"),
		cr:  cr,
		ll:  ll,
		iss: iss,
	}
}

func (s *AuthenticationService) Login(ctx context.Context, req *smsgwpb.LoginRequest) (*smsgwpb.LoginResponse, error) {
	ip, ua := requestMeta(ctx)

	if req.Username == "" || req.Password == "" {
		s.ll.Record(ctx, 0, req.Username, ip, ua, false, "missing credentials")
		return nil, smsgwpb.ErrorBadRequest("username and password are required")
	}
	c, err := s.cr.GetByUsername(ctx, req.Username)
	if err != nil {
		s.ll.Record(ctx, 0, req.Username, ip, ua, false, err.Error())
		return nil, err
	}
	// Uniform error to avoid username enumeration.
	if c == nil {
		s.ll.Record(ctx, 0, req.Username, ip, ua, false, "unknown user")
		return nil, smsgwpb.ErrorUnauthorized("invalid credentials")
	}
	if c.Status == "OFF" {
		s.ll.Record(ctx, c.ID, req.Username, ip, ua, false, "account disabled")
		return nil, smsgwpb.ErrorUnauthorized("account disabled")
	}
	if err := auth.VerifyPassword(c.PasswordHash, req.Password); err != nil {
		s.ll.Record(ctx, c.ID, req.Username, ip, ua, false, "wrong password")
		return nil, smsgwpb.ErrorUnauthorized("invalid credentials")
	}

	access, refresh, err := s.iss.IssuePair(c.ID, c.Username, string(c.Authority))
	if err != nil {
		s.ll.Record(ctx, c.ID, req.Username, ip, ua, false, err.Error())
		return nil, smsgwpb.ErrorInternalError("issue token: %s", err.Error())
	}

	s.cr.RecordLogin(ctx, c.ID, ip)
	s.ll.Record(ctx, c.ID, req.Username, ip, ua, true, "")

	exp := s.iss.AccessTTLSeconds()
	return &smsgwpb.LoginResponse{
		AccessToken:  access,
		RefreshToken: refresh,
		TokenType:    "Bearer",
		ExpiresIn:    &exp,
	}, nil
}

func (s *AuthenticationService) RefreshToken(ctx context.Context, req *smsgwpb.RefreshTokenRequest) (*smsgwpb.LoginResponse, error) {
	c, err := s.iss.VerifyRefresh(req.RefreshToken)
	if err != nil {
		return nil, err
	}
	// Re-fetch the client so revocation (status=OFF) takes effect immediately.
	// strconv.ParseUint enforces both digit-only input and uint32 range
	// atomically — the previous hand-rolled byte loop silently overflowed
	// on a crafted 20-digit subject.
	parsed, err := strconv.ParseUint(c.Subject, 10, 32)
	if err != nil {
		return nil, smsgwpb.ErrorUnauthorized("invalid subject")
	}
	id := uint32(parsed)
	cli, err := s.cr.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if cli.Status == "OFF" {
		return nil, smsgwpb.ErrorUnauthorized("account disabled")
	}
	access, refresh, err := s.iss.IssuePair(id, c.Username, c.Authority)
	if err != nil {
		return nil, smsgwpb.ErrorInternalError("issue token: %s", err.Error())
	}
	exp := s.iss.AccessTTLSeconds()
	return &smsgwpb.LoginResponse{
		AccessToken:  access,
		RefreshToken: refresh,
		TokenType:    "Bearer",
		ExpiresIn:    &exp,
	}, nil
}

// Logout revokes the bearer access token AND, if the client also
// supplied X-Refresh-Token, that refresh token too. Both jti values
// land on the in-process Denylist until their natural expiry (no
// shared store yet — multi-replica deployments need Redis; documented
// in CLAUDE.md as a v1 limitation).
//
// Returns success even when no token can be parsed: clients call
// Logout speculatively and a 4xx here would just confuse them.
func (s *AuthenticationService) Logout(ctx context.Context, req *smsgwpb.LogoutRequest) (*emptypb.Empty, error) {
	tr, ok := transport.FromServerContext(ctx)
	if !ok {
		return &emptypb.Empty{}, nil
	}
	h := tr.RequestHeader()
	if v := h.Get("Authorization"); strings.HasPrefix(v, "Bearer ") {
		if c, err := s.iss.VerifyAny(strings.TrimPrefix(v, "Bearer ")); err == nil {
			s.iss.Revoke(c)
		}
	}
	if v := h.Get("X-Refresh-Token"); v != "" {
		if c, err := s.iss.VerifyAny(v); err == nil {
			s.iss.Revoke(c)
		}
	}
	return &emptypb.Empty{}, nil
}

// GetMe returns the authenticated client's profile + abilities.
func (s *AuthenticationService) GetMe(ctx context.Context, _ *emptypb.Empty) (*smsgwpb.GetMeResponse, error) {
	id := auth.FromContext(ctx)
	if id == nil {
		return nil, smsgwpb.ErrorUnauthorized("not authenticated")
	}
	c, err := s.cr.Get(ctx, id.ClientID)
	if err != nil {
		return nil, err
	}
	return &smsgwpb.GetMeResponse{
		User: &smsgwpb.ApiClientPublic{
			Id:        c.Id,
			Username:  c.Username,
			Email:     c.Email,
			Authority: c.Authority,
		},
		Abilities: abilitiesFor(c.Authority),
	}, nil
}

// abilitiesFor returns the static ability list for an authority. Kept
// here rather than in the auth package because it is a presentation
// concern (drives the frontend menu) rather than an authorization one.
func abilitiesFor(authority string) []*smsgwpb.Ability {
	switch authority {
	case "API_CLIENT":
		return []*smsgwpb.Ability{
			{Action: "create", Subject: "sms"},
			{Action: "list", Subject: "sms"},
			{Action: "get", Subject: "sms"},
			{Action: "get", Subject: "dlr"},
		}
	case "API_VIEWER":
		return []*smsgwpb.Ability{
			{Action: "list", Subject: "sms"},
			{Action: "get", Subject: "sms"},
			{Action: "get", Subject: "dlr"},
		}
	default:
		return nil
	}
}
