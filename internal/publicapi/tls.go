package publicapi

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ChallengePath is where an ACME CA fetches http-01 tokens.
const ChallengePath = "/.well-known/acme-challenge/"

// LoadStaticTLS loads the public HTTPS keypair (never the mesh identity).
// Both paths empty means not configured (nil, nil); one path alone, an
// unreadable or mismatched pair, or a leaf outside its validity window is an
// error the caller reports while plain HTTP keeps serving.
func LoadStaticTLS(certPath, keyPath string) (*tls.Config, error) {
	if certPath == "" && keyPath == "" {
		return nil, nil
	}
	if certPath == "" || keyPath == "" {
		return nil, errors.New("public tls: certificate and key files go together")
	}
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("public tls: load keypair: %w", err)
	}
	if pair.Leaf == nil {
		if pair.Leaf, err = x509.ParseCertificate(pair.Certificate[0]); err != nil {
			return nil, fmt.Errorf("public tls: parse certificate: %w", err)
		}
	}
	now := time.Now()
	if now.Before(pair.Leaf.NotBefore) {
		return nil, fmt.Errorf("public tls: certificate not valid until %s", pair.Leaf.NotBefore.Format(time.RFC3339))
	}
	if now.After(pair.Leaf.NotAfter) {
		return nil, fmt.Errorf("public tls: certificate expired on %s", pair.Leaf.NotAfter.Format(time.RFC3339))
	}
	return &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}, nil
}

// WithChallenge answers ACME http-01 requests ahead of the Hermes routes
// (no authentication, the CA sends none); everything else goes to h.
func WithChallenge(h, challenge http.Handler) http.Handler {
	if challenge == nil {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, ChallengePath) {
			challenge.ServeHTTP(w, r)
			return
		}
		h.ServeHTTP(w, r)
	})
}
