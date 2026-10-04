# Implementation validation

## Legacy capture (T001) — 2026-10-04

- `sg docker -c "python3 scripts/capture_legacy_runtime.py"`: built the legacy server and `cmd/test-linkmobility` from `../go-tangra-sms-gw` (clean checkout, read-only, `-mod=readonly`), ran them against an isolated `postgres:16` container and in-process failure/callback fakes, and recorded 154 runtime cases plus the legacy schema into `tests/fixtures/legacy/runtime/` in about one minute. Two consecutive runs produced identical files after normalization. Containers and the capture network are removed on exit.
- `python3 scripts/capture_legacy.py`: codec goldens and the original JWT/password race tests (unchanged from the earlier session).
- The earlier "Docker access denied" note is obsolete: Docker works when wrapped in `sg docker -c` with the sandbox disabled.

## Foundation (T006–T013) — 2026-10-04

- `go vet ./...` and `go vet -tags integration ./...`: clean.
- `go test -race -count=1 ./...`: config, sealed, authz, audit, metrics, repo and app unit tests pass.
- `sg docker -c "go test -race -tags integration -count=1 ./..."`: repository (cross-tenant references, RLS, owner views, atomic receipt aggregation, terminal guard, rollback, sequences, revocations, imports), app lifecycle (readiness, admin, extra listener, drain, failed-build cleanup) and seeded harness suites pass against `postgres:16` via testcontainers. Without docker they skip.

## US1 public Hermes listener (T014–T023) — 2026-10-04

- `go vet ./...` and `go vet -tags integration ./...`: clean.
- `go test -race -count=1 ./...`: auth (claims, kinds, expiry, `alg:none`, revocation, authority agreement, login log reasons), send pipeline (validation order, tenant references, blocks before rendering, bounds, uncertain carrier outcome stored once, early terminal status kept, evidence scrubbing), rendering/part counts, rate limiter, trusted proxies, Voicecom request/response/status table and the public router/codec (in-memory stores) pass.
- `sg docker -c "go test -race -tags integration -count=1 ./..."`: `tests/contract` replays the legacy capture against the full application: 154 cases accounted for (133 replayed byte for byte, 19 of them with the documented V4 changes of `docs/compatibility.md`, 3 observations, 18 receipt/callback cases deferred to US2). `tests/integration/public_test.go` runs two tenants with two client owners and a viewer: 100 concurrent sends are accepted and reach the mock carrier exactly once each under their stored UUID, blocked/disabled/foreign-reference/viewer/admin sends store nothing and make no carrier call, no read crosses an owner or tenant, and the page cap holds. Repository and app suites still pass (the app now binds the public listener itself).

## US2 receipts and callbacks (T024–T029) — 2026-10-04

- `go vet ./...` and `go vet -tags integration ./...`: clean.
- `go test -race -count=1 ./...`: receipt processor (tenant from the stored message, digest token comparison, forged/missing/prefix/foreign tokens, unknown and malformed receipts change nothing, sanitized audit, callbacks only for Hermes clients), receipt binding/acknowledgement and the rejected-receipt budget, and the callback dispatcher (exact payload and signature, unsigned callbacks, six attempts with doubling backoff, redirects not followed, queue overflow, private/rebinding/split-horizon destinations refused without retry, shutdown cancellation) pass.
- `sg docker -c "go test -race -tags integration -count=1 ./..."`: `tests/contract` replays all 154 captured cases (146 over HTTP, 19 with documented V4 changes, 8 observations including the 100 concurrent receipts and the four callback summaries) and races 100 mixed receipts plus 20 forged ones on one message (`TestReceiptTerminalRace`); `tests/integration/delivery_test.go` covers seeded messages with duplicate, out-of-order, forged and foreign-tenant receipts, polling, signed/unsigned pushes, a failing receiver that does not delay acknowledgement or polling, and a restart that keeps receipts and drops pending callback retries. All other suites still pass.

## US3 management backend (T030–T037, T047) — 2026-10-04

- `go vet ./...` and `go vet -tags integration ./...`: clean.
- `go test -race -count=1 ./...`: manifest (routes derived from the OpenAPI document, 29 protected routes under `/api/sms-gw/v1` with gateway-valid snake_case parameters, permissions/roles/grants/abilities/nav consistent, auth registration valid), handler/manifest parity (every declared operation handled with exactly its `x-freya-permission`; undeclared or mismatched routes refused at construction), chain order (authentication and permission before body validation), provider credential redaction/merge, template variables, dashboard (only predefined queries reach Prometheus, window bounds, NaN/Inf, not configured and unreachable states) and enrollment wiring pass.
- `sg docker -c "go test -race -tags integration -count=1 ./..."`: `tests/contract/management_test.go` runs the complete application with administrator, sender and viewer operators of one tenant and an administrator of another: role denials (403), foreign-tenant probes (404 for get/patch/delete/preview/reset/messages/receipts), secrets never in any response (carrier token, generated `dlr_token` after its one-time reveal, callback secret, password hashes), round-trip of redacted provider config, generated and reset passwords working on the public Hermes login, block references limited to the tenant, manual send through the public pipeline recorded with the platform actor, in-use deletes (409), dashboard unavailable state; refusals for missing/unknown/Hermes tokens (401), every legacy identity header (403, also with a valid token), verification and permission-check outages (503), malformed and oversized bodies, bad path parameters, and the separation of the public and management listeners. Repository list sorting/search/clamp and sealed creates pass against PostgreSQL. All earlier suites still pass.
- Not exercised here: lease registration and auth permission registration against a running gateway/auth (the stack deployment is T050; the workers are covered only by construction and the manifest/registration validation above).

## Unavailable prerequisites

None for Phases 1–4 and the US3 backend. UI package installation uses the registry token in later phases.

## Next sequential gate

US1–US4 may start on the foundation. Later phases must replay `tests/fixtures/legacy/runtime/` and implement the deliberate changes listed in `docs/compatibility.md`. No production deployment, real carrier send or certificate issuance took place.
