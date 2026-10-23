#!/usr/bin/env bash
# test.sh — boot the canary balancer against two fake sms-gw instances and
# assert its routing. Local only: no Docker, no root.
#
#   deploy/canary/test.sh
#
# Needs: go, curl (with HTTP/2), jq, openssl, socat or python3, flock.
# HAProxy 2.8: $HAPROXY_BIN, else `haproxy` on PATH if it is 2.8, else the
# Ubuntu package is fetched with `apt-get download haproxy` and unpacked with
# `dpkg -x` into the work directory (no root needed).
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
# Short work dir: unix socket paths are limited to ~100 bytes.
WORK=$(mktemp -d "${XDG_RUNTIME_DIR:-/tmp}/smscanary.XXXXXX")
PIDS=()
HAP_PID=
cleanup() {
	local p
	[[ -n $HAP_PID ]] && kill "$HAP_PID" 2>/dev/null || true
	for p in "${PIDS[@]}"; do kill "$p" 2>/dev/null || true; done
	wait 2>/dev/null || true
	if [[ ${KEEP_WORK:-0} == 1 ]]; then echo "work dir kept: $WORK"; else rm -rf "$WORK"; fi
}
trap cleanup EXIT

PASS=0
FAIL=0
ok() { PASS=$((PASS + 1)); printf '  ok   %s\n' "$1"; }
bad() { FAIL=$((FAIL + 1)); printf '  FAIL %s\n' "$1"; }
eq() { # eq <name> <want> <got>
	if [[ $2 == "$3" ]]; then ok "$1"; else bad "$1 (want '$2', got '$3')"; fi
}
section() { printf '\n== %s\n' "$1"; }

# ---------------------------------------------------------------- haproxy --
if [[ -z ${HAPROXY_BIN:-} ]]; then
	if command -v haproxy >/dev/null && haproxy -v | grep -q 'version 2\.8\.'; then
		HAPROXY_BIN=$(command -v haproxy)
	else
		echo "fetching haproxy 2.8 (apt-get download + dpkg -x)"
		mkdir -p "$WORK/deb" "$WORK/hap"
		(cd "$WORK/deb" && apt-get download haproxy >/dev/null)
		dpkg -x "$WORK"/deb/haproxy_*.deb "$WORK/hap"
		HAPROXY_BIN=$WORK/hap/usr/sbin/haproxy
	fi
fi
"$HAPROXY_BIN" -v | head -1
export HAPROXY_BIN

# ----------------------------------------------------------------- fakes ---
FAKE=$WORK/fakebackend
(cd "$HERE/fakebackend" && go build -o "$FAKE" .)

