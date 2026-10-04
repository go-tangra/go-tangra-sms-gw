// Package smsgwmanifest declares what the sms-gw module registers: with the
// application gateway its routes (derived from the embedded OpenAPI
// document's x-freya-permission), permissions, CASL abilities and
// navigation; with auth its permissions, module roles and built-in role
// grants (contracts/management-api.md, contracts/platform.md). Public so the
// contract tests, the UI build and the gateway can validate it.
package smsgwmanifest

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/go-tangra/go-tangra-auth/sdk/v4/pkg/authclient"
	"github.com/go-tangra/go-tangra-portal/sdk/v4/pkg/gatewayclient"

	"github.com/go-tangra/go-tangra-sms-gw/v4/api/openapi"
)

// Module identity.
const (
	Module      = "sms-gw"
	DisplayName = "SMS Gateway"
	Version     = "1.0.0"
	APIPrefix   = "/api/sms-gw"
	// RemotePrefix is where the module serves its federated remote; the
	// gateway relays it under /m/sms-gw/ and the module does not own /ui.
	RemotePrefix = "/ui"
)

// OpenAPI operation extensions.
const (
	PermissionExtension = "x-freya-permission"
	PublicExtension     = "x-freya-public"
	BodyLimitExtension  = "x-freya-max-body-bytes"
	TimeoutExtension    = "x-freya-timeout-seconds"
)

// Permissions the module registers (contracts/management-api.md).
var Permissions = []gatewayclient.Permission{
	{Resource: "providers", Action: "read", Description: "List and read SMS providers and provider types (credentials redacted)"},
	{Resource: "providers", Action: "manage", Description: "Create, change and delete SMS providers and their credentials"},
	{Resource: "templates", Action: "read", Description: "List, read and preview SMS templates"},
	{Resource: "templates", Action: "manage", Description: "Create, change and delete SMS templates"},
	{Resource: "clients", Action: "read", Description: "List and read Hermes API clients (no passwords or secrets)"},
	{Resource: "clients", Action: "manage", Description: "Create, change and delete Hermes API clients and reset their passwords"},
	{Resource: "blocks", Action: "read", Description: "List and read recipient blocks"},
	{Resource: "blocks", Action: "manage", Description: "Create, change and delete recipient blocks"},
	{Resource: "messages", Action: "read", Description: "Search messages and read their details and delivery receipts"},
	{Resource: "messages", Action: "send", Description: "Send SMS messages from the portal"},
	{Resource: "dashboard", Action: "read", Description: "Read the gateway monitoring dashboard"},
}

// Module roles auth provides in every tenant.
var (
	senderPermissions = []string{"providers:read", "templates:read", "messages:send", "messages:read"}
	viewerPermissions = []string{"providers:read", "templates:read", "messages:read", "dashboard:read"}
	Roles             = []authclient.ModuleRole{
		{Slug: "administrator", DisplayName: DisplayName + " administrator", Description: "Every SMS gateway permission", Permissions: PermissionRefs()},
		{Slug: "sender", DisplayName: DisplayName + " sender", Description: "Send SMS messages and read providers, templates and messages", Permissions: senderPermissions},
		{Slug: "viewer", DisplayName: DisplayName + " viewer", Description: "Read providers, templates, messages and the dashboard", Permissions: viewerPermissions},
	}
)

// Grants are the built-in role grants: owner and admin hold every
// permission; no broad member grant.
var Grants = map[string][]string{"owner": PermissionRefs(), "admin": PermissionRefs()}

// Methods proxied by the gateway: none (the module has no gRPC API).
var Methods []gatewayclient.Method

// Abilities are the CASL rules bound to the permissions.
var Abilities = []gatewayclient.Ability{
	{Action: []string{"read"}, Subject: []string{"SmsProvider"}, Requires: "providers:read"},
	{Action: []string{"create", "update", "delete"}, Subject: []string{"SmsProvider"}, Requires: "providers:manage"},
	{Action: []string{"read"}, Subject: []string{"SmsTemplate"}, Requires: "templates:read"},
	{Action: []string{"create", "update", "delete"}, Subject: []string{"SmsTemplate"}, Requires: "templates:manage"},
	{Action: []string{"read"}, Subject: []string{"SmsApiClient"}, Requires: "clients:read"},
	{Action: []string{"create", "update", "delete"}, Subject: []string{"SmsApiClient"}, Requires: "clients:manage"},
	{Action: []string{"read"}, Subject: []string{"SmsBlock"}, Requires: "blocks:read"},
	{Action: []string{"create", "update", "delete"}, Subject: []string{"SmsBlock"}, Requires: "blocks:manage"},
	{Action: []string{"read"}, Subject: []string{"SmsMessage"}, Requires: "messages:read"},
	{Action: []string{"send"}, Subject: []string{"SmsMessage"}, Requires: "messages:send"},
	{Action: []string{"read"}, Subject: []string{"SmsDashboard"}, Requires: "dashboard:read"},
}

// Nav lists the navigation contributions.
var Nav = []gatewayclient.NavEntry{
	{Title: "SMS providers", Path: "/sms-gw/providers", Icon: "mdi-cellphone-wireless", Order: 900, Requires: "providers:read"},
	{Title: "SMS templates", Path: "/sms-gw/templates", Icon: "mdi-file-document-edit-outline", Order: 901, Requires: "templates:read"},
	{Title: "SMS API clients", Path: "/sms-gw/api-clients", Icon: "mdi-key-chain-variant", Order: 902, Requires: "clients:read"},
	{Title: "SMS blocks", Path: "/sms-gw/blocks", Icon: "mdi-cancel", Order: 903, Requires: "blocks:read"},
	{Title: "SMS messages", Path: "/sms-gw/messages", Icon: "mdi-message-processing-outline", Order: 904, Requires: "messages:read"},
	{Title: "SMS dashboard", Path: "/sms-gw/dashboard", Icon: "mdi-chart-line", Order: 905, Requires: "dashboard:read"},
}

