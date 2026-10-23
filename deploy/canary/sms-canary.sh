#!/usr/bin/env bash
# sms-canary.sh — operate the sms-gw V3 -> V4 canary balancer.
#
#   sms-canary.sh status                 percent, pins, backend health
#   sms-canary.sh set <0-100>            share of unpinned clients on V4
#   sms-canary.sh pin <user> <v3|v4>     pin one API client (username)
#   sms-canary.sh unpin <user>           remove a pin (client follows percent)
#   sms-canary.sh reload                 validate, refresh the TLS cert, reload
#   sms-canary.sh parity [--allow-anon]  read-only V3/V4 HTTP differential check
#
# Rollback of everything to V3:   set 0, then pin every V4 client to v3 (or
# unpin it), e.g.  sms-canary.sh set 0 && sms-canary.sh pin jobsbg v3 ...
#
# The map files are the only state. Every change replaces the file atomically
# (temp file next to the target + mv), validates the configuration and
# reloads HAProxy hitlessly: SIGUSR2 to the master (haproxy -W), which starts
# a new worker on the same listeners and lets the old one finish its
# requests. Server health is dumped to the server-state file right before the
# reload so the new worker keeps it (a DOWN V4 stays DOWN, clients keep
# falling back to V3 without a 503 window).
#
# Environment:
#   SMS_CANARY_DIR         LB directory on the host     [/srv/sms-gw-lb]
#                          (maps/, conf.d/, run/admin.sock, compose.yaml)
#   SMS_CANARY_CONTAINER   LB container name            [sms-gw-lb]
#   SMS_CANARY_HAPROXY_PID local (non-Docker) runs only: PID of the HAProxy
#                          master; reload signals it directly and validates
#                          with $SMS_CANARY_DIR/entrypoint.sh check
#   SMS_CANARY_NO_RELOAD=1 change the files only (e.g. LB not running yet)
#   SMS_CANARY_PARITY_BIN  smsgw-parity binary       [$SMS_CANARY_DIR/bin/smsgw-parity]
#   SMS_PARITY_BEARER      bearer token for `parity` (else --allow-anon)
#   SMS_CANARY_PARITY_CMD  replaces the built-in `parity` command entirely
set -euo pipefail

DIR=${SMS_CANARY_DIR:-/srv/sms-gw-lb}
MAPS=$DIR/maps
SOCK=$DIR/run/admin.sock
STATE=$DIR/run/servers.state
CONTAINER=${SMS_CANARY_CONTAINER:-sms-gw-lb}

