//go:build integration

package main

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/go-tangra/go-tangra/v4/preflight"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/store/storetest"
)

// TestPreflightDatabase runs the database probe against a real server:
// success, a wrong password, a missing database and a server without TLS
// are told apart, and the password is never reported.
func TestPreflightDatabase(t *testing.T) {
	db := storetest.Start(t)
	with := func(mut func(*url.URL)) string {
		u, err := url.Parse(db.AppDSN)
		if err != nil {
			t.Fatal(err)
		}
		mut(u)
		return u.String()
	}
	cases := []struct {
		name, dsn string
		status    preflight.Status
		detail    string
	}{
		{"connects", db.AppDSN, preflight.Pass, "SELECT 1 ok"},
		{"wrong password", with(func(u *url.URL) { u.User = url.UserPassword(u.User.Username(), "not-the-password") }), preflight.Fail, "authentication failed"},
		{"missing database", with(func(u *url.URL) { u.Path = "/no_such_db" }), preflight.Fail, "does not exist"},
		{"server without tls", with(func(u *url.URL) {
			q := u.Query()
			q.Set("sslmode", "require")
			u.RawQuery = q.Encode()
		}), preflight.Fail, "TLS"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := dbCheck("database: db.dsn", tc.dsn).Run(context.Background())
			if tc.name == "server without tls" && r.Status == preflight.Pass {
				t.Skip("this server offers TLS")
			}
			if r.Status != tc.status || !strings.Contains(r.Detail, tc.detail) {
				t.Fatalf("%+v", r)
			}
			if strings.Contains(r.Detail, "not-the-password") {
				t.Fatal("password reported")
			}
		})
	}
}