// Exposes are the federated remote's exports.
var Exposes = []string{"./routes", "./nav"}

// BuiltinRoles lists the built-in role slugs auth knows, in a stable order.
var BuiltinRoles = []string{"owner", "admin", "member", "auditor", "operator"}

// PermissionRefs lists "resource:action" for every declared permission.
func PermissionRefs() []string {
	out := make([]string, 0, len(Permissions))
	for _, p := range Permissions {
		out = append(out, p.Resource+":"+p.Action)
	}
	return out
}

// Load parses the embedded document.
func Load() (*openapi3.T, error) {
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(openapi.SMSGW)
	if err != nil {
		return nil, fmt.Errorf("smsgwmanifest: openapi: %w", err)
	}
	if err := doc.Validate(context.Background(), openapi3.DisableExamplesValidation()); err != nil {
		return nil, fmt.Errorf("smsgwmanifest: openapi: %w", err)
	}
	return doc, nil
}

// Routes derives the gateway routes from the document: every operation lies
// under APIPrefix and carries one declared x-freya-permission; public routes
// are refused.
func Routes(doc *openapi3.T) ([]gatewayclient.Route, error) {
	known := map[string]bool{}
	for _, p := range PermissionRefs() {
		known[p] = true
	}
	var routes []gatewayclient.Route
	for p, item := range doc.Paths.Map() {
		if !strings.HasPrefix(p, APIPrefix+"/") {
			return nil, fmt.Errorf("smsgwmanifest: %s is outside %s", p, APIPrefix)
		}
		for m, op := range item.Operations() {
			r := gatewayclient.Route{Method: strings.ToUpper(m), Path: p}
			perm, _ := op.Extensions[PermissionExtension].(string)
			switch public, _ := op.Extensions[PublicExtension].(bool); {
			case public:
				return nil, fmt.Errorf("smsgwmanifest: %s %s must not be public", r.Method, p)
			case perm == "":
				return nil, fmt.Errorf("smsgwmanifest: %s %s declares no permission", r.Method, p)
			case !known[perm]:
				return nil, fmt.Errorf("smsgwmanifest: %s %s uses undeclared permission %q", r.Method, p, perm)
			}
			r.Permission = perm
			if v, ok := op.Extensions[BodyLimitExtension]; ok {
				n, ok := v.(float64)
				if !ok || n <= 0 {
					return nil, fmt.Errorf("smsgwmanifest: %s %s has a bad body limit", r.Method, p)
				}
				r.MaxBodyBytes = uint64(n)
			}
			if v, ok := op.Extensions[TimeoutExtension]; ok {
				n, ok := v.(float64)
				if !ok || n <= 0 || n > 600 {
					return nil, fmt.Errorf("smsgwmanifest: %s %s has a bad timeout", r.Method, p)
				}
				r.Timeout = time.Duration(n) * time.Second
			}
			routes = append(routes, r)
		}
	}
	sort.Slice(routes, func(i, j int) bool { return routes[i].Method+" "+routes[i].Path < routes[j].Method+" "+routes[j].Path })
	return routes, nil
}

// ValidateDeclarations checks roles, grants, navigation and abilities
// against the declared permissions.
func ValidateDeclarations() error {
	known := map[string]bool{}
	for _, p := range PermissionRefs() {
		known[p] = true
	}
	check := func(what string, refs ...string) error {
		for _, r := range refs {
			if !known[r] {
				return fmt.Errorf("smsgwmanifest: %s references undeclared permission %q", what, r)
			}
		}
		return nil
	}
	for _, r := range Roles {
		if err := check("role "+r.Slug, r.Permissions...); err != nil {
			return err
		}
	}
	for role, refs := range Grants {
		if err := check("grant "+role, refs...); err != nil {
			return err
		}
	}
	for _, n := range Nav {
		if err := check("nav "+n.Path, n.Requires); err != nil {
			return err
		}
	}
	for _, a := range Abilities {
		if err := check("ability", a.Requires); err != nil {
			return err
		}
	}
	return nil
}

// Manifest builds the gateway manifest from the embedded OpenAPI document.
func Manifest() (gatewayclient.Manifest, error) {
	doc, err := Load()
	if err != nil {
		return gatewayclient.Manifest{}, err
	}
	if err := ValidateDeclarations(); err != nil {
		return gatewayclient.Manifest{}, err
	}
	routes, err := Routes(doc)
	if err != nil {
		return gatewayclient.Manifest{}, err
	}
	return gatewayclient.Manifest{
		Module: Module, DisplayName: DisplayName, Version: Version,
		Prefixes:    []string{APIPrefix},
		Routes:      routes,
		Methods:     Methods,
		Permissions: Permissions,
		Abilities:   Abilities,
		Exposes:     Exposes,
		Nav:         Nav,
	}, nil
}

// Registration is what the module registers with auth at start and every
// five minutes: its permissions, module roles and built-in role grants.
func Registration() authclient.Registration {
	reg := authclient.Registration{Module: Module, DisplayName: DisplayName, Roles: Roles, BuiltinGrants: Grants}
	for _, p := range Permissions {
		reg.Permissions = append(reg.Permissions, authclient.Permission{Resource: p.Resource, Action: p.Action, Description: p.Description})
	}
	return reg
}
