# Migration from the legacy (v3) sms-gw

`smsgw-migrate` imports a legacy database snapshot into one V4 tenant. It reads the source in one read-only, repeatable-read transaction, never writes it, and never connects to anything but the snapshot named in its configuration. The rehearsal record is in [validation.md](validation.md); operations after cutover are in [operations.md](operations.md).

## What is imported

All seven legacy entity types, with their ids, password hashes, references, statuses and timestamps (source precision):

| Legacy table | V4 | Transformations (counted in the report) |
|---|---|---|
| `sms_api_client` | same ids and usernames (globally unique), bcrypt hashes copied unchanged, `API_ADMIN` kept as data | callback secret sealed with the KEK, bound to tenant and client (`callback_secrets_sealed`); `API_ADMIN` has no public read-all privilege in V4 (reported) |
| `sms_provider` | same ids, names, type, channel, status, `retention_days` | configuration resealed (`provider_configs_sealed`), public part stored with credentials masked; Viber channels and unregistered types are kept as data and reported |
| `sms_template` | same ids, fragments, status | — |
| `sms_block` | same ids | `provider_id` 0 → NULL, every provider of the tenant (`zero_provider_blocks_to_null`); `create_by` → `created_by` `legacy:<id>` |
| `sms_message` | same UUIDs, owner, carrier SID, recipient, statuses, texts, data, times | owner 0/NULL (legacy admin sends) → the configured platform actor (`admin_sends_to_platform_actor`); `template_id` 0 → NULL (`zero_template_messages_to_null`); gzip evidence decompressed (`evidence_decompressed`) and scrubbed of carrier tokens and receipt tokens (`evidence_scrubbed`) |
| `sms_dlr` | same ids, aggregation `(message, status)`, `parts_received` | — |
| `sms_login_log` | same ids | client 0/NULL → unresolved, no tenant (`unresolved_logins`); records of clients that no longer exist → unresolved (`logins_of_missing_clients_unresolved`) |

Also reported: rows with a legacy `delete_time` (never enforced by the legacy service; imported as live rows, `soft_deleted_rows_imported`), missing timestamps (filled from the other timestamp or the import time, `timestamps_defaulted`), and source columns the import does not know (`unsupported`). Legacy logout revocations lived in process memory and do not exist in the database. Nothing is dropped silently: a record that cannot be stored (invalid username, non-UUID message id, non-string provider configuration value, out-of-range number, …) is an error that fails the run; a record whose reference is missing in the snapshot (receipt without message, message whose provider, template or owner is gone, block of a deleted provider) is an orphan that fails the run unless `exclude_orphans` is set, in which case it is left out and listed under `excluded`.

## Configuration

`deploy/legacy-import.dev.yaml` (no credentials):

```yaml
source:
  dsn: { env: SMSGW_LEGACY_DSN }   # or { file: … } (mode 0640 or stricter); never a flag
tenants:
  fixture-tenant: "00000000-0000-0000-0000-000000000001"
platform_actor: legacy-admin        # recorded for legacy admin sends
exclude_orphans: false
```

The destination is the service configuration (`-config`): `db.migrate_dsn` (or `db.dsn`) and the KEK. Bootstrap the destination first (`smsgwsvc bootstrap`). The KEK must be the one the service will run with: sealed values are bound to it.

## Running

```bash
export SMSGW_LEGACY_DSN='postgres://…/sms_gw_snapshot?sslmode=…'   # read-only snapshot copy
smsgw-migrate -source-config deploy/legacy-import.dev.yaml -config deploy/dev.yaml -tenant fixture-tenant -dry-run
smsgw-migrate -source-config deploy/legacy-import.dev.yaml -config deploy/dev.yaml -tenant fixture-tenant -apply
```

`-platform-actor` and `-exclude-orphans` override the file. The JSON report (stdout) has `source` counts, the `plan` per entity (`insert`/`update`/`unchanged`), `destination` counts, `transformations`, `unsupported`, `excluded`, `errors`, `conflicts`, `reconciled` and the `source_fingerprint` (SHA-256 of the snapshot content). It never contains secrets, password hashes or message texts. Exit status 0 = succeeded or already applied, 1 = refused or failed. Every run is recorded in `sms_import_run` (mode, status, fingerprint, counts, report, checkpoint).

