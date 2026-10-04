# V4 management API contract

Module `sms-gw`; exclusive registered prefix `/api/sms-gw`; versioned routes `/api/sms-gw/v1`. Authoritative implementation artifact is `api/openapi/sms-gw.yaml`; generate manifest routes and UI types from it. Browser traffic reaches the module through the portal over mesh mTLS. All operations also verify the forwarded bearer using auth SDK and require UserID + TenantID; ignore caller-supplied tenant/role headers.

| Resource routes below /api/sms-gw/v1 | Methods / operations | Permission |
|---|---|---|
| /providers | GET list; POST create | providers:read; providers:manage |
| /providers/{id} | GET; PATCH; DELETE | providers:read; providers:manage; providers:manage |
| /provider-types | GET type/field metadata | providers:read |
| /templates | GET; POST | templates:read; templates:manage |
| /templates/{id} | GET; PATCH; DELETE | templates:read; templates:manage; templates:manage |
| /templates/{id}/preview | POST render/variables/encoding counts | templates:read |
| /api-clients | GET; POST | clients:read; clients:manage |
| /api-clients/{id} | GET; PATCH; DELETE | clients:read; clients:manage; clients:manage |
| /api-clients/{id}/reset-password | POST reset; one-time result | clients:manage |
| /blocks | GET; POST | blocks:read; blocks:manage |
| /blocks/{id} | GET; PATCH; DELETE | blocks:read; blocks:manage; blocks:manage |
| /messages | GET searchable list; POST manual send | messages:read; messages:send |
| /messages/{id} | GET details | messages:read |
| /messages/{id}/dlrs | GET receipt history | messages:read |
| /dashboard/instant; /dashboard/range | POST constrained monitoring query (deployment-wide totals across tenants) | dashboard:read |

Requests cannot set tenant or actor. Manual send uses the same domain validation/carrier pipeline as public send but records the verified platform actor separately. List queries include bounded page/pageSize, documented sorting and resource filters; messages support recipient prefix, SID and API client username. Mutation schemas reject unknown security-sensitive fields, validate registered provider/channel type, immutable username and cross-tenant references. Preview computes the same rendering/encoding/parts rules used on send and never submits to a carrier.

Provider config and callback secrets are write-only: return a set/redacted indicator, not plaintext; unchanged indicator retains current secret and explicit clear follows schema. API-client password is never returned; reset can return newly generated password once with no-store and no logging. Protected raw message evidence is sanitized before disclosure. Historical deleted-reference behavior must match the captured source where safe, with a documented policy for new tenant constraints.

Responses: list `{items,total}`, detail DTOs as declared by OpenAPI, creates 201, updates 200, deletes 204, reset 200. Validation 400, authentication 401, permission 403, absent/foreign-tenant object 404, uniqueness/in-use conflict 409, rate limit 429, unavailable dependency 503. Use V4 opaque error handling/correlation IDs; never include secrets or an upstream raw body. These are new management conventions and are not applied to the Hermes adapter.

Dashboard only permits the existing module metric queries/windows (15m through 7d), bounds range/step/timeouts and never proxies arbitrary user-supplied PromQL. Missing monitoring returns an explicit unavailable result without affecting SMS endpoints.

Module roles: administrator receives all listed permissions; sender receives providers:read, templates:read, messages:send and messages:read; viewer receives providers:read, templates:read and messages:read; monitoring receives only dashboard:read. The dashboard aggregates metrics that carry no tenant label, so it exposes deployment-wide totals across tenants (no records or identifiers); it is therefore a role of its own and never part of ordinary viewing. Built-in owner/admin grants receive all permissions; no broad member grant by default. These roles are registered with auth, replacing legacy platform:admin/sms:admin string checks. Every operation checks its permission server-side and scopes all data to identity tenant.
