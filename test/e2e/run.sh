#!/usr/bin/env bash
# End-to-end call tests: the real Asterisk image + voipd, with a second
# Asterisk ("peer") playing the carrier and two phones (extensions 10, 11).
#
#   test/e2e/run.sh            run all scenarios
#   KEEP=1 test/e2e/run.sh     keep containers and work dir for debugging
#
# Requires: docker, go, sqlite3. Uses host networking for the PBX, so ports
# 5060/5061/8088/5038 on this machine must be free.
source "$(dirname "$0")/lib.sh"

calls_done() { [ "$(sql "SELECT count(*) FROM calls WHERE ended_at IS NOT NULL")" -ge "$1" ]; }
last_call() { sql "SELECT $1 FROM calls ORDER BY started_at DESC LIMIT 1"; }

build
log "start peer (carrier + phones)"
start_peer "$PEER"
PEER_IP=$(ip_of "$PEER")
echo "peer at $PEER_IP"

log "seed database"
seed "$PEER_IP:5060"

log "start pbx + voipd"
start_voipd
start_pbx

wait_for "trunk registered at carrier" 40 registered
wait_for "phone 10 registered" 40 phone_up kitchen
wait_for "phone 11 registered" 40 phone_up office
registered && ok "trunk registered at carrier"
check "web API up" ok "$(curl -s http://127.0.0.1:$HTTP_PORT/healthz)"
check "API rejects requests without a session" 401 "$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:$HTTP_PORT/api/v1/calls)"
phone_up kitchen && ok "phone 10 registered"
phone_up office && ok "phone 11 registered"

n=0 # completed calls so far

log "scenario: inbound call, both phones ring, 11 answers"
peer "database put test phone10 ring" >/dev/null
peer "database put test phone11 answer:2" >/dev/null
peer "database put test talk 4" >/dev/null
peer "database put test callerid 01701234567" >/dev/null
peer "channel originate Local/call@carrier-out extension s@carrier-talk" >/dev/null
n=$((n + 1))
wait_for "inbound call finished" 30 calls_done $n
check "status" answered "$(last_call status)"
check "direction" inbound "$(last_call direction)"
check "answered by" 11 "$(last_call answered_by)"
check "caller normalized" +491701234567 "$(last_call caller)"
check "callee is the DID" +492011111111 "$(last_call callee)"
check "phone 11 saw national caller id" 01701234567 "$(peer 'database get test phone11_callerid' | sed -n 's/^Value: //p')"
CALL=$(last_call id)
check "phone 10 leg released, never answered" "1" \
	"$(sql "SELECT count(*) FROM call_legs WHERE call_id='$CALL' AND endpoint='kitchen' AND answered_at IS NULL AND ended_at IS NOT NULL")"
check "both talking legs have a codec" "2" \
	"$(sql "SELECT count(*) FROM call_legs WHERE call_id='$CALL' AND answered_at IS NOT NULL AND codec <> ''")"
check "rtp received on both talking legs" "2" \
	"$(sql "SELECT count(*) FROM call_legs WHERE call_id='$CALL' AND rx_packets > 50 AND mos IS NOT NULL")"
sql "SELECT '    leg ' || endpoint || ': codec=' || codec || ' rx=' || ifnull(rx_packets,'-') || ' lost=' || ifnull(rx_lost,'-') || ' jitter_ms=' || ifnull(round(jitter_ms,2),'-') || ' mos=' || ifnull(mos,'-') || ' cause=' || hangup_cause FROM call_legs WHERE call_id='$CALL'"

log "scenario: inbound call nobody answers (ring timeout 4s)"
peer "database put test phone10 ring" >/dev/null
peer "database put test phone11 ring" >/dev/null
peer "channel originate Local/call@carrier-out application Wait 15" >/dev/null
n=$((n + 1))
wait_for "unanswered call finished" 30 calls_done $n
check "status" missed "$(last_call status)"
check "both phones rang and were released" "2" \
	"$(sql "SELECT count(*) FROM call_legs WHERE call_id='$(last_call id)' AND kind='extension' AND ended_at IS NOT NULL AND hangup_cause NOT LIKE 'originate failed%'")"
check "ring time ~ timeout" 4 \
	"$(sql "SELECT CAST(round((ended_at - started_at) / 1000.0) AS INT) FROM calls WHERE id='$(last_call id)'")"

log "scenario: caller hangs up while ringing"
peer "channel originate Local/call@carrier-out application Wait 15" >/dev/null
ringing() { [ "$(sql "SELECT count(*) FROM call_legs l JOIN calls c ON c.id=l.call_id WHERE c.ended_at IS NULL AND l.kind='extension'")" -ge 2 ]; }
wait_for "phones ringing" 15 ringing
peer "channel request hangup all" >/dev/null
n=$((n + 1))
wait_for "cancelled call finished" 30 calls_done $n
check "status" cancelled "$(last_call status)"

log "scenario: phone 10 calls out (national number -> E.164 on trunk)"
peer "database put test outbound_talk 3" >/dev/null
peer "channel originate Local/0301234567@phone-out application Wait 15" >/dev/null
n=$((n + 1))
wait_for "outbound call finished" 30 calls_done $n
check "status" answered "$(last_call status)"
check "direction" outbound "$(last_call direction)"
check "carrier received E.164" +49301234567 "$(peer 'database get test last_outbound' | sed -n 's/^Value: //p')"
check "trunk leg has codec" "1" \
	"$(sql "SELECT count(*) FROM call_legs WHERE call_id='$(last_call id)' AND kind='trunk' AND codec <> ''")"

log "scenario: internal call 10 -> 11, 11 is busy"
peer "database put test phone11 busy" >/dev/null
peer "channel originate Local/11@phone-out application Wait 15" >/dev/null
n=$((n + 1))
wait_for "internal call finished" 30 calls_done $n
check "status" busy "$(last_call status)"
check "direction" internal "$(last_call direction)"

log "scenario: internal call 10 -> 11 answered"
peer "database put test phone11 answer:1" >/dev/null
peer "database put test talk 2" >/dev/null
peer "channel originate Local/11@phone-out application Wait 15" >/dev/null
n=$((n + 1))
wait_for "internal call finished" 30 calls_done $n
check "status" answered "$(last_call status)"
check "answered by" 11 "$(last_call answered_by)"

summary
