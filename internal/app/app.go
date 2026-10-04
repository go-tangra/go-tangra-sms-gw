// Package app wires the sms-gw service: configuration → Freya runtime →
// PostgreSQL, KEK envelope, audit and metrics → operator verification over
// the pooled auth connection → management routes on the mesh HTTP server,
// the application admin listener (health, readiness, metrics) and the public
// Hermes listener (public.go). Further listeners and background workers
// attach through AddServer and Go; Run starts and drains everything with one
// lifecycle context.
package app

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	authv1 "github.com/go-tangra/go-tangra-auth/sdk/v4/api/proto/auth/v1"
	"github.com/go-tangra/go-tangra-auth/sdk/v4/pkg/authclient"
	freya "github.com/go-tangra/go-tangra/v4"
	"github.com/go-tangra/go-tangra/v4/transport"
	"github.com/go-tangra/go-tangra/v4/transport/tlsconf"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/audit"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/auth"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/authz"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/config"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/metrics"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/provider"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/publicapi"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/repo"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/sealed"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/sms"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/store"
)

// Options override infrastructure (tests) and attach optional parts.
type Options struct {
	Logger   slog.Handler
	Freya    []freya.Option
	KEK      []byte           // nil = from config
	Verifier authz.Verifier   // nil = authclient over the pooled auth connection
	Checker  authz.Checker    // nil = auth.v1.Authorization/Check, cached
	Migrate  bool             // apply migrations with db.migrate_dsn first
	Register func(*App) error // mounts later stories after the core is wired
}

// App holds every wired component.
type App struct {
	Cfg       config.Config
	Log       *slog.Logger
	Freya     *freya.App
	Store     *store.Store
	Repo      *repo.Postgres
	Envelope  *sealed.Envelope
	JWTSecret sealed.Secret
	Audit     *audit.Writer
	Metrics   *metrics.Metrics
	Authz     *authz.Authz
	Verifier  authz.Verifier
	// Management is the mesh HTTP API (reached only through the gateway);
	// unmatched paths answer 404.
	Management *http.ServeMux
	// Hermes domain and the public listener (public.go).
	Auth    *auth.Service
	SMS     *sms.Service
	Senders *provider.Cache
	Public  *publicapi.Server

	publicAddr net.Addr

	servers  []server
	workers  []func(context.Context)
	closers  []func()
	admin    *http.Server
	adminLis net.Listener
	ran      bool
	close    sync.Once
}

type server struct {
	name string
	srv  *http.Server
	lis  net.Listener
}

