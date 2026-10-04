# Legacy contract evidence

Captured from the local source checkout `../go-tangra-sms-gw`, which is never modified. `provenance.json` records the source commit, SHA-256 fingerprints of the behaviour-defining files and the runtime capture metadata. `source/` preserves the proto and behaviour definitions; `routes.json` lists all generated public routes and the raw receipt overrides.

## Codec goldens (top level)

`scripts/capture_legacy.py` (`make capture-legacy`) encodes representative values with the source protobuf bindings and Kratos v2 codec in a temporary module, and runs the original JWT/password tests with the race detector. These files (`login.json`, `send.json`, `error-*.json`, `dlr-ack.json`, `callback.json`, ...) pin the transport encoding: camel-case JSON names, quoted uint64, the `"DLR_OK"` JSON string acknowledgement and the HMAC callback recipe.

## Runtime recordings (`runtime/`)

`scripts/capture_legacy_runtime.py` (`make capture-legacy-runtime`, needs Docker) builds the legacy server and its `cmd/test-linkmobility` mock from source, starts an isolated `postgres:16` container on a TEST-NET-2 docker network, lets the legacy ent auto-migration create the schema, seeds sanitized records over SQL and drives the running service over HTTP. Carrier failure modes (reject code, HTTP 500, dropped connection, slow response) come from an in-process fake; outbound callbacks are received in-process on the bridge address `198.51.100.1`, which the legacy anti-SSRF dialer accepts. A second gateway instance with default limits records rate limiting and restart behaviour. Nothing contacts a real carrier and every credential is a capture-only value.

Each `runtime/<case>.json` holds the request, the exact response status/content type/body and, where relevant, `observed` side effects (database rows with gunzipped raw evidence, receipt rows, carrier submissions, callback deliveries). `legacy-schema.sql` is the schema the legacy service created; `manifest.json` lists every case.

Normalization replaces only nondeterministic values after their semantics were checked: generated UUIDs (`{{msg:...}}`), JWTs (`{{access_token:...}}`; claims are in `jwt-claims.json`), RFC 3339 times, HTTP dates, the carrier DLR clock, local ports and the capture-only secrets (`{{jwt_secret}}`, `{{dlr_token:...}}`, `{{carrier_token}}`, `{{callback_secret}}`). Numeric ids are deterministic because every run starts from an empty database; two consecutive runs produced identical files. Two observations are timing dependent and labelled as such: `dlr-100-concurrent-same-status` and the delivery total in `webhook-signed`.

Never point either harness at production or use real carrier credentials. `docs/compatibility.md` lists the behaviours these recordings established and which of them V4 deliberately changes.
