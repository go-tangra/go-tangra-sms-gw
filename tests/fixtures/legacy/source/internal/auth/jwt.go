package auth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Token kinds. Refresh tokens cannot be used as access tokens and vice
// versa — the kind is encoded in the JWT body and verified separately.
const (
	KindAccess  = "access"
	KindRefresh = "refresh"
)

// Defaults applied when the corresponding env var / config value is unset.
const (
	defaultAccessTTL  = 2 * time.Hour
	defaultRefreshTTL = 7 * 24 * time.Hour
	issuer            = "sms-gw"
)

// Claims is the JWT body. Kept compatible with what hermes used so that
// any existing client-side decoder reading `sub` and `authority` keeps
// working unchanged.
type Claims struct {
	Authority string `json:"authority,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Username  string `json:"username,omitempty"`
	jwt.RegisteredClaims
}

// minimumSecretBytes guards against weak HMAC secrets. 32 bytes (256
// bits) matches the SHA-256 output and the OWASP cheat sheet.
const minimumSecretBytes = 32

// Issuer mints and verifies access + refresh tokens.
type Issuer struct {
	secret     []byte
	accessTTL  time.Duration
	refreshTTL time.Duration
	denylist   *Denylist
}

// NewIssuer constructs an Issuer with the supplied secret and TTLs.
// Returns an error when the secret is shorter than minimumSecretBytes
// (32). Empty TTLs fall back to sensible defaults. The Denylist must be
// created separately and assigned with SetDenylist (Wire injects it).
func NewIssuer(secret string, accessTTL, refreshTTL time.Duration) (*Issuer, error) {
	if len(secret) < minimumSecretBytes {
		return nil, fmt.Errorf("jwt secret too short: need at least %d bytes, got %d", minimumSecretBytes, len(secret))
	}
	if accessTTL <= 0 {
		accessTTL = defaultAccessTTL
	}
	if refreshTTL <= 0 {
		refreshTTL = defaultRefreshTTL
	}
	return &Issuer{
		secret:     []byte(secret),
		accessTTL:  accessTTL,
		refreshTTL: refreshTTL,
	}, nil
}

// SetDenylist wires the revocation store into the Issuer. Called once
// at boot from the constructor wrapper in factory.go.
func (i *Issuer) SetDenylist(d *Denylist) { i.denylist = d }

// AccessTTLSeconds is exposed for the LoginResponse.expires_in field.
func (i *Issuer) AccessTTLSeconds() int64 { return int64(i.accessTTL.Seconds()) }

// IssuePair returns (accessToken, refreshToken). The same jti is used
// across the pair so a server-side store can correlate them later (the
// current implementation is stateless).
func (i *Issuer) IssuePair(clientID uint32, username, authority string) (string, string, error) {
	jti, err := newJTI()
	if err != nil {
		return "", "", err
	}
	access, err := i.signWithKind(clientID, username, authority, KindAccess, jti, i.accessTTL)
	if err != nil {
		return "", "", err
	}
	refresh, err := i.signWithKind(clientID, username, authority, KindRefresh, jti, i.refreshTTL)
	if err != nil {
		return "", "", err
	}
	return access, refresh, nil
}

// VerifyAccess parses and validates an access token. Returns its claims
// or one of the sentinel auth errors.
func (i *Issuer) VerifyAccess(raw string) (*Claims, error) {
	c, err := i.verify(raw)
	if err != nil {
		return nil, err
	}
	if c.Kind != KindAccess {
		return nil, ErrWrongKind
	}
	if i.denylist != nil && i.denylist.IsRevoked(c.ID) {
		return nil, ErrInvalidToken
	}
	return c, nil
}

// VerifyRefresh parses and validates a refresh token.
func (i *Issuer) VerifyRefresh(raw string) (*Claims, error) {
	c, err := i.verify(raw)
	if err != nil {
		return nil, err
	}
	if c.Kind != KindRefresh {
		return nil, ErrWrongKind
	}
	if i.denylist != nil && i.denylist.IsRevoked(c.ID) {
		return nil, ErrInvalidToken
	}
	return c, nil
}

// Revoke adds the JTI from a parsed Claims to the denylist until its
// natural expiry. No-op if no denylist is wired. Returns the expiry so
// callers (e.g. AuthenticationService.Logout) can log it.
func (i *Issuer) Revoke(c *Claims) (time.Time, bool) {
	if i.denylist == nil || c == nil || c.ID == "" || c.ExpiresAt == nil {
		return time.Time{}, false
	}
	i.denylist.Revoke(c.ID, c.ExpiresAt.Time)
	return c.ExpiresAt.Time, true
}

// VerifyAny parses without checking the kind, used by Logout to revoke
// access AND refresh tokens by either id. Skips the denylist check
// (revoking an already-revoked token is fine — IsRevoked stays true).
func (i *Issuer) VerifyAny(raw string) (*Claims, error) { return i.verify(raw) }

func (i *Issuer) signWithKind(clientID uint32, username, authority, kind, jti string, ttl time.Duration) (string, error) {
	now := time.Now()
	claims := &Claims{
		Authority: authority,
		Kind:      kind,
		Username:  username,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   strconv.FormatUint(uint64(clientID), 10),
			Issuer:    issuer,
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
			ID:        jti,
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return tok.SignedString(i.secret)
}

func (i *Issuer) verify(raw string) (*Claims, error) {
	if raw == "" {
		return nil, ErrMissingToken
	}
	parsed, err := jwt.ParseWithClaims(raw, &Claims{}, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, ErrInvalidToken
		}
		return i.secret, nil
	})
	if err != nil {
		if isExpired(err) {
			return nil, ErrExpiredToken
		}
		return nil, ErrInvalidToken
	}
	if !parsed.Valid {
		return nil, ErrInvalidToken
	}
	c, ok := parsed.Claims.(*Claims)
	if !ok {
		return nil, ErrInvalidToken
	}
	return c, nil
}

func newJTI() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// isExpired uses errors.Is so it traverses both Unwrap()->error and
// Unwrap()->[]error chains (the latter is how jwt/v5 joins multiple
// validation errors).
func isExpired(err error) bool {
	return errors.Is(err, jwt.ErrTokenExpired) || errors.Is(err, jwt.ErrTokenNotValidYet)
}
