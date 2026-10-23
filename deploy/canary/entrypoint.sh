#!/bin/sh
# Container entrypoint for the sms-gw canary balancer (haproxy:2.8 image).
#
# v3's certificate and key are separate files on a read-only mount; HAProxy
# 2.8 wants one PEM, so they are concatenated into a private tmpfs file at
# start. Then HAProxy runs in master-worker mode in the foreground, loading
# every *.cfg in CANARY_CONF_DIR in lexical order. SIGUSR2 to PID 1 reloads
# (re-reading the config and the map files).
#
#   entrypoint.sh          start the balancer (container command)
#   entrypoint.sh check    only validate the configuration with the same
#                          defaults (sms-canary.sh runs this before a reload:
#                          docker exec sms-gw-lb /usr/local/etc/haproxy/entrypoint.sh check)
#
# Environment (defaults in brackets):
#   CANARY_TLS_CERT   certificate chain file      [required unless CANARY_TLS_PEM exists]
#   CANARY_TLS_KEY    private key file            [required unless CANARY_TLS_PEM exists]
#   CANARY_TLS_PEM    combined PEM to build/use   [/run/canary-tls/sms-gw.pem]
#   CANARY_CONF_DIR   config directory            [/usr/local/etc/haproxy/conf.d]
#   HAPROXY_BIN       haproxy binary              [haproxy]
#   plus the variables documented in conf.d/10-canary.cfg.
set -eu

: "${CANARY_CONF_DIR:=/usr/local/etc/haproxy/conf.d}"
: "${CANARY_TLS_PEM:=/run/canary-tls/sms-gw.pem}"
: "${CANARY_MAP_DIR:=/usr/local/etc/haproxy/maps}"
: "${CANARY_RUN_DIR:=/var/run/sms-canary}"
: "${CANARY_HTTP_BIND:=:9901}"
: "${CANARY_PROXY_BIND:=127.0.0.1:9902}"
: "${CANARY_TLS_BIND:=:443}"
: "${CANARY_STATS_BIND:=127.0.0.1:8404}"
: "${CANARY_DNS:=127.0.0.11:53}"
: "${HAPROXY_BIN:=haproxy}"
export CANARY_TLS_PEM CANARY_MAP_DIR CANARY_RUN_DIR CANARY_HTTP_BIND \
	CANARY_PROXY_BIND CANARY_TLS_BIND CANARY_STATS_BIND CANARY_DNS

: "${SMS_GW_V3_ADDR:?SMS_GW_V3_ADDR is required (v3 plain HTTP, host:port)}"
: "${SMS_GW_V4_ADDR:?SMS_GW_V4_ADDR is required (v4 plain HTTP, host:port)}"

if [ "${1:-}" = check ]; then
	exec "$HAPROXY_BIN" -c -f "$CANARY_CONF_DIR"
fi

if [ -n "${CANARY_TLS_CERT:-}" ] || [ -n "${CANARY_TLS_KEY:-}" ]; then
	: "${CANARY_TLS_CERT:?CANARY_TLS_CERT and CANARY_TLS_KEY go together}"
	: "${CANARY_TLS_KEY:?CANARY_TLS_CERT and CANARY_TLS_KEY go together}"
	umask 077
	mkdir -p "$(dirname "$CANARY_TLS_PEM")"
	{ cat "$CANARY_TLS_CERT"; echo; cat "$CANARY_TLS_KEY"; } >"$CANARY_TLS_PEM.tmp"
	mv "$CANARY_TLS_PEM.tmp" "$CANARY_TLS_PEM"
fi
[ -r "$CANARY_TLS_PEM" ] || { echo "entrypoint: no TLS PEM at $CANARY_TLS_PEM" >&2; exit 1; }
for m in pins.map canary.map; do
	[ -r "$CANARY_MAP_DIR/$m" ] || { echo "entrypoint: missing $CANARY_MAP_DIR/$m" >&2; exit 1; }
done
mkdir -p "$CANARY_RUN_DIR"

"$HAPROXY_BIN" -c -q -f "$CANARY_CONF_DIR"
exec "$HAPROXY_BIN" -W -db -f "$CANARY_CONF_DIR" "$@"
