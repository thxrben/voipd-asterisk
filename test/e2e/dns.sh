#!/usr/bin/env bash
# DNS/NAPTR tests modelled on Deutsche Telekom: the trunk registrar is a
# domain with only NAPTR -> SRV -> A records (no A record of its own),
# pointing at two carrier servers ("SBCs") with different priorities. The
# trunk has two numbers (MSNs), each registered separately.
#
# Covers: RFC 3263 resolution in Asterisk, per-number routing, failover to
# the secondary server, and that voipd never re-registers a trunk while a
# call is running on it.
#
#   test/e2e/dns.sh            (~6 minutes)
#   KEEP=1 test/e2e/dns.sh
source "$(dirname "$0")/lib.sh"

PEER2=voipd-e2e-peer2
DNS=voipd-e2e-dns
N1=+492011111111
N2=+492012222222

calls_done() { [ "$(sql "SELECT count(*) FROM calls WHERE ended_at IS NOT NULL")" -ge "$1" ]; }
last_call() { sql "SELECT $1 FROM calls ORDER BY started_at DESC LIMIT 1"; }
registered_at() { docker exec "$1" asterisk -rx "pjsip show contacts" 2>/dev/null | grep -q "$2/"; }
both_at() { registered_at "$1" "$N1" && registered_at "$1" "$N2"; }
# A server keeps an old binding until it expires, so "has a contact" does
# not show where the trunk registers *now*. Count REGISTER requests a server
# received for each number since a point in time (peers log SIP traffic).
registers_since() { # <peer> <unix-seconds> <number>
	docker logs --since "$2" "$1" 2>&1 | grep -A12 "^REGISTER sip:" | grep -c "^To: <sip:$3@" || true
}
both_register_at_since() { # <peer> <unix-seconds>
	[ "$(registers_since "$1" "$2" "$N1")" -ge 1 ] && [ "$(registers_since "$1" "$2" "$N2")" -ge 1 ]
}
pbx_registered() { [ "$(pbx "pjsip show registrations" | grep -c Registered)" -ge 2 ]; }
p1() { docker exec "$PEER" asterisk -rx "$*" >/dev/null 2>&1; }

# zone <priority sbc1> <priority sbc2>
zone() {
	cat >"$WORK/dns/zone" <<ZONE
carrier.e2e. 1 IN NAPTR 10 50 "s" "SIP+D2U" "" _sip._udp.carrier.e2e.
_sip._udp.carrier.e2e. 1 IN SRV $1 50 5060 sbc1.carrier.e2e.
_sip._udp.carrier.e2e. 1 IN SRV $2 50 5060 sbc2.carrier.e2e.
sbc1.carrier.e2e. 1 IN A $PEER_IP
sbc2.carrier.e2e. 1 IN A $PEER2_IP
ZONE
}

build
(cd "$ROOT" && CGO_ENABLED=0 go build -o "$WORK/fakedns" ./test/e2e/fakedns)
docker rm -f "$PEER2" "$DNS" >/dev/null 2>&1 || true

log "start two carrier servers and DNS"
start_peer "$PEER"
start_peer "$PEER2"
PEER_IP=$(ip_of "$PEER")
PEER2_IP=$(ip_of "$PEER2")
mkdir -p "$WORK/dns"
zone 10 20
for p in "$PEER" "$PEER2"; do docker exec "$p" asterisk -rx "pjsip set logger on" >/dev/null; done
docker run -d --name "$DNS" --entrypoint /fakedns/fakedns \
	-v "$WORK/fakedns:/fakedns/fakedns:ro" -v "$WORK/dns:/data" "$IMAGE" >/dev/null
CONTAINERS+=("$DNS")
DNS_IP=$(ip_of "$DNS")
echo "nameserver $DNS_IP" >"$WORK/resolv.conf"
echo "sbc1 (prio 10) $PEER_IP, sbc2 (prio 20) $PEER2_IP, dns $DNS_IP"

log "seed: two numbers on one trunk, each with its own route"
seed "carrier.e2e" 60
sql "
INSERT INTO trunk_numbers(id, trunk_id, number) VALUES (2, 1, '$N2');
INSERT INTO ring_groups(id, name, ring_timeout, fallback) VALUES (2, 'office', 10, 'hangup');
INSERT INTO ring_group_members VALUES (2, 2);
INSERT INTO did_routes VALUES (2, 2);"
cat >"$WORK/voipd.yaml" <<YAML
trunks:
  grace: 5s
  first_retry: 10s
  max_backoff: 30s
  poll_every: 5s
  dns_check_every: 3s
