# Dependency and porting map

The service module is github.com/go-tangra/go-tangra-sms-gw/v4. Local baseline: framework v4.3.1, auth/portal/LCM SDK v4.1.0, Go 1.26.3 and toolchain 1.26.8, pgx v5.11.0, goose v3.28.0. Framework brings Kratos v3 transitively. No legacy common, Wire or tx7do bootstrap packages are used.

| Source | Destination | Migration |
|---|---|---|
| cmd/server bootstrap + wire | cmd/smsgwsvc, internal/app | Freya lifecycle and pooled SDK clients |
| internal/data/ent/schema and repos | internal/store, internal/repo | SQL tables preserving IDs, tenant-safe references and explicit migrations |
| internal/auth | internal/auth | Retain Hermes claims/password behavior; persist revocations |
| internal/provider | internal/provider | Port Voicecom and provider metadata |
| internal/service SMS/template | internal/sms, internal/render | Separate domain pipeline from wire adapters |
| internal/server | internal/publicapi, internal/httpapi | Separate legacy public edge from V4 mesh management |
| internal/cert and registration | internal/identity, internal/app | Framework identity and V4 SDK registration |
| frontend | ui | V4 UI kit and federation; no copied build artifacts |
| webhook, acme, retention, metrics | corresponding internal packages | Preserve semantics and attach cancellable lifecycle |

UI uses @go-tangra/ui ^4.3.0, Vue ^3.5, Router ^5, Pinia ^4, CASL ability ^7/vue ^3, Zod ^4, Tailwind ^4 and FlyonUI ^2. Install requires GitHub Packages read permission through NODE_AUTH_TOKEN; tokens are never committed. Dependency resolution is checked independently of runtime Docker availability.
