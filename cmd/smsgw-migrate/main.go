// Command smsgw-migrate imports a legacy (v3) sms-gw database snapshot into
// one V4 tenant:
//
//	smsgw-migrate -source-config deploy/legacy-import.dev.yaml -config deploy/dev.yaml -tenant fixture-tenant -dry-run
//	smsgw-migrate -source-config deploy/legacy-import.dev.yaml -config deploy/dev.yaml -tenant fixture-tenant -apply
//
// The source is opened read-only (one repeatable-read snapshot); its DSN is a
// secret reference in the source configuration, never a flag. The
// destination uses db.migrate_dsn (or db.dsn) and the KEK of the service
// configuration. The JSON report goes to stdout; the exit status is 0 when
// the run succeeded (or the snapshot was already applied), 1 otherwise.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/config"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/migrate"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/sealed"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/store"
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	fs := flag.NewFlagSet("smsgw-migrate", flag.ContinueOnError)
	srcPath := fs.String("source-config", "deploy/legacy-import.dev.yaml", "legacy import configuration (read-only source reference)")
	cfgPath := fs.String("config", "deploy/dev.yaml", "sms-gw service configuration (destination database and KEK)")
	tenant := fs.String("tenant", "", "destination tenant: a UUID or a name from the import configuration")
	dryRun := fs.Bool("dry-run", false, "validate and compare only; nothing is imported")
	apply := fs.Bool("apply", false, "import in one transaction and reconcile")
	actor := fs.String("platform-actor", "", "platform actor recorded for legacy admin sends (overrides platform_actor)")
	exclude := fs.Bool("exclude-orphans", false, "leave out (and list) records with missing references instead of failing")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dryRun == *apply {
		fmt.Fprintln(os.Stderr, "smsgw-migrate: exactly one of -dry-run or -apply is required")
		return 2
	}
	src, err := migrate.LoadSourceConfig(*srcPath)
	if err != nil {
		return fail(err)
	}
	tid, err := src.Tenant(*tenant)
	if err != nil {
		return fail(err)
	}
	if *actor != "" {
		src.PlatformActor = *actor
	}
	if len(src.PlatformActor) > 128 {
		return fail(errors.New("platform actor is longer than 128"))
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return fail(err)
	}
	if _, _, err := config.ApplyLegacyEnv(&cfg, os.LookupEnv); err != nil {
		return fail(err)
	}
	kek, err := sealed.LoadKEK(cfg.KEK.Source, cfg.KEK.Path, cfg.KEK.Env)
	if err != nil {
		return fail(err)
	}
	env, err := sealed.NewEnvelope(kek)
	if err != nil {
		return fail(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	dsn, err := src.Source.DSN.Read()
	if err != nil {
		return fail(err)
	}
	pc, err := pgx.ParseConfig(dsn)
	if err != nil {
		return fail(errors.New("source dsn is invalid"))
	}
	pc.RuntimeParams["default_transaction_read_only"] = "on"
	pc.RuntimeParams["application_name"] = "smsgw-migrate"
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	conn, err := pgx.ConnectConfig(cctx, pc)
	cancel()
	if err != nil {
		return fail(fmt.Errorf("source: %w", err))
	}
	defer conn.Close(context.Background())
	dstDSN := cfg.DB.MigrateDSN
	if dstDSN == "" {
		dstDSN = cfg.DB.DSN
	}
	dst, err := store.Open(ctx, dstDSN, 4)
	if err != nil {
		return fail(fmt.Errorf("destination: %w", err))
	}
	defer dst.Close()
	mode := migrate.ModeDryRun
	if *apply {
		mode = migrate.ModeApply
	}
	rep, err := migrate.Import(ctx, conn, dst, env, tid, mode, migrate.Options{PlatformActor: src.PlatformActor,
		ExcludeOrphans: src.ExcludeOrphans || *exclude})
	if err != nil {
		return fail(err)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(rep)
	if rep.Status == migrate.StatusFailed {
		return 1
	}
	return 0
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "smsgw-migrate:", err)
	return 1
}
