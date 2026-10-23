# sms-gw V3 → V4 canary balancer

An HAProxy 2.8 front for host `sms-gw` that runs sms-gw v3 and v4 side by
side and decides, per request, which version answers:

- **API clients stick to one version.** The sticky key is the client's
  username, read from the JWT (`Authorization: Bearer`), from the login body,
  or from the refresh token in the refresh body. `pins.map` pins a username
  to `v3` or `v4`; otherwise `canary.map`'s `percent N` sends usernames whose
  `crc32 % 100 < N` to v4. The rollout uses pins: homes → cars → jobsbg.
- **Messages stay with the version that sent them.** v3 mints UUIDv4 message
  ids, v4 mints UUIDv7. Message reads (`/hermes/v1/sms/{id}`,
  `/hermes/v1/sms/dlr/{id}`, and the same under `/v1`) and carrier DLRs
  (`/dlr`, `/hermes/v1/sms/dlr`, keyed by `request_id`/`requestId`) go to the
  version named by the UUID. Carriers keep calling the same callback URL as
  before.
- **Everything else goes to v3**, as it does today: anonymous requests, `/`,
  `/metrics`, `/health`, assets and unknown paths.

The design is the user's draft config. It was reviewed, fixed and tested
here. Every change is marked `CHANGED:` in `conf.d/10-canary.cfg` and listed
under [Review of the draft](#review-of-the-draft).

> **Hard prerequisite:** v4 must mint **UUIDv7** message ids (a separate
> change to v4) before any client is moved to v4. If it doesn't, DLRs for
> messages v4 sent are routed to v3, which drops them, because v3 doesn't have
> those messages.
>
> Multipart messages are fine. v3 deduplicates multipart DLRs on
> `(message_id, message_status)` and counts the parts, which means carriers
> echo the parent `request_id` for every part. That parent id is the UUID the
> LB routes on.

## Files

| File | Purpose |
|---|---|
| `conf.d/10-canary.cfg` | Routing rules (frontend `sms_public`), backends, stats. Based on the user's draft. |
| `conf.d/20-tls.cfg` | Public TLS edge on :443. Terminates TLS and forwards to `sms_public` over the container loopback with PROXY v2. |
| `maps/pins.map`, `maps/canary.map` | Routing state. The files are the only state. |
| `entrypoint.sh` | Container entrypoint. Builds the combined PEM from `fullchain.pem` and `privkey.pem`, checks the config, then runs `haproxy -W -db`. `entrypoint.sh prepare` rebuilds and validates the PEM before every reload, so a renewed certificate needs no restart. |
| `compose.yaml` | Standalone compose project for the LB (`/srv/sms-gw-lb`). |
| `sms-canary.sh` | Operator script: `status`, `set`, `pin`, `unpin`, `reload`, and the `parity` hook. |
| `test.sh`, `fakebackend/` | Local test. Runs real HAProxy 2.8 against two fake backends. |

## Architecture

```
 clients / carriers ──TLS──▶ host :443 (only port the LB publishes)
                               │  docker DNAT keeps the client source IP
                     ┌─────────▼──────────────── container sms-gw-lb ───────────┐
                     │ frontend sms_tls  (mode tcp, ssl, alpn h2,http/1.1)      │
                     │        │ send-proxy-v2 → 127.0.0.1:9902 (container lo)   │
                     │ frontend sms_public (mode http, routing rules)           │
                     │   ├─ sms_v3 ─▶ compose-sms-gw-service-1:9901 (compose_app-tier, LB .223.250)
                     │   └─ sms_v4 ─▶ tangra-sms-gw-sms-gw-1:9901 (tangra-sms-gw_default, LB .255.250)
                     │ admin socket + server-state → /srv/sms-gw-lb/run (host)  │
                     └──────────────────────────────────────────────────────────┘
 untouched: host :9900/:9901 → v3 container (the v3 portal 10.1.111.150 uses them)
            host 127.0.0.1:9902 → v4 container (v4's own publish)
            v4 mesh 89.25.32.111:9983/9984 via the v4 gateway (v4 portal)
```

### Routing order (first match wins)

| # | Request | Route (`route=` in the log) | V4 down |
|---|---|---|---|
| 1 | `GET /dlr`, `/hermes/v1/sms/dlr` with a UUIDv7 `request_id`/`requestId` | `dlr-v4` | 503 (no fallback: v3 doesn't have the message) |
| 1 | the same DLR paths, anything else | `dlr-v3` | — |
| 1 | `/{hermes/,}v1/sms/{,dlr/}<UUIDv7>` (full anchored UUID) | `id-v4` | 503 |
| 1 | the same with any other full UUID | `id-v3` | — |
| 2 | username pinned `v4` | `pin-v4` | `fallback-v3` |
| 2 | username pinned `v3` | `pin-v3` | — |
| 2 | unpinned username, `crc32 % 100 < percent` | `bucket-v4` | `fallback-v3` |
| 3 | everything else | `default-v3` | — |

Where the username comes from:

| Path | Source |
|---|---|
| `POST /hermes/v1/login` | Body `username`. Form if the Content-Type subtype is `x-www-form-urlencoded`, JSON otherwise (the same codec choice both services make). The Authorization header is ignored here. |
| `POST /hermes/v1/refresh_token` | Body `refresh_token`/`refreshToken` (JSON or form) is a JWT. The username comes from its payload. |
| `POST /hermes/v1/logout` | Bearer, else the `X-Refresh-Token` JWT, else none (→ v3). |
| anything else | `Authorization: Bearer` JWT payload `username`. |

The JWT is **not verified** at the LB. Both backends verify it, so a forged
token can only pick a backend, where it then fails authentication. The key is
`username`, not `sub`, because the pins are written by humans by name and
login only carries the name. Only usernames matching
`^[A-Za-z0-9._@+-]{1,128}$` are used as keys. This is the same rule
`sms-canary.sh` applies when pinning. Any other username (for example
non-ASCII, quotes or control characters) takes the v3 default and is never
logged raw.

### Behaviour worth knowing

- **No double sends.** The backend is chosen before the request is sent, and
  each backend has exactly one server. A send is never retried on the other
  version. HAProxy retries only failed *connections* (`retries 2`), where
  nothing was sent.
- **V4 down.** After the health checks mark v4 DOWN (`inter 5s fall 3`, about
  15 s), clients routed to v4 by pin or percent are served by v3
  (`fallback-v3`). Before that point, a request to a dead v4 gets
  `503 {"code":503,"reason":"SERVICE_UNAVAILABLE",...}`. If v4 dies after it
  accepted a send, the client gets a 502 or 504 and may retry, like with any
  outage today. Fallback is acceptable **only because both versions use the
  same JWT secret**: a v4-issued token works on v3 and the other way round.
  Token revocations (logout) are per instance and are not shared.
- **Health survives reloads.** `sms-canary.sh` dumps `show servers state` to
  `run/servers.state` before every SIGUSR2. Without that, a new worker would
  start with v4 "UP" while v4 is down, and v4 clients would get 503 instead of
  the fallback for about 15 s. On a cold start the file may be stale. A server
  it records as DOWN stays DOWN until 2 checks pass (about 10 s).
- **Message-addressed requests never fall back.** A v7 id or DLR always goes
  to v4.
- **Bodies.** Only login and refresh wait for the body (`wait-for-body`, up to
  10 s), and `tune.bufsize 81920` lets a body at the services' 64 KiB cap fit
  in one buffer. A larger body is forwarded whole, and the backend applies its
  own cap. If the username sits beyond the first ~79 KiB, the request takes
  the v3 default. Sends are streamed and not buffered.
- **Client IP.** `X-Forwarded-For` and `X-Real-IP` sent by clients are
  dropped. The LB sets `X-Real-IP: <client>` and `X-Forwarded-For: <client>`.
  The backends must trust the LB's source address: its fixed /32 on each
  network (`192.168.223.250` on `compose_app-tier`, `172.21.255.250` on
  `tangra-sms-gw_default`). See runbook steps 0, 1 and 3.
- **Responses are passed through.** The LB adds no response headers (the
  debug header is off in prod). HTTP/1.1 header names keep Go's casing through
  `h1-case-adjust`, which covers the standard headers both services send. A
  header outside that list would arrive lower-cased, which HTTP allows. Errors
  the LB generates itself (400/403/408/500/502/503/504) use the services' JSON
  error shape, never HAProxy HTML. `/lb-health` is answered by the LB
  (`monitor-uri`).
- **TLS** matches v3's Go server: TLS 1.2 minimum, TLS 1.3, Go's TLS 1.2
  cipher list (ECDHE AEAD and ECDHE-CBC-SHA, no RSA key exchange), and ALPN
  `h2,http/1.1`. Go's `net/http` offers h2 when `NextProtos` is unset, which is
  what v3 (kratos) does, so clients that negotiate h2 today still get h2.
- **Logs.** One line per request: client, frontend, backend/server, timings,
  status, bytes, `"METHOD /path HTTP/x"` (no query string), `route=` and
  `user=`. No tokens, bodies or query strings, so no phone numbers or DLR
  parameters. Health transitions are logged. TLS handshake failures are logged
  by `sms_tls`. Read them with `docker logs sms-gw-lb`.
- **Stats** are bound to the container loopback (`127.0.0.1:8404`) and never
  published. `sms-canary.sh status` reads `show stat` from the admin socket.

### Two admin UIs during the canary

The v3 portal (admin-service 10.1.111.150 → v3 gRPC :9900 and UI assets
:9901) and the v4 portal (gateway → mesh 89.25.32.111:9983/9984,
`/api/sms-gw`, UI remote `/m/sms-gw`) are **not** behind the LB, and both
keep working. However:

- **Each UI shows only its own instance's data.** Messages, DLRs and login
  logs of clients routed to the other version don't appear in it.
- **Config edits don't propagate.** Providers, templates, API clients and
  blocks edited in one UI don't reach the other. During the canary, either
  freeze these edits or make each one in **both** UIs with identical values,
  keeping ids aligned (v4 was imported with the same ids, so routing,
  templates and provider ids must keep matching).
- **Provider `callback_url` stays the same** in both:
  `https://sms-gw.verax.net/hermes/v1/sms/dlr`. The LB routes DLRs by
  `request_id`, so there is no `/v4/...` callback. Never change it in the v4
  UI.

No `seed` step is needed: prod v4 already holds the same clients, providers,
templates and blocks, imported with `smsgw-migrate`. A v3 → v4
`pg_dump --data-only` COPY wouldn't fit v4's schema anyway (tenant ids,
constraints, different tables).

## sms-canary.sh

Run it on the host as root, from anywhere. It defaults to
`SMS_CANARY_DIR=/srv/sms-gw-lb` and the container `sms-gw-lb`.

```
sms-canary.sh status                 # percent, pins, backend health (admin socket)
sms-canary.sh pin homes v4           # pin, then validate + hitless reload
sms-canary.sh unpin homes            # back to the percent rule
sms-canary.sh set 10                 # percent of unpinned clients on v4 (warns when lowering)
sms-canary.sh reload                 # rebuild+validate PEM and config in the container, dump health, SIGUSR2
sms-canary.sh parity ...             # HOOK: runs $SMS_CANARY_PARITY_CMD (differential harness, wired separately)
```

Each change rewrites the map atomically (mktemp next to the file, then `mv`).
It then runs `entrypoint.sh prepare` inside the container. That rebuilds the
PEM from the current certificate files into a candidate, then runs
`haproxy -c` against it, which also catches a key that doesn't match its
certificate. The candidate is installed only if that passes; if it fails,
nothing is replaced or signalled. Next it saves
the server state, sends SIGUSR2 to the master (PID 1 in the container), and
waits until the admin socket is answered by the new worker. Listeners are
handed over, so in-flight and new requests are not dropped. The test proves
this with 300 requests across 3 reloads.

## Local test

```
deploy/canary/test.sh
```

The test needs no Docker and no root. It uses `$HAPROXY_BIN`, or `haproxy`
2.8 on `PATH`, or fetches the Ubuntu package with `apt-get download haproxy`
and unpacks it with `dpkg -x`. It builds `fakebackend`, which echoes the
backend name, path, query and headers, and mints HS256 JWTs shaped like the
services' tokens. It then boots the real `entrypoint.sh` and `conf.d/` with a
self-signed certificate, drives `sms-canary.sh`, and runs 194 assertions:
pins, JSON and form login (reordered fields, whitespace, unicode,
`\u`-escapes, charset, a body near 64 KiB, a 200 KB body), refresh (JSON,
form, camelCase), Bearer (missing, malformed, no username claim, Basic),
logout, UUID v7/v4 on all four id paths and both DLR paths with the query
intact, everything else → v3, percent buckets against Python `zlib.crc32`
(raising N never moves a client back), status, rollback, hitless reload
under load, refused broken config, XFF/X-Real-IP overwrite, HTTP/2 and
ALPN, TLS 1.0/1.1 and RSA-kex refused, header case and body pass-through,
debug header only on request and only in a debug build, stats not public,
v4 down (fallback, 503 JSON for v7 ids and DLRs, state kept across a
reload), v3 down, certificate renewal picked up by a reload (a mismatched
cert/key pair is refused and the old certificate stays), restart from
files, and log hygiene.

## Runbook (production)

Steps marked **⚠ PROD CHANGE** change production. Everything else is
read-only. Nothing here has been executed.

### Production facts (surveyed read-only, 2026-10-23)

| | v3 | v4 |
|---|---|---|
| container | `compose-sms-gw-service-1` | `tangra-sms-gw-sms-gw-1` |
| compose project / dir | `compose` in `/root/.docker/compose`, service `sms-gw-service` | `tangra-sms-gw` in `/srv/sms-gw-v4` |
| network (subnet) | `compose_app-tier` (192.168.208.0/20); v3 is .4, alias `sms-gw-service` | `tangra-sms-gw_default` (172.21.0.0/16), alias `sms-gw` |
| plain HTTP inside | `:9901` | `:9901` |
| LB fixed address (compose.yaml) | `192.168.223.250` | `172.21.255.250` |

- Docker 28.2.2, compose v5.1.3.
- Certificate: `/root/.docker/compose/ssl/fullchain.pem` and `privkey.pem`
  (Let's Encrypt, SAN sms-gw.verax.net + sms.verax.net, expires 2026-12-22).
- Renewal: root cron and `certbot.timer` run `certbot renew` with the nginx
  authenticator (nginx is normally inactive). The deploy hook
  `/etc/letsencrypt/renewal-hooks/deploy/sms-gw.sh` copies the sms.verax.net
  cert and key into `/root/.docker/compose/ssl`, then runs
  `docker restart sms-gw`.
  **Existing bug:** no container is named `sms-gw`, so that restart fails, and
  today v3 only picks up a renewed certificate when it happens to be
  restarted for some other reason. Step 6 points the hook at the LB.

### 0. Pre-checks (read-only)

```
# Fixed LB addresses must be free (and inside the subnets):
docker network inspect compose_app-tier -f '{{range .Containers}}{{.IPv4Address}} {{.Name}}{{println}}{{end}}' | grep -F 192.168.223.250 || echo free
docker network inspect tangra-sms-gw_default -f '{{range .Containers}}{{.IPv4Address}} {{.Name}}{{println}}{{end}}' | grep -F 172.21.255.250 || echo free
# Where v3's TRUSTED_PROXY_CIDRS and the 443 publish are defined:
grep -n -e 'TRUSTED_PROXY_CIDRS' -e '443' /root/.docker/compose/*.y*ml /root/.docker/compose/.env 2>/dev/null
docker port compose-sms-gw-service-1          # today: 9443/tcp -> 0.0.0.0:443, plus 9900 and 9901
docker exec compose-sms-gw-service-1 sh -c 'wget -qO- 127.0.0.1:9901/health || curl -s 127.0.0.1:9901/health'
docker exec tangra-sms-gw-sms-gw-1 sh -c 'wget -qO- 127.0.0.1:9901/health || curl -s 127.0.0.1:9901/health'
```

**Fixed-address caveat.** Docker only accepts a fixed `ipv4_address` on a
network whose subnet was *configured by the user* (an `ipam` block when the
network was created). Compose's default networks usually get their subnet
auto-assigned. In that case the LB fails to start with "user specified IP
address is supported only when connecting to networks with user configured
subnets". The preflight in step 2 tries the same addresses, so this shows up
before the cutover. If it does:

- **(a) Preferred:** trust the whole network subnet instead of a /32:
  `192.168.208.0/20` for v3 and `172.21.0.0/16` for v4. Only the stack's own
  containers live on those networks. Remove the two `ipv4_address` lines from
  `compose.yaml`.
- **(b) Alternative:** create a small user-subnet network shared by the LB and
  each backend. This changes the v3 and v4 compose files more than (a) does.

Never trust the LB's *dynamic* address: it changes when the container is
recreated.

### 1. Prepare v4 ⚠ PROD CHANGE (v4 only; no client impact, v4 has no traffic)

In `/srv/sms-gw-v4`, set the v4 config:

```yaml
public:
  trusted_proxies: [172.21.255.250/32]   # the LB's fixed address (fallback (a): 172.21.0.0/16)
  max_body_bytes: 65536
public_auth:
  jwt_secret: { file: ... }       # SAME secret as v3's SMS_GW_JWT_SECRET
  access_ttl_seconds: 7200
  refresh_ttl_seconds: 604800
rate_limits:                      # v3 prod values (SMS_GW_LOGIN_*/SEND_*; DLR = v3 defaults)
  login_per_minute: 600
  login_burst: 100
  send_per_minute: 6000
  send_burst: 200
  dlr_per_minute: 500
  dlr_burst: 100
```

Deploy the v4 image that mints UUIDv7 message ids, then restart v4 with
`cd /srv/sms-gw-v4 && docker compose up -d`. Check:

- `curl -s 127.0.0.1:9902/health` returns `{"status":"ok"}`.
- The secrets match:
  `printf %s "$V3_SECRET" | sha256sum` equals `sha256sum` of v4's secret
  file. Mind trailing newlines: compare the bytes the service actually reads.
- Provider `callback_url`s in v4 are unchanged
  (`https://sms-gw.verax.net/hermes/v1/sms/dlr`).

### 2. Install the LB ⚠ PROD CHANGE (new files and a test container only, no traffic yet)

```
install -d -m 755 /srv/sms-gw-lb /srv/sms-gw-lb/maps /srv/sms-gw-lb/conf.d
install -d -m 700 /srv/sms-gw-lb/run
cp compose.yaml entrypoint.sh sms-canary.sh /srv/sms-gw-lb/ && cp conf.d/*.cfg /srv/sms-gw-lb/conf.d/ && cp maps/*.map /srv/sms-gw-lb/maps/
cd /srv/sms-gw-lb && docker compose config >/dev/null && docker compose pull
```

Preflight on a loopback test port, with 443 untouched. It uses the same
networks **and the same fixed addresses**, so it also proves Docker accepts
them:

```
docker run -d --name sms-gw-lb-preflight --user 0:0 \
  --network compose_app-tier --ip 192.168.223.250 \
  -e SMS_GW_V3_ADDR=compose-sms-gw-service-1:9901 -e SMS_GW_V4_ADDR=tangra-sms-gw-sms-gw-1:9901 \
  -e CANARY_TLS_CERT=/ssl/fullchain.pem -e CANARY_TLS_KEY=/ssl/privkey.pem \
  -e CANARY_TLS_BIND=:8443 -e CANARY_RUN_DIR=/tmp/run -e CANARY_DROP_USER=haproxy \
  -v /srv/sms-gw-lb/conf.d:/usr/local/etc/haproxy/conf.d:ro -v /srv/sms-gw-lb/maps:/usr/local/etc/haproxy/maps:ro \
  -v /srv/sms-gw-lb/entrypoint.sh:/usr/local/etc/haproxy/entrypoint.sh:ro -v /root/.docker/compose/ssl:/ssl:ro \
  -p 127.0.0.1:8443:8443 --entrypoint /usr/local/etc/haproxy/entrypoint.sh haproxy:2.8
docker network connect --ip 172.21.255.250 tangra-sms-gw_default sms-gw-lb-preflight
curl -s --resolve sms-gw.verax.net:8443:127.0.0.1 https://sms-gw.verax.net:8443/health      # {"status":"ok"} from v3
openssl s_client -connect 127.0.0.1:8443 -servername sms-gw.verax.net </dev/null 2>/dev/null | openssl x509 -noout -fingerprint -sha256 -enddate
openssl s_client -connect 127.0.0.1:443 -servername sms-gw.verax.net </dev/null 2>/dev/null | openssl x509 -noout -fingerprint -sha256 -enddate   # v3 today
docker logs sms-gw-lb-preflight 2>&1 | tail      # both servers UP, route=default-v3
docker rm -f sms-gw-lb-preflight                 # frees the fixed addresses again
```

Compare the two fingerprints. If v3 serves an older certificate than the
files hold (renewed after v3's last restart, because of the hook bug), the LB
will serve the newer one, which is the correct one.

If `docker run --ip` or `network connect --ip` is refused, use fallback (a)
from step 0 and adjust step 1 and step 3 to the subnets.

### 3. Cut over :443 from v3 to the LB ⚠ PROD CHANGE (seconds of downtime on :443)

In v3's compose configuration under `/root/.docker/compose` (service
`sms-gw-service`), make exactly two changes. Back up the files first.

1. **Remove the `443:9443` port line.** Ports 9900 and 9901 stay published
   exactly as they are, because the v3 portal (10.1.111.150) uses them. No
   other port line changes.
2. **Append the LB's address to `TRUSTED_PROXY_CIDRS`**, wherever it is
   defined (compose `environment:` or the env file):
   `10.1.111.0/24,89.25.32.0/24,192.168.223.250/32` (fallback (a):
   `192.168.208.0/20`).

The second change is recommended because the cutover recreates the v3
container anyway, so it costs no extra downtime. Without it, v3 records the
LB's address as every client's IP: in login logs, audit, DLR
`remote_address`, and the per-IP login and DLR rate-limit buckets, which all
clients would then share (600/min login, 500/min DLR).

Run the cutover in a low-traffic window, with the two commands back to back
(images already pulled):

```
cd /root/.docker/compose && docker compose up -d sms-gw-service \
  && cd /srv/sms-gw-lb && docker compose up -d
```

`up -d sms-gw-service` recreates only v3, which briefly interrupts 9900/9901
for the portal too. Never run `docker compose down` on the `compose` project
while the LB is attached to `compose_app-tier`.

Verify:

1. `curl -s https://sms-gw.verax.net/health` from outside returns
   `{"status":"ok"}`. Check the certificate fingerprint, and that
   `openssl s_client -tls1_1` is refused.
2. `docker inspect -f '{{range $k,$v := .NetworkSettings.Networks}}{{$k}}={{$v.IPAddress}} {{end}}' sms-gw-lb`
   shows `compose_app-tier=192.168.223.250 tangra-sms-gw_default=172.21.255.250`.
   `docker exec compose-sms-gw-service-1 env | grep TRUSTED_PROXY_CIDRS` shows
   the /32.
3. `docker logs -f sms-gw-lb` shows `route=default-v3 user=jobsbg|cars|homes`
   on sends and logins, `route=dlr-v3` on DLRs, and both servers UP.
4. v3 DB (`tangra_sms`): `SELECT username, login_ip, event_time FROM
   sms_login_log ORDER BY id DESC LIMIT 10;` shows client IPs, not
   192.168.223.250. `SELECT remote_address, create_time FROM sms_dlr ORDER BY
   create_time DESC LIMIT 10;` shows the carrier's IPs.
5. **v3 portal:** the SMS pages load (message list, send drawer, providers,
   templates, dashboard).
6. **v4 portal:** the SMS pages load (the same set) through the gateway.
7. `/srv/sms-gw-lb/sms-canary.sh status`: percent 0, no pins, both backends
   UP.

### 4. Move homes ⚠ PROD CHANGE

```
sms-canary.sh pin homes v4
```

Note the time T. Verify:

- LB log: `route=pin-v4 user=homes` on login and sends, status 200.
- v4 DB: `SELECT id, substr(id::text,15,1) AS ver, status_code,
  status_message, remote_address, create_time FROM sms_message WHERE
  user_name='homes' AND create_time > 'T' ORDER BY create_time DESC;` Every
  `ver` must be `7`. **Stop and roll back if not.** `remote_address` must be
  the client's IP.
- DLRs reach v4: LB log `route=dlr-v4 ... 200`, and v4 DB `SELECT
  d.message_id, d.message_status, d.remote_address, d.create_time FROM
  sms_dlr d JOIN sms_message m ON m.id = d.message_id WHERE m.user_name='homes'
  AND m.create_time > 'T';` has rows, with `sms_message.status_code` moving
  from accepted to delivered.
- Nothing for homes lands on v3 after T: in the v3 DB, `SELECT count(*) FROM
  sms_message WHERE user_name='homes' AND create_time > 'T';` returns 0. The
  v3 logs show no `dlr parent load failed` or `dlr token mismatch` lines for
  v7 ids.
- Old homes messages (v4 UUIDs) still resolve on v3 by id and by DLR
  (`route=id-v3` / `dlr-v3`).

### 5. Move cars, then jobsbg ⚠ PROD CHANGE

Use `sms-canary.sh pin cars v4`, then later `sms-canary.sh pin jobsbg v4`.
Repeat the step 4 checks for each. jobsbg carries about 76% of the traffic,
so also watch the v4 send rate limits (6000/min, burst 200) and latency
(`%Tr` in the log).

### 6. Certificate renewal hook ⚠ PROD CHANGE (do this right after step 3, well before 2026-12-22)

After the cutover the LB serves :443. It builds its PEM from
`/root/.docker/compose/ssl` at start and again on every
`sms-canary.sh reload`. The SIGUSR2 reload re-reads the rebuilt PEM, so no
restart is needed. In `/etc/letsencrypt/renewal-hooks/deploy/sms-gw.sh`,
replace the broken line `docker restart sms-gw` with:

```
/srv/sms-gw-lb/sms-canary.sh reload
```

The reload validates the new cert and key pair first. If they don't match,
the hook fails, and the LB keeps serving the previous certificate. Don't add a
v3 restart. v3's own TLS listener is no longer published after the cutover,
and v3 reads the same files whenever it restarts.

Verify without waiting for a renewal:
`/srv/sms-gw-lb/sms-canary.sh reload`, then
`openssl s_client -connect sms-gw.verax.net:443 </dev/null 2>/dev/null | openssl x509 -noout -enddate`.
After the real renewal (around late November), the end date moves.
`certbot renew --dry-run` doesn't run deploy hooks; run
`certbot renew --dry-run --run-deploy-hooks` only if you want to exercise the
hook. The LB doesn't use port 80, so it doesn't interfere with the nginx
authenticator.

### Rollback

- **One client ⚠ PROD CHANGE:** `sms-canary.sh pin <user> v3`. The change is
  hitless and immediate. Messages already sent by v4 keep resolving on v4 by
  id and DLR, so **keep v4 running** until their DLRs have arrived (carrier
  DLRs can lag by hours). While the client is on v3, the v3 UI won't list its
  v4-sent messages.
- **All clients ⚠ PROD CHANGE:** `sms-canary.sh set 0`, then
  `sms-canary.sh pin <user> v3` for each v4 pin.
- **Remove the LB ⚠ PROD CHANGE:** first make sure every client is on v3 and
  v4's DLRs have drained. Then `cd /srv/sms-gw-lb && docker compose down`,
  restore the `443:9443` line (and the old `TRUSTED_PROXY_CIDRS`) in
  `/root/.docker/compose`, and run `docker compose up -d sms-gw-service`.
  :443 is down for seconds. Point the certbot hook at
  `docker restart compose-sms-gw-service-1`, the real container name. Any
  later v4 DLR would then reach v3 and be dropped.

## Review of the draft

Changes to the user's draft. Each one is marked `CHANGED:` in the config.

1. **`listen stats` on `:8404`.** It was reachable on every interface. It now
   binds to the container loopback and is never published. Status is read
   from the admin socket.
2. **Admin socket.** It moved from `/tmp/haproxy.sock` to the host-mounted
   `run/` directory, with `expose-fd listeners`, so the script can reach it
   from the host.
3. **`id_path` and `v7_id_path` were prefix matches.**
   `/hermes/v1/sms/<v7>/anything`, and any id that merely started like a
   UUID, were routed by id. Both regexes now require a full, anchored UUID,
   and v7 also requires the RFC 4122 variant nibble `[89ab]`. The same
   applies to the DLR `request_id` and `requestId` regexes.
4. **`option http-buffer-request` in `defaults`.** It buffered every request,
   sends included, and with the default 16 KiB buffer a 64 KiB login body
   couldn't be inspected. It was replaced by `wait-for-body` on login and
   refresh only, plus `tune.bufsize 81920`.
5. **Sticky-key extraction.**
   - Login and refresh read the body before any Bearer header.
   - The body codec is chosen by Content-Type, as the services do.
   - Form values are URL-decoded (`url_dec(1)`).
   - The refresh token is also read from form bodies.
   - Logout falls back to `X-Refresh-Token`.
   - Usernames outside the script's validation regex are dropped (no log
     injection, no bucket).
6. **Debug header.** The draft echoed `X-Sms-Gw-Route` to any client that
   asked. Now the rule exists only when the LB runs with `CANARY_DEBUG` set
   (unset in prod). The request header is never forwarded.
7. **Log format.** `%{+Q}r` (the full request line, including the DLR query
   with phone numbers and statuses) was replaced by method, path and
   version. `%tsc` and the connection counters were added.
8. **Error pages.** LB-generated errors use the services' JSON shape, not
   HAProxy HTML.
9. **Fallback visibility.** Requests routed to v3 because v4 is down are
   logged as `route=fallback-v3`, not `default-v3`. A pinned-v4 user no
   longer drops into the percent rule when v4 is down.
10. **`server-state-file`.** Added, so a DOWN v4 stays DOWN across a SIGUSR2
    reload instead of producing about 15 s of 503s.
11. **TLS.** `20-tls.cfg` was written as described: TCP mode TLS termination
    with PROXY v2 to `127.0.0.1:9902` inside the container. That address is
    the container loopback, so it doesn't clash with v4's host
    `127.0.0.1:9902`. The cipher list mirrors Go's, with TLS 1.2 minimum and
    ALPN `h2,http/1.1`.
12. **Header case.** HTTP/1.1 response header names keep Go's casing
    (`h1-case-adjust`).
13. **Environment-driven addresses and paths.** Bind addresses, map and run
    directories, and the DNS resolver come from the environment
    (`entrypoint.sh` sets the prod defaults), so the same files run in the
    container and in `test.sh`. `CANARY_DROP_USER` lets the master start as
    root (to read the root-only cert mount) while the workers run as
    `haproxy`.
14. **Certificate reloads.** `entrypoint.sh prepare` rebuilds and validates
    the PEM on every reload, so a renewed certificate is served after
    `sms-canary.sh reload` without a restart. A mismatched pair is refused.

Checked and kept as is, on HAProxy 2.8.30: `http_auth_bearer` (also accepts a
lower-case `bearer`), `jwt_payload_query` (no value for malformed tokens),
`json_query` (decodes ASCII `\u` escapes and is lenient with truncated JSON;
the backend then answers the 400), `req.body_param`, `crc32,mod(100)`
(matches `zlib.crc32`), `sub(txn.pct)`, `nbsrv`, `resolvers` with
`init-addr last,libc,none`.
