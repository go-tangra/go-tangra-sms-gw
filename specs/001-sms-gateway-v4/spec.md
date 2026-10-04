# Feature Specification: SMS Gateway for Tangra V4

**Feature Branch**: No Git branch available in this workspace; feature identifier `001-sms-gateway-v4`.
**Created**: 2026-10-04
**Status**: Draft — validated for planning
**Input**: User description: "use speckit to create a specification and tasks for a project /home/jadmin/projects/go-tangra/go-tangra-sms-gw but to be compatible with V4 of the tangra framework"

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Continue sending SMS through existing integrations (Priority: P1)

An existing SMS API consumer changes the gateway host and continues to authenticate, send messages, and read its own messages and delivery receipts without changing its request or response handling.

**Why this priority**: Sending SMS and preserving existing integrations are the gateway's primary business purpose.
**Independent Test**: With seeded client, provider and template records and a mock carrier, log in, send, list, fetch and poll receipts using captured legacy requests; compare the observable responses to the source gateway.

**Acceptance Scenarios**:
1. **Given** an enabled client and provider, **When** the client logs in and submits a valid template-based send, **Then** exactly one message is recorded and exactly one carrier submission occurs with the rendered text and the existing response envelope.
2. **Given** two clients with messages, **When** either client lists, fetches or polls receipts, **Then** only its own records are visible; a read-only client cannot send.
3. **Given** expired, revoked, wrong-kind or malformed credentials, **When** a protected request is submitted, **Then** access is denied with the established error contract and no carrier call occurs.
4. **Given** a global or provider-specific active recipient block, **When** a send is attempted, **Then** it fails before rendering or persistence and leaves no message record or carrier submission.

### User Story 2 - Track delivery reliably (Priority: P1)

A consumer receives accurate delivery state through polling or its configured signed callback even when the carrier repeats or reorders multipart receipts.

**Why this priority**: Consumers need trustworthy delivery outcomes and carriers depend on the receipt acknowledgement contract.
**Independent Test**: Seed a message directly; submit authenticated carrier callbacks in duplicate, out-of-order and invalid-token sequences; inspect receipts, final state and a local callback receiver without requiring the admin UI.

**Acceptance Scenarios**:
1. **Given** a matching message and valid receipt token, **When** multiple callbacks report the same status, **Then** one receipt record exists for that message/status with an incremented parts count.
2. **Given** a terminal message, **When** a late receipt arrives, **Then** it is recorded for audit and the terminal message state remains unchanged.
3. **Given** a missing/wrong token, malformed receipt or unknown message, **When** the carrier calls either receipt endpoint, **Then** it receives the established acknowledgement and no unauthorized state change occurs.
4. **Given** a client with a configured callback, **When** a receipt is accepted, **Then** the client receives the existing payload and signature format; blocked destinations or failed outbound requests do not block receipt ingestion.

### User Story 3 - Administer the gateway from Tangra V4 (Priority: P1)

A platform operator opens the gateway in the V4 portal and manages providers, templates, API clients and blocks, searches messages, inspects receipts, sends manually and views the dashboard according to assigned permissions.

**Why this priority**: V4 compatibility requires usable, authorized administration inside the platform.
**Independent Test**: With seeded data and V4 identities for administrator, sender and viewer, register the module, navigate all pages and exercise each allowed and denied action through the portal and directly against the protected service.

**Acceptance Scenarios**:
1. **Given** an administrator, **When** the module is registered, **Then** its permitted navigation and pages load and provider type metadata drives the provider configuration form.
2. **Given** a sender, **When** a manual send uses a template, **Then** variable discovery, preview, character/part counts and server-side send authorization agree; a viewer cannot submit the same send directly.
3. **Given** an operator searching messages, **When** recipient prefix, carrier SID or client username filters are applied, **Then** matching records and their receipt history are shown with bounded pagination.
4. **Given** a user lacking a permission or belonging to a different tenant, **When** that user requests a management operation or record, **Then** access is denied regardless of navigation visibility or supplied identity headers.
5. **Given** unavailable monitoring, **When** the dashboard opens, **Then** it reports unavailability while SMS and other management operations remain usable.

### User Story 4 - Migrate and operate safely (Priority: P2)