YAML
export VOIPD_DNS_SERVER="$DNS_IP:53"
start_voipd "$WORK/voipd.yaml"
start_pbx -v "$WORK/resolv.conf:/etc/resolv.conf:ro"

log "scenario: NAPTR -> SRV -> A resolution"
START=$(date +%s)
within "both numbers registered at the priority-10 server" 40 both_at "$PEER"
check "no REGISTER reached the priority-20 server" 0 \
	"$(($(registers_since "$PEER2" "$START" "$N1") + $(registers_since "$PEER2" "$START" "$N2")))"
wait_for "phones registered" 30 phone_up kitchen
wait_for "phones registered" 30 phone_up office

log "scenario: each number routes to its own ring group"
p1 "database put test talk 2"
p1 "database put test phone10 answer:1"
p1 "database put test phone11 ring"
p1 "database put test did $N1"
p1 "channel originate Local/call@carrier-out extension s@carrier-talk"
n=1
wait_for "call to $N1 finished" 30 calls_done $n
check "call to $N1: callee" "$N1" "$(last_call callee)"
check "call to $N1: answered by 10 (group home)" 10 "$(last_call answered_by)"

p1 "database put test phone11 answer:1"
p1 "database put test did $N2"
p1 "channel originate Local/call@carrier-out extension s@carrier-talk"
n=$((n + 1))
wait_for "call to $N2 finished" 30 calls_done $n
check "call to $N2: callee" "$N2" "$(last_call callee)"
check "call to $N2: answered by 11 (group office)" 11 "$(last_call answered_by)"
check "call to $N2: only phone 11 rang" "office" \
	"$(sql "SELECT group_concat(endpoint) FROM call_legs WHERE call_id='$(last_call id)' AND kind='extension'")"

log "scenario: DNS change during a call must not touch the trunk"
p1 "database put test phone10 answer:1"
p1 "database put test talk 30"
p1 "database put test did $N1"
p1 "channel originate Local/call@carrier-out extension s@carrier-talk"
in_call() { [ "$(sql "SELECT count(*) FROM calls WHERE ended_at IS NULL AND status='answered'")" -ge 1 ]; }
wait_for "call answered" 20 in_call
T=$(now_ms)
zone 20 10 # the carrier now prefers sbc2
sleep 12 # DNS watcher checks every 3s
check "no re-registration while the call is up" 0 \
	"$(sql "SELECT count(*) FROM sip_events WHERE type='trunk.reconnecting' AND at >= $T")"
n=$((n + 1))
wait_for "long call finished" 40 calls_done $n
check "call was not cut short (talk >= 25s)" 1 \
	"$(sql "SELECT (ended_at - answered_at) >= 25000 FROM calls ORDER BY started_at DESC LIMIT 1")"
END=$(sql "SELECT ended_at FROM calls ORDER BY started_at DESC LIMIT 1")
check "no re-registration at any time during the call" 0 \
	"$(sql "SELECT count(*) FROM sip_events WHERE type='trunk.reconnecting' AND at >= $T AND at < $END")"
dns_kick() { [ "$(sql "SELECT count(*) FROM sip_events WHERE type='trunk.reconnecting' AND at >= $END AND at < $END + 15000 AND json_extract(detail,'$.reason') LIKE '%dns_changed%'")" -ge 2 ]; }
within "deferred re-registration runs right after the call (both numbers)" 15 dns_kick
S2=$((END / 1000 - 1))
within "both numbers now register at the new preferred server" 20 both_register_at_since "$PEER2" "$S2"
wait_for "registrations healthy" 20 pbx_registered

log "scenario: preferred server dies -> failover to the other SRV target"
S3=$(date +%s)
docker pause "$PEER2" >/dev/null
within "both numbers fail over to sbc1" 150 both_register_at_since "$PEER" "$S3"
within "registrations healthy again (served by sbc1)" 60 pbx_registered
docker unpause "$PEER2" >/dev/null

log "event log"
sql "SELECT strftime('%H:%M:%S', at/1000, 'unixepoch', 'localtime') || '  ' || printf('%-19s', type) || ' ' || subject || ' ' || detail FROM sip_events ORDER BY at" | cut -c1-170

summary
