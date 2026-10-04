# Implementation validation

## Legacy capture (T001) — 2026-10-04

- `sg docker -c "python3 scripts/capture_legacy_runtime.py"`: built the legacy server and `cmd/test-linkmobility` from `../go-tangra-sms-gw` (clean checkout, read-only, `-mod=readonly`), ran them against an isolated `postgres:16` container and in-process failure/callback fakes, and recorded 154 runtime cases plus the legacy schema into `tests/fixtures/legacy/runtime/` in about one minute. Two consecutive runs produced identical files after normalization. Containers and the capture network are removed on exit.
- `python3 scripts/capture_legacy.py`: codec goldens and the original JWT/password race tests (unchanged from the earlier session).
- The earlier "Docker access denied" note is obsolete: Docker works when wrapped in `sg docker -c` with the sandbox disabled.

## Unavailable prerequisites

None for Phase 1. UI package installation uses the registry token in later phases.

## Next sequential gate

Phase 2 (T006–T013) may proceed: the compatibility gate is closed. Later phases must replay `tests/fixtures/legacy/runtime/` and implement the deliberate changes listed in `docs/compatibility.md`. No production deployment, real carrier send or certificate issuance took place.
