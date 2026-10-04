# Compatibility decisions and capture status

The Hermes listener preserves legacy public routes and payload conventions; V4 management is a separate contract. Source snapshots, codec goldens and database-backed runtime recordings of the legacy service are in tests/fixtures/legacy/ (see its README). V4 parity tests replay `tests/fixtures/legacy/runtime/`.

Source inspection corrected one planning error: logout is deliberately whitelisted, succeeds without a token and independently validates/revokes Authorization and X-Refresh-Token when supplied. It must remain accessible after access-token expiry. The request id does not grant revocation authority. The source rate-limit key for send is client ID, with IP fallback; it is not a concatenated client/IP key. Source logout uses an in-memory denylist; destination persistent revocation is an intentional improvement.

Security changes planned: tenant-scoped data, fail-closed unsupported authority, credential sealing/redaction, bounded queries and protected V4 management. Exact raw-evidence redaction and pagination limits must be documented with runtime fixtures before release.

The public Hermes listener is implemented (US1, see below); receipt ingestion, callbacks, management and migration follow in later stories. No legacy database, carrier or production certificates have been touched.

## Captured transport behavior

The actual Kratos v2 protobuf JSON codec emits camel-case field names unless a proto has an explicit json_name override: `endpointResponse`, `returnCode`, `statusMessage`, `rawRequest`, `rawResponse`, `requestId`, `messageStatus`, `remoteAddrerss`. Proto source snake-case identifiers do not mean snake-case JSON. Proto uint64 fields encode as quoted decimal strings, including recipient `to`; the callback dispatcher uses standard JSON and emits numeric `to`. The source unsigned Message.status encodes initial -1 as 4294967295.

The receipt wrapper uses `Result(200, "DLR_OK")`: with its default JSON codec the body is the JSON string `"DLR_OK"` and Content-Type application/json, not an unquoted plain-text body. Keep this source-observed distinction in the adapter. `tests/fixtures/legacy/dlr-ack.json` records the codec output.

Codec goldens use synthetic, sanitized values; the runtime recordings below come from the running legacy service.

## Runtime-established legacy behaviour (T001)

Recorded from the running legacy service (source commit in `tests/fixtures/legacy/provenance.json`). File names refer to `tests/fixtures/legacy/runtime/`.

Preserved on the public listener:

- Routes and aliases: `/hermes/v1/{login,refresh_token,logout,me}`; send/list/get/receipts under both `/hermes/v1/sms` and `/v1/sms`; carrier receipts only via `GET /dlr` and `GET /hermes/v1/sms/dlr`. Unknown paths and wrong methods (including `POST /dlr`, `GET /hermes/v1/logout`) return 404 `text/plain` `404 page not found`, not 405 (`surface-*`, `dlr-post-method`, `logout-get-method`).
- Login accepts JSON and form bodies; `grant_type` is not checked; empty username or password is 400 `BAD_REQUEST`; unknown user and wrong password share 401 `invalid credentials`; disabled is 401 `account disabled`; malformed JSON is 400 `CODEC` (`login-*`). `expires_in` is the quoted string `"7200"`.
- JWT: HS256, claims `authority, kind, username, iss=sms-gw, sub=<client id>, iat=nbf, exp, jti`; access 7200 s, refresh 604800 s; the pair shares one 32-hex `jti` (`jwt-claims`). Error messages: `missing bearer token`, `invalid token` (bad signature, `alg:none`, non-`Bearer ` scheme including lowercase `bearer`), `token expired` (also for a future `nbf`), `wrong token kind` (missing or mismatched `kind`).
- Refresh issues a new pair but does not rotate: the old refresh token stays usable, and the authority claim is copied from the presented token, not re-read from the account (`refresh-json`, `refresh-stale-authority-claim`). Disabled account is 401 `account disabled`; unknown client is 404 `api client not found`; a subject above uint32 is 401 `invalid subject`.
- Logout is unauthenticated and always `200 {}`; it revokes whatever valid Bearer/`X-Refresh-Token` it is given. Because the pair shares a jti, revoking the refresh token also revokes its access token, including after the access token expired (`logout-*`, `me-after-logout`, `refresh-after-logout`).
- `GET /me` returns the database profile and static abilities (API_CLIENT four, API_VIEWER three, any other authority `[]`); it does not check the authority claim. A token whose subject does not resolve is 404 `api client not found`.
- Send validation order and errors: authority (403 unless API_CLIENT), providerId 0 (400), recipient 0/length 7–15 (400), provider missing (404)/non-SMS/OFF (400), active block for the provider or provider 0 (400), templateId 0 (400), template missing (404)/OFF/non-SMS (400), missing `body` fragment (400), bad template syntax (500 `INTERNAL_ERROR`), unknown provider type (500). None of these creates a row (`send-rejections-persisted`). Disabled blocks are not enforced. Unknown body fields (`text`, `defer`) are ignored; `to` accepts a JSON number or string; proto names `provider_id`/`template_id` are accepted alongside `providerId`/`templateId`. Missing template variables render as `<no value>`.
- Message JSON always has `sms:null`, `rawRequest:""`, `rawResponse:""`, `userName:""` and `providerId:0` (the legacy mapper never fills them); the send response also has empty `providerName`/`apiClientUsername`, which Get/List fill. Status is uint32: a message read while its carrier call is pending shows `4294967295` (`get-while-carrier-pending`).
- Carrier outcomes: a carrier return code is stored and returned with HTTP 200 (`2001` → `sms_invalid_sid`); HTTP 5xx or a dropped connection returns HTTP 500 with an empty reason and the carrier error text, stores status 500 with that text, and is never resent (`send-carrier-*`).
- List: default page 1/size 50, negative values fall back to defaults, ordering is newest first, `pageSize` is unbounded (100000 accepted), `nopaging`, `orderBy` and `or` are ignored; `query` is a JSON object with `recipient` (prefix), `sid`, `status`, `api_client_username`; malformed `query` is ignored; a non-numeric `page` is 400 `CODEC` (`list-*`).
- Ownership: API_CLIENT and API_VIEWER see only their own messages and receipts (foreign or unknown ids are 404 `sms not found`); an unknown authority sees nothing.
- Receipts: always `200 "DLR_OK"` (`application/json`). Query names are the snake-case proto names; camel-case names are not bound. Wrong or missing `dlr_token` and unknown/empty `request_id` change nothing; a provider without a stored token accepts any token. Duplicate `(message, status)` receipts increment `parts_received`; once the message is terminal (1, 2, 16, ≥1000) later receipts are stored but do not change it; unknown codes are stored with empty status text.
- Outbound callback: `POST` JSON `{"message_id","channel","message_status","status_text","to","from","timestamp"}` with numeric `to`/`timestamp` and the canonical slug as `status_text`. With a secret, headers go out as `X-Smsgw-Timestamp` and `X-Smsgw-Signature` (Go canonical spelling of `X-SmsGw-*`; header names are case-insensitive) and the signature verifies as documented above; with an empty secret no signature headers are sent. Non-2xx and redirects are retried for six attempts with 0.2/0.4/0.8/1.6/3.2 s gaps; redirects are never followed (`webhook-*`). Every accepted receipt, duplicates included, produces a callback.
- Rate limits: login burst 5 per client IP (6th attempt 429 `rate limit exceeded for <ip>`), send burst 20 per client id (21st 429 `rate limit exceeded for client:<id>`), other clients unaffected (`login-rate-limited`, `send-rate-limited`).

Legacy defects and exposures that V4 deliberately changes (each needs its own V4 test):