// Build validates cfg and connects every dependency; nothing is served yet.
func Build(ctx context.Context, cfg config.Config, o Options) (a *App, err error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	handler := o.Logger
	if handler == nil {
		handler = slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{ReplaceAttr: sealed.ReplaceAttr})
	}
	a = &App{Cfg: cfg, Log: slog.New(handler), Management: http.NewServeMux()}
	built := a
	defer func() {
		if err != nil {
			built.Close()
		}
	}()
	for _, w := range cfg.Warnings() {
		a.Log.Warn(w)
	}
	secret, err := cfg.PublicAuth.JWTSecret.Read()
	if err != nil {
		return nil, err
	}
	if len(secret) < 32 {
		return nil, errors.New("app: public_auth.jwt_secret must be at least 32 bytes")
	}
	a.JWTSecret = sealed.Secret(secret)
	kek := o.KEK
	if kek == nil {
		if kek, err = sealed.LoadKEK(cfg.KEK.Source, cfg.KEK.Path, cfg.KEK.Env); err != nil {
			return nil, err
		}
	}
	if a.Envelope, err = sealed.NewEnvelope(kek); err != nil {
		return nil, err
	}
	dbCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if o.Migrate {
		dsn := cfg.DB.MigrateDSN
		if dsn == "" {
			dsn = cfg.DB.DSN
		}
		if err := store.Migrate(dbCtx, dsn); err != nil {
			return nil, err
		}
	}
	if a.Store, err = store.Open(dbCtx, cfg.DB.DSN, cfg.DB.MaxConns); err != nil {
		return nil, err
	}
	a.closers = append(a.closers, a.Store.Close)
	a.Repo = repo.NewPostgres(a.Store, cfg.Query.DefaultPageSize, cfg.Query.MaxPageSize)
	runtime := cfg.Config
	// Freya's own operations listener stays on an ephemeral loopback port;
	// the application admin listener below adds database readiness.
	runtime.Admin.Addr = "127.0.0.1:0"
	runtime.Admin.AllowNonLoopback = false
	fopts := append([]freya.Option{freya.WithLogger(handler)}, o.Freya...)
	if a.Freya, err = freya.New(runtime, fopts...); err != nil {
		return nil, fmt.Errorf("app: secure runtime: %w", err)
	}
	a.closers = append(a.closers, a.closeFreya)
	if a.Metrics, err = metrics.New(a.Freya.Metrics().Meter("sms-gw")); err != nil {
		return nil, err
	}
	a.Audit = audit.NewWriter(a.Repo, 1024, func(err error) { a.Log.Error("audit write failed", "err", err) })
	a.closers = append(a.closers, a.Audit.Close)
	verifier, checker := o.Verifier, o.Checker
	if verifier == nil || checker == nil {
		conn, err := a.Freya.Client(ctx, cfg.Gateway.AuthService)
		if err != nil {
			return nil, fmt.Errorf("app: auth client: %w", err)
		}
		if verifier == nil {
			verifier = authclient.New(authclient.Config{Issuer: cfg.Gateway.Issuer}, authclient.GRPCKeys{Client: authv1.NewKeysClient(conn)},
				authclient.GRPCRevocations{Client: authv1.NewSessionsClient(conn)})
		}
		if checker == nil {
			checker = authz.NewCache(authz.AuthChecker{Client: authv1.NewAuthorizationClient(conn)}, 30*time.Second, 10000)
		}
	}
	a.Verifier = verifier
	a.Authz = authz.New(verifier, checker)
	a.Management.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"reason": "not_found"})
	})
	a.Freya.HTTP().HandlePrefix("/", a.Management)
	a.Go(a.purgeRevocations)
	if err := a.buildAdmin(); err != nil {
		return nil, err
	}
	if err := a.buildPublic(); err != nil {
		return nil, err
	}
	if o.Register != nil {
		if err := o.Register(a); err != nil {
			return nil, err
		}
	}
	return a, nil
}

// Go registers a background worker; it runs from Run until the lifecycle
// context is cancelled and Run waits for it to return.
func (a *App) Go(f func(context.Context)) { a.workers = append(a.workers, f) }

// AddServer binds an additional listener (the public Hermes edge) now, so a
// conflict fails Build; Run serves it and drains it on shutdown.
func (a *App) AddServer(name, addr string, h http.Handler, tlsCfg *tls.Config) (net.Addr, error) {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("app: %s listener: %w", name, err)
	}
	if tlsCfg != nil {
		lis = tls.NewListener(lis, tlsCfg)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second,
		IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10, ErrorLog: slog.NewLogLogger(a.Log.Handler(), slog.LevelWarn)}
	a.servers = append(a.servers, server{name, srv, lis})
	return lis.Addr(), nil
}

// Readiness reports the identity and database state.
type Readiness struct {
	Identity string `json:"identity"`
	Database string `json:"database"`
}

