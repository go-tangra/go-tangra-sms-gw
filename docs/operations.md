# Operations

Migration from the legacy service, cutover and rollback are in [migration.md](migration.md).

## Deployment, bootstrap and version

- `smsgwsvc bootstrap -config <file>` prepares a deployment and exits: it validates the configuration (legacy `SMS_GW_*` variables applied), applies the migrations with `db.migrate_dsn`, grants the role of `db.dsn` its table rights (the migration grants only a role named `smsgw_app` that already exists), checks that this role cannot bypass row-level security (superuser or BYPASSRLS is refused in production, reported as `row_level_security: bypassed` otherwise), seals and opens a probe with the KEK and reads the Hermes JWT secret (at least 32 bytes). It is idempotent (`migrations_applied: 0` on a second run) and creates no accounts, providers or other records; the JSON report holds no secret. Exit status 1 on any failed check.
- The service applies pending migrations on start unless started with `-no-migrate` (then run bootstrap first, e.g. as an init job with the migration role, and give the service only the application role).
- `smsgwsvc version` prints the build version (`-ldflags -X main.version=…`, set from `APP_VERSION` by the Dockerfile); the same value is reported to the gateway at registration. The image also contains `smsgw-migrate`.
- Local dependencies: `deploy/compose.yaml` (project `smsgw`, loopback ports only): `postgres` (127.0.0.1:5437, database `sms_gw`, role `smsgw_app` from `deploy/init-db.sql`), `carrier` (mock Voicecom, 127.0.0.1:9916), `callbacks` (client callback receiver, 127.0.0.1:9917), profile `legacy` adds `legacy-db` (127.0.0.1:5438, the captured legacy schema plus `tests/fixtures/legacy/snapshot.sql`), profile `acme` adds `pebble` (local ACME CA, host network, directory `https://127.0.0.1:14010/dir`), profile `service` runs the image with `deploy/container.yaml` and mounted `/secrets`, `/identity`, `/state` and `/acme`. The platform (gateway, auth, portal, lcm) is a sibling deployment reached through `discovery`, `gateway` and `enroll`. Development-only settings in these files (`webhook.allow_http`/`allow_private`, `enroll.insecure`, `acme.allow_insecure_directory`, plaintext database connections, "dev" passwords) are refused or warned about by configuration validation outside `env: dev`.

## Configuration and legacy environment

The YAML file (unknown keys are rejected) is the configuration; the legacy environment still overrides it, and an unparseable value stops startup instead of falling back silently. Secrets are references (`{file: …}` with mode 0640 or stricter, or `{env: …}`), never values in the file.

| Legacy variable | Configuration |
|---|---|
| `SMS_GW_JWT_SECRET` | `public_auth.jwt_secret` (becomes `{env: SMS_GW_JWT_SECRET}`) |
| `SMS_GW_ACCESS_TTL_SECONDS`, `SMS_GW_REFRESH_TTL_SECONDS` | `public_auth.access_ttl_seconds`, `refresh_ttl_seconds` |
| `HTTP_PUBLIC_BIND`, `SMS_GW_TLS_BIND` | `public.http_addr`, `public.https_addr` |
| `SMS_GW_TLS_CERT_PATH`, `SMS_GW_TLS_KEY_PATH` | `public.tls_cert_file`, `public.tls_key_file` |
| `TRUSTED_PROXY_CIDRS` | `public.trusted_proxies` |
| `SMS_GW_{LOGIN,SEND,DLR}_{RPM,BURST}` | `rate_limits.*` |
| `SMS_GW_MSISDN_MIN_DIGITS`, `SMS_GW_ALLOWED_PREFIXES`, `SMS_GW_BLOCKED_PREFIXES` | `recipients.*` |
| `SMS_GW_ALLOW_HTTP_WEBHOOKS` | `webhook.allow_http` |
| `SMS_GW_HOUSEKEEPING_INTERVAL` | `retention.interval` (Go duration, 1m–24h; the legacy bare-seconds form is refused) |
| `SMS_GW_PROMETHEUS_URL` | `monitoring.prometheus_url` |
| `SMS_GW_ACME_*` (`ENABLED`, `DOMAINS`, `EMAIL`, `DIRECTORY_URL`, `DIRECTORY_CA_BUNDLE`, `CACHE_DIR`, `ACCEPT_TOS`, `RENEW_BEFORE`, `CHALLENGE`, `HTTP_BIND`, `PREFETCH`, `EAB_KID`, `EAB_HMAC_KEY`, `ALLOW_INSECURE_DIRECTORY`) | `acme.*` (`EAB_HMAC_KEY` becomes an env reference) |