- A receipt that arrives before the carrier response is overwritten: terminal `1 sms_delivered` became `0 sms_provider_accepted` when the slow carrier answered (`send-carrier-response-after-early-dlr`). V4 keeps the terminal state.
- Logout revocations are process memory only; a revoked token was accepted again after restart (`restart-revoked-access-accepted`). V4 persists revocations.
- API_ADMIN tokens read every client's messages and receipts over the public API but cannot send (`get-admin-any`, `list-admin-all`, `dlrs-admin`, `send-admin-authority`). V4 fails closed for unsupported authorities.
- Raw carrier evidence is stored gzip-compressed with the carrier `token` and the `dlr_token` callback URL in clear (`send-roundtrip` `db_row`), provider configuration and callback secrets are plaintext columns, and carrier error text with internal URLs is returned to clients. V4 seals secrets, scrubs evidence and returns sanitized errors; the response envelope and status code stay.
- `/metrics` and the embedded admin UI are served unauthenticated on the public listener. V4 keeps both off the public listener.
- Unbounded `pageSize`. V4 caps page size; the cap is a documented security change.
- Concurrent duplicate receipts were all counted in this run (100/100), but the legacy read-then-increment is not atomic. V4 makes the increment a single atomic upsert.

## V4 persistence and configuration decisions (Phase 2)

- Every `sms_*` row carries `tenant_id`; references are composite `(tenant_id, id)` foreign keys, so a provider, template, API client or message of another tenant cannot be referenced. Row-level security (application role without BYPASSRLS) backs the explicit predicates. Hermes login and token subjects, and carrier receipts, resolve their tenant from the stored account or message (named cross-tenant lookups).
- Usernames stay globally unique (Hermes login carries no tenant) and keep the legacy `^[A-Za-z0-9_]{4,50}$` rule; numeric ids keep the legacy uint32 range and global uniqueness so imported ids and public `sub`/`providerId` values survive.
- Legacy zero references become SQL NULL: block `provider_id` 0 (all providers of the tenant) and message `template_id` 0.
- A message records exactly one actor: a Hermes API client (`api_client_id`) or a platform operator (`platform_actor`). Legacy admin-path sends (owner 0) need an explicit platform actor on import.
- Deleting a provider, template or API client that messages (or blocks) reference is refused (`ErrReference`, 409 in the management API) instead of the legacy dangling reference; retention deletes a message and its receipts together.
- `API_ADMIN` remains storable data; its public read-all privilege is not carried over (see above).
- Provider configuration and callback secrets are sealed with the deployment KEK, bound to tenant and row; raw carrier evidence passes through `sealed.Evidence` before storage or display.
- Message lists are bounded: default page 50 (legacy), maximum `query.max_page_size` (500 by default) instead of unbounded.
- Logout revocations persist in `sms_token_revocation` (jti and expiry only, never the token) and are purged after expiry.
- Legacy `SMS_GW_*` environment variables still configure the service; secrets become references, obsolete variables (legacy mTLS/registration) are reported and ignored, and an unparseable value now stops startup instead of silently using the default.
- Management requests are authorized only by the verified operator token and auth permission checks; legacy `x-md-global-*` identity headers are refused with 403.

## Public listener parity (US1, T014–T023)

