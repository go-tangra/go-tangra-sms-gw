# Implementation Plan: SMS Gateway for Tangra V4

**Branch**: No Git branch available; feature `001-sms-gateway-v4` | **Date**: 2026-10-04 | **Spec**: [spec.md](spec.md)
**Input**: `specs/001-sms-gateway-v4/spec.md`

## Summary

Preserve legacy Hermes clients and the SMS delivery workflow while rebuilding administration on the checked-out V4 platform. Port domain code into this repository, use V4 mesh identity and SDK registration, add tenant-scoped PostgreSQL persistence and a V4 federated UI, and supply a repeatable isolated legacy import. Public SMS authentication remains separate from operator authentication. This plan defines future implementation; no gateway runtime exists here yet.

## Technical Context

**Language/Version**: Go 1.26.3, toolchain go1.26.8; TypeScript/Vue 3, Node 22.
**Primary Dependencies**: go-tangra/v4 v4.3.1; auth/sdk/v4, portal/sdk/v4 and lcm/sdk/v4 v4.1.0; Kratos v3.0.0 as required by framework; pgx v5.11.0; goose v3.28.0; existing JWT, provider HTTP and ACME libraries revalidated against Go baseline. UI uses @go-tangra/ui ^4 (local reference ^4.3.0), FlyonUI, Tailwind 4, Vue Router 5, Pinia 4, CASL 7/3 and Zod 4.
**Storage**: PostgreSQL with explicit SQL migrations, tenant predicates and composite relationship checks; encrypted provider/callback secrets; persistent public ACME cache and internal identity files where configured.
**Testing**: Go unit/race, public golden contract fixtures, PostgreSQL integration, real V4 auth/portal/LCM stack with mock carrier and local ACME, Vitest/Playwright for UI; OpenAPI/manifest/handler parity checks.
**Target Platform**: Linux service/container and Tangra V4 portal; independent mesh and public listeners.
**Project Type**: SMS gateway service with federated management UI and migration CLI.
**Performance Goals**: Preserve configured source limits (login 10/min/IP and sends 100/min/client (IP fallback) by default); acceptance includes 100 valid sends and concurrent 100-receipt aggregation. No invented production throughput SLA.
**Constraints**: Public wire preservation, tenant/owner isolation, no credential logging, no blind resend on uncertain carrier acceptance, protected internal identity, no legacy bootstrap/common/Wire dependency.
**Scale/Scope**: One module, seven management areas, seven legacy entity types plus tenant/audit/revocation/import metadata. Voicecom is the sole carrier implementation in scope; unsupported channel values remain rejected.

## Constitution Check

**Pre-research**: `.specify/memory/constitution.md` contains template placeholders only. No ratified principles or approval gates exist; template examples are not binding. Proceed under explicit user scope and the locally verified V4 contracts.
**Post-design**: Pass. Separate trust domains, tenant-scoped storage, explicit contracts, narrow SDK imports, mock-based verification and reversible isolated migration satisfy the stated specification. No constitution exceptions are claimed. Ratifying a constitution is outside this request.

## Project Structure

### Documentation (this feature)

```text
specs/001-sms-gateway-v4/
├── spec.md
├── plan.md
├── research.md
├── data-model.md
├── quickstart.md
├── checklists/requirements.md
├── contracts/public-api.md
├── contracts/management-api.md
├── contracts/platform.md
└── tasks.md
```

### Source Code (repository root; proposed)

```text
cmd/smsgwsvc/{main.go,bootstrap.go,version.go}
cmd/smsgw-migrate/main.go
cmd/test-linkmobility/main.go
internal/
  app/                # Freya wiring, workers and registration
  config/             # config, validation and env compatibility
  identity/           # optional LCM adapter, no private CA reimplementation
  store/migrations/   # legacy-compatible tables plus tenant constraints
  repo/               # explicit pgx repositories and transactions
  sealed/             # credential envelopes and redaction
  auth/               # Hermes client JWT/password/revocation
  authz/              # platform identity/permission/tenant checks
  provider/voicecom/   # carrier sender, type metadata and status mapping
  render/             # bounded template rendering and SMS length rules
  sms/                # send orchestration and ownership
  dlr/                # receipt validation and transactional aggregation
  webhook/            # bounded signed callback workers and safe URLs
  ratelimit/          # public login/send protection
  trustedproxy/       # safe caller address resolution
  acme/               # public automatic certificate workers
  housekeeper/        # retention worker
  metrics/            # low-cardinality telemetry and query service
  audit/              # sanitized action/login events
  httpapi/            # V4 management routes only
  publicapi/          # exact Hermes wire adapter and optional public TLS
  migrate/            # snapshot validation/import/reconciliation
api/openapi/sms-gw.yaml
pkg/smsgwmanifest/manifest.go
ui/{src,embed.go,embed_stub.go,module-federation.config.ts}
deploy/{dev.yaml,compose.yaml,policy.yaml,init-db.sql}
tests/{contract,integration,fixtures}
docs/{migration.md,operations.md,dependencies.md}
Makefile
Dockerfile
.github/workflows/ci.yaml
```

**Structure Decision**: Follow V4 notification service lifecycle and UI conventions, retaining SMS-specific public adapters. Use SQL instead of copying Ent-generated code; do not copy legacy frontend build output, certificates, environment files or generated bootstrap wiring. Internal management uses HTTP through the V4 portal; retain public proto definitions as contract evidence rather than promising obsolete internal RPC compatibility. A new cross-service SMS SDK is outside scope.

## Design and delivery decisions

- Foundation establishes module/config/lifecycle, tenant schema, repository contracts, secret encryption and operator auth. Public sends, DLR handling, management and migration then form separately testable slices with explicit dependencies in tasks.md.
- Management contract is authoritative OpenAPI. Every operation has a permission; manifest and handlers must match. Protected endpoints verify bearer identity and tenant through auth SDK even over a trusted mesh caller.
- Public adapter owns legacy status/errors, including uint64 JSON conventions and misspelled receipt field. Schema design uses signed delivery status internally and a fixture-backed public conversion.
- Public tenant comes from API client; platform tenant comes from verified token. UUID and numeric IDs cannot bypass tenant predicates. Callback lookup derives tenant from a stored message, with secret validation before mutation.
- Transactional DLR upsert and message lock prevent terminal-state races. Outbound callback work is bounded and isolated; preserve source retry/drop behavior after fixture verification, without inventing durable retries.
- Import and rollback preserve source installation. Import handles secret sealing and credentials explicitly and reports all unsupported data rather than silently dropping it.

## Verification and release gates

FR-017 explicitly requests automated verification. Capture source contracts before replacing implementations. Run Go race tests and real PostgreSQL isolation/concurrency checks; validate all OpenAPI routes against manifest and handlers; run the V4 identity/registration/permission scenarios and UI checks. Migration reconciles every entity and reference twice. Public TLS is verified with local/static certificates and a local ACME CA. The final acceptance stack never sends to a real carrier. Deployment into a production platform is a later operational action.
