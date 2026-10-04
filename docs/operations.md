# Operations

## Carrier receipts

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