- Dry-run validates and compares only; nothing is imported.
- Apply writes all inserts and updates in one transaction, advances the id sequences past the imported maxima, reads everything back and commits only if the destination equals the transformed snapshot record for record (secrets compared after opening them with the KEK). A failure rolls the whole apply back; `checkpoint.phase` names the entity being written when it failed. Rerun after fixing the cause.
- Rerunning apply with the same snapshot changes nothing (`already_applied`, every record `unchanged`). A later snapshot of the same source applies only its changes (status updates, new receipts and messages).
- Conflicts refuse the run before anything is written: ids or usernames held by another tenant, names already used by other records in the tenant, receipts colliding on `(message, status)`, sealed values that do not open with the configured KEK, and destination records of the tenant that are not in the snapshot (new traffic, manual edits, or rows the legacy retention deleted after an earlier import). Import into an empty tenant.

## Token continuity

Passwords always survive (hashes are copied). Hermes tokens survive only when `public_auth.jwt_secret` is the legacy `SMS_GW_JWT_SECRET` (the claim layout is unchanged; V4 additionally checks that the account exists, is enabled and holds the claimed authority). Tokens the legacy service had revoked by logout become valid again until they expire (at most the refresh lifetime, 7 days by default), exactly as after any legacy restart. Choose a new secret instead to force every client to log in again (no client-side change other than re-login).

## Cutover

1. Rehearse on a throwaway copy: restore a snapshot, bootstrap a scratch destination, dry-run, apply, apply again, verify with Hermes logins and receipt reads; repeat until the report is clean.
2. Prepare: back up the legacy database; bootstrap the production destination (empty tenant) with its KEK backed up; configure providers' receipt routing (the `callback_url` host in provider configurations is carried over unchanged — it must reach the V4 public edge after the switch).
3. Pause sends at the edge (Hermes `POST /hermes/v1/sms` and login), keep receipts flowing to the legacy service, and wait until receipts and callbacks quiet down (in-flight callbacks are lost on stop in both versions).
4. Stop the legacy service and take the final snapshot (`pg_dump`), restore it read-only for the import.
5. Dry-run, then apply into the empty tenant. The report must be `succeeded` and `reconciled` with the expected counts.
6. Switch routing of the Hermes public paths and the carrier receipt paths (`/dlr`, `/hermes/v1/sms/dlr`) to the V4 public listener; start V4 traffic. Keep the gap between steps 4 and 6 short: receipts that cannot be delivered in it are lost unless the carrier retries.
7. Verify: a client login, a send through each provider, a receipt, a callback, and the post-cutover query below returning only the new traffic.

## Rollback

Before V4 accepted traffic: route back to the legacy service; its database was never written.

After V4 accepted traffic, changing routing alone is insufficient: the legacy service does not know messages sent through V4, and carriers report those receipts to the receipt URL of the V4 provider configuration.

1. Pause sends on V4 (edge), keep the V4 receipt route reachable.
2. List what exists only in V4 (record the result):

   ```sql
   WITH cut AS (SELECT max(finished_at) t FROM sms_import_run WHERE mode = 'apply' AND status = 'succeeded')
   SELECT 'message', m.id::text, m.status_code, m.create_time FROM sms_message m, cut WHERE m.create_time > cut.t
   UNION ALL SELECT 'receipt', d.id::text, d.message_status, d.create_time FROM sms_dlr d, cut WHERE d.create_time > cut.t
   UNION ALL SELECT 'client change', c.id::text, NULL, c.update_time FROM sms_api_client c, cut WHERE c.update_time > cut.t
   ORDER BY 4;
   ```

3. Re-apply client, provider, template and block changes made in V4 to the legacy service by hand (management edits after cutover); the legacy service cannot import V4 data.
4. Route Hermes login/send/list back to the legacy service. Clients asking the legacy service for messages sent through V4 get `404 sms not found`; tell them, or keep V4 read access for those ids.
5. Keep the V4 receipt route (and V4 itself, sends paused) running until the V4-sent messages are terminal, so their receipts and callbacks are not lost; then stop V4.
6. A later cutover starts again from a fresh final snapshot into an empty destination tenant (re-import over V4 traffic is refused by design).
