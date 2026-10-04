# Tangra SMS Gateway V4

The `sms-gw` module of the go-tangra V4 platform: the legacy Hermes public SMS API (login, send, read, receipt polling) on its own listener, carrier receipt ingestion with signed client callbacks, and a tenant-scoped management API with a federated portal UI. It replaces the v3 gateway at `../go-tangra-sms-gw` (read-only source of behavior).

Module `github.com/go-tangra/go-tangra-sms-gw/v4`, image `ghcr.io/go-tangra/go-tangra-sms-gw`. Specification and contracts: `specs/001-sms-gateway-v4/`. Decisions: `docs/compatibility.md` (preserved and intentionally changed behavior), `docs/operations.md`, `docs/migration.md`, `docs/dependencies.md`; evidence: `docs/validation.md`.

## Layout

| Path | Contents |
|---|---|
| `cmd/smsgwsvc` | the service (`smsgwsvc -config …`, `smsgwsvc bootstrap`, `smsgwsvc version`) |
| `cmd/smsgw-migrate` | read-only legacy snapshot import (`-dry-run`, `-apply`) |
| `internal/publicapi`, `internal/hermes`, `internal/auth`, `internal/sms`, `internal/render`, `internal/provider` | Hermes public listener, client tokens, send pipeline, templates, Voicecom carrier |
| `internal/dlr`, `internal/webhook` | receipt ingestion and client callbacks |
| `internal/httpapi`, `internal/authz`, `internal/metrics` | management API, operator authorization, dashboard |
| `internal/repo`, `internal/store`, `internal/sealed` | PostgreSQL (row-level security per tenant), migrations, sealed secrets |
| `internal/acme`, `internal/housekeeper`, `internal/migrate` | public HTTPS, retention, legacy import |
| `pkg/smsgwmanifest` | gateway manifest and auth registration (permissions, roles, nav, abilities) |
| `api/openapi/sms-gw.yaml` | management API contract (embedded; gateway routes are derived from it) |
| `ui/` | Vue federated remote `sms-gw`, embedded with `-tags ui` |
| `deploy/` | development configuration, local compose (PostgreSQL, mock carrier, callback receiver, pebble) |
| `tests/contract`, `tests/integration` | legacy replay, management, delivery, operations and platform acceptance |

## Setup

Requirements: Go 1.26 (toolchain 1.26.8; unset a stale `GOROOT`), Node 22, Docker (testcontainers and compose). `GOWORK=off`: dependencies are the published go-tangra V4 modules pinned in `go.mod`.

```sh
go mod download
cd ui && NODE_AUTH_TOKEN=$(gh auth token) npm ci   # read:packages for @go-tangra/* on npm.pkg.github.com
```

Development secrets are generated locally and gitignored (`deploy/kek.dev`, 32 random bytes; `deploy/jwt.dev.key`, at least 32 bytes; the mesh SVID under `../../.dev/ca/`). No token, key, certificate or legacy data belongs in source control.

## Build and test

```sh
make generate          # UI API types from api/openapi/sms-gw.yaml
make lint              # go vet + UI eslint/vue-tsc/no-legacy check
make test              # go test -race ./...
make test-ui           # vitest
make test-integration  # PostgreSQL and pebble via docker; skips without docker (SMSGW_TEST_REQUIRE_DOCKER=1 fails instead)
make build             # go build ./...
make build-ui-embedded # UI build + go build -tags ui
NODE_AUTH_TOKEN=$(gh auth token) make image
```

The image (distroless, nonroot) holds `smsgwsvc` (UI embedded, version from `APP_VERSION`) and `smsgw-migrate`, owns the `/state` and `/acme` mount points and contains no key material; configuration, secrets and identity are mounted (`deploy/container.yaml`). CI is `.github/workflows/ci.yaml`: vet/race/build, manifest parity, the PostgreSQL integration suite, UI lint/unit/build and the image (pushed to GHCR on `v4` pushes and `v*` tags; no `latest`). The portal e2e (`ui/tests/e2e`, Playwright) runs against a platform stack with a signed-in operator.

## Run locally

```sh
make compose-up                                              # PostgreSQL :5437, mock carrier :9916, callback receiver :9917
go run ./cmd/smsgwsvc bootstrap -config deploy/dev.yaml      # idempotent migrations and the smsgw_app role; creates no accounts
go run -tags ui ./cmd/smsgwsvc -config deploy/dev.yaml
```

