//go:build integration

package migrate_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/app/apptest"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/migrate"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/repo"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/sealed"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/store"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/store/storetest"
)

const (
	tenantA      = "0b2f6a1e-4c55-4c8e-9d1a-000000000a0a"
	tenantB      = "0b2f6a1e-4c55-4c8e-9d1a-000000000b0b"
	actor        = "legacy-admin"
	carrierToken = "snapshot-carrier-token-not-production"
	dlrToken     = "snapshotdlrtoken0123456789abcdef"
	jwtSecret    = "snapshot-jwt-secret-0123456789abcdef-not-production"
	msg101       = "00000000-0000-4000-8000-000000000101"
	msg102       = "00000000-0000-4000-8000-000000000102"
)

type fixture struct {
	db     *storetest.DB
	owner  *store.Store // migration role
	legacy *pgx.Conn    // read-only snapshot connection
	admin  *pgx.Conn    // writes to the legacy fixture database (test only)
	env    *sealed.Envelope
	kek    []byte
}

// setup loads the captured legacy schema and the representative snapshot
// into a separate database next to a fresh, migrated destination.
func setup(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	db := storetest.Start(t)
	f := &fixture{db: db, kek: make([]byte, 32)}
	_, _ = rand.Read(f.kek)
	var err error
	if f.env, err = sealed.NewEnvelope(f.kek); err != nil {
		t.Fatal(err)
	}
	if f.owner, err = store.Open(ctx, db.OwnerDSN, 4); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.owner.Close)
	server, err := pgx.Connect(ctx, db.OwnerDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close(ctx)
	name := "legacy_" + strings.ReplaceAll(repo.NewID()[:13], "-", "")
	if _, err := server.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(db.OwnerDSN)
	u.Path = "/" + name
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), db.OwnerDSN)
		if err == nil {
			_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			_ = c.Close(context.Background())
		}
	})
	if f.admin, err = pgx.Connect(ctx, u.String()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.admin.Close(context.Background()) })
	for _, file := range []string{"runtime/legacy-schema.sql", "snapshot.sql"} {
		sql, err := os.ReadFile(filepath.Join(apptest.Root(), "tests/fixtures/legacy", file))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.admin.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
	}
	if _, err := f.admin.Exec(ctx, "SELECT pg_catalog.set_config('search_path', 'public', false)"); err != nil {
		t.Fatal(err)
	}
	cfg, _ := pgx.ParseConfig(u.String())
	cfg.RuntimeParams["default_transaction_read_only"] = "on"
	if f.legacy, err = pgx.ConnectConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.legacy.Close(context.Background()) })
	return f
}

func (f *fixture) run(t *testing.T, tenant, mode string, o migrate.Options) *migrate.Report {
	t.Helper()
	rep, err := migrate.Import(context.Background(), f.legacy, f.owner, f.env, tenant, mode, o)
	if err != nil {
		t.Fatalf("%s: %v", mode, err)
	}
	return rep
}

func (f *fixture) scalar(t *testing.T, q string, args ...any) string {
	t.Helper()
	var s string
	if err := f.owner.Tx(context.Background(), store.System(), func(tx pgx.Tx) error { return tx.QueryRow(context.Background(), q, args...).Scan(&s) }); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return s
}

func (f *fixture) legacyScalar(t *testing.T, q string) string {
	t.Helper()
	var s string
	if err := f.admin.QueryRow(context.Background(), q).Scan(&s); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return s
}

var sourceCounts = map[string]int{migrate.EntClients: 5, migrate.EntProviders: 4, migrate.EntTemplates: 4, migrate.EntBlocks: 4,
	migrate.EntMessages: 6, migrate.EntReceipts: 3, migrate.EntLogins: 5}

func noFailures(t *testing.T, rep *migrate.Report) {
	t.Helper()
	if len(rep.Errors) > 0 || len(rep.Conflicts) > 0 {
		b, _ := json.MarshalIndent(rep, "", " ")
		t.Fatalf("unexpected failures: %s", b)
	}
}

