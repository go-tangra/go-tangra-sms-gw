//go:build integration

package main

import (
	"context"
	"encoding/base64"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/app/apptest"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/config"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/repo"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/store/storetest"
)

// TestBootstrapIsIdempotentAndCreatesNoData bootstraps an empty database
// twice: the first run migrates and grants the application role, the second
// changes nothing; no account or other record appears.
func TestBootstrapIsIdempotentAndCreatesNoData(t *testing.T) {
	ctx := context.Background()
	server := storetest.Start(t) // provides the server and the smsgw_app role
	admin, err := pgx.Connect(ctx, server.OwnerDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	name := "boot_" + strings.ReplaceAll(repo.NewID()[:13], "-", "")
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)") })
	dsn := func(base string) string {
		u, _ := url.Parse(base)
		u.Path = "/" + name
		return u.String()
	}

	c, err := config.Load(filepath.Join(apptest.Root(), "deploy", "dev.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	kek, jwt := filepath.Join(dir, "kek"), filepath.Join(dir, "jwt")
	if err := os.WriteFile(kek, []byte(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jwt, []byte(strings.Repeat("j", 40)), 0o600); err != nil {
		t.Fatal(err)
	}
	c.KEK.Path, c.PublicAuth.JWTSecret = kek, config.SecretRef{File: jwt}
	c.DB.DSN, c.DB.MigrateDSN = dsn(server.AppDSN), dsn(server.OwnerDSN)

	first, err := prepare(ctx, c)
	if err != nil {
		t.Fatalf("%+v: %v", first, err)
	}
	if first.Migrated != 1 || first.Schema != 1 || first.AppRole != "smsgw_app" || first.RowSecurity != "enforced" || first.KEK != "ok" || first.JWTSecret != "ok" {
		t.Fatalf("first bootstrap %+v", first)
	}
	second, err := prepare(ctx, c)
	if err != nil || second.Migrated != 0 || second.Schema != 1 {
		t.Fatalf("second bootstrap %+v: %v", second, err)
	}
	conn, err := pgx.Connect(ctx, dsn(server.OwnerDSN))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var rows int
	if err := conn.QueryRow(ctx, `SELECT (SELECT count(*) FROM sms_api_client) + (SELECT count(*) FROM sms_provider) + (SELECT count(*) FROM sms_template)
		+ (SELECT count(*) FROM sms_block) + (SELECT count(*) FROM sms_message)`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("bootstrap created %d records: %v", rows, err)
	}

	// The superuser as application role is reported (and refused in
	// production, whose configuration this workstation cannot satisfy).
	c.DB.DSN = dsn(server.OwnerDSN)
	if res, err := prepare(ctx, c); err != nil || res.RowSecurity != "bypassed" {
		t.Fatalf("%+v: %v", res, err)
	}
}
