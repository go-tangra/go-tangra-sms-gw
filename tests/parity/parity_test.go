//go:build parity

package parity

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	. "github.com/go-tangra/go-tangra-sms-gw/v4/internal/parity"
)

// preFlood logs in fresh pairs for the rate-limit steps (earlier steps
// revoked some of the corpus tokens).
func preFlood(suffix string) []Step {
	return []Step{
		{Name: "ratelimit-prelogin-c-" + suffix, Group: "ratelimit", Req: post("/hermes/v1/login", login("client_c")), Capture: captureLogin("c2")},
		{Name: "ratelimit-prelogin-b-" + suffix, Group: "ratelimit", Req: post("/hermes/v1/login", login("client_b")), Capture: captureLogin("b2")},
		{Name: "ratelimit-prelogin-v-" + suffix, Group: "ratelimit", Req: post("/hermes/v1/login", login("viewer_v")), Capture: captureLogin("v2")},
	}
}

// TestParity runs the corpus against the legacy service (v3.0.9 unless
// SMSGW_PARITY_V3_REF names another ref) and V4 with production rate
// limits, then the rate-limit steps again with both services' defaults.
func TestParity(t *testing.T) {
	s := newStack(t)
	s.startV3(profProd)
	s.seedV3()
	s.importV4()
	v4 := s.startV4(profProd)
	a, b := NewSide("v3", s.v3URL()), NewSide("v4", v4)
	for k, v := range forged() {
		a.Set(k, v)
		b.Set(k, v)
	}
	outs := Run(corpus(), a, b, KnownDifferences)

	// The read-only live tool against the same pair, with a v3-issued token.
	// It runs before the floods: those drain the per-IP login bucket, and a
	// 429 here would leave no token.
	tok, ok := a.Do(post("/hermes/v1/login", login("client_a"))).JSON()["access_token"].(string)
	if !ok {
		t.Fatal("login for the live tool returned no access_token")
	}
	for _, mode := range [][]string{{"SMS_PARITY_BEARER=" + tok}, {"SMS_PARITY_BEARER="}} {
		args := []string{"-baseline-url", a.Base, "-candidate-url", b.Base}
		if mode[0] == "SMS_PARITY_BEARER=" {
			args = append(args, "-allow-anon")
		}
		cmd := exec.Command(s.liveBin, args...)
		cmd.Env = append(os.Environ(), mode...)
		out, err := cmd.CombinedOutput()
		t.Logf("smsgw-parity %v:\n%s", args, out)
		if err != nil {
			t.Errorf("smsgw-parity %v: %v", args, err)
		}
	}
	steps := append(preFlood(profProd.name), floods(profProd)...)
	outs = append(outs, Run(steps, a, b, KnownDifferences)...)

	// Defaults: restart both with their built-in limits and no trusted proxy.
	s.stopV3()
	s.stopV4()
	s.startV3(profDefaults)
	b.Base = s.startV4(profDefaults)
	steps = append(preFlood(profDefaults.name), floods(profDefaults)...)
	outs = append(outs, Run(steps, a, b, KnownDifferences)...)

	var buf bytes.Buffer
	failures := Report(&buf, outs)
	dir := os.Getenv("SMSGW_PARITY_OUT")
	if dir == "" {
		dir = t.TempDir()
	}
	path := filepath.Join(dir, "parity-report.txt")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	var full []map[string]any
	for _, o := range outs {
		{
			full = append(full, map[string]any{"case": o.Name, "diffs": o.Diffs, "request": o.Req.Method + " " + o.Req.Path, "requestBody": o.Req.Body,
				"v3": map[string]any{"status": o.V3.Status, "headers": o.H3, "body": o.N3}, "v4": map[string]any{"status": o.V4.Status, "headers": o.H4, "body": o.N4}})
		}
	}
	raw, _ := json.MarshalIndent(full, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "parity-outcomes.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("report: %s\n%s", path, buf.String())
	if failures > 0 {
		t.Errorf("%d unexpected or must-fix differences (report %s)", failures, path)
	}
}