read -r P_V3 P_V4 P_HTTP P_PROXY P_TLS P_STATS < <(python3 -I -c '
import socket
socks = [socket.socket() for _ in range(6)]
for s in socks: s.bind(("127.0.0.1", 0))
print(*[s.getsockname()[1] for s in socks])')

SECRET=canary-test-secret
mint() { "$FAKE" mint -secret "$SECRET" -user "$1" -kind "${2:-access}"; }

start_fake() { # start_fake <name> <port>
	"$FAKE" serve -name "$1" -addr "127.0.0.1:$2" >"$WORK/fake-$1.log" 2>&1 &
	PIDS+=($!)
	eval "PID_$1=$!"
}
start_fake v3 "$P_V3"
start_fake v4 "$P_V4"

# --------------------------------------------------------------- balancer --
# Same layout as the host directory (/srv/sms-gw-lb): conf.d, maps, run.
LB=$WORK/lb
mkdir -p "$LB/maps" "$LB/run" "$WORK/tls"
ln -s "$HERE/conf.d" "$LB/conf.d"
ln -s "$HERE/entrypoint.sh" "$LB/entrypoint.sh"
cp "$HERE/maps/pins.map" "$HERE/maps/canary.map" "$LB/maps/"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
	-keyout "$WORK/tls/key.pem" -out "$WORK/tls/cert.pem" -days 2 \
	-subj /CN=sms-gw.test 2>/dev/null

export SMS_GW_V3_ADDR=127.0.0.1:$P_V3 SMS_GW_V4_ADDR=127.0.0.1:$P_V4
export CANARY_CONF_DIR=$LB/conf.d
export CANARY_TLS_CERT=$WORK/tls/cert.pem CANARY_TLS_KEY=$WORK/tls/key.pem
export CANARY_TLS_PEM=$WORK/tls/combined.pem
export CANARY_MAP_DIR=$LB/maps CANARY_RUN_DIR=$LB/run
export CANARY_HTTP_BIND=127.0.0.1:$P_HTTP CANARY_PROXY_BIND=127.0.0.1:$P_PROXY
export CANARY_TLS_BIND=127.0.0.1:$P_TLS CANARY_STATS_BIND=127.0.0.1:$P_STATS
export CANARY_DNS=127.0.0.1:53
# Operator script against the same directory; it signals the local master.
export SMS_CANARY_DIR=$LB
CANARY_SOCK=$LB/run/admin.sock
SMSC=$HERE/sms-canary.sh
HAPLOG=$WORK/haproxy.log

hap() { printf '%s\n' "$1" | socat -t 5 - "UNIX-CONNECT:$CANARY_SOCK"; }
srv_state() { # srv_state <backend>: UP|DOWN
	hap "show servers state $1" | awk 'NR > 2 && NF > 6 { print ($6 == 2 ? "UP" : "DOWN") }'
}
wait_state() { # wait_state <backend> <UP|DOWN>
	local i
	for i in $(seq 1 60); do
		[[ $(srv_state "$1" 2>/dev/null) == "$2" ]] && return 0
		sleep 0.5
	done
	echo "timeout waiting for $1 to be $2" >&2
	return 1
}
start_hap() { # start_hap [extra env assignments...]
	env "$@" "$HERE/entrypoint.sh" >>"$HAPLOG" 2>&1 &
	HAP_PID=$!
	export SMS_CANARY_HAPROXY_PID=$HAP_PID
	local i
	for i in $(seq 1 50); do [[ -S $CANARY_SOCK ]] && break; sleep 0.1; done
	wait_state sms_v3 UP
	wait_state sms_v4 UP
}
stop_hap() {
	kill "$HAP_PID"
	wait "$HAP_PID" 2>/dev/null || true
	HAP_PID=
	rm -f "$CANARY_SOCK"
}
start_hap CANARY_DEBUG=1
MASTER_PID=$HAP_PID

BASE=https://127.0.0.1:$P_TLS
# call <curl args...>: prints "<backend>" of the answering fake (or the
# HTTP status when the balancer answered itself).
call() {
	local out
	out=$(curl -sk --max-time 10 -w '\n%{http_code}' "$@") || { echo "curl-error"; return; }
	local code=${out##*$'\n'} body=${out%$'\n'*}
	local be
	be=$(jq -r '.backend // empty' <<<"$body" 2>/dev/null || true)
	if [[ -n $be ]]; then echo "$be"; else echo "status-$code"; fi
}
# route_of <curl args...>: the balancer's routing reason (debug header).
route_of() {
	curl -sk --max-time 10 -o /dev/null -D - -H 'X-Sms-Gw-Debug: 1' "$@" |
		tr -d '\r' | awk -F': ' 'tolower($1) == "x-sms-gw-route" { print $2 }'
}
login_json() { call -X POST "$BASE/hermes/v1/login" -H 'Content-Type: application/json' --data-binary "$1"; }
login_form() { call -X POST "$BASE/hermes/v1/login" -H 'Content-Type: application/x-www-form-urlencoded' --data-binary "$1"; }
bearer() { call -H "Authorization: Bearer $1" "${@:2}"; }

V7=0190b6f2-3c4d-7abc-8def-0123456789ab
V7B=01929a6e-0000-7fff-bfff-ffffffffffff
U4=6f1c2d3e-4b5a-4c6d-9e8f-0a1b2c3d4e5f

# ------------------------------------------------------------------ tests --
worker() { hap "show info" | awk -F': ' '$1 == "Pid" { print $2 }'; }
section "pins (sms-canary.sh pin + hitless reload)"
W0=$(worker)
eq "pin before: homes on v3" v3 "$(login_json '{"username":"homes"}')"
"$SMSC" pin homes v4 >/dev/null
"$SMSC" pin cars v3 >/dev/null
"$SMSC" pin client.x@y+z v4 >/dev/null
eq "pins.map written" "homes v4|cars v3|client.x@y+z v4" "$(awk '!/^#/ && NF { printf "%s%s %s", sep, $1, $2; sep="|" }' "$LB/maps/pins.map")"
eq "loaded map == file" "cars v3|client.x@y+z v4|homes v4" "$(hap "show map $LB/maps/pins.map" | awk 'NF >= 3 { print $2, $3 }' | sort | paste -sd'|')"
eq "reload replaced the worker" yes "$([[ $(worker) != "$W0" ]] && echo yes || echo no)"
eq "same master process (hitless reload, not a restart)" yes "$(kill -0 "$MASTER_PID" && [[ $HAP_PID == "$MASTER_PID" ]] && echo yes || echo no)"
eq "no temp files left next to the maps" 0 "$(find "$LB/maps" -name '.*' -type f | wc -l)"
eq "comments in map files kept" yes "$(grep -q '^# sms-gw canary pins' "$LB/maps/pins.map" && echo yes || echo no)"
eq "re-pin replaces, does not duplicate" 1 "$("$SMSC" pin cars v3 >/dev/null; grep -c '^cars ' "$LB/maps/pins.map")"
eq "invalid username refused" refused "$("$SMSC" pin 'a b' v4 2>/dev/null && echo accepted || echo refused)"
eq "unicode username refused (validation regex)" refused "$("$SMSC" pin 'дом' v4 2>/dev/null && echo accepted || echo refused)"
eq "invalid version refused" refused "$("$SMSC" pin homes v5 2>/dev/null && echo accepted || echo refused)"
eq "percent > 100 refused" refused "$("$SMSC" set 101 2>/dev/null && echo accepted || echo refused)"
eq "parity hook not wired -> clear error" refused "$("$SMSC" parity 2>/dev/null && echo accepted || echo refused)"
eq "pinned user with a pinnable username" v4 "$(login_json '{"username":"client.x@y+z"}')"

section "login, JSON body"
eq "homes (pinned v4)" v4 "$(login_json '{"username":"homes","password":"x"}')"
eq "homes route reason" pin-v4 "$(route_of -X POST "$BASE/hermes/v1/login" -H 'Content-Type: application/json' -d '{"username":"homes","password":"x"}')"
eq "cars (pinned v3)" v3 "$(login_json '{"username":"cars","password":"x"}')"
eq "unknown user" v3 "$(login_json '{"username":"nobody","password":"x"}')"
eq "fields reordered" v4 "$(login_json '{"password":"x","grant_type":"password","username":"homes"}')"
eq "whitespace/newlines" v4 "$(login_json $'{\n  "password" : "x" ,\n\t"username"\t:\t"homes"\n}')"
eq "no Content-Type (JSON like the services)" v4 "$(call -X POST "$BASE/hermes/v1/login" --data-binary '{"username":"homes"}' -H 'Content-Type:')"
eq "charset parameter" v4 "$(call -X POST "$BASE/hermes/v1/login" -H 'Content-Type: application/json; charset=utf-8' --data-binary '{"username":"homes"}')"
eq "unicode password" v4 "$(login_json '{"username":"homes","password":"пароль✓"}')"
# Usernames outside ^[A-Za-z0-9._@+-]{1,128}$ cannot be pinned and are not
# sticky keys: they take the v3 default (README).
eq "unicode username -> v3 default" default-v3 "$(route_of -X POST "$BASE/hermes/v1/login" -H 'Content-Type: application/json' -d '{"username":"дом"}')"
eq "\\u-escaped ASCII username decoded" v4 "$(login_json '{"username":"\u0068omes","password":"x"}')"
eq "nested username is not the field" v3 "$(login_json '{"meta":{"username":"homes"},"username":"nobody"}')"
eq "username not a string" v3 "$(login_json '{"username":123}')"
# json_query is lenient with a truncated document; the backend answers the
# codec error itself (both versions reject it identically).
eq "truncated JSON still keyed (backend answers the 400)" v4 "$(login_json '{"username":"homes"')"
eq "not JSON at all" v3 "$(login_json 'username: homes')"
eq "empty body" v3 "$(login_json '')"
eq "username with quote dropped" default-v3 "$(route_of -X POST "$BASE/hermes/v1/login" -H 'Content-Type: application/json' -d '{"username":"ho\"mes"}')"
eq "username with newline dropped" default-v3 "$(route_of -X POST "$BASE/hermes/v1/login" -H 'Content-Type: application/json' -d '{"username":"homes\n"}')"
eq "GET login is not inspected" v3 "$(call "$BASE/hermes/v1/login")"

section "login, form body"
eq "form" v4 "$(login_form 'username=homes&password=x')"
eq "form reordered" v4 "$(login_form 'password=a%26b&username=homes')"
eq "form url-encoded username" v4 "$(login_form 'username=client.x%40y%2Bz&password=x')"
eq "form url-encoded unicode password" v4 "$(login_form 'username=homes&password=%D0%BF%D0%B0%D1%80')"
eq "form unknown" v3 "$(login_form 'username=nobody&password=x')"
eq "form pinned v3" v3 "$(login_form 'username=cars&password=x')"
eq "form body sent as JSON content type" v3 "$(login_json 'username=homes&password=x')"
eq "JSON body sent as form content type" v3 "$(login_form '{"username":"homes"}')"

section "login, body near the 64 KiB cap"
PAD=$(head -c 65000 /dev/zero | tr '\0' 'a')
BODY='{"username":"homes","password":"'$PAD'"}'
eq "username first, ${#BODY} bytes" v4 "$(login_json "$BODY")"
BODY='{"password":"'$PAD'","username":"homes"}'
eq "username last, ${#BODY} bytes" v4 "$(login_json "$BODY")"
GOT=$(curl -sk -X POST "$BASE/hermes/v1/login" -H 'Content-Type: application/json' --data-binary "$BODY" | jq -r .body_len)
eq "whole body forwarded" "${#BODY}" "$GOT"
{ printf '{"password":"'; head -c 200000 /dev/zero | tr '\0' 'a'; printf '","username":"homes"}'; } >"$WORK/big.json"
eq "oversized body (200 KB, key past the buffer) forwarded to the v3 default" v3 "$(call -X POST "$BASE/hermes/v1/login" -H 'Content-Type: application/json' --data-binary @"$WORK/big.json")"
eq "oversized body forwarded whole (backend enforces the cap)" "$(wc -c <"$WORK/big.json")" "$(curl -sk -X POST "$BASE/hermes/v1/login" -H 'Content-Type: application/json' --data-binary @"$WORK/big.json" | jq -r .body_len)"
eq "large send body passes through" v4 "$(bearer "$(mint homes)" -X POST "$BASE/hermes/v1/sms" -H 'Content-Type: application/json' --data-binary "{\"text\":\"$PAD\"}")"

section "refresh_token"
RT_HOMES=$(mint homes refresh)
RT_CARS=$(mint cars refresh)
eq "JSON refresh_token homes" v4 "$(call -X POST "$BASE/hermes/v1/refresh_token" -H 'Content-Type: application/json' -d "{\"refresh_token\": \"$RT_HOMES\"}")"
eq "JSON refreshToken homes" v4 "$(call -X POST "$BASE/hermes/v1/refresh_token" -H 'Content-Type: application/json' -d "{\"grant_type\":\"refresh_token\",\"refreshToken\":\"$RT_HOMES\"}")"
eq "form refresh_token homes" v4 "$(call -X POST "$BASE/hermes/v1/refresh_token" -H 'Content-Type: application/x-www-form-urlencoded' -d "refresh_token=$RT_HOMES")"
eq "JSON refresh_token cars" v3 "$(call -X POST "$BASE/hermes/v1/refresh_token" -H 'Content-Type: application/json' -d "{\"refresh_token\":\"$RT_CARS\"}")"
eq "garbage refresh token" v3 "$(call -X POST "$BASE/hermes/v1/refresh_token" -H 'Content-Type: application/json' -d '{"refresh_token":"not.a.jwt"}')"
eq "refresh body wins over Authorization" v3 "$(bearer "$(mint homes)" -X POST "$BASE/hermes/v1/refresh_token" -H 'Content-Type: application/json' -d "{\"refresh_token\":\"$RT_CARS\"}")"

section "Authorization: Bearer"
AT_HOMES=$(mint homes)
AT_CARS=$(mint cars)
eq "send homes" v4 "$(bearer "$AT_HOMES" -X POST "$BASE/hermes/v1/sms" -H 'Content-Type: application/json' -d '{"to":"x"}')"
eq "send homes /v1/sms" v4 "$(bearer "$AT_HOMES" -X POST "$BASE/v1/sms" -d '{}')"
eq "list homes" v4 "$(bearer "$AT_HOMES" "$BASE/hermes/v1/sms?page=1")"
eq "me homes" v4 "$(bearer "$AT_HOMES" "$BASE/hermes/v1/me")"
eq "send cars" v3 "$(bearer "$AT_CARS" -X POST "$BASE/hermes/v1/sms" -d '{}')"
eq "send unknown user" v3 "$(bearer "$(mint nobody)" -X POST "$BASE/hermes/v1/sms" -d '{}')"
eq "token without username claim" v3 "$(bearer "$(mint '')" -X POST "$BASE/hermes/v1/sms" -d '{}')"
eq "malformed token" v3 "$(bearer "abc" -X POST "$BASE/hermes/v1/sms" -d '{}')"
eq "three garbage segments" v3 "$(bearer "a.b.c" -X POST "$BASE/hermes/v1/sms" -d '{}')"
eq "no Authorization" v3 "$(call -X POST "$BASE/hermes/v1/sms" -d '{}')"
eq "Basic auth" v3 "$(call -u homes:x -X POST "$BASE/hermes/v1/sms" -d '{}')"
eq "lower-case bearer scheme" v4 "$(call -H "Authorization: bearer $AT_HOMES" "$BASE/hermes/v1/me")"

section "logout"
eq "Bearer homes" v4 "$(bearer "$AT_HOMES" -X POST "$BASE/hermes/v1/logout")"
eq "X-Refresh-Token homes" v4 "$(call -X POST "$BASE/hermes/v1/logout" -H "X-Refresh-Token: $RT_HOMES")"
eq "Bearer wins over X-Refresh-Token" v3 "$(bearer "$AT_CARS" -X POST "$BASE/hermes/v1/logout" -H "X-Refresh-Token: $RT_HOMES")"
eq "neither" v3 "$(call -X POST "$BASE/hermes/v1/logout")"

section "message ids route by UUID version (owner wins over the client pin)"
for p in /hermes/v1/sms/ /v1/sms/ /hermes/v1/sms/dlr/ /v1/sms/dlr/; do
	eq "${p}{v7} cars token" v4 "$(bearer "$AT_CARS" "$BASE$p$V7")"
	eq "${p}{v7 upper-case} no token" v4 "$(call "$BASE$p${V7B^^}")"
	eq "${p}{v4} homes token" v3 "$(bearer "$AT_HOMES" "$BASE$p$U4")"
	eq "${p}{non-uuid} homes token (client pin)" v4 "$(bearer "$AT_HOMES" "$BASE${p}abc")"
	eq "${p}{v7}/extra is not an id path" v3 "$(call "$BASE$p$V7/extra")"
	eq "${p}{v7 prefix only} is not an id" v3 "$(call "$BASE$p${V7:0:16}")"
done
eq "v7 with bad variant nibble" v3 "$(call "$BASE/hermes/v1/sms/0190b6f2-3c4d-7abc-1def-0123456789ab")"
eq "id route reason" id-v4 "$(route_of "$BASE/hermes/v1/sms/$V7")"

section "carrier DLRs route by request_id"
for p in /dlr /hermes/v1/sms/dlr; do
	Q="request_id=$V7&status=1&phone=359888123456&ts=2026-10-23%2010:00:00"
	eq "$p request_id v7" v4 "$(call "$BASE$p?$Q")"
	eq "$p query kept intact" "$Q" "$(curl -sk "$BASE$p?$Q" | jq -r .query)"
	eq "$p path kept" "$p" "$(curl -sk "$BASE$p?$Q" | jq -r .path)"
	eq "$p requestId v7" v4 "$(call "$BASE$p?status=2&requestId=$V7")"
	eq "$p request_id v4" v3 "$(call "$BASE$p?request_id=$U4&status=1")"
	eq "$p no request_id" v3 "$(call "$BASE$p?status=1")"
	eq "$p homes token does not matter" v3 "$(bearer "$AT_HOMES" "$BASE$p?request_id=$U4")"
done
eq "dlr route reason" dlr-v4 "$(route_of "$BASE/dlr?request_id=$V7")"

section "everything else -> v3"
eq "GET /health" default-v3 "$(route_of "$BASE/health")"
eq "GET /health body" '{"status":"ok"}' "$(curl -sk "$BASE/health")"
for p in / /metrics /assets/index.js /stats /unknown /v4/dlr "/hermes/v1/sms/dlr/"; do
	eq "GET $p" v3 "$(call "$BASE$p")"
done
eq "/lb-health answered by the balancer" "status-200" "$(call "$BASE/lb-health")"

section "percent buckets (crc32(username) % 100 < N -> v4)"
"$SMSC" unpin homes >/dev/null
mapfile -t USERS < <(python3 -I -c '
import zlib
for i in range(40):
    u = "client%02d" % i
    print(u, zlib.crc32(u.encode()) % 100)')
declare -A WAS_V4=()
monotonic=yes
for n in 0 10 35 70 100; do
	"$SMSC" set "$n" >/dev/null
	mismatch=0
	for line in "${USERS[@]}"; do
		u=${line% *} b=${line#* }
		want=v3
		((b < n)) && want=v4
		got=$(bearer "$(mint "$u")" "$BASE/hermes/v1/me")
		[[ $got == "$want" ]] || mismatch=$((mismatch + 1))
		if [[ -n ${WAS_V4[$u]:-} && $got != v4 ]]; then monotonic=no; fi
		[[ $got == v4 ]] && WAS_V4[$u]=1
	done
	eq "percent $n: 40 clients match crc32 buckets" 0 "$mismatch"
done
eq "raising percent never moved a client back to v3" yes "$monotonic"
eq "pin v3 beats percent 100" v3 "$(bearer "$AT_CARS" "$BASE/hermes/v1/me")"
eq "bucket route reason" bucket-v4 "$(route_of -H "Authorization: Bearer $(mint client00)" "$BASE/hermes/v1/me")"
eq "anonymous stays v3 at percent 100" v3 "$(call "$BASE/hermes/v1/me")"
eq "lowering percent warns" yes "$("$SMSC" set 0 2>&1 >/dev/null | grep -q 'WARNING: lowering percent 100 -> 0' && echo yes || echo no)"
eq "percent file" "percent 0" "$(awk '!/^#/ && NF' "$LB/maps/canary.map")"
eq "percent 0: bucket client back on v3" v3 "$(bearer "$(mint client00)" "$BASE/hermes/v1/me")"
"$SMSC" pin homes v4 >/dev/null

section "status"
ST=$("$SMSC" status)
eq "status shows percent" yes "$(grep -q '^percent: 0$' <<<"$ST" && echo yes || echo no)"
eq "status shows pins" yes "$(grep -q '^  homes v4$' <<<"$ST" && echo yes || echo no)"
eq "status shows both backends UP (admin socket, not public stats)" 2 "$(grep -cE '^  sms_v[34]/v[34] UP ' <<<"$ST")"

section "rollback to v3 (set 0 + pin v3)"
"$SMSC" pin jobsbg v4 >/dev/null
"$SMSC" set 50 >/dev/null
eq "jobsbg on v4 before rollback" v4 "$(bearer "$(mint jobsbg)" "$BASE/hermes/v1/me")"
"$SMSC" set 0 >/dev/null 2>&1
"$SMSC" pin jobsbg v3 >/dev/null
"$SMSC" pin homes v3 >/dev/null
eq "jobsbg back on v3" v3 "$(bearer "$(mint jobsbg)" "$BASE/hermes/v1/me")"
eq "homes back on v3" v3 "$(login_json '{"username":"homes"}')"
eq "no v4 pin left in the file" "" "$(awk '!/^#/ && NF && $2 != "v3" && $1 != "client.x@y+z"' "$LB/maps/pins.map")"
"$SMSC" pin homes v4 >/dev/null

section "hitless reload under load"
: >"$WORK/load.txt"
(for i in $(seq 1 300); do curl -sk --max-time 5 -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $AT_HOMES" "$BASE/hermes/v1/me" >>"$WORK/load.txt"; done) &
LOADER=$!
for i in 1 2 3; do "$SMSC" reload >/dev/null; sleep 0.3; done
wait "$LOADER"
eq "300 requests across 3 reloads: all 200" "300" "$(grep -c '^200$' "$WORK/load.txt")"
mkdir -p "$WORK/badlb/conf.d" "$WORK/badlb/maps" "$WORK/badlb/run"
ln -s "$HERE/entrypoint.sh" "$WORK/badlb/entrypoint.sh"
cp "$LB"/maps/*.map "$WORK/badlb/maps/"
printf 'frontend broken\n    this-is-not-a-keyword\n' >"$WORK/badlb/conf.d/10-canary.cfg"
W1=$(worker)
eq "broken config refused, nothing signalled" refused "$(SMS_CANARY_DIR=$WORK/badlb "$SMSC" reload >/dev/null 2>&1 && echo accepted || echo refused)"
eq "worker unchanged after the refused reload" "$W1" "$(worker)"

section "client address headers"
H=$(curl -sk -H 'X-Forwarded-For: 6.6.6.6' -H 'X-Real-IP: 6.6.6.6' -H 'Forwarded: for=6.6.6.6' "$BASE/hermes/v1/me")
eq "X-Forwarded-For replaced" "[127.0.0.1]" "$(jq -r '.headers["X-Forwarded-For"]' <<<"$H")"
eq "X-Real-IP replaced" "[127.0.0.1]" "$(jq -r '.headers["X-Real-Ip"]' <<<"$H")"
H=$(curl -sk -H 'X-Forwarded-For: 6.6.6.6, 7.7.7.7' -H "Authorization: Bearer $AT_HOMES" "$BASE/hermes/v1/me")
eq "spoofed XFF list replaced (v4 path)" "[127.0.0.1]" "$(jq -r '.headers["X-Forwarded-For"]' <<<"$H")"
eq "debug request header not forwarded" null "$(curl -sk -H 'X-Sms-Gw-Debug: 1' "$BASE/" | jq -r '.headers["X-Sms-Gw-Debug"]')"

section "TLS / HTTP versions / response headers"
eq "HTTP/2 negotiated via ALPN" 2 "$(curl -sk --http2 -o /dev/null -w '%{http_version}' "$BASE/health")"
eq "HTTP/2 routes like HTTP/1.1" v4 "$(call --http2 -H "Authorization: Bearer $AT_HOMES" "$BASE/hermes/v1/me")"
eq "HTTP/2 login body inspected" v4 "$(call --http2 -X POST "$BASE/hermes/v1/login" -H 'Content-Type: application/json' -d '{"username":"homes"}')"
eq "HTTP/1.1 when asked" 1.1 "$(curl -sk --http1.1 -o /dev/null -w '%{http_version}' "$BASE/health")"
ALPN=$(openssl s_client -connect "127.0.0.1:$P_TLS" -alpn h2,http/1.1 </dev/null 2>/dev/null | sed -n 's/^ALPN protocol: //p')
eq "ALPN prefers h2 (as Go net/http)" h2 "$ALPN"
eq "TLS 1.2 accepted" yes "$(openssl s_client -connect "127.0.0.1:$P_TLS" -tls1_2 </dev/null >/dev/null 2>&1 && echo yes || echo no)"
eq "TLS 1.3 accepted" yes "$(openssl s_client -connect "127.0.0.1:$P_TLS" -tls1_3 </dev/null >/dev/null 2>&1 && echo yes || echo no)"
eq "TLS 1.1 refused" no "$(openssl s_client -connect "127.0.0.1:$P_TLS" -tls1_1 -cipher 'DEFAULT:@SECLEVEL=0' </dev/null >/dev/null 2>&1 && echo yes || echo no)"
eq "TLS 1.0 refused" no "$(openssl s_client -connect "127.0.0.1:$P_TLS" -tls1 -cipher 'DEFAULT:@SECLEVEL=0' </dev/null >/dev/null 2>&1 && echo yes || echo no)"
eq "TLS 1.2 RSA-kex cipher refused (as Go)" no "$(openssl s_client -connect "127.0.0.1:$P_TLS" -tls1_2 -cipher AES128-GCM-SHA256 </dev/null >/dev/null 2>&1 && echo yes || echo no)"
HDRS=$(curl -sk --http1.1 -D - -o /dev/null "$BASE/hermes/v1/me" | tr -d '\r' | sed 1d | cut -d: -f1 | sort | paste -sd,)
DIRECT=$(curl -s -D - -o /dev/null "http://127.0.0.1:$P_V3/hermes/v1/me" | tr -d '\r' | sed 1d | cut -d: -f1 | sort | paste -sd,)
eq "HTTP/1.1 response header names identical to the backend's (case too)" "$DIRECT" "$HDRS"
B1=$(curl -sk "$BASE/hermes/v1/me" | jq -c 'del(.headers, .remote)')
B2=$(curl -s -H 'X-Forwarded-For: 127.0.0.1' "http://127.0.0.1:$P_V3/hermes/v1/me" | jq -c 'del(.headers, .remote)')
eq "response body untouched" "$B2" "$B1"
eq "no route header without the debug request header" "" "$(curl -sk -D - -o /dev/null "$BASE/" | grep -i '^x-sms-gw-route' || true)"
eq "route header on request (debug build)" default-v3 "$(route_of "$BASE/")"

section "stats page is not public"
eq "/stats on :443 is v3's, not HAProxy's" v3 "$(call "$BASE/stats")"
eq "stats bind default is loopback" '127.0.0.1:8404' "$(sed -n 's/^: "${CANARY_STATS_BIND:=\(.*\)}"$/\1/p' "$HERE/entrypoint.sh")"
eq "stats served on its loopback bind" 200 "$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$P_STATS/stats")"
eq "compose publishes only 443 (not 9900/9901/9902/8404)" '"443:443"' "$(sed -n '/^    ports:/,/^    [a-z]/{s/^ *- //p}' "$HERE/compose.yaml" | paste -sd,)"

section "v4 down: client traffic falls back to v3, v4-owned messages do not"
kill "$PID_v4"
wait "$PID_v4" 2>/dev/null || true
wait_state sms_v4 DOWN
eq "pinned v4 login -> v3" v3 "$(login_json '{"username":"homes"}')"
eq "pinned v4 send -> v3" v3 "$(bearer "$AT_HOMES" -X POST "$BASE/hermes/v1/sms" -d '{}')"
eq "fallback route reason" fallback-v3 "$(route_of -H "Authorization: Bearer $AT_HOMES" "$BASE/hermes/v1/me")"
"$SMSC" set 100 >/dev/null
eq "bucket user -> v3" v3 "$(bearer "$(mint client00)" "$BASE/hermes/v1/me")"
"$SMSC" reload >/dev/null
eq "after a reload V4 stays DOWN (server-state-file): still fallback, no 503" fallback-v3 "$(route_of -H "Authorization: Bearer $AT_HOMES" "$BASE/hermes/v1/me")"
"$SMSC" set 0 >/dev/null 2>&1
eq "v7 DLR -> 503 (no v3 fallback)" status-503 "$(call "$BASE/dlr?request_id=$V7")"
eq "v7 message id -> 503" status-503 "$(call "$BASE/hermes/v1/sms/$V7")"
ERR=$(curl -sk -D - "$BASE/hermes/v1/sms/$V7" | tr -d '\r')
eq "503 body is the services' JSON error" '{"code":503,"reason":"SERVICE_UNAVAILABLE","message":"service unavailable","metadata":{}}' "$(tail -1 <<<"$ERR")"
eq "503 has no HAProxy HTML" 0 "$(grep -ci 'haproxy\|<html' <<<"$ERR" || true)"
eq "503 content type" "content-type: application/json" "$(grep -i '^content-type' <<<"$ERR")"
start_fake v4 "$P_V4"
wait_state sms_v4 UP
eq "v4 back: pinned user returns to v4" v4 "$(login_json '{"username":"homes"}')"

section "v3 down"
kill "$PID_v3"
wait "$PID_v3" 2>/dev/null || true
wait_state sms_v3 DOWN
eq "anonymous -> 503 JSON" status-503 "$(call "$BASE/")"
eq "pinned v4 unaffected" v4 "$(bearer "$AT_HOMES" "$BASE/hermes/v1/me")"
start_fake v3 "$P_V3"
wait_state sms_v3 UP
eq "v3 back" v3 "$(call "$BASE/")"

section "restart without CANARY_DEBUG: state from files, no debug header"
"$SMSC" pin cars v4 >/dev/null
stop_hap
start_hap
eq "pins survive a restart (file is the source of truth)" v4 "$(bearer "$AT_CARS" "$BASE/hermes/v1/me")"
eq "debug header ignored in the default build" "" "$(route_of "$BASE/")"

section "access log"
sleep 1
eq "log line names route, user and backend" yes "$(grep -q 'sms_public sms_v4/v4 .* 200 .*"POST /hermes/v1/login HTTP/2.0" route=pin-v4 user=homes' "$HAPLOG" && echo yes || echo no)"
eq "no JWTs in the log" 0 "$(grep -c 'eyJ' "$HAPLOG" || true)"
eq "no DLR query strings (phone numbers) in the log" 0 "$(grep -c '359888123456' "$HAPLOG" || true)"
eq "no request bodies in the log" 0 "$(grep -c 'password' "$HAPLOG" || true)"
eq "health transitions logged" yes "$(grep -q 'sms_v4/v4 is DOWN' "$HAPLOG" && echo yes || echo no)"

printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
[[ $FAIL -eq 0 ]]
