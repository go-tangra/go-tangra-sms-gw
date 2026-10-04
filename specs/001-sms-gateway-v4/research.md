# Research: SMS Gateway V4

Research date: 2026-10-04. Evidence is the local source tree, not a claim about the latest published release. Source paths below are relative to this repository. No external lookup is required to define compatibility with these checkouts.

## R1 — Compatibility boundary

**Decision**: Rebuild platform integration in this workspace while porting proven SMS domain behavior. Preserve public Hermes HTTP contracts and aliases; replace legacy internal admin/protobuf-descriptor registration with an explicit V4 management HTTP contract.
**Rationale**: `../go-tangra-sms-gw/README.md`, `protos/sms_gw/service/v1/*.proto`, `internal/server/http.go`, `internal/server/dlr_handler.go`, `internal/service/` and `e2e/` define the behavior. The referenced `CLAUDE.md` is absent; implementation and fixtures are authoritative. Generated Kratos v2 HTTP bindings cannot simply be reused in V4.
**Alternatives considered**: A dependency-only upgrade leaves legacy identity, gateway metadata and frontend integration incompatible. Retaining the entire old admin transport duplicates obsolete platform plumbing.

## R2 — Framework and dependency baseline

**Decision**: Module `github.com/go-tangra/go-tangra-sms-gw/v4`; Go 1.26.3 with toolchain go1.26.8; framework `github.com/go-tangra/go-tangra/v4 v4.3.1`; auth, portal and LCM SDKs `/sdk/v4 v4.1.0`; Kratos `/v3 v3.0.0` only where framework integration requires it. Use pgx v5.11.0 and goose v3.28.0 for persistence/migrations. Resolve indirect dependencies during implementation rather than copying another module's complete go.mod.
**Rationale**: `../go-tangra/go.mod` and `../go-tangra-notification-v4/go.mod` establish the local V4 baseline. Explicit SQL simplifies preserving legacy tables while adding tenant boundaries, without retaining tx7do mixins or Wire/bootstrap packages.
**Alternatives considered**: Keep Ent with custom mixins: feasible but adds generation work and legacy coupling. Upgrade to unverified dependency versions: unnecessary.

## R3 — Identity, lifecycle and two trust domains

**Decision**: Use `freya.New`, `App.HTTP`, `App.GRPC`, pooled `App.Client`, `Ready` and `Run` lifecycle; file SPIFFE identity for development and LCM network enrollment for deployments. Mesh policy admits only the gateway on management routes. Run public Hermes HTTP and optional public HTTPS separately, with their own authentication and certificate lifecycle. All goroutines, registration clients, certificate workers, dispatcher and housekeeper stop and drain via a shared lifecycle context.
**Rationale**: `../go-tangra-notification-v4/internal/app/app.go`, `deploy/dev.yaml`, `deploy/policy.yaml`, `../go-tangra/docs/security-model.md`, `identity/` and `transport/` require verified identity and deny-by-default policy. Public clients/carriers cannot present mesh SVIDs.
**Alternatives considered**: Disable mesh TLS for callbacks: breaks the framework trust boundary. Expose management on the public listener: unnecessary risk and incompatible identity assumptions.

## R4 — Public authentication and tenant mapping

**Decision**: Port existing JWT/password/denylist behavior and exact wire codec independently of auth SDK operator verification. Store an immutable tenant on each API client; derive tenant from resolved enabled client, never from body/header. Keep usernames globally unique to preserve login without a new tenant parameter. Unsupported `API_ADMIN` authority fails closed pending an explicit compatibility decision backed by source behavior. Persist revocations or clearly bounded expiry semantics across restart. Public login records unresolved failures without manufacturing a tenant.
**Rationale**: `../go-tangra-sms-gw/internal/auth/`, `internal/service/authentication.go`, `internal/data/ent/schema/api_client.go` distinguish public accounts from portal people. Source schema has API_ADMIN but middleware only recognizes API_CLIENT as writable. Platform identities require verified UserID and TenantID.
**Alternatives considered**: Replace public login with platform login: breaks clients. Tenant-prefixed login names: breaks existing login requests. Global unscoped data: violates V4 deployment expectations.

## R5 — Management permissions and UI registration

**Decision**: Register module `sms-gw`, exclusive API prefix `/api/sms-gw`, operations at `/api/sms-gw/v1`, permission pairs and module roles. Derive manifest from OpenAPI, register auth roles/grants separately and use gatewayclient lease recovery. Module assets live at `/ui/`; browser assets are relayed under `/m/sms-gw/`. Expose only federation exports declared by the manifest. Verify bearer identity and check permissions in the service; gateway headers alone convey no authority.
**Rationale**: `../go-tangra-notification-v4/pkg/notificationmanifest/manifest.go`, `internal/app/permissions.go`, `internal/httpapi/middleware.go`, `internal/httpapi/deps.go`, and `../go-tangra-portal-v4/sdk/pkg/gatewayclient/` define these contracts. Legacy `x-md-global-*` role checks are obsolete.
**Alternatives considered**: Register only portal metadata: does not register auth roles. Copy legacy menu YAML: lacks V4 route/permission contracts.

## R6 — Domain preservation and security

**Decision**: Port Voicecom sender/registry/status mapping, rendering, blocks, rate limits, proxy handling, public TLS/ACME and safe callback dispatcher, adapting framework imports. Preserve DLR `(message_id,status)` aggregation, acknowledgement and terminal state semantics with transactional locking. Existing duplicate callbacks increase parts count; this does not distinguish transport retries from actual parts. Keep raw request/response evidence protected and scrub credentials; document any deliberate redaction change. Seal provider and callback secrets at rest with a deployment KEK, borrowing the V4 notification envelope approach.
**Rationale**: Source `internal/provider/`, `internal/service/sms.go`, `internal/data/sms_repo.go`, `internal/webhook/`, `internal/housekeeper/`, `internal/acme/`, and schema files establish these rules. `../go-tangra-notification-v4/internal/sealed/` supplies a local encryption reference. A provider timeout must not trigger blind resend because acceptance may already have occurred.
**Alternatives considered**: New queue/retry delivery architecture: scope expansion. Claim exactly-once carrier delivery: cannot be guaranteed. Preserve plaintext credential exposure: conflicts with FR-012.

## R7 — Migration and deployment

**Decision**: Use a separate destination database. Import a read-only legacy snapshot into a required tenant, preserving global numeric IDs, message UUIDs, hashes, associations and timestamps; reject collisions rather than remap public IDs. Dry-run validates first; transactional apply records source fingerprint and checkpoints, advances sequences and reseals secrets. Source service remains untouched. Cutover pauses sends, drains callbacks, takes final snapshot, verifies destination, then changes routing. Rollback after new traffic requires reconciliation, not merely changing DNS.
**Rationale**: Existing schemas have no tenant field, numeric client/provider/template/block IDs and UUID messages. In-place destructive conversion unnecessarily risks the working source deployment.
**Alternatives considered**: Automatic tenant inference: source has no trustworthy mapping. Dual writers: duplicate-send and divergent-state risk. Entire production migration during specification: outside the request.

## Resolved uncertainties and limits

No design clarification remains. Runtime fixture capture must settle exact legacy defaults, nulls, unsigned status rendering and error details before code is changed. Dependency availability, deployment names and production tenant/secret values are implementation prerequisites, not guessed credentials. The placeholder constitution imposes no ratified gates. Tests use mock carriers and local ACME; no production messages or certificates are required.