Obsolete and ignored (reported at start): `GRPC_ADVERTISE_ADDR`, `HTTP_ADVERTISE_ADDR`, `ADMIN_GRPC_ENDPOINT`, `FRONTEND_ENTRY_URL`, `SMSGW_CA_CERT_PATH`, `SMSGW_SERVER_CERT_PATH`, `SMSGW_SERVER_KEY_PATH`, `LCM_BOOTSTRAP_ENDPOINT`, `MODULE_BOOTSTRAP_SECRET`, `LCM_CA_FINGERPRINT`, `CERTS_DIR` — mesh identity, registration and the admin transport are V4 platform functions now. Differences from the legacy precedence: static TLS and ACME together are a configuration error (the legacy service silently preferred ACME).

## Secrets and persistent state

| What | Where | Loss means |
|---|---|---|
| KEK (`kek.path`/`kek.env`, 32 bytes, base64 or raw) | secret store, mounted 0600 | provider configurations and callback secrets no longer open (providers cannot send, callbacks are skipped); keep it backed up with the database backups |
| Hermes JWT secret (`public_auth.jwt_secret`) | secret store | every Hermes token becomes invalid (clients log in again); changing it on purpose is the way to revoke all tokens |
| Database | PostgreSQL | all data; back up with the KEK |
| Mesh identity: file SVID (`identity.file`) or enrollment state (`enroll.state_file`) and the single-use token (`enroll.token_file`) | mounted volume (`/state`) | with enrollment: a new single-use token is needed to enroll again |
| ACME account key and certificates (`acme.cache_dir`) | persistent volume (`/acme`), 0700, written by the service user | re-issuance from the CA at the next start (CA rate limits); an unwritable directory disables public HTTPS (see below) |

Secrets never appear in logs, audit records, metrics, reports or API responses; log attributes with credential names are redacted.

## Identity enrollment, rotation and expiry

- File identity (`identity.provider: file`): the certificate, key and bundle are polled every 2 s; replacing them rotates the identity without a restart (`identity renewed` and an `identity_renewed` audit event with the new serial).
- Network enrollment (`enroll.enabled`, `identity.provider: provided`): first start enrolls with the single-use token over `enroll.enroll_url`, later starts reuse `enroll.state_file`, renewal runs over mTLS to `enroll.lcm_grpc` before expiry. A token that is unreadable, used or expired stops startup; mint a new one with the platform's enrollment tooling.
- Expiry without renewal: `identity expired and no renewal available: refusing all calls until renewed` (audit `identity_expired`). Readiness turns 503 (`"identity":"unavailable"`), the gateway lease is released (the gateway stops routing), mesh calls in both directions are refused, and protected operations fail closed. The public Hermes listener is a separate trust domain and keeps serving. A valid identity restores readiness and the registration within seconds.
- Pooled mesh connections keep the identity they were established with until they reconnect; a rotated identity is used for every new connection (verified in `tests/integration/operations_test.go`).
- Registration outages (gateway or auth unreachable, also at startup) are retried indefinitely (`gateway connection unavailable; retrying`, `auth registration unavailable; retrying` every 5 s) and never affect readiness or the public listener.

## Public HTTPS