An operator imports existing gateway data into a V4 deployment, verifies compatibility, and switches traffic with an auditable rollback path and normal platform operations.

**Why this priority**: Existing credentials, identifiers and message history must survive the migration.
**Independent Test**: Import a representative isolated database copy, reconcile every entity and relationship, authenticate an existing client, read historical receipts, restart the deployment and rehearse rollback before production traffic.

**Acceptance Scenarios**:
1. **Given** a legacy snapshot and an explicitly selected destination tenant, **When** migration runs in dry-run and then apply mode, **Then** identifiers, password hashes, references, statuses, timestamps and retention settings are preserved, conflicts are reported, and rerunning does not duplicate records.
2. **Given** a healthy deployment, **When** it restarts or its platform identity rotates, **Then** it resumes serving with persisted data and registration, and gracefully drains in-flight work on shutdown.
3. **Given** static public certificates or valid automatic-certificate configuration, **When** public HTTPS is enabled, **Then** it serves the same public operations while internal identity remains separate; public certificate configuration failure leaves internal administration available and reports the failure.
4. **Given** configured provider retention, **When** housekeeping runs, **Then** expired messages and their receipts are removed together, a zero retention value preserves records, and other tenants/providers are unaffected.

### Edge Cases

- Disabled client/provider/template; unknown provider type; unsupported channel; malformed recipient, properties, template or paging; foreign tenant references.
- Template variables missing, oversized rendered output, and characters at GSM/Unicode multipart boundaries.
- Carrier timeout after possible acceptance: record an explicit uncertain/failure result without automatic resend; no exactly-once delivery claim across network failure.
- Receipt before carrier response persistence, concurrent multipart callbacks, negative initial status and legacy unsigned status representation, unknown carrier codes and terminal-to-terminal receipts.
- Token refresh/logout and password reset races; attempted cross-client and cross-tenant reads; `API_ADMIN` exists in source schema but must not silently gain new authority.
- DNS rebinding, redirects and private-address destinations for callback URLs; forwarded client addresses from untrusted proxies.
- Auth or portal unavailable during startup, expired internal identity, unavailable monitoring, full database or unwritable public certificate cache.
- Duplicate names/IDs in import, orphaned receipts, source credentials absent from a snapshot, and switching back after V4 has accepted new messages.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Preserve the existing public login, logout, refresh, current-client, send, list, get and receipt-polling contracts, including supported aliases, field spellings, token claims, status/error envelopes and default pagination behavior.
- **FR-002**: Keep API-client credentials distinct from platform operator identities; enabled read/write clients can send, read-only clients cannot, and clients cannot read another client's messages or receipts. Unsupported authority values MUST fail closed.
- **FR-003**: Preserve the existing Voicecom/LinkMobility provider, extensible provider-type metadata, configuration validation, provider selection and enabled/disabled behavior. Additional carriers and Viber delivery are outside this migration.
- **FR-004**: Preserve template management, rendering from request properties, manual-send preview and encoding/part calculation; reject invalid inputs before a carrier call.
- **FR-005**: Enforce active global and provider-specific blocks before rendering or message persistence.
- **FR-006**: Record message identity, owner, provider/template references, carrier SID, timestamps and delivery state; preserve initial, intermediate and terminal status semantics and prevent late receipts from replacing terminal outcomes.
- **FR-007**: Authenticate carrier receipts using the stored receipt secret/token, aggregate repeated message/status callbacks atomically, preserve parts counts and audit history, and retain the source's `200 DLR_OK` acknowledgement even when a receipt is rejected.
- **FR-008**: Preserve per-client outbound receipt callbacks, signature and payload conventions, destination safety checks and bounded asynchronous dispatch; failed push MUST NOT prevent polling or receipt ingestion.
- **FR-009**: Register management operations, permissions, roles, navigation and UI assets with Tangra V4; verify operator identity and permissions at the service as well as at the portal, without trusting legacy role headers.
- **FR-010**: Provide provider/template/client/block CRUD, password reset, provider-type discovery, message/receipt inspection, filters, manual send and existing dashboard windows through the V4 portal.
- **FR-011**: Scope all management data to the verified tenant; bind every public API client to one tenant at creation/import and derive public request tenant from that client. No caller-supplied tenant override may cross the boundary.
- **FR-012**: Protect secrets from responses, logs, audit and UI; preserve existing password hashes during migration and record operational/security events without message bodies or token material.
- **FR-013**: Support V4 service identity enrollment/loading, identity rotation, protected internal transport, health/readiness, configuration validation, observable failures, registration recovery and graceful shutdown. Expired identity MUST deny protected traffic.
- **FR-014**: Preserve public HTTP and optional static/automatic HTTPS behavior, challenge paths, certificate persistence and renewal with separate internal/public keys; expose management only through protected internal transport.
- **FR-015**: Preserve login/send limits, trusted-proxy rules, monitoring and provider retention; optional monitoring failure MUST NOT stop core operations.
- **FR-016**: Provide a dry-run and repeatable import into an explicit tenant with entity/relationship reconciliation and a backup/cutover/rollback procedure. Existing IDs and credentials MUST remain usable; token continuity requires explicitly preserving compatible secrets or documenting re-login.
- **FR-017**: Supply automated public-contract, permission/tenant-isolation, delivery-race, migration and V4 platform-integration verification using mock carriers, plus UI validation for every management page. Real carrier sends are not needed for acceptance.

