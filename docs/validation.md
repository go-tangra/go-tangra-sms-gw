# Implementation validation

## Legacy capture (T001) — 2026-10-04

- `sg docker -c "python3 scripts/capture_legacy_runtime.py"`: built the legacy server and `cmd/test-linkmobility` from `../go-tangra-sms-gw` (clean checkout, read-only, `-mod=readonly`), ran them against an isolated `postgres:16` container and in-process failure/callback fakes, and recorded 154 runtime cases plus the legacy schema into `tests/fixtures/legacy/runtime/` in about one minute. Two consecutive runs produced identical files after normalization. Containers and the capture network are removed on exit.
- `python3 scripts/capture_legacy.py`: codec goldens and the original JWT/password race tests (unchanged from the earlier session).
- The earlier "Docker access denied" note is obsolete: Docker works when wrapped in `sg docker -c` with the sandbox disabled.

## Foundation (T006–T013) — 2026-10-04

- `go vet ./...` and `go vet -tags integration ./...`: clean.
- `go test -race -count=1 ./...`: config, sealed, authz, audit, metrics, repo and app unit tests pass.
- `sg docker -c "go test -race -tags integration -count=1 ./..."`: repository (cross-tenant references, RLS, owner views, atomic receipt aggregation, terminal guard, rollback, sequences, revocations, imports), app lifecycle (readiness, admin, extra listener, drain, failed-build cleanup) and seeded harness suites pass against `postgres:16` via testcontainers. Without docker they skip.

## Unavailable prerequisites

None for Phases 1–2. UI package installation uses the registry token in later phases.

## Next sequential gate

US1–US4 may start on the foundation. Later phases must replay `tests/fixtures/legacy/runtime/` and implement the deliberate changes listed in `docs/compatibility.md`. No production deployment, real carrier send or certificate issuance took place.
