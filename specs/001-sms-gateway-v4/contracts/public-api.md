# Public Hermes compatibility contract

Applies to the dedicated public HTTP listener (legacy default :9901) and optional public HTTPS (:9443). Management endpoints and UI assets must not appear on these listeners. Source of exact schema/codec/errors: `../go-tangra-sms-gw/protos/sms_gw/service/v1/{authentication,sms,common,sms_gw_error}.proto`, `internal/server/http.go`, `internal/server/dlr_handler.go`, `internal/service/` and `internal/auth/`. Before implementation, capture HTTP fixtures under `tests/fixtures/legacy/`; prose below does not substitute for fixture coverage of every operation.

| Method | Path | Identity / behavior |
|---|---|---|
| POST | /hermes/v1/login | Existing credentials/grant fields; issue Hermes access_token, refresh_token, token_type, expires_in and scope |
| POST | /hermes/v1/refresh_token | Validate refresh token kind/expiry/revocation; preserve form and response |
| POST | /hermes/v1/logout | Unauthenticated/idempotent; independently verify Bearer and X-Refresh-Token before revocation |
| GET | /hermes/v1/me | Verified client; sanitized user and abilities |
| POST | /hermes/v1/sms; /v1/sms | API_CLIENT only; template/provider lookup in client's tenant; return data and endpoint_response |
| GET | /hermes/v1/sms; /v1/sms | Client-owned list; legacy PagingRequest fields and defaults |
| GET | /hermes/v1/sms/{id}; /v1/sms/{id} | Client-owned message; do not leak foreign owner or tenant existence |
| GET | /hermes/v1/sms/dlr/{id}; /v1/sms/dlr/{id} | Client-owned receipt list, items and total |
| GET | /hermes/v1/sms/dlr; /dlr | Carrier query authentication via dlr_token; always source-compatible 200 DLR_OK |
| GET | /.well-known/acme-challenge/{token} | Public ACME http-01 responder when enabled; exact prefix ahead of general routing |

Literal receipt routes take precedence over `{id}` patterns. Capture any additional aliases actually registered in source HTTP code; do not extrapolate authentication aliases from SMS aliases. Metrics are on a network-restricted scrape surface, not an unauthenticated public management API.

## Required shape preservation

Send fields: numeric legacy `to`, `sms` (from, encoding, concatenate, validity with ttl/units, mccmnc), properties map, providerId and templateId. A template renders the body; no invented free-text field or scheduled-send implementation. Response retains `data` and `endpoint_response`, carrier return_code/return_message, channels.sms.send_order/message_parts and declared reserved Viber fields. Keep source JSON default omissions, 64-bit encoding and integer status conversion through golden fixtures.

Message retains UUID id, numeric owner/SID/to/priority, defer, sms, text, status/status_message, protected raw evidence, user_name, providerId/providerName, apiClientUsername, createTime/updateTime. Receipts retain request_id, channel, SID, message_status, to/from/timestamp, legacy `remote_addrerss` typo, status_text, partsReceived and timestamps. Redact credentials in raw evidence without changing field names and document resulting content differences.

Pagination retains page, pageSize, query, or, orderBy and nopaging. Allowlist filter/order fields and bound work even when callers request no paging; document any cap as a security change with a fixture. Unknown/malformed filters must not become SQL text. Source error HTTP statuses, reason strings and envelopes govern protected public routes; management error format is separate.

JWT uses source HS256 claim conventions including numeric-client sub, username, authority, access/refresh distinction and TTL defaults (7200/604800 seconds). Secret is at least 32 bytes. Password hashes are preserved. Unsupported authority fails closed; operator tokens cannot authorize public requests and Hermes tokens cannot authorize management. No tenant parameter is added: derive tenant from the persisted account.

## Receipt and callback semantics

Inbound fields are request_id, channel, sid, message_status, to, from, timestamp and dlr_token. Resolve stored message/provider, check token in constant time, enforce tenant-consistent relationships, then atomically aggregate receipts and apply nonterminal transitions. Invalid receipts acknowledge without changing data; report sanitized metrics/audit rather than returning diagnostics to the carrier.

Outbound callback is JSON POST with the source dispatcher payload. When secret is nonempty, `X-SmsGw-Timestamp` is Unix seconds and `X-SmsGw-Signature` is lower-case hex HMAC-SHA256 over `timestamp + "." + exact body bytes`. Preserve source optional unsigned behavior for empty secret unless deliberately changed and documented. The source worker has queue capacity 4096, four workers, six attempts, ten-second request timeout and 200/400/800/1600/3200 ms backoff; only 2xx succeeds, redirects are not followed, and response drain is bounded to 64 KiB. Validate DNS/address at connection time against private destinations. Local HTTP/private test receiver access requires explicit development-only configuration. No durable queue delivery guarantee is promised.

## Source codec evidence (implementation capture)

The actual JSON encoding is captured in `tests/fixtures/legacy/`. Protobuf fields use camel-case JSON defaults (`endpointResponse`, `returnCode`, `statusMessage`, `rawRequest`, `requestId`, `messageStatus`, `remoteAddrerss`) unless explicitly overridden. Source identifiers above describe semantics, not literal JSON spellings. Proto uint64 values encode as strings; outbound callback standard JSON numbers remain numeric. `Result(200, "DLR_OK")` produces a JSON string body `"DLR_OK"` with application/json. Preserve the observed source contract rather than inferring plain text from README prose. Database-backed application capture is still pending.