func TestImportDryRunApplyRerunReconcile(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	o := migrate.Options{PlatformActor: actor}

	dry := f.run(t, tenantA, migrate.ModeDryRun, o)
	noFailures(t, dry)
	if dry.Status != migrate.StatusSucceeded || dry.Reconciled || len(dry.Fingerprint) != 64 {
		t.Fatalf("dry-run %s reconciled=%v", dry.Status, dry.Reconciled)
	}
	for ent, n := range sourceCounts {
		if dry.Source[ent] != n || dry.Plan[ent].Insert != n || dry.Destination[ent] != 0 {
			t.Fatalf("dry-run %s: source %d plan %+v destination %d", ent, dry.Source[ent], dry.Plan[ent], dry.Destination[ent])
		}
	}
	if f.scalar(t, "SELECT count(*)::text FROM sms_import_run WHERE mode = 'dry_run' AND status = 'succeeded'") != "1" {
		t.Fatal("dry-run not recorded")
	}
	tr := dry.Transformations
	if tr["zero_provider_blocks_to_null"] != 2 || tr["zero_template_messages_to_null"] != 3 || tr["admin_sends_to_platform_actor"] != 2 ||
		tr["evidence_scrubbed"] != 1 || tr["evidence_decompressed"] != 2 || tr["unresolved_logins"] != 2 || tr["logins_of_missing_clients_unresolved"] != 1 ||
		tr["soft_deleted_rows_imported"] != 1 || tr["callback_secrets_sealed"] != 1 || tr["provider_configs_sealed"] != 4 {
		t.Fatalf("transformations %v", tr)
	}
	if b, _ := json.Marshal(dry); bytes.Contains(b, []byte(carrierToken)) || bytes.Contains(b, []byte(dlrToken)) || bytes.Contains(b, []byte("$2a$")) {
		t.Fatal("report leaks a secret or password hash")
	}

	apply := f.run(t, tenantA, migrate.ModeApply, o)
	noFailures(t, apply)
	if apply.Status != migrate.StatusSucceeded || !apply.Reconciled || apply.Fingerprint != dry.Fingerprint {
		t.Fatalf("apply %s reconciled=%v", apply.Status, apply.Reconciled)
	}
	for ent, n := range sourceCounts {
		if apply.Destination[ent] != n {
			t.Fatalf("destination %s = %d, want %d", ent, apply.Destination[ent], n)
		}
	}
	checkImported(t, f)

	again := f.run(t, tenantA, migrate.ModeApply, o)
	noFailures(t, again)
	if again.Status != migrate.StatusAlreadyApplied || again.AlreadyApplied != apply.RunID || !again.Reconciled {
		t.Fatalf("second apply %s %q", again.Status, again.AlreadyApplied)
	}
	for ent, n := range sourceCounts {
		if p := again.Plan[ent]; p.Insert != 0 || p.Update != 0 || p.Unchanged != n || again.Destination[ent] != n {
			t.Fatalf("second apply %s: %+v destination %d", ent, p, again.Destination[ent])
		}
	}
	if verify := f.run(t, tenantA, migrate.ModeDryRun, o); !verify.Reconciled || verify.Status != migrate.StatusSucceeded {
		t.Fatalf("dry-run after apply must reconcile: %+v", verify.Plan)
	}

	// A later snapshot of the same source (final snapshot before cutover):
	// the status change and the new receipt are applied, nothing duplicates.
	if _, err := f.admin.Exec(ctx, `UPDATE sms_message SET status_code = 1, status_message = 'sms_delivered', update_time = '2025-03-01T10:05:00Z' WHERE id = '`+msg102+`';
		INSERT INTO sms_dlr (id, create_time, update_time, channel, sid, status_text, message_status, recipient, sender, "timestamp", remote_address, parts_received, message_id)
		VALUES (44, '2025-03-01T10:05:00Z', '2025-03-01T10:05:00Z', 'sms', 9999, 'sms_delivered', 1, 359888000101, '', 1740823500, '192.0.2.50', 1, '`+msg102+`')`); err != nil {
		t.Fatal(err)
	}
	final := f.run(t, tenantA, migrate.ModeApply, o)
	noFailures(t, final)
	if final.Status != migrate.StatusSucceeded || final.Plan[migrate.EntMessages].Update != 1 || final.Plan[migrate.EntReceipts].Insert != 1 ||
		final.Destination[migrate.EntReceipts] != 4 || final.Destination[migrate.EntMessages] != 6 {
		t.Fatalf("final snapshot: %s %+v %+v", final.Status, final.Plan[migrate.EntMessages], final.Plan[migrate.EntReceipts])
	}

	hermes(t, f)

	// Destination traffic after cutover: a re-import is refused.
	r := f.db.Repo()
	if _, err := r.CreateMessage(ctx, repo.Message{ID: repo.NewID(), TenantID: tenantA, Actor: repo.ClientActor(1), Recipient: "359888000999", ProviderID: 10, Text: "new"}); err != nil {
		t.Fatal(err)
	}
	refused := f.run(t, tenantA, migrate.ModeApply, o)
	if refused.Status != migrate.StatusFailed || len(refused.Conflicts) == 0 || !strings.Contains(refused.Conflicts[0].Reason, "not in the snapshot") {
		t.Fatalf("re-import after new traffic must be refused: %+v", refused.Conflicts)
	}

	// Sequences continue after the imported maxima.
	c, err := r.CreateClient(ctx, repo.APIClient{TenantID: tenantA, Username: "after_import", PasswordHash: "h", Authority: "API_CLIENT", Status: repo.On})
	if err != nil || c.ID != 8 {
		t.Fatalf("next client id %d: %v", c.ID, err)
	}
	b, err := r.CreateBlock(ctx, repo.Block{TenantID: tenantA, Recipient: "359888000777", BlockType: repo.ObjectSMS, Status: repo.On})
	if err != nil || b.ID != 34 {
		t.Fatalf("next block id %d: %v", b.ID, err)
	}
}