### Key Entities *(include if feature involves data)*

- **Tenant scope**: Verified platform tenant or the tenant bound to a public API client; owns every gateway record.
- **API client**: Existing numeric identity, immutable username, password hash, authority, status, login metadata and optional callback URL/secret.
- **Provider**: Numeric identity, name, provider type, channel, protected configuration, status and retention days.
- **Template**: Numeric identity, name, channel, named text fragments and status.
- **Block**: Recipient, optional provider scope, channel, status, reason and creator.
- **SMS message**: UUID, owner, provider/template references, recipient, rendered text, carrier SID, request/response evidence, timestamps and status.
- **Delivery receipt**: Message association, carrier status, timestamp, sender/recipient, source address and aggregated parts count.
- **Login/security event**: Caller/client, tenant when resolvable, outcome, timestamp and sanitized source metadata.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: 100% of captured legacy public-operation scenarios retain their methods, paths, payload semantics, token/error conventions and visible outcomes, except documented secret redaction and tenant isolation.
- **SC-002**: 100% of negative authorization scenarios deny viewer writes, foreign-client reads, foreign-tenant access and forged receipt state changes.
- **SC-003**: Concurrent replay of 100 receipts for one message/status produces one aggregated record with parts count 100; all late-receipt scenarios preserve terminal outcomes.
- **SC-004**: All seven management areas (providers, templates, clients, blocks, messages/receipts, manual send, dashboard) are usable from the V4 portal under their respective permissions.
- **SC-005**: A migration fixture containing every entity type retains 100% of IDs, references, hashes, statuses and timestamps, and a second import produces zero duplicate records.
- **SC-006**: In the local acceptance stack, 100 valid mock-carrier sends complete without gateway-caused loss, duplicate carrier submission or cross-owner disclosure; restart, registration recovery and identity-rotation scenarios pass.

## Assumptions

- Source of behavior is the local `../go-tangra-sms-gw` checkout; destination is this `go-tangra-sms-gw-v4` workspace. This request produces design artifacts and tasks, not implementation.
- Compatibility targets the checked-out Tangra V4 framework and V4 auth, portal and LCM SDKs; pinned versions and integration details belong in the plan.
- Existing public accounts remain gateway-managed. Platform authentication is for operators; public username lookup remains unambiguous across tenants, with globally unique legacy usernames.
- Tenant isolation is required by V4 deployment conventions. A legacy installation is assigned to one explicit tenant per import; no automatic inference from legacy users.
- Public-client ownership rules remain unchanged. Manual platform sends use a distinct actor identity and never borrow an API client's numeric owner.
- Captured wire responses govern compatibility when README prose and runtime code differ; intentional security changes are documented and covered by tests.
- PostgreSQL remains the persistent store. No new carrier, scheduled sending, notification-service integration, live stream, backup product UI or throughput guarantee is added.
- The existing constitution is an unratified placeholder; no governance principles are inferred from its template examples.
