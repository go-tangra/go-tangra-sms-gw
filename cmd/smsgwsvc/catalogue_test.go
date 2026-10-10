package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/config"
)

var placeholderRE = regexp.MustCompile(`\$\{([A-Z][A-Z0-9_]*)\}`)

// The join bundle's templates, rendered with values of the shape the gateway
// fills in, are a valid production configuration and policy.
func TestJoinBundleRendersToValidProductionConfig(t *testing.T) {
	values := map[string]string{
		"TRUST_DOMAIN": "infra.example.org", "GATEWAY_ISSUER": "https://portal.example.org:8443",
		"LCM_ENROLL_URL": "https://portal.example.org:8443/api/lcm/v1/enroll", "LCM_GRPC": "portal.example.org:9945",
		"GATEWAY_GRPC": "portal.example.org:9643", "AUTH_GRPC": "portal.example.org:9543", "MESH_TENANT_ID": "00000000-0000-0000-0000-000000000001",
		"GEN_PASSWORD_1": strings.Repeat("a", 48), "GEN_PASSWORD_2": strings.Repeat("b", 48),
	}
	dir := t.TempDir()
	for _, name := range []string{"config.yaml", "policy.yaml"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "bundle", name))
		if err != nil {
			t.Fatal(err)
		}
		var missing []string
		rendered := placeholderRE.ReplaceAllStringFunc(string(raw), func(m string) string {
			k := placeholderRE.FindStringSubmatch(m)[1]
			v, ok := values[k]
			if !ok {
				missing = append(missing, k)
			}
			return v
		})
		if len(missing) > 0 {
			t.Fatalf("%s uses placeholders the gateway does not fill: %v", name, missing)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(rendered), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := config.Load(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if errs := cfg.ValidateAll(); len(errs) > 0 {
		t.Fatalf("rendered config invalid: %v", errs)
	}
	if !cfg.IsProduction() || cfg.Gateway.Issuer != "https://portal.example.org:8443" {
		t.Fatalf("%+v", cfg.Gateway)
	}
	pol, _ := os.ReadFile(filepath.Join(dir, "policy.yaml"))
	if !strings.Contains(string(pol), "spiffe://infra.example.org/svc/gateway") {
		t.Fatal(string(pol))
	}
}
