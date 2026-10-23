// Package parity is the live v3/v4 differential tester of the public Hermes
// API. It builds the legacy service (v3, read-only checkout next to this
// repository), runs it and V4 side by side, each on its own throwaway
// PostgreSQL, both against the legacy mock carrier, seeds identical data
// (v3 by SQL, V4 through cmd/smsgw-migrate as in production) and sends the
// same request corpus to both, diffing status, headers and body.
//
// The tests need the parity build tag and local PostgreSQL binaries; they
// never run under a plain `go test ./...`:
//
//	go test -tags parity -count=1 -v ./tests/parity/
//
// Environment: SMSGW_PARITY_V3_SRC (legacy checkout, default
// ../go-tangra-sms-gw), SMSGW_PARITY_PG_BIN (directory with initdb, pg_ctl
// and postgres; default ~/.cache/tangra-sdk/pg-16.9.0/bin or the PATH),
// SMSGW_PARITY_OUT (directory for the report, default a temporary one).
package parity