// checkImported reconciles the destination against the legacy rows column
// by column for what the import must preserve, and checks what it must
// transform (sealing, scrubbing, NULL references, platform actor).
func checkImported(t *testing.T, f *fixture) {
	t.Helper()
	ctx := context.Background()
	same := func(what, legacyQ, destQ string) {
		t.Helper()
		if l, d := f.legacyScalar(t, legacyQ), f.scalar(t, destQ); l != d {
			t.Fatalf("%s differs:\nlegacy      %s\ndestination %s", what, l, d)
		}
	}
	same("clients", `SELECT string_agg(concat_ws('|', id, username, password_hash, coalesce(email, ''), authority, status, last_login_time, coalesce(last_login_ip, ''),
		coalesce(dlr_callback_url, ''), create_time, update_time), ';' ORDER BY id) FROM sms_api_client`,
		`SELECT string_agg(concat_ws('|', id, username, password_hash, email, authority, status, last_login_time, last_login_ip, dlr_callback_url,
		create_time, update_time), ';' ORDER BY id) FROM sms_api_client WHERE tenant_id = '`+tenantA+`'`)
	same("providers", `SELECT string_agg(concat_ws('|', id, name, type, object_type, status, retention_days, create_time, update_time), ';' ORDER BY id) FROM sms_provider`,
		`SELECT string_agg(concat_ws('|', id, name, type, object_type, status, retention_days, create_time, update_time), ';' ORDER BY id) FROM sms_provider WHERE tenant_id = '`+tenantA+`'`)
	same("templates", `SELECT string_agg(concat_ws('|', id, name, object_type, templates::text, status, create_time, update_time), ';' ORDER BY id) FROM sms_template`,
		`SELECT string_agg(concat_ws('|', id, name, object_type, templates::text, status, create_time, update_time), ';' ORDER BY id) FROM sms_template WHERE tenant_id = '`+tenantA+`'`)
	same("blocks", `SELECT string_agg(concat_ws('|', id, recipient, coalesce(description, ''), coalesce(nullif(provider_id, 0)::text, 'null'), block_type, status, create_time, update_time), ';' ORDER BY id) FROM sms_block`,
		`SELECT string_agg(concat_ws('|', id, recipient, description, coalesce(provider_id::text, 'null'), block_type, status, create_time, update_time), ';' ORDER BY id) FROM sms_block WHERE tenant_id = '`+tenantA+`'`)
	same("messages", `SELECT string_agg(concat_ws('|', id, coalesce(nullif(create_by, 0)::text, 'platform'), sid, recipient, priority, provider_id, coalesce(nullif(template_id, 0)::text, 'null'),
		coalesce(user_name, ''), coalesce(data::text, 'null'), coalesce(dlr_ts, 0), status_code, message, coalesce(status_message, ''), coalesce(remote_address, ''), create_time, update_time), ';' ORDER BY id) FROM sms_message`,
		`SELECT string_agg(concat_ws('|', id, coalesce(api_client_id::text, actor_kind), sid, recipient, priority, provider_id, coalesce(template_id::text, 'null'),
		user_name, coalesce(data::text, 'null'), dlr_ts, status_code, message, status_message, remote_address, create_time, update_time), ';' ORDER BY id) FROM sms_message WHERE tenant_id = '`+tenantA+`'`)
	same("receipts", `SELECT string_agg(concat_ws('|', id, message_id, channel, sid, coalesce(status_text, ''), message_status, recipient, sender, "timestamp", coalesce(remote_address, ''), parts_received, create_time, update_time), ';' ORDER BY id) FROM sms_dlr`,
		`SELECT string_agg(concat_ws('|', id, message_id, channel, sid, status_text, message_status, recipient, sender, "timestamp", remote_address, parts_received, create_time, update_time), ';' ORDER BY id) FROM sms_dlr WHERE tenant_id = '`+tenantA+`'`)
	same("logins", `SELECT string_agg(concat_ws('|', id, CASE WHEN client_id IN (SELECT id FROM sms_api_client) THEN client_id::text ELSE 'unresolved' END, username, event_time, success,
		coalesce(error_message, ''), coalesce(login_ip, ''), coalesce(user_agent, '')), ';' ORDER BY id) FROM sms_login_log`,
		`SELECT string_agg(concat_ws('|', id, coalesce(client_id::text, 'unresolved'), username, event_time, success, error_message, login_ip, user_agent), ';' ORDER BY id) FROM sms_login_log`)
	if f.scalar(t, "SELECT string_agg(platform_actor, ',' ORDER BY id) FROM sms_message WHERE actor_kind = 'platform'") != actor+","+actor {
		t.Fatal("legacy admin sends must carry the platform actor")
	}
	if f.scalar(t, "SELECT count(*)::text FROM sms_login_log WHERE tenant_id IS NULL") != "3" {
		t.Fatal("unresolved login records must keep no tenant")
	}

	r := f.db.Repo()
	p, err := r.GetProvider(ctx, tenantA, 10)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := f.env.OpenConfig(p.ConfigSealed, sealed.ProviderAD(tenantA, 10))
	if err != nil || cfg["token"] != carrierToken || cfg["dlr_token"] != dlrToken || len(cfg) != 7 {
		t.Fatalf("provider configuration must reseal in full: %v", err)
	}
	if pub, _ := json.Marshal(p.ConfigPublic); bytes.Contains(pub, []byte(carrierToken)) || bytes.Contains(pub, []byte(dlrToken)) || !bytes.Contains(pub, []byte(sealed.Marker)) {
		t.Fatalf("public configuration leaks: %s", pub)
	}
	if _, err := f.env.OpenConfig(p.ConfigSealed, sealed.ProviderAD(tenantB, 10)); err == nil {
		t.Fatal("sealed configuration must be bound to the tenant")
	}
	c, err := r.GetClient(ctx, tenantA, 1)
	if err != nil {
		t.Fatal(err)
	}
	if s, err := f.env.OpenString(c.CallbackSecretSealed, sealed.CallbackAD(tenantA, 1)); err != nil || s != "snapshot-callback-secret-not-production" {
		t.Fatalf("callback secret: %v", err)
	}
	m, err := r.GetMessage(ctx, repo.TenantView(tenantA), msg101)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range [][]byte{m.RawRequest, m.RawResponse} {
		if len(ev) == 0 || bytes.Contains(ev, []byte(carrierToken)) || bytes.Contains(ev, []byte(dlrToken)) {
			t.Fatalf("evidence must be stored decompressed and scrubbed: %q", ev)
		}
	}
	if !bytes.Contains(m.RawRequest, []byte("Hello Ana, code 42.")) || !bytes.HasPrefix(m.RawResponse, []byte("HTTP/1.1 200 OK")) {
		t.Fatalf("evidence content lost: %q", m.RawRequest)
	}
}

