# Data Model: SMS Gateway V4

Source schema: `../go-tangra-sms-gw/internal/data/ent/schema/*.go`. The names below preserve existing `sms_*` tables and IDs; physical SQL definitions are implementation tasks. All times are imported at source precision. Avoid silently normalizing wire values.

| Entity / table | Fields and validation | Relationships / tenant rules |
|---|---|---|
| API client / sms_api_client | Existing numeric id; tenant_id; globally unique immutable username, 4–50 alphanumeric/underscore characters; bcrypt password_hash; email; authority; ON/OFF; login metadata; callback URL and sealed secret; timestamps | Exactly one immutable tenant; source API_ADMIN is retained as data but denied unsupported privileges; client owner references resolve in same tenant |
| Provider / sms_provider | Existing numeric id; tenant_id; nonempty name up to 128 chars; type up to 32; channel; sealed config; ON/OFF; retention_days (0 = forever); timestamps | Name unique within tenant; referenced by messages and optional blocks; only registered supported provider/channel combinations may send |
| Template / sms_template | Existing numeric id; tenant_id; name up to 128; object_type; fragment map, SMS body; ON/OFF; timestamps | Name unique within tenant; referenced template must belong to message tenant; 0 legacy template_id means no template, represented explicitly in SQL/adapter |
| Block / sms_block | Existing numeric id; tenant_id; recipient up to 64; description up to 255; provider_id (0 = all tenant providers); channel; ON/OFF; creator; timestamps | Provider-scoped reference cannot point into a foreign tenant; enforce before rendering/persistence |
| Message / sms_message | UUID id; tenant_id; legacy numeric API owner OR distinct platform actor; sid; recipient; priority; provider_id; optional template_id; defer reserved; username; raw request/response evidence; original payload; dlr_ts; signed status_code; rendered body; status text; address; timestamps | Exactly one actor kind; imported public owners retain numeric IDs; all provider/template/client references tenant-consistent; raw evidence excludes credentials; no new deferred-send behavior |
| Receipt / sms_dlr | Existing receipt id; tenant_id; message_id; channel; sid; status_text; message_status; numeric legacy recipient; sender; carrier timestamp; address; parts_received >= 1; timestamps | Tenant-consistent message relationship; unique (message_id, message_status); atomic upsert increments parts count; preserve existing IDs on import |
| Login log / sms_login_log | Existing numeric id; nullable resolved tenant/client; supplied username; event_time; success; sanitized error; IP; user agent | Unknown-user failure may be unresolved; management never exposes another tenant's resolved log; no password/token data |
| Audit event / sms_audit | Event id; tenant; actor kind/id; action; target; outcome; request id; time | Append-only, closed action vocabulary; no bodies, bearer tokens or decrypted secrets |
| Token revocation / sms_token_revocation | Client/tenant; token fingerprint or existing issuer-compatible key; expiry | Persist logout semantics without storing bearer token; expire bounded records |
| Import run / sms_import_run | Source fingerprint; destination tenant; mode; checkpoint; counts; result; time | Idempotent source/tenant import tracking; no legacy password/plaintext secret in reports |

## State transitions

Message starts at -1 (`sms_gw_accepted`); carrier acceptance is 0; intermediate SMSC-delivered is 8; terminal values are 1 delivered, 2 delivery failed, 16 rejected and source system/validation codes 1000–4004. Use the source status mapping as the exhaustive authority. Receipt mutation locks its parent and updates only nonterminal states. Once terminal, subsequent valid callbacks remain audit receipts but do not replace the terminal state. Unknown codes are retained as evidence and must not invent success. Carrier timeouts record the source-compatible failure/uncertainty without a resend.

Client/provider/template/block ON/OFF is preserved. A disabled block remains stored but is not enforced. Password reset/logout/disable checks follow captured source behavior and FR-002; any tightening is documented. Changes to authority do not create a platform operator identity.

## Persistence constraints

All reads, writes, list filters and joins require trusted tenant scope. Composite keys/FKs or equivalent transaction checks ensure tenant consistency; globally unique UUID/numeric IDs alone do not provide authorization. Numeric sequences advance past imported maxima. Foreign references and orphan receipts are rejected in dry-run. Deleting providers/templates/clients must explicitly preserve historical message readability or reject in-use deletions; acceptance fixtures define source-compatible behavior. Retention deletes messages and their associated receipts in the same transaction, per tenant/provider; audit/login retention is not inferred from provider retention.

The send pipeline validates authorization, tenant references, provider/channel status, blocks and template inputs before submitting. Exactly one gateway record exists per accepted invocation; concurrent duplicate client requests remain separate sends because no legacy idempotency key is promised. Handle early DLR arrival against the pre-submission message UUID without losing a terminal state when the provider response is later saved.

## Migration transformations

Preserve source IDs, ownership, provider/template/block references, request data, delivery fields and timestamps. Add the chosen tenant to all resolved records and retain unresolved login failures safely. Re-encrypt plaintext provider configuration/callback secrets with the destination key. Password hashes are copied without rehashing. Keep raw evidence only after secret scrubbing; record deliberate redaction in reconciliation output. Preserve compatible issuer secrets when token continuity is requested; otherwise force documented re-login. Source snapshot and destination are separate databases.
