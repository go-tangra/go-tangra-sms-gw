// Command smsgw-parity compares two running sms-gw deployments, the legacy
// baseline (v3) and the V4 candidate, over the public Hermes API with a
// read-only request corpus, and prints the difference table:
//
//	SMS_PARITY_BEARER=<v3-issued access token> smsgw-parity -baseline-url http://sms-gw-v3:9901 -candidate-url http://sms-gw-v4:9901
//	smsgw-parity -baseline-url ... -candidate-url ... -allow-anon
//	smsgw-parity -json-style /path/to/legacy/binary
//
// It never sends an SMS, logs in, posts a carrier receipt or logs out the
// given token (see parity.LiveSteps). The bearer token must be valid on both
// (the deployments share the JWT secret); without one, -allow-anon runs only
// the unauthenticated probes. The exit status is 1 when a difference is
// neither documented nor a legacy defect (parity.KnownDifferences), 2 on a
// usage error. -json-style prints the protobuf-go JSON flavour of a legacy
// binary, the value for V4's public.legacy_json_style. Build it static
// (CGO_ENABLED=0) to run it from a minimal container on the service network.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/parity"
)

func main() { os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr)) }

func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("smsgw-parity", flag.ContinueOnError)
	fs.SetOutput(stderr)
	baseline := fs.String("baseline-url", "", "legacy (v3) public base URL, e.g. http://sms-gw:9901")
	candidate := fs.String("candidate-url", "", "V4 public base URL")
	anon := fs.Bool("allow-anon", false, "run only the unauthenticated probes when SMS_PARITY_BEARER is unset")
	style := fs.String("json-style", "", "print the protobuf-go JSON style (spaced|compact) of this legacy binary and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *style != "" {
		s, err := parity.LegacyJSONStyle(*style)
		if err != nil {
			fmt.Fprintln(stderr, "smsgw-parity:", err)
			return 2
		}
		fmt.Fprintln(stdout, s)
		return 0
	}
	if *baseline == "" || *candidate == "" {
		fmt.Fprintln(stderr, "smsgw-parity: -baseline-url and -candidate-url are required")
		return 2
	}
	bearer := getenv("SMS_PARITY_BEARER")
	if bearer == "" && !*anon {
		fmt.Fprintln(stderr, "smsgw-parity: set SMS_PARITY_BEARER (a v3-issued access token) or pass -allow-anon")
		return 2
	}
	a, b := parity.NewSide("v3", *baseline), parity.NewSide("v4", *candidate)
	if bearer != "" {
		a.Set("lit:bearer", bearer)
		b.Set("lit:bearer", bearer)
	}
	outs := parity.Run(parity.LiveSteps(bearer != ""), a, b, parity.KnownDifferences)
	if parity.Report(stdout, outs) > 0 {
		return 1
	}
	return 0
}