`tests/contract/public_api_test.go` replays the capture scenario of `scripts/capture_legacy_runtime.py` in its order against the complete V4 application (PostgreSQL, sealed provider configuration, mock carriers with the capture's accept/reject/HTTP 500/drop/slow modes) with the same accounts, ids, templates and blocks, and compares each recorded response byte for byte (status, content type, body) after the capture's normalization. All 154 captured cases are accounted for: 133 replayed, 3 checked as observations (JWT claims, login log rows, no rows for rejected sends), 18 receipt-ingestion and callback cases deferred to US2 (their effects are applied through the repository so later reads still match). The test fails on any case that is neither matched, explicitly changed (table `v4Changes`) nor deferred.

Wire details the adapter reproduces: protojson member separator `", "` and declaration field order with every field emitted; uint64 values (`to`, receipt `id`/`to`/`timestamp`, `expires_in`) as strings; the unsigned status (`4294967295` while the carrier call is pending); no HTML escaping (`<no value>`); request decoding with protobuf-go's JSON decoder over runtime descriptors of the legacy messages, so proto and JSON field names, numbers as strings and unknown-field tolerance behave as before and decode errors keep the source text (`body unmarshal proto:\u00a0…`, the no-break space of the source build); the form codec and query binding (`parsing field "page": …`); bodies decoded before authentication and rate limiting; unmatched paths and methods answer `404 page not found` (text/plain); `GET /health` stays.

Deliberate differences in the replay (each asserted explicitly):

- `/` (embedded admin UI) and `/metrics` answer 404 on the public listener (`surface-root`, `surface-metrics`).
- `API_ADMIN` fails closed: get and receipt reads are `404 sms not found`, lists are empty (`get-admin-any`, `dlrs-admin`, `list-admin-all`, `list-admin-filter-username`); `/me` still returns the account with no abilities.
- Early receipts keep the terminal state: the late carrier answer does not overwrite it, so the send response and every list showing that message report `1 sms_delivered` (`send-carrier-response-after-early-dlr`, nine list cases); the status-0 filter no longer lists it (`list-filter-status-zero`).
- Carrier transport errors no longer name the carrier URL: `voicecom: endpoint error` instead of `voicecom: endpoint error: Post "http://…": EOF`, in the response and the stored status text (`send-carrier-connection-drop` and lists); any URL in a carrier error is redacted. HTTP-status and callback-configuration errors keep their text; the envelope (500, empty reason) is unchanged.
- Logout revocations persist: a token revoked before a restart stays revoked (`restart-revoked-access-accepted` is `401 invalid token`).
- Refresh issues the account's current authority instead of copying the presented claim (`refresh-stale-authority-claim` observation).
- Raw carrier evidence is stored scrubbed and uncompressed: the carrier `token` and the `dlr_token` of the callback URL are `[REDACTED]`; the request body is otherwise the legacy one byte for byte (checked against the recorded rows). Public reads never return evidence (as before: `rawRequest`/`rawResponse` are always empty).

Further V4 behaviour on the public listener, not captured by the recording:

- A token is accepted only while its account exists and is enabled: a disabled account's access token is `401 account disabled`, a removed account's `404 api client not found` (the source let both act until expiry). The tenant always comes from the stored account.
- The effective authority is the token claim only while it equals the account's authority; otherwise the caller is treated as unsupported (sees nothing, cannot send) until it logs in or refreshes.
- Unknown usernames take a dummy bcrypt comparison, so they cannot be distinguished from wrong passwords by timing.
- Message lists are capped at `query.max_page_size` (default 500; the recording's 13 messages are unaffected); `tests/integration/public_test.go` checks the cap.
- Request bodies are limited to `public.max_body_bytes` (default 64 KiB): `400 BAD_REQUEST request body too large`.
- Template rendering is bounded: at most 64 properties of 4 KiB each (`400 too many template properties` / `template property … is too long`), templates of 16 KiB, rendered text of 16 KiB and formatting widths of 1024 (`500 execute template: template output exceeds the limit`).
- The carrier exchange is detached from the client connection and bounded to 25 s: a client that disconnects mid-send cannot leave the message without its recorded carrier outcome. A lost or failed exchange is stored once (status 500) and never resent.
- Receipt routes (`GET /dlr`, `GET /hermes/v1/sms/dlr`) are reserved ahead of the `{id}` patterns and acknowledge with `"DLR_OK"`; until US2 wires the processor (T026/T027) the acknowledgement has no effect.

Configured defaults (`deploy/dev.yaml`, `config.Default`, legacy variables in brackets): login 10/min burst 5 per client address [`SMS_GW_LOGIN_RPM`, `SMS_GW_LOGIN_BURST`], send 100/min burst 20 per client [`SMS_GW_SEND_RPM`, `SMS_GW_SEND_BURST`], receipt 500/min burst 100 per address (applied by US2) [`SMS_GW_DLR_*`]; limiter key maps bounded at 50 000 (login) and 10 000 (send) keys with LRU eviction. Forwarded client addresses (`X-Real-IP`, then the leftmost `X-Forwarded-For`) count only from `public.trusted_proxies` [`TRUSTED_PROXY_CIDRS`], which accepts addresses or CIDRs; an invalid entry stops startup instead of being ignored. Recipient policy: 7–15 digits [`SMS_GW_MSISDN_MIN_DIGITS`], optional allowed/blocked prefixes [`SMS_GW_ALLOWED_PREFIXES`, `SMS_GW_BLOCKED_PREFIXES`]. Tokens: access 7200 s, refresh 604800 s, secret of at least 32 bytes.