die() { echo "sms-canary: $*" >&2; exit 1; }
warn() { echo "sms-canary: WARNING: $*" >&2; }
usage() { sed -n '4,9p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }

valid_username() { [[ $1 =~ ^[A-Za-z0-9._@+-]{1,128}$ ]] || die "invalid username '$1' (allowed: ^[A-Za-z0-9._@+-]{1,128}\$)"; }

# replace_file <target> <awk program> [awk -v args...]: rewrite target through
# awk into a temp file next to it, then mv over it (atomic on one filesystem).
replace_file() {
	local target=$1 prog=$2 tmp; shift 2
	[[ -f $target ]] || die "$target not found"
	tmp=$(mktemp "$(dirname "$target")/.$(basename "$target").XXXXXX")
	if ! awk "$@" "$prog" "$target" >"$tmp"; then rm -f "$tmp"; die "cannot rewrite $target"; fi
	chmod --reference="$target" "$tmp" 2>/dev/null || chmod 644 "$tmp"
	mv -f "$tmp" "$target"
}

# Map entries without comments/blank lines.
entries() { awk '!/^[[:space:]]*(#|$)/ { print $1, $2 }' "$1"; }
current_percent() { entries "$MAPS/canary.map" | awk '$1 == "percent" { p = $2 } END { print (p == "" ? 0 : p) }'; }

# One runtime API command over the admin socket (socat, else python3).
hap() {
	[[ -S $SOCK ]] || die "admin socket $SOCK not found (is the balancer running?)"
	if command -v socat >/dev/null 2>&1; then
		printf '%s\n' "$1" | socat -t 5 - "UNIX-CONNECT:$SOCK"
	else
		python3 -I -c '
import socket, sys
s = socket.socket(socket.AF_UNIX); s.settimeout(5); s.connect(sys.argv[1])
s.sendall((sys.argv[2] + "\n").encode()); out = b""
while True:
    b = s.recv(65536)
    if not b: break
    out += b
sys.stdout.write(out.decode())' "$SOCK" "$1"
	fi
}

# Validate the configuration and rebuild the TLS PEM from the current
# certificate files (entrypoint.sh prepare), with the entrypoint's defaults —
# the container env alone lacks them. A renewed certificate is therefore
# picked up by every reload.
check_config() {
	if [[ -n ${SMS_CANARY_HAPROXY_PID:-} ]]; then
		CANARY_CONF_DIR=$DIR/conf.d "$DIR/entrypoint.sh" prepare
	else
		docker exec "$CONTAINER" /usr/local/etc/haproxy/entrypoint.sh prepare
	fi
}

reload() {
	if [[ ${SMS_CANARY_NO_RELOAD:-0} == 1 ]]; then
		echo "files changed; reload skipped (SMS_CANARY_NO_RELOAD=1)"
		return
	fi
	check_config >/dev/null || die "configuration/certificate check failed; NOT reloaded (the running balancer is unchanged; a map file change is already on disk — fix it and run reload)"
	# Carry health across the reload (see server-state-file in 10-canary.cfg).
	if [[ -S $SOCK ]]; then
		hap "show servers state" >"$STATE.tmp" && mv -f "$STATE.tmp" "$STATE" || warn "could not dump server state"
	fi
	local old new i
	old=$(worker_pid)
	if [[ -n ${SMS_CANARY_HAPROXY_PID:-} ]]; then
		kill -USR2 "$SMS_CANARY_HAPROXY_PID"
	else
		docker kill -s USR2 "$CONTAINER" >/dev/null
	fi
	# Done when the admin socket is answered by a new worker.
	for i in $(seq 1 100); do
		new=$(worker_pid)
		if [[ -n $new && $new != "$old" ]]; then
			echo "reloaded (worker $old -> $new)"
			return
		fi
		sleep 0.1
	done
	die "reload signalled but no new worker after 10 s — check the balancer log"
}

worker_pid() { hap "show info" 2>/dev/null | awk -F': ' '$1 == "Pid" { print $2 }' || true; }

status() {
	echo "percent: $(current_percent)"
	echo "pins:"
	entries "$MAPS/pins.map" | sed 's/^/  /'
	echo "backends:"
	# show stat CSV: pxname,svname,...,status(18),...,check_status(37)
	hap "show stat" | awk -F, '
		$1 ~ /^sms_v[34]$/ && $2 != "FRONTEND" && $2 != "BACKEND" {
			printf "  %s/%s %s (check: %s, last change %ss ago)\n", $1, $2, $18, $37, $24 }'
}

[[ $# -ge 1 ]] || usage
cmd=$1; shift
case $cmd in
status)
	status
	;;
set)
	[[ $# -eq 1 && $1 =~ ^[0-9]+$ ]] && (($1 <= 100)) || die "usage: set <0-100>"
	old=$(current_percent)
	if (($1 < old)); then
		warn "lowering percent $old -> $1: clients in buckets $1..$((old - 1)) move back to V3." \
			"Their V4-sent messages stay readable by id, but V3 lists show only V3 data."
	fi
	replace_file "$MAPS/canary.map" '
		!/^[[:space:]]*(#|$)/ && $1 == "percent" { if (!done) print "percent", n; done = 1; next }
		{ print }
		END { if (!done) print "percent", n }' -v n="$1"
	echo "percent $old -> $1"
	reload
	;;
pin)
	[[ $# -eq 2 ]] || die "usage: pin <user> <v3|v4>"
	valid_username "$1"
	[[ $2 == v3 || $2 == v4 ]] || die "version must be v3 or v4"
	replace_file "$MAPS/pins.map" '
		!/^[[:space:]]*(#|$)/ && $1 == u { if (!done) print u, v; done = 1; next }
		{ print }
		END { if (!done) print u, v }' -v u="$1" -v v="$2"
	echo "pinned $1 -> $2"
	reload
	;;
unpin)
	[[ $# -eq 1 ]] || die "usage: unpin <user>"
	valid_username "$1"
	replace_file "$MAPS/pins.map" '!/^[[:space:]]*(#|$)/ && $1 == u { next } { print }' -v u="$1"
	echo "unpinned $1"
	reload
	;;
reload)
	reload
	;;
parity)
	# V3/V4 HTTP differential check with smsgw-parity (cmd/smsgw-parity,
	# `make parity-tool`). It is read-only: no sends, logins, receipts or
	# logouts. It runs in a throwaway container on the LB network, so it reaches
	# both backends directly, by container name, without going through the LB.
	# Bearer: SMS_PARITY_BEARER (a token valid on both instances); without one
	# pass --allow-anon. SMS_CANARY_PARITY_CMD still overrides everything.
	if [[ -n ${SMS_CANARY_PARITY_CMD:-} ]]; then
		# shellcheck disable=SC2086 # the command string is operator-provided
		exec $SMS_CANARY_PARITY_CMD "$@"
	fi
	PARITY_BIN=${SMS_CANARY_PARITY_BIN:-$DIR/bin/smsgw-parity}
	[[ -x $PARITY_BIN ]] || die "parity: $PARITY_BIN not found (build it with \`make parity-tool\` and copy it there)"
	anon=()
	for a in "$@"; do
		case $a in
		--allow-anon) anon=(-allow-anon) ;;
		*) die "parity: unknown argument $a" ;;
		esac
	done
	[[ ${#anon[@]} -gt 0 || -n ${SMS_PARITY_BEARER:-} ]] || die "parity: set SMS_PARITY_BEARER or pass --allow-anon"
	exec docker run --rm --network "${SMS_CANARY_NETWORK:-sms-gw-lb}" \
		-v "$PARITY_BIN:/smsgw-parity:ro" -e SMS_PARITY_BEARER \
		docker.io/library/alpine:3.20 /smsgw-parity \
		-baseline-url "http://${SMS_CANARY_V3:-compose-sms-gw-service-1:9901}" \
		-candidate-url "http://${SMS_CANARY_V4:-tangra-sms-gw-sms-gw-1:9901}" \
		"${anon[@]}"
	;;
*)
	usage
	;;
esac