// Ready checks the mesh identity and the database (each bounded).
func (a *App) Ready(ctx context.Context) (Readiness, bool) {
	r := Readiness{Identity: "ok", Database: "ok"}
	if a.Freya == nil || !a.Freya.Ready() {
		r.Identity = "unavailable"
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if a.Store == nil || a.Store.Ping(ctx) != nil {
		r.Database = "unreachable"
	}
	return r, r.Identity == "ok" && r.Database == "ok"
}

func (a *App) buildAdmin() error {
	lis, err := net.Listen("tcp", a.Cfg.Admin.Addr)
	if err != nil {
		return fmt.Errorf("app: admin listener: %w", err)
	}
	a.adminLis = lis
	host, _, _ := net.SplitHostPort(lis.Addr().String())
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		tlsCfg, err := tlsconf.ServerConfig(a.Freya.Provider(), transport.TLSOptions(a.Freya))
		if err != nil {
			_ = lis.Close()
			return errors.New("app: admin mTLS unavailable")
		}
		a.adminLis = tls.NewListener(lis, tlsCfg)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		state, ok := a.Ready(r.Context())
		code := http.StatusOK
		if !ok {
			code = http.StatusServiceUnavailable
		}
		writeJSON(w, code, state)
	})
	mux.Handle("GET /metrics", a.Freya.Metrics().Handler())
	a.admin = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8 << 10}
	return nil
}

// AdminAddr is the bound admin listener address.
func (a *App) AdminAddr() string { return a.adminLis.Addr().String() }

// Run serves until ctx is done, then stops listeners and waits for workers.
func (a *App) Run(ctx context.Context) error {
	a.ran = true
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	start := func(f func(context.Context)) {
		wg.Add(1)
		go func() { defer wg.Done(); f(wctx) }()
	}
	if v, ok := a.Verifier.(*authclient.Verifier); ok {
		start(func(ctx context.Context) {
			for ctx.Err() == nil {
				if err := v.Start(ctx, func(error) { a.Log.Warn("operator verification refresh unavailable") }); err == nil {
					return
				}
				if !pause(ctx, 2*time.Second) {
					return
				}
			}
		})
	}
	for _, w := range a.workers {
		start(w)
	}
	failed := make(chan error, len(a.servers)+1)
	serve := func(name string, srv *http.Server, lis net.Listener) {
		start(func(context.Context) {
			if err := srv.Serve(lis); err != nil && !errors.Is(err, http.ErrServerClosed) {
				failed <- fmt.Errorf("app: %s listener: %w", name, err)
			}
		})
	}
	serve("admin", a.admin, a.adminLis)
	for _, s := range a.servers {
		serve(s.name, s.srv, s.lis)
	}
	freyaDone := make(chan error, 1)
	go func() { freyaDone <- a.Freya.Run(wctx) }()
	var err error
	select {
	case err = <-freyaDone:
		cancel()
	case err = <-failed:
		cancel()
		<-freyaDone
	}
	a.shutdownServers()
	wg.Wait()
	return err
}

func (a *App) shutdownServers() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range append([]server{{"admin", a.admin, a.adminLis}}, a.servers...) {
		if s.srv != nil {
			_ = s.srv.Shutdown(ctx)
		}
		if s.lis != nil {
			_ = s.lis.Close()
		}
	}
}

// purgeRevocations drops expired Hermes token revocations hourly.
func (a *App) purgeRevocations(ctx context.Context) {
	for {
		if _, err := a.Repo.PurgeRevocations(ctx, time.Now()); err != nil && ctx.Err() == nil {
			a.Log.Warn("revocation purge failed; retrying next hour")
		}
		if !pause(ctx, time.Hour) {
			return
		}
	}
}

func (a *App) closeFreya() {
	if !a.ran {
		// Transports bind in freya.New; Run with a cancelled context drives
		// their shutdown before Close releases the runtime.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_ = a.Freya.Run(ctx)
	}
	a.Freya.Close()
}

// Close releases everything Build acquired (idempotent, reverse order).
func (a *App) Close() {
	a.close.Do(func() {
		if !a.ran {
			a.shutdownServers()
		}
		for i := len(a.closers) - 1; i >= 0; i-- {
			a.closers[i]()
		}
	})
}

func pause(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
