# Shared helpers for the e2e test scripts. Source it; set PBX/PEER names
# before calling start_* functions.
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
WORK=$(mktemp -d -t voipd-e2e.XXXX)
IMAGE=${IMAGE:-voipd-asterisk:22.11.0}
PBX=${PBX:-voipd-e2e-pbx}
PEER=${PEER:-voipd-e2e-peer}
CONTAINERS=()
VOIPD_PID=

export VOIPD_ARI_PASSWORD=e2e-ari VOIPD_AMI_PASSWORD=e2e-ami
export VOIPD_DB_PATH=$WORK/voipd.db VOIPD_GENERATED_DIR=$WORK/gen VOIPD_AREA_CODE=201

PASS=0
FAIL=0

cleanup() {
	stop_voipd
	if [ -n "${KEEP:-}" ]; then
		echo "KEEP set: containers ${CONTAINERS[*]} and $WORK left in place"
		return
	fi
	[ ${#CONTAINERS[@]} -gt 0 ] && docker rm -f "${CONTAINERS[@]}" >/dev/null 2>&1 || true
	rm -rf "$WORK"
}
trap cleanup EXIT

log() { printf '\n=== %s\n' "$*"; }
ok() { PASS=$((PASS + 1)); printf '  ok    %s\n' "$*"; }
bad() { FAIL=$((FAIL + 1)); printf '  FAIL  %s\n' "$*"; }

# check <description> <expected> <actual>
check() {
	if [ "$2" = "$3" ]; then ok "$1"; else bad "$1: expected '$2', got '$3'"; fi
}

# wait_for <description> <timeout-seconds> <command...>; records a failure
# on timeout but keeps going so later checks show the consequences.
wait_for() {
	local desc=$1 timeout=$2
	shift 2
	local end=$((SECONDS + timeout))
	until "$@" >/dev/null 2>&1; do
		if [ $SECONDS -ge $end ]; then
			bad "timed out after ${timeout}s waiting for: $desc"
			return 0
		fi
		sleep 0.5
	done
}

# within <description> <max-seconds> <command...>: like wait_for, but
# passes with the measured duration.
within() {
	local desc=$1 timeout=$2 start=$SECONDS
	shift 2
	local end=$((SECONDS + timeout))
	until "$@" >/dev/null 2>&1; do
		if [ $SECONDS -ge $end ]; then
			bad "$desc: not within ${timeout}s"
			return 0
		fi
		sleep 0.5
	done
	ok "$desc ($((SECONDS - start))s, limit ${timeout}s)"
}

pbx() { docker exec "$PBX" asterisk -rx "$*" 2>/dev/null; }
peer() { docker exec "$PEER" asterisk -rx "$*" 2>/dev/null; }
sql() { sqlite3 "$VOIPD_DB_PATH" "$1"; }
ip_of() { docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$1"; }

build() {
	log "build"
	(cd "$ROOT" && go build -o "$WORK/voipd" ./cmd/voipd)
	docker image inspect "$IMAGE" >/dev/null
	docker rm -f "$PBX" "$PEER" >/dev/null 2>&1 || true
}

# start_peer <name>: the carrier + phones Asterisk on the docker bridge.
start_peer() {
	local name=$1
	docker rm -f "$name" >/dev/null 2>&1 || true
	docker run -d --name "$name" --add-host pbx:host-gateway -e VOIPD_CONFIG_WAIT=0 \
		-e VOIPD_ARI_PASSWORD=x -e VOIPD_AMI_PASSWORD=x \
		-v "$ROOT/test/e2e/peer:/usr/share/voipd/asterisk-conf:ro" "$IMAGE" >/dev/null
	CONTAINERS+=("$name")
	# A real carrier is up before we register; make sure the peer is too, or
	# the first REGISTER goes unanswered and the retry comes 30s later.
	peer_ready() { docker exec "$name" asterisk -rx "pjsip show endpoint pbx" 2>/dev/null | grep -q "Endpoint:  pbx"; }
	wait_for "peer $name ready" 60 peer_ready
}

# start_pbx [docker args...]: the system under test, host networking.
# Start voipd first: Asterisk waits for the config voipd writes.
start_pbx() {
	docker rm -f "$PBX" >/dev/null 2>&1 || true
	mkdir -p "$VOIPD_GENERATED_DIR"
	docker run -d --name "$PBX" --network host "$@" \
		-e VOIPD_ARI_PASSWORD -e VOIPD_AMI_PASSWORD -e VOIPD_UID="$(id -u)" -e VOIPD_CONFIG_WAIT=30 \
		-v "$VOIPD_GENERATED_DIR:/etc/asterisk/voipd" "$IMAGE" >/dev/null
	CONTAINERS+=("$PBX")
	wait_for "pbx asterisk up" 30 pbx "core show uptime"
}

# seed <registrar> [reg_expiration]: trunk with DID +492011111111 routed to
# extensions 10 (kitchen) and 11 (office).
seed() {
	local registrar=$1 expiration=${2:-480}
	mkdir -p "$VOIPD_GENERATED_DIR"
	"$WORK/voipd" -migrate 2>/dev/null
	sql "
INSERT INTO trunks(id, name, profile, registrar, transport, dial_format, auth_username, auth_password, reg_expiration)
  VALUES (1, 'carrier', 'generic', '$registrar', 'udp', 'e164', 'u1', 'p1', $expiration);
INSERT INTO trunk_numbers(id, trunk_id, number) VALUES (1, 1, '+492011111111');
INSERT INTO extensions(id, number, name, sip_username, sip_password) VALUES
  (1, '10', 'Kitchen', 'kitchen', 'kitchen-pw'),
  (2, '11', 'Office', 'office', 'office-pw');
INSERT INTO ring_groups(id, name, ring_timeout, fallback) VALUES (1, 'home', 4, 'hangup');
INSERT INTO ring_group_members VALUES (1, 1), (1, 2);
INSERT INTO did_routes VALUES (1, 1);"
}

# start_voipd [config file]
start_voipd() {
	"$WORK/voipd" ${1:+-config "$1"} >>"$WORK/voipd.log" 2>&1 &
	VOIPD_PID=$!
}

stop_voipd() {
	if [ -n "$VOIPD_PID" ]; then
		kill "$VOIPD_PID" 2>/dev/null && wait "$VOIPD_PID" 2>/dev/null || true
		VOIPD_PID=
	fi
}

registered() { pbx "pjsip show registrations" | grep -q "Registered"; }
phone_up() { pbx "pjsip show contacts" | grep "$1/" | grep -qE "Avail|NonQual"; }

# sip_events_count <type> [since-unix-ms]
sip_events_count() { sql "SELECT count(*) FROM sip_events WHERE type='$1' AND at >= ${2:-0}"; }
now_ms() { date +%s%3N; }

summary() {
	log "voipd log warnings/errors"
	grep -E "level=(WARN|ERROR)" "$WORK/voipd.log" | cut -c1-220 || echo "  (none)"
	printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
	[ "$FAIL" -eq 0 ]
}
