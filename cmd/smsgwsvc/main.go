// Command smsgwsvc runs the sms-gw module: the V4 management API on the
// mesh and the legacy Hermes edge on its own listener. `smsgwsvc bootstrap
// -config <file>` prepares the database and checks the deployment secrets;
// `smsgwsvc version` prints the build version.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/app"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/config"
	"github.com/go-tangra/go-tangra-sms-gw/v4/ui"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "bootstrap" {
		os.Exit(bootstrap(os.Args[2:]))
	}
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("smsgwsvc", flag.ContinueOnError)
	path := fs.String("config", "deploy/dev.yaml", "configuration file")
	noMigrate := fs.Bool("no-migrate", false, "do not apply database migrations on start")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return fail(err)
	}
	applied, obsolete, err := config.ApplyLegacyEnv(&cfg, os.LookupEnv)
	if err != nil {
		return fail(err)
	}
	if len(applied) > 0 {
		fmt.Fprintln(os.Stderr, "smsgwsvc: legacy environment applied:", strings.Join(applied, ", "))
	}
	if len(obsolete) > 0 {
		fmt.Fprintln(os.Stderr, "smsgwsvc: obsolete legacy environment ignored:", strings.Join(obsolete, ", "))
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	opts := app.Options{Migrate: !*noMigrate}
	if remote, ok := ui.Remote(); ok {
		opts.Remote = remote
	}
	a, err := app.Build(ctx, cfg, opts)
	if err != nil {
		return fail(err)
	}
	defer a.Close()
	if err := a.Run(ctx); err != nil {
		return fail(err)
	}
	return 0
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "smsgwsvc:", err)
	return 1
}
