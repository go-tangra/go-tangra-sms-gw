// Package auth is Hermes client authentication: the source HS256 token
// issuer (claims, kinds and lifetimes unchanged), bcrypt password checks and
// the login, refresh, logout and current-client operations with persistent
// revocation.
package auth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/hermes"
)

// Token kinds.
const (
	KindAccess  = "access"
	KindRefresh = "refresh"
)

const issuer = "sms-gw"

// MinSecretBytes is the minimum HMAC secret length.
const MinSecretBytes = 32

// Source error messages.
var (
	ErrMissingToken = hermes.Unauthorized("missing bearer token")
	ErrInvalidToken = hermes.Unauthorized("invalid token")
	ErrExpiredToken = hermes.Unauthorized("token expired")
	ErrWrongKind    = hermes.Unauthorized("wrong token kind")
)

// Claims is the source token body.
type Claims struct {
	Authority string `json:"authority,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Username  string `json:"username,omitempty"`
	jwt.RegisteredClaims
}

// ClientID parses the subject as the uint32 client id.
func (c *Claims) ClientID() (int64, bool) {
	n, err := strconv.ParseUint(c.Subject, 10, 32)
	return int64(n), err == nil && n > 0
}

// Issuer mints and parses Hermes tokens.
type Issuer struct {
	secret     []byte
	accessTTL  time.Duration
	refreshTTL time.Duration
	now        func() time.Time
}

// NewIssuer requires a secret of at least 32 bytes.
func NewIssuer(secret string, accessTTL, refreshTTL time.Duration) (*Issuer, error) {
	if len(secret) < MinSecretBytes {
		return nil, fmt.Errorf("auth: jwt secret must be at least %d bytes", MinSecretBytes)
	}
	if accessTTL <= 0 {
		accessTTL = 2 * time.Hour
	}
	if refreshTTL <= 0 {
		refreshTTL = 7 * 24 * time.Hour
	}
	return &Issuer{secret: []byte(secret), accessTTL: accessTTL, refreshTTL: refreshTTL, now: time.Now}, nil
}

// AccessTTLSeconds is the login response expires_in.
func (i *Issuer) AccessTTLSeconds() int64 { return int64(i.accessTTL.Seconds()) }

// IssuePair returns an access and a refresh token sharing one random jti.
func (i *Issuer) IssuePair(clientID int64, username, authority string) (access, refresh string, err error) {
	var b [16]byte
	if _, err = rand.Read(b[:]); err != nil {
		return "", "", err
	}
	jti := hex.EncodeToString(b[:])
	if access, err = i.sign(clientID, username, authority, KindAccess, jti, i.accessTTL); err != nil {
		return "", "", err
	}
	refresh, err = i.sign(clientID, username, authority, KindRefresh, jti, i.refreshTTL)
	return access, refresh, err
}

func (i *Issuer) sign(clientID int64, username, authority, kind, jti string, ttl time.Duration) (string, error) {
	now := i.now()
	return jwt.NewWithClaims(jwt.SigningMethodHS256, &Claims{Authority: authority, Kind: kind, Username: username,
		RegisteredClaims: jwt.RegisteredClaims{Subject: strconv.FormatInt(clientID, 10), Issuer: issuer, IssuedAt: jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(ttl)), ID: jti}}).SignedString(i.secret)
}

// Parse verifies signature (HMAC only) and time claims, not the kind.
func (i *Issuer) Parse(raw string) (*Claims, error) {
	if raw == "" {
		return nil, ErrMissingToken
	}
	tok, err := jwt.ParseWithClaims(raw, &Claims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, ErrInvalidToken
		}
		return i.secret, nil
	}, jwt.WithTimeFunc(i.now))
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) || errors.Is(err, jwt.ErrTokenNotValidYet) {
			return nil, ErrExpiredToken
		}
		return nil, ErrInvalidToken
	}
	c, ok := tok.Claims.(*Claims)
	if !ok || !tok.Valid {
		return nil, ErrInvalidToken
	}
	return c, nil
}

// ParseKind is Parse plus the kind check.
func (i *Issuer) ParseKind(raw, kind string) (*Claims, error) {
	c, err := i.Parse(raw)
	if err != nil {
		return nil, err
	}
	if c.Kind != kind {
		return nil, ErrWrongKind
	}
	return c, nil
}
