# Quickstart validation: SMS Gateway V4

Acceptance commands for the implemented module. Every command below runs in this repository; actual results are recorded in [docs/validation.md](../../docs/validation.md).

## Prerequisites

Go 1.26/toolchain 1.26.8, Node 22, Docker Compose, and access to the local V4 auth, portal, LCM and framework checkouts. UI installation needs package-read access for @go-tangra/ui; provide credentials through environment/BuildKit secrets. Use isolated database fixtures, mock carrier, callback receiver and local ACME CA; never configure a real carrier for acceptance.

## Build and local startup

From the repository root:

```bash
make generate
make lint
make test
make build
make compose-up
go run ./cmd/smsgwsvc bootstrap -config deploy/dev.yaml
go run -tags ui ./cmd/smsgwsvc -config deploy/dev.yaml
```

`deploy/compose.yaml` supplies PostgreSQL and a mock LinkMobility carrier and supports connecting to V4 auth/portal/LCM using explicit sibling path/configuration variables. `deploy/dev.yaml` supplies public :9901 and optional HTTPS :9902, distinct mesh/admin addresses, development identity/secret paths and monitoring configuration. Bootstrap applies idempotent migrations only; no sample credentials or real sends are created without an explicit fixture command.

## Public compatibility (US1, FR-001–005)

```bash
go test ./tests/contract/... -count=1
make test-integration
```

Capture source fixtures first; exercise every method/path in [public-api.md](contracts/public-api.md), including all aliases and errors. Seed two clients, a viewer, provider/template and global/provider blocks. Verify exact payload semantics, client ownership, invalid grants/token kinds and no row/carrier call on blocked sends. Submit 100 valid sends through the mock and reconcile exactly 100 messages and carrier invocations. Test tenant references and limits without adding a tenant parameter to legacy login.

## Receipt pipeline (US2, FR-006–008)

Seed a message; submit valid, malformed and forged callbacks to both receipt endpoints. Fire 100 concurrent identical message/status receipts: one row and count 100. Test early callbacks, late terminal receipts, out-of-order states, signed callback bytes, private-address/redirect blocking, queue overflow, retry exhaustion and shutdown cancellation. Invalid callbacks preserve 200 DLR_OK but never mutate a message. Poll remains available when callback receiver is down.

## Portal management (US3, FR-009–012)

```bash
cd ui
npm ci
npm run lint
npm run test:unit
npm run build
npm run test:e2e
```

Register module and auth permissions/roles. Sign in as administrator, sender, viewer and monitoring in two tenants (the dashboard needs the monitoring role; viewer is refused). Load all seven management areas through the V4 portal, exercise permissions both through UI and direct mesh endpoint, and verify tenant isolation, secret redaction, filters, preview/parts counts and unavailable monitoring. Manifest, remote exports, OpenAPI and handlers agree; public listener cannot reach management. Use development certificates for direct mesh tests; no insecure mesh opt-out.

## Migration and operation (US4, FR-013–016)

The migration command:

```bash
go run ./cmd/smsgw-migrate -source-config deploy/legacy-import.dev.yaml -config deploy/dev.yaml -tenant fixture-tenant -dry-run
go run ./cmd/smsgw-migrate -source-config deploy/legacy-import.dev.yaml -config deploy/dev.yaml -tenant fixture-tenant -apply
```

Source config points to a read-only snapshot connection; secrets are references, never command-line literal DSNs. Reconcile all seven legacy entity types, IDs/references/hashes/statuses/times/retention, and explicit redaction differences. Rerun apply with no duplicates and test conflicting IDs/orphans/missing tenant/key. Authenticate an imported client and read old receipts. Verify preserved JWT secret continuity or documented re-login.

Restart services; rotate/expire internal identity; interrupt auth/portal and restore them; confirm registration recovers, protected traffic fails closed as required and clean shutdown drains workers. Test public static TLS and local ACME issuance/renewal/cache persistence and nonfatal invalid-public-cert cases. Run retention twice, including zero retention and foreign-tenant/provider controls.

Follow `docs/migration.md` for a paused-send final snapshot/cutover and rollback rehearsal. After destination accepts new traffic, reconcile new messages and receipt routing before rollback; changing routing alone is insufficient. Record fixture counts, platform versions and command results in `docs/validation.md`. Production rollout remains a later step.

## Platform acceptance (FR-001–017, SC-001–006)

```bash
make test-integration   # includes tests/integration/platform_test.go
```

`TestPlatformAcceptance` runs the whole module against gateway and auth services speaking the real V4 gRPC contracts over mTLS (fakes, so it runs in CI) with the mock carrier; the same flow through the real portal is the freya-stack run recorded in docs/validation.md.