Listeners: public Hermes `127.0.0.1:9901` (optional HTTPS `:9902`), mesh mTLS HTTP `:9984` and gRPC `:9983` (reached only through the gateway), admin `:9593` (`/healthz`, `/readyz`, `/metrics`). On the platform the module needs a gateway allow-list entry for `/api/sms-gw` and a mesh identity (file SVID or lcm enrollment).

## Configuration

YAML (`-config`); secrets are file or environment references. Legacy environment variables (`HTTP_PUBLIC_BIND`, `SMS_GW_TLS_*`, `SMS_GW_JWT_SECRET`, `SMS_GW_*_RPM`, `SMS_GW_PROMETHEUS_URL`, …) override their keys; see `docs/operations.md`.

| Key | Meaning |
|---|---|
| `service_name`, `trust_domain`, `identity`, `enroll` | mesh identity: file SVID or lcm enrollment (state under `/state`) |
| `server`, `admin`, `discovery`, `gateway` | mesh listeners, admin listener, gateway/auth services and portal issuer |
| `db.dsn`, `db.migrate_dsn` | application role (RLS enforced) and migration role |
| `kek` | key-encryption key for sealed provider credentials and callback secrets |
| `public`, `public_auth` | Hermes listener, trusted proxies, body limit; client JWT secret and token lifetimes |
| `acme` | optional automatic public HTTPS (`cache_dir`, domains, directory) |
| `rate_limits`, `recipients` | login/send/receipt token buckets; recipient digit and prefix policy |
| `webhook` | client callback queue and workers; `allow_http`/`allow_private` are development only |
| `retention.interval` | retention sweep (per-provider `retention_days`, 0 keeps forever) |
| `monitoring.prometheus_url` | dashboard source; empty means "not configured" |
| `query` | Hermes list page sizes |

## APIs

- Public Hermes API (legacy wire contract, `specs/001-sms-gateway-v4/contracts/public-api.md`): `POST /hermes/v1/login`, `/refresh_token`, `/logout`, `GET /hermes/v1/me`, `POST|GET /hermes/v1/sms` (and `/v1/sms`), `GET /hermes/v1/sms/{id}`, `GET /hermes/v1/sms/dlr/{id}`, carrier receipts `GET /dlr` and `/hermes/v1/sms/dlr` (always `200 "DLR_OK"`), `/health`. API clients use their own credentials and are bound to one tenant.
- Management API (`api/openapi/sms-gw.yaml`, `/api/sms-gw/v1`, 29 operations): providers and provider types, templates and preview, API clients and password reset, blocks, messages with manual send and receipts, dashboard. Reached only through the gateway with the operator's platform token; every operation is checked again in the module and scoped to the token's tenant. Secrets are write-only.

## Permissions and roles

Registered with auth at startup and every five minutes (`pkg/smsgwmanifest`).

| Permission | Allows | administrator | sender | viewer | monitoring |
|---|---|:-:|:-:|:-:|:-:|
| `providers:read` | list/read providers and types (credentials redacted) | yes | yes | yes | |
| `providers:manage` | create/change/delete providers and credentials | yes | | | |
| `templates:read` | list/read/preview templates | yes | yes | yes | |
| `templates:manage` | create/change/delete templates | yes | | | |
| `clients:read` | list/read API clients | yes | | | |
| `clients:manage` | create/change/delete API clients, reset passwords | yes | | | |
| `blocks:read` | list/read recipient blocks | yes | | | |
| `blocks:manage` | create/change/delete blocks | yes | | | |
| `messages:read` | search messages, read details and receipts | yes | yes | yes | |
| `messages:send` | manual send from the portal | yes | yes | | |
| `dashboard:read` | monitoring dashboard: **deployment-wide totals across all tenants** | yes | | | yes |

Built-in grants: owner and admin hold every permission; member gets nothing. The dashboard's metrics carry no tenant label, so `dashboard:read` is deliberately not part of viewer and its navigation entry shows only with it: grant the monitoring role only to people allowed to see deployment-wide volumes. Legacy `platform:admin`/`sms:admin` strings and identity headers have no effect.

## Migration

`cmd/smsgw-migrate` imports a read-only legacy snapshot into one explicit tenant (dry-run, apply, repeatable, reconciled); IDs and password hashes are kept. Procedure, token continuity, cutover and rollback: `docs/migration.md`.