The public Hermes edge always serves plain HTTP on `public.http_addr`. HTTPS on `public.https_addr` is optional and uses its own keys, never the mesh identity:

- Static (`public.tls_cert_file`/`tls_key_file`): loaded once at start (rotate by restarting), TLS 1.2 minimum; a pair that does not load, does not match, is expired or not yet valid disables HTTPS.
- ACME (`acme.enabled`): certificates for `acme.domains` from `acme.directory_url` (optionally trusting only `acme.directory_ca_bundle`), with EAB when configured. `http-01` is answered under `/.well-known/acme-challenge/` on the plain public listener (ahead of the Hermes routes; a dedicated listener on `acme.http_addr` is optional and its bind failure is only a warning); `tls-alpn-01` on the HTTPS listener. The account key and certificates are cached in `acme.cache_dir` (checked writable at start): a restart serves the cached certificate without contacting the CA. Renewal is automatic at `notAfter − renew_before` (plus up to 1 h jitter); `acme.prefetch` obtains the certificate 3 s after start instead of at the first handshake.
- Failures never stop the service: an unusable certificate, cache directory, CA bundle or EAB key, or an HTTPS port that cannot be bound, logs a warning (`public certificate unusable…`, `acme configured but unusable…`, `public https listener unavailable…`) and readiness reports `"public_tls":"unavailable"` while staying 200. `public_tls` is `disabled`, `static`, `acme` or `unavailable`.

## Retention

The housekeeper deletes messages older than their provider's `retention_days` (per tenant and provider; 0 keeps them forever) together with their receipts (one statement, cascade), oldest first in batches of 1000. The first pass runs 30 s after start, then every `retention.interval` (1m–24h, default 1h); a pass never overlaps the next, is bounded to 10 minutes, and stops between batches on shutdown (the batch in flight rolls back). A failing provider does not stop the others. Login records, audit records and token revocations are not affected by provider retention (revocations are purged after the token's expiry). Telemetry: `sms_gw.housekeeping.runs{outcome=ok|partial|error}`, `sms_gw.housekeeping.messages_deleted{provider=<type>}`, and an info log per tenant provider with deletions.

## Metrics

Served on the admin listener (`/metrics`, never on the public listener):

- `sms_gw.send{provider,outcome}`, `sms_gw.send.duration{provider,outcome}` — Hermes and manual sends (`ok`, `forbidden`, `validation_error`, `blocked`, `provider_error`, `rate_limited`, `internal_error`, …).
- `sms_gw.dlr.received{status}`, `sms_gw.dlr.rejected{reason}`, `sms_gw.delivery.latency{provider}` — receipts (see below).
- `sms_gw.webhook.delivery{outcome}` — client callbacks.
- `sms_gw.auth.login{outcome}` — Hermes logins.
- `sms_gw.housekeeping.*` — retention.
- Framework metrics of the mesh transports and identity.

## Failure diagnosis

| Symptom | Check |
|---|---|
| `/readyz` 503, `identity: unavailable` | identity expired or not loaded: identity logs/audit (`identity_expired`), certificate files or enrollment state and lcm reachability |
| `/readyz` 503, `database: unreachable` | `db.dsn`, network, pool exhaustion; sends and receipts fail closed |
| `public_tls: unavailable` | the warning logged at start names the cause; plain HTTP keeps working |
| Module missing from the portal | gateway lease (`gateway lease` logs), gateway allow-list for `spiffe://<td>/svc/sms-gw` → `/api/sms-gw`, readiness |
| Operators get 503 | auth unreachable for token verification or permission checks (fail closed) |
| `sms_gw.dlr.rejected{reason="unavailable"}` | database or provider configuration (KEK) problem; carriers must resend |
| Provider sends fail with an internal error after a restore | KEK differs from the one that sealed the configuration |

Carriers call `GET /dlr` or `GET /hermes/v1/sms/dlr` on the public listener with the receipt in the query and the provider's `dlr_token`. The answer is always `200 "DLR_OK"`; what happened is visible only in telemetry:

