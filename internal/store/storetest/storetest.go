//go:build integration

// Package storetest starts an isolated, migrated PostgreSQL database for
// integration tests: SMSGW_TEST_PG_DSN names an existing server (a fresh
// database is created on it), otherwise a postgres:16 container is started.
// Tests skip when neither is available.
package storetest

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/repo"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/store"
)

// DB is one migrated database.
type DB struct {
	OwnerDSN string // migration role (superuser)
	AppDSN   string // smsgw_app: no BYPASSRLS, as in production
	Store    *store.Store
}

// Repo returns the repository over the application role.
func (d *DB) Repo() *repo.Postgres { return repo.NewPostgres(d.Store, 50, 500) }

// Start returns a fresh migrated database.
func Start(t testing.TB) *DB {
	t.Helper()
	ctx := context.Background()
	server := os.Getenv("SMSGW_TEST_PG_DSN")
	if server == "" {
		c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{Started: true, ContainerRequest: testcontainers.ContainerRequest{
			Image: "postgres:16", ExposedPorts: []string{"5432/tcp"}, Env: map[string]string{"POSTGRES_PASSWORD": "test"},
			WaitingFor: wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(2 * time.Minute)}})
		if err != nil {
			t.Skipf("postgres unavailable (set SMSGW_TEST_PG_DSN or provide docker): %v", err)
		}
		t.Cleanup(func() { _ = testcontainers.TerminateContainer(c) })
		host, _ := c.Host(ctx)
		port, err := c.MappedPort(ctx, "5432/tcp")
		if err != nil {
			t.Fatal(err)
		}
		server = "postgres://postgres:test@" + host + ":" + port.Port() + "/postgres?sslmode=disable"
	}
	admin := connect(t, server)
	defer admin.Close(ctx)
	name := "smsgw_" + strings.ReplaceAll(repo.NewID()[:13], "-", "")
	for _, q := range []string{"CREATE DATABASE " + name,
		"DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'smsgw_app') THEN CREATE ROLE smsgw_app LOGIN PASSWORD 'app' NOBYPASSRLS; END IF; END $$"} {
		if _, err := admin.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		conn := connect(t, server)
		defer conn.Close(context.Background())
		_, _ = conn.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	d := &DB{OwnerDSN: withDB(t, server, name, ""), AppDSN: withDB(t, server, name, "smsgw_app:app")}
	if err := store.Migrate(ctx, d.OwnerDSN); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, d.AppDSN, 8)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	d.Store = st
	return d
}

func connect(t testing.TB, dsn string) *pgx.Conn {
	t.Helper()
	var last error
	for i := 0; i < 60; i++ {
		conn, err := pgx.Connect(context.Background(), dsn)
		if err == nil {
			return conn
		}
		last = err
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("postgres: %v", last)
	return nil
}

func withDB(t testing.TB, dsn, name, userinfo string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	if userinfo != "" {
		user, pass, _ := strings.Cut(userinfo, ":")
		u.User = url.UserPassword(user, pass)
	}
	return u.String()
}