// hermes authenticates imported clients on the public listener of a V4
// instance over the imported database, reads historical receipts and
// accepts a token the legacy service issued with the preserved secret.
func hermes(t *testing.T, f *fixture) {
	t.Helper()
	run := apptest.Start(t, apptest.Options{DSN: f.db.AppDSN, KEK: f.kek, JWTSecret: jwtSecret})
	defer run.Stop()
	call := func(method, path, token, body string) (int, map[string]any) {
		req, _ := http.NewRequest(method, run.Public+path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		out := map[string]any{}
		_ = json.Unmarshal(raw, &out)
		return resp.StatusCode, out
	}
	code, out := call("POST", "/hermes/v1/login", "", `{"username":"client_a","password":"Passw0rd!"}`)
	if code != 200 || out["access_token"] == nil {
		t.Fatalf("imported client login: %d %v", code, out)
	}
	tok := out["access_token"].(string)
	code, out = call("GET", "/hermes/v1/sms/dlr/"+msg101, tok, "")
	if items, _ := out["items"].([]any); code != 200 || len(items) != 2 {
		t.Fatalf("historical receipts: %d %v", code, out)
	}
	if code, out = call("GET", "/hermes/v1/sms/"+msg101, tok, ""); code != 200 || out["data"].(map[string]any)["status"] != float64(1) {
		t.Fatalf("historical message: %d %v", code, out)
	}
	if code, out = call("POST", "/hermes/v1/login", "", `{"username":"disabled_d","password":"Passw0rd!"}`); code != 401 || out["message"] != "account disabled" {
		t.Fatalf("disabled client: %d %v", code, out)
	}
	now := time.Now()
	legacy := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"authority": "API_CLIENT", "kind": "access", "username": "client_b", "iss": "sms-gw",
		"sub": "2", "iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(time.Hour).Unix(), "jti": "0123456789abcdef0123456789abcdef"})
	signed, err := legacy.SignedString([]byte(jwtSecret))
	if err != nil {
		t.Fatal(err)
	}
	if code, out = call("GET", "/hermes/v1/me", signed, ""); code != 200 || out["user"].(map[string]any)["username"] != "client_b" {
		t.Fatalf("legacy token with the preserved secret: %d %v", code, out)
	}
}

func TestImportRefusesConflictsAndOrphans(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	o := migrate.Options{PlatformActor: actor}

	if rep := f.run(t, tenantA, migrate.ModeDryRun, migrate.Options{}); rep.Status != migrate.StatusFailed || len(rep.Errors) != 2 {
		t.Fatalf("admin sends without a platform actor must fail: %+v", rep.Errors)
	}
	if _, err := migrate.Import(ctx, f.legacy, f.owner, f.env, "fixture-tenant", migrate.ModeApply, o); err == nil {
		t.Fatal("a tenant that is not a UUID must be refused")
	}

	// Another tenant holds a snapshot id and a snapshot username.
	r := f.db.Repo()
	if _, err := r.CreateClient(ctx, repo.APIClient{ID: 1, TenantID: tenantB, Username: "someone", PasswordHash: "h", Authority: "API_CLIENT", Status: repo.On}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CreateClient(ctx, repo.APIClient{ID: 100, TenantID: tenantB, Username: "client_b", PasswordHash: "h", Authority: "API_CLIENT", Status: repo.On}); err != nil {
		t.Fatal(err)
	}
	rep := f.run(t, tenantA, migrate.ModeApply, o)
	if rep.Status != migrate.StatusFailed || len(rep.Conflicts) != 2 || rep.Destination[migrate.EntProviders] != 0 {
		t.Fatalf("conflicts must refuse the apply before writing: %+v", rep.Conflicts)
	}
	for _, id := range []int64{1, 100} {
		if err := r.DeleteClient(ctx, tenantB, id); err != nil {
			t.Fatal(err)
		}
	}

	// Orphans fail by default and are excluded (listed) on request.
	// (The legacy schema has no provider foreign key; receipts lose their
	// message as NULL when it is deleted.)
	if _, err := f.admin.Exec(ctx, `INSERT INTO sms_message (id, create_by, create_time, update_time, sid, recipient, provider_id, message)
		VALUES ('00000000-0000-4000-8000-0000000001ff', 1, now(), now(), 1, '359888000900', 77, 'orphan');
		INSERT INTO sms_dlr (id, create_time, update_time, channel, sid, message_status, recipient, sender, "timestamp", parts_received, message_id)
		VALUES (60, now(), now(), 'sms', 1, 2, 1, '', 0, 1, NULL), (61, now(), now(), 'sms', 1, 2, 1, '', 0, 1, '00000000-0000-4000-8000-0000000001ff')`); err != nil {
		t.Fatal(err)
	}
	if rep := f.run(t, tenantA, migrate.ModeDryRun, o); rep.Status != migrate.StatusFailed || len(rep.Errors) != 2 {
		t.Fatalf("orphans must fail the dry-run: %+v", rep.Errors)
	}
	rep = f.run(t, tenantA, migrate.ModeApply, migrate.Options{PlatformActor: actor, ExcludeOrphans: true})
	noFailures(t, rep)
	if rep.Status != migrate.StatusSucceeded || len(rep.Excluded) != 3 || rep.Destination[migrate.EntReceipts] != 3 || rep.Destination[migrate.EntMessages] != 6 {
		t.Fatalf("excluded orphans: %+v", rep.Excluded)
	}

	// A different KEK cannot verify (or silently overwrite) sealed values.
	other := make([]byte, 32)
	_, _ = rand.Read(other)
	wrong, _ := sealed.NewEnvelope(other)
	rep, err := migrate.Import(ctx, f.legacy, f.owner, wrong, tenantA, migrate.ModeDryRun, migrate.Options{PlatformActor: actor, ExcludeOrphans: true})
	if err != nil || rep.Status != migrate.StatusFailed || !strings.Contains(rep.Conflicts[0].Reason, "does not open with the configured KEK") {
		t.Fatalf("wrong KEK: %v %+v", err, rep)
	}
}

func TestImportNeverWritesTheSource(t *testing.T) {
	f := setup(t)
	before := f.legacyScalar(t, "SELECT md5(string_agg(t::text, ';' ORDER BY t::text)) FROM (SELECT * FROM sms_message) t")
	f.run(t, tenantA, migrate.ModeApply, migrate.Options{PlatformActor: actor})
	if after := f.legacyScalar(t, "SELECT md5(string_agg(t::text, ';' ORDER BY t::text)) FROM (SELECT * FROM sms_message) t"); after != before {
		t.Fatal("the source changed")
	}
	if _, err := f.legacy.Exec(context.Background(), "UPDATE sms_message SET status_code = 0"); err == nil {
		t.Fatal("the source connection must be read-only")
	}
}
