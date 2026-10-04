// Package store owns the PostgreSQL pool, the embedded migrations and
// tenant-scoped transactions. Row-level security backs every explicit tenant
// predicate of the repositories.
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrations embed.FS

// migrateLock serialises concurrent migrators (several replicas starting).
const migrateLock = 7241011

// Errors.
var (
	ErrNotFound  = errors.New("store: not found")
	ErrConflict  = errors.New("store: conflict")
	ErrReference = errors.New("store: reference missing, in use or in another tenant")
	ErrInvalid   = errors.New("store: value violates a constraint")
	ErrTenant    = errors.New("store: invalid tenant scope")
)

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ValidTenant reports a canonical lower-case tenant UUID.
func ValidTenant(id string) bool { return uuidRE.MatchString(id) }

// Store is the pgx pool with tenant-scoped transactions.
type Store struct{ pool *pgxpool.Pool }

// Open connects with the application role.
func Open(ctx context.Context, dsn string, maxConns int32) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("store: invalid dsn")
	}
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Ping reports database reachability (readiness).
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Migrate applies the embedded migrations (migration role) under an advisory lock.
func Migrate(ctx context.Context, dsn string) error {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return errors.New("store: migrate: invalid dsn")
	}
	db := stdlib.OpenDB(*cfg)
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", migrateLock); err != nil {
		return fmt.Errorf("store: migrate lock: %w", err)
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", migrateLock) }()
	files, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return err
	}
	p, err := goose.NewProvider(goose.DialectPostgres, db, files)
	if err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	return nil
}

// Scope selects the row-level security settings of a transaction.
type Scope struct {
	TenantID string // app.tenant_id
	System   bool   // app.system: cross-tenant work only
}

// Tenant scopes a transaction to one tenant.
func Tenant(id string) Scope { return Scope{TenantID: id} }

// System is the cross-tenant scope (login, receipt resolution, retention).
func System() Scope { return Scope{System: true} }

// Tx runs fn in a transaction with the scope applied via set_config(local).
// A scope that is neither a valid tenant nor system is refused.
func (s *Store) Tx(ctx context.Context, scope Scope, fn func(pgx.Tx) error) error {
	if !scope.System && !ValidTenant(scope.TenantID) {
		return ErrTenant
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if scope.TenantID != "" {
		if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", scope.TenantID); err != nil {
			return err
		}
	}
	if scope.System {
		if _, err := tx.Exec(ctx, "SELECT set_config('app.system', 'on', true)"); err != nil {
			return err
		}
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// numericTables carry identity ids that imports may set explicitly.
var numericTables = []string{"sms_api_client", "sms_provider", "sms_template", "sms_block", "sms_dlr", "sms_login_log", "sms_audit"}

// SyncSequences advances every identity sequence past the largest stored id
// (after an import inserted explicit ids). It needs the migration role.
func (s *Store) SyncSequences(ctx context.Context) error {
	return s.Tx(ctx, System(), func(tx pgx.Tx) error {
		for _, t := range numericTables {
			q := fmt.Sprintf("SELECT setval(pg_get_serial_sequence('%[1]s', 'id'), COALESCE((SELECT max(id) FROM %[1]s), 1), (SELECT count(*) > 0 FROM %[1]s))", t)
			if _, err := tx.Exec(ctx, q); err != nil {
				return fmt.Errorf("store: sequence %s: %w", t, err)
			}
		}
		return nil
	})
}

// Classify maps PostgreSQL errors onto the store sentinels; other errors
// pass through unchanged.
func Classify(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "23505":
			return ErrConflict
		case "23503":
			return ErrReference
		case "23514", "23502", "22001", "22P02", "22003":
			return ErrInvalid
		}
	}
	return err
}
