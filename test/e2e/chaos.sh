#!/usr/bin/env bash
# Chaos tests for trunk reliability: break the upstream in the ways that
# kept FreePBX trunks down, and require voipd to notice, report and recover
# on its own within bounded time.
#
#   test/e2e/chaos.sh          (~12 minutes)
#   KEEP=1 test/e2e/chaos.sh   keep containers for debugging
#
# The trunk registers to "carrier.e2e", served by a fake DNS server, so
# DNS outages and carrier IP changes can be simulated too.
source "$(dirname "$0")/lib.sh"

PEER2=voipd-e2e-peer2
DNS=voipd-e2e-dns
EXPIRATION=30 # seconds; real trunks use 480, this keeps the test short

build
(cd "$ROOT" && CGO_ENABLED=0 go build -o "$WORK/fakedns" ./test/e2e/fakedns)
docker rm -f "$PEER2" "$DNS" >/dev/null 2>&1 || true

log "start carriers and fake DNS"
start_peer "$PEER"
start_peer "$PEER2"
PEER_IP=$(ip_of "$PEER")
PEER2_IP=$(ip_of "$PEER2")
mkdir -p "$WORK/dns"
echo "carrier.e2e. 1 IN A $PEER_IP" >"$WORK/dns/zone"
docker run -d --name "$DNS" --entrypoint /fakedns/fakedns \
	-v "$WORK/fakedns:/fakedns/fakedns:ro" -v "$WORK/dns:/data" "$IMAGE" >/dev/null
CONTAINERS+=("$DNS")
DNS_IP=$(ip_of "$DNS")
echo "carrier A $PEER_IP, carrier B $PEER2_IP, dns $DNS_IP"
echo "nameserver $DNS_IP" >"$WORK/resolv.conf"

log "start pbx + voipd (short supervisor timings)"
seed "carrier.e2e" "$EXPIRATION"
cat >"$WORK/voipd.yaml" <<YAML
trunks:
  grace: 5s
  first_retry: 10s
  max_backoff: 30s
  reregister_attempts: 2
  restart_after: 100s
  restart_cooldown: 10m
  poll_every: 5s
  dns_check_every: 3s
YAML
export VOIPD_DNS_SERVER="$DNS_IP:53"
start_voipd "$WORK/voipd.yaml"
start_pbx -v "$WORK/resolv.conf:/etc/resolv.conf:ro"

registered_at() { docker exec "$1" asterisk -rx "pjsip show contacts" 2>/dev/null | grep -q "+492011111111/"; }
events_since() { [ "$(sip_events_count "$1" "$2")" -ge 1 ]; }
peer_auth() {
	docker exec "$PEER" sh -c "printf '%s\n' '$1' > /etc/asterisk/pbx_auth.conf"
	peer "module reload res_pjsip.so" >/dev/null
}

within "trunk registers via DNS (carrier.e2e -> A)" 40 registered
within "trunk.up announced" 10 events_since trunk.up 0

log "scenario: carrier unreachable, then back"
T=$(now_ms)
docker pause "$PEER" >/dev/null
within "trunk.down announced" 100 events_since trunk.down "$T"
within "voipd escalates (trunk.reconnecting)" 40 events_since trunk.reconnecting "$T"
T=$(now_ms)
docker unpause "$PEER" >/dev/null
within "trunk.up after carrier returns" 60 events_since trunk.up "$T"
check "recovery reports downtime" 1 \
	"$(sql "SELECT count(*) FROM sip_events WHERE type='trunk.up' AND at >= $T AND json_extract(detail,'$.down_seconds') > 0")"

log "scenario: carrier rejects credentials (FreePBX stopped retrying here)"
T=$(now_ms)
peer_auth "auth = carrier-wrong"
within "trunk.down after auth rejection" 100 events_since trunk.down "$T"
T=$(now_ms)
peer_auth "auth = carrier-auth"
within "trunk.up once credentials are accepted again" 75 events_since trunk.up "$T"
peer_auth ""

log "scenario: DNS outage"
T=$(now_ms)
touch "$WORK/dns/down"
within "trunk.down while DNS is unreachable" 100 events_since trunk.down "$T"
T=$(now_ms)
rm "$WORK/dns/down"
within "trunk.up after DNS returns" 75 events_since trunk.up "$T"

log "scenario: carrier moves to a new IP (DNS target change)"
T=$(now_ms)
echo "carrier.e2e. 1 IN A $PEER2_IP" >"$WORK/dns/zone"
within "registered at the new carrier IP" 20 registered_at "$PEER2"
check "re-registration was forced by the DNS watcher" 1 \
	"$(sql "SELECT count(*) > 0 FROM sip_events WHERE type='trunk.reconnecting' AND at >= $T AND json_extract(detail,'$.reason')='dns_changed'")"

log "scenario: Asterisk restarts"
T=$(now_ms)
docker restart -t 2 "$PBX" >/dev/null
within "engine.down reported" 20 events_since engine.down "$T"
within "engine.up reported" 40 events_since engine.up "$T"
within "trunk registered again" 40 registered

log "scenario: long outage escalates to an engine restart"
T=$(now_ms)
docker pause "$PEER2" >/dev/null
within "trunk.down announced" 100 events_since trunk.down "$T"
restarted() { [ "$(sql "SELECT count(*) FROM sip_events WHERE type='trunk.reconnecting' AND at >= $T AND json_extract(detail,'$.action')='restart'")" -ge 1 ]; }
within "voipd restarts the engine after restart_after (100s)" 200 restarted
within "voipd reconnects to the restarted engine" 40 events_since engine.up "$T"
uptime_after=$(pbx "core show uptime seconds" | sed -n 's/System uptime: //p')
since_outage=$(( ($(now_ms) - T) / 1000 ))
[ -n "$uptime_after" ] && [ "$uptime_after" -lt "$since_outage" ] &&
	ok "Asterisk really restarted (uptime ${uptime_after}s < ${since_outage}s since the outage began)" ||
	bad "Asterisk did not restart (uptime ${uptime_after:-?}s, outage began ${since_outage}s ago)"
T=$(now_ms)
docker unpause "$PEER2" >/dev/null
within "trunk.up after the outage" 75 events_since trunk.up "$T"
check "only one engine restart (cooldown)" 1 \
	"$(sql "SELECT count(*) FROM sip_events WHERE type='trunk.reconnecting' AND json_extract(detail,'$.action')='restart'")"

log "event log"
sql "SELECT strftime('%H:%M:%S', at/1000, 'unixepoch', 'localtime') || '  ' || printf('%-19s', type) || ' ' || detail FROM sip_events ORDER BY at" | cut -c1-160

summary
