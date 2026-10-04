package smsgwmanifest_test

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/go-tangra/go-tangra-sms-gw/v4/pkg/smsgwmanifest"
)

// TestRoles checks the module roles of contracts/management-api.md.
func TestRoles(t *testing.T) {
	want := map[string][]string{
		"administrator": smsgwmanifest.PermissionRefs(),
		"sender":        {"providers:read", "templates:read", "messages:send", "messages:read"},
		"viewer":        {"providers:read", "templates:read", "messages:read"},
		"monitoring":    {"dashboard:read"},
	}
	if len(smsgwmanifest.Roles) != len(want) {
		t.Fatalf("%d roles", len(smsgwmanifest.Roles))
	}
	for _, r := range smsgwmanifest.Roles {
		if !slices.Equal(r.Permissions, want[r.Slug]) || r.DisplayName == "" || r.Description == "" {
			t.Errorf("%s: %+v", r.Slug, r)
		}
	}
	all := smsgwmanifest.PermissionRefs()
	if !slices.Equal(all, []string{"providers:read", "providers:manage", "templates:read", "templates:manage", "clients:read", "clients:manage",
		"blocks:read", "blocks:manage", "messages:read", "messages:send", "dashboard:read"}) {
		t.Fatalf("permissions %v", all)
	}
	// Owner and admin hold everything; no broad member grant.
	if len(smsgwmanifest.Grants) != 2 || !slices.Equal(smsgwmanifest.Grants["owner"], all) || !slices.Equal(smsgwmanifest.Grants["admin"], all) {
		t.Fatalf("grants %v", smsgwmanifest.Grants)
	}
}

func TestRegistration(t *testing.T) {
	reg := smsgwmanifest.Registration()
	if err := reg.Validate(); err != nil {
		t.Fatal(err)
	}
	if reg.Module != "sms-gw" || len(reg.Permissions) != len(smsgwmanifest.Permissions) || len(reg.Roles) != 4 {
		t.Fatalf("%+v", reg)
	}
	if req := reg.Request(); !req.GetDeclaresRoles() || len(req.GetBuiltinGrants()) != 2 {
		t.Fatalf("%+v", req)
	}
}

// TestManifest checks the gateway manifest: every OpenAPI operation is a
// protected route under the module prefix with gateway-valid path
// parameters, and every declaration names a declared permission.
func TestManifest(t *testing.T) {
	m, err := smsgwmanifest.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Proto(); err != nil {
		t.Fatal(err)
	}
	if err := smsgwmanifest.ValidateDeclarations(); err != nil {
		t.Fatal(err)
	}
	param := regexp.MustCompile(`^\{[a-z][a-z0-9_]*\}$`)
	path := regexp.MustCompile(`^/[A-Za-z0-9._~-]+(/([A-Za-z0-9._~-]+|\{[a-z][a-z0-9_]*\}))*$`)
	perms := map[string]bool{}
	for _, p := range smsgwmanifest.PermissionRefs() {
		perms[p] = true
	}
	used := map[string]bool{}
	for _, r := range m.Routes {
		if !strings.HasPrefix(r.Path, "/api/sms-gw/v1/") || !path.MatchString(r.Path) || r.Public || !perms[r.Permission] {
			t.Errorf("route %+v", r)
		}
		for _, seg := range strings.Split(r.Path, "/") {
			if strings.HasPrefix(seg, "{") && !param.MatchString(seg) {
				t.Errorf("%s: parameter %s", r.Path, seg)
			}
		}
		used[r.Permission] = true
	}
	if len(m.Routes) != 29 || len(used) != len(perms) {
		t.Fatalf("%d routes using %d of %d permissions", len(m.Routes), len(used), len(perms))
	}
	if !slices.Equal(m.Prefixes, []string{"/api/sms-gw"}) || len(m.Methods) != 0 || !slices.Equal(m.Exposes, []string{"./routes", "./nav"}) {
		t.Fatalf("%+v", m)
	}
	for _, n := range m.Nav {
		if !strings.HasPrefix(n.Path, "/sms-gw/") {
			t.Errorf("nav %+v", n)
		}
	}
	for _, a := range m.Abilities {
		for _, s := range a.Subject {
			if !strings.HasPrefix(s, "Sms") {
				t.Errorf("ability subject %s may collide with other modules", s)
			}
		}
	}
}