- `sms_gw.dlr.received{status}` counts receipts by reported status; `sms_gw.dlr.rejected{reason}` counts those acknowledged without effect (`bad_token`, `unknown_message`, `malformed`, `rate_limited`, `unavailable`).
- Forged tokens are also audited as `receipt.rejected` (tenant and message id only).
- `unavailable` means the database or the provider configuration could not be read; the receipt is lost unless the carrier resends it, so alert on it.

`rate_limits.dlr_per_minute`/`dlr_burst` (default 500/min, burst 100) budget rejected receipts per client address. Valid receipts never consume it, so a carrier is not throttled; an address that keeps sending forged or unknown receipts is acknowledged without processing until its bucket refills. Behind a load balancer, list it in `public.trusted_proxies` so the budget applies to the real carrier address.

Receipts for a message are serialized by a row lock; terminal states (1, 2, 16, ≥1000) are final and later receipts are kept only as history.

## Client callbacks

Every accepted receipt of a message sent by a Hermes client with a callback URL is queued for a `POST` to that URL (signed when the client has a secret). The queue is in memory and best effort, like the source:

- `webhook.queue_size` (default 4096) bounds the queue; when it is full the new callback is dropped (`sms_gw.webhook.delivery{outcome="dropped"}`).
- `webhook.workers` (default 4) deliver in parallel; each callback gets six attempts with 0.2, 0.4, 0.8, 1.6 and 3.2 s gaps and a 10 s timeout per attempt. Only 2xx counts; redirects are not followed. Final outcomes are `delivered` or `failed`.
- A destination that is not public (private, loopback, link-local, CGNAT, documentation or reserved ranges, or a name resolving to one) is refused without retry. `webhook.allow_http` and `webhook.allow_private` exist for local receivers only and are refused in production.
- Nothing is persisted: callbacks still queued or between retries when the service stops (deploy, restart, crash) are lost, and in-flight attempts are cancelled. There is no later redelivery. Clients that need every state change must poll `GET /hermes/v1/sms/dlr/{id}` (receipts are stored before the callback is queued, so polling always shows them).
- A slow or failing receiver occupies a worker for at most about 66 s per callback; it delays other clients' callbacks only when all workers are busy, and never delays receipt ingestion or the carrier acknowledgement.

Drain callbacks before a planned cutover by pausing sends and waiting until `sms_gw.webhook.delivery` stops increasing.

## Platform registration and identity

- Mesh identity: with `enroll.enabled` (and `identity.provider: provided`) the service enrolls for its SVID over lcm at start (`lcmidentity.NewNet`: local key, single-use join token from `enroll.token_file`, state persisted in `enroll.state_file`, renewal over mTLS); otherwise the configured Freya identity provider applies. An unreadable token or failed enrolment stops startup.
- Once ready (identity and database), the service keeps a gateway lease (`gateway.service`) for the manifest built from `api/openapi/sms-gw.yaml`, advertising its actual mesh HTTP endpoint; when readiness is lost the lease is released and re-established after recovery. Separately it registers its permissions, module roles (administrator, sender, viewer) and owner/admin grants with auth (`gateway.auth_service`), retrying every 5 s until accepted and refreshing every 5 minutes. Both stop on shutdown. The gateway must allow the `sms-gw` identity to own `/api/sms-gw` (stack allow-list).
- Operator permission decisions are cached for 30 s; a role change in auth applies within that time. Verification or permission-check outages answer 503 and never allow.

## Monitoring dashboard

`monitoring.prometheus_url` (legacy `SMS_GW_PROMETHEUS_URL`) points the dashboard at the Prometheus that scrapes the admin listener's `/metrics`. Empty disables it (`available: false`, `not_configured`); errors show as `unreachable` and never affect SMS endpoints. Only the predefined queries run. The figures aggregate every tenant of the deployment.
