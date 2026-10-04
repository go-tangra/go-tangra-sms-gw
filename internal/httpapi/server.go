// Package httpapi is the V4 management API (api/openapi/sms-gw.yaml),
// mounted on the mesh HTTP server and reached only through the gateway.
// Every request is matched against the OpenAPI document, authenticated with
// the forwarded operator token, authorized for the operation's
// x-freya-permission in the token's tenant, bounded and validated before a
// handler runs. Handlers are installed with the permission they enforce; a
// route that is not declared, or whose permission differs from the
// document, is refused at construction (strict handler/manifest parity).
package httpapi

import (
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/authz"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/metrics"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/provider"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/sealed"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/sms"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/webhook"
	"github.com/go-tangra/go-tangra-sms-gw/v4/pkg/smsgwmanifest"
)

// Route is a declared method and path template.
type Route struct{ Method, Path string }

func (r Route) String() string { return r.Method + " " + r.Path }

// MaxBodyBytes bounds request bodies of operations without their own limit.
const MaxBodyBytes = 64 << 10

// Auditor records sanitized audit events.
type Auditor = sms.Auditor

// Config wires the API.
type Config struct {
	Store     Store
	Authz     *authz.Authz
	Envelope  *sealed.Envelope
	SMS       *sms.Service
	Senders   *provider.Cache
	Dashboard *metrics.Dashboard
	Audit     Auditor
	Webhook   webhook.Policy
	Log       *slog.Logger
}

type operation struct {
	permission string
	bodyLimit  int64
}

// Server serves the management API.
type Server struct {
	cfg     Config
	log     *slog.Logger
	router  routers.Router
	ops     map[Route]operation
	mux     *http.ServeMux
	handled map[Route]string
}

var formats sync.Once

// LoadDocument parses and validates the embedded OpenAPI document; uuid
// formats are validated (RFC 9562).
func LoadDocument() (*openapi3.T, error) {
	formats.Do(func() {
		openapi3.DefineStringFormatValidator("uuid", openapi3.NewRegexpFormatValidator(openapi3.FormatOfStringForUUIDOfRFC9562))
	})
	return smsgwmanifest.Load()
}

// New builds the API with every handler installed.
func New(cfg Config) (*Server, error) {
	doc, err := LoadDocument()
	if err != nil {
		return nil, err
	}
	doc.Servers = nil
	router, err := gorillamux.NewRouter(doc)
	if err != nil {
		return nil, fmt.Errorf("httpapi: router: %w", err)
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	s := &Server{cfg: cfg, log: cfg.Log, router: router, ops: map[Route]operation{}, mux: http.NewServeMux(), handled: map[Route]string{}}
	for p, item := range doc.Paths.Map() {
		for m, op := range item.Operations() {
			perm, _ := op.Extensions[smsgwmanifest.PermissionExtension].(string)
			o := operation{permission: perm, bodyLimit: MaxBodyBytes}
			if n, ok := op.Extensions[smsgwmanifest.BodyLimitExtension].(float64); ok && n > 0 {
				o.bodyLimit = int64(n)
			}
			s.ops[Route{strings.ToUpper(m), p}] = o
		}
	}
	if err := s.routes(); err != nil {
		return nil, err
	}
	if missing := s.Missing(); len(missing) > 0 {
		return nil, fmt.Errorf("httpapi: routes without a handler: %v", missing)
	}
	return s, nil
}

// handle installs h for a declared route that requires permission.
func (s *Server) handle(method, path, permission string, h http.HandlerFunc) error {
	rt := Route{method, path}
	op, ok := s.ops[rt]
	switch {
	case !ok:
		return fmt.Errorf("httpapi: %s is not declared in the OpenAPI document", rt)
	case op.permission != permission:
		return fmt.Errorf("httpapi: %s enforces %q but declares %q", rt, permission, op.permission)
	case s.handled[rt] != "":
		return fmt.Errorf("httpapi: %s installed twice", rt)
	}
	s.handled[rt] = permission
	s.mux.Handle(method+" "+path, h)
	return nil
}

// Declared lists the document's operations, sorted.
func (s *Server) Declared() []Route {
	out := make([]Route, 0, len(s.ops))
	for rt := range s.ops {
		out = append(out, rt)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// Handled maps every installed route to the permission its handler enforces.
func (s *Server) Handled() map[Route]string {
	out := make(map[Route]string, len(s.handled))
	for k, v := range s.handled {
		out[k] = v
	}
	return out
}

// Missing lists declared routes without a handler.
func (s *Server) Missing() []Route {
	var out []Route
	for _, rt := range s.Declared() {
		if s.handled[rt] == "" {
			out = append(out, rt)
		}
	}
	return out
}

// RemoteHandler serves the federated remote build: hashed assets are
// immutable, everything else is revalidated; no directory listings.
func RemoteHandler(dist fs.FS) http.Handler {
	files := http.FileServerFS(dist)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if st, err := fs.Stat(dist, p); p == "" || err != nil || st.IsDir() {
			writeError(w, ErrNotFound)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if strings.HasPrefix(p, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}
