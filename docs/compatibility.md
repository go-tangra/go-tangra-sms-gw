# Compatibility decisions and capture status

The Hermes listener preserves legacy public routes and payload conventions; V4 management is a separate contract. Source snapshots and checksums are in tests/fixtures/legacy/. Runtime parity is not yet demonstrated.

Source inspection corrected one planning error: logout is deliberately whitelisted, succeeds without a token and independently validates/revokes Authorization and X-Refresh-Token when supplied. It must remain accessible after access-token expiry. The request id does not grant revocation authority. The source rate-limit key for send is client ID, with IP fallback; it is not a concatenated client/IP key. Source logout uses an in-memory denylist; destination persistent revocation is an intentional improvement.

Security changes planned: tenant-scoped data, fail-closed unsupported authority, credential sealing/redaction, bounded queries and protected V4 management. Exact raw-evidence redaction and pagination limits must be documented with runtime fixtures before release.

No public runtime implementation or migration has been performed yet. No legacy database, carrier or production certificates have been touched.

## Captured transport behavior

The actual Kratos v2 protobuf JSON codec emits camel-case field names unless a proto has an explicit json_name override: `endpointResponse`, `returnCode`, `statusMessage`, `rawRequest`, `rawResponse`, `requestId`, `messageStatus`, `remoteAddrerss`. Proto source snake-case identifiers do not mean snake-case JSON. Proto uint64 fields encode as quoted decimal strings, including recipient `to`; the callback dispatcher uses standard JSON and emits numeric `to`. The source unsigned Message.status encodes initial -1 as 4294967295.

The receipt wrapper uses `Result(200, "DLR_OK")`: with its default JSON codec the body is the JSON string `"DLR_OK"` and Content-Type application/json, not an unquoted plain-text body. Keep this source-observed distinction in the adapter. `tests/fixtures/legacy/dlr-ack.json` records the codec output.

Representative values in fixtures are synthetic, sanitized samples. Error reasons/statuses and field encoding are captured from the actual source codec; full application behavior remains unverified until isolated legacy runtime capture can run.
