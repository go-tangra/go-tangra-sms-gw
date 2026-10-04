package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/config"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/sealed"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/store"
)

// bootstrap prepares a deployment and exits: it validates the configuration,
// applies the migrations with the migration role, grants the application
// role its rights, checks that the application role cannot bypass
// row-level security, round-trips the KEK and reads the Hermes JWT secret.
// It is idempotent and creates no accounts, providers or other data.
func bootstrap(args []string) int {
	fs := flag.NewFlagSet("smsgwsvc bootstrap", flag.ContinueOnError)
	path := fs.String("config", "deploy/dev.yaml", "configuration file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return fail(err)
	}
	if _, _, err := config.ApplyLegacyEnv(&cfg, os.LookupEnv); err != nil {
		return fail(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	res, err := prepare(ctx, cfg)
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(res)
	if err != nil {
		return fail(err)
	}
	return 0
}

// Prepared is the bootstrap report (no secrets).
type Prepared struct {
	Version     string `json:"version"`
	Migrated    int    `json:"migrations_applied"`
	Schema      int64  `json:"schema_version"`
	AppRole     string `json:"app_role"`
	RowSecurity string `json:"row_level_security"` // enforced | bypassed
	KEK         string `json:"kek"`
	JWTSecret   string `json:"jwt_secret"`
	Accounts    int    `json:"accounts_created"`
}

func prepare(ctx context.Context, cfg config.Config) (Prepared, error) {
	res := Prepared{Version: version}
	if err := cfg.Validate(); err != nil {
		return res, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	migrateDSN := cfg.DB.MigrateDSN
	if migrateDSN == "" {
		migrateDSN = cfg.DB.DSN
	}
	var err error
	if res.Migrated, res.Schema, err = store.MigrateVersion(ctx, migrateDSN); err != nil {
		return res, err
	}
	role, err := dsnUser(cfg.DB.DSN)
	if err != nil {
		return res, err
	}
	if migrateDSN != cfg.DB.DSN {
		if err := store.GrantApp(ctx, migrateDSN, role); err != nil {
			return res, err
		}
	}
	st, err := store.Open(ctx, cfg.DB.DSN, 2)
	if err != nil {
		return res, err
	}
	defer st.Close()
	name, bypass, err := st.Role(ctx)
	if err != nil {
		return res, fmt.Errorf("bootstrap: application role: %w", err)
	}
	res.AppRole, res.RowSecurity = name, "enforced"
	if bypass {
		res.RowSecurity = "bypassed"
		if cfg.IsProduction() {
			return res, errors.New("bootstrap: the application role bypasses row-level security (superuser or BYPASSRLS); use a dedicated role")
		}
	}
	if err := st.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "SELECT 1 FROM sms_provider LIMIT 1")
		return err
	}); err != nil {
		return res, fmt.Errorf("bootstrap: application role cannot read the schema: %w", err)
	}
	kek, err := sealed.LoadKEK(cfg.KEK.Source, cfg.KEK.Path, cfg.KEK.Env)
	if err != nil {
		return res, err
	}
	env, err := sealed.NewEnvelope(kek)
	if err != nil {
		return res, err
	}
	blob, err := env.Seal([]byte("probe"), []byte("sms-gw:bootstrap"))
	if err == nil {
		_, err = env.Open(blob, []byte("sms-gw:bootstrap"))
	}
	if err != nil {
		return res, fmt.Errorf("bootstrap: kek: %w", err)
	}
	res.KEK = "ok"
	secret, err := cfg.PublicAuth.JWTSecret.Read()
	if err != nil {
		return res, err
	}
	if len(secret) < 32 {
		return res, errors.New("bootstrap: public_auth.jwt_secret must be at least 32 bytes")
	}
	res.JWTSecret = "ok"
	return res, nil
}

func dsnUser(dsn string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil || u.User.Username() == "" {
		return "", errors.New("bootstrap: db.dsn must be a postgres:// URL naming the application role")
	}
	return u.User.Username(), nil
}
