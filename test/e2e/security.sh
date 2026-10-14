#!/usr/bin/env bash
# End-to-end SIP access control: phones only from allowed networks (the
# carrier line is not affected), and a sender with a wrong password is
# banned after repeated failures until unbanned.
#
#   test/e2e/security.sh        (~2 min)
source "$(dirname "$0")/lib.sh"

calls_done() { [ "$(sql "SELECT count(*) FROM calls WHERE ended_at IS NOT NULL")" -ge "$1" ]; }
last_call() { sql "SELECT $1 FROM calls ORDER BY started_at DESC LIMIT 1"; }

build
log "start peer, voipd and pbx"
start_peer "$PEER"
PEER_IP=$(ip_of "$PEER")
seed "$PEER_IP:5060"
export VOIPD_ADMIN_PASSWORD_HASH=$(echo "e2e-admin-password" | "$WORK/voipd" hash-password 2>/dev/null)
start_voipd
start_pbx
wait_for "trunk registered at carrier" 40 registered
wait_for "phone 10 registered" 40 phone_up kitchen

API=http://127.0.0.1:$HTTP_PORT
JAR=$WORK/cookies
curl -s -c "$JAR" -o /dev/null -H 'Content-Type: application/json' \
	-d '{"username":"admin","password":"e2e-admin-password"}' "$API/auth/local"
CSRF=$(curl -s -b "$JAR" "$API/api/v1/me" | sed -n 's/.*"csrf_token":"\([^"]*\)".*/\1/p')
api() { # api METHOD PATH [JSON]
	curl -s -b "$JAR" -X "$1" -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' ${3:+-d "$3"} "$API/api/v1$2"
}
settings() { # settings ACL_ENABLED ALLOWED_NETS_JSON MAX_FAILURES
	api PUT /security/settings "{\"acl_enabled\":$1,\"allowed_nets\":$2,\"ban_enabled\":true,\"max_failures\":$3,\"window_minutes\":10,\"ban_minutes\":60}"
}
peer_reg() { peer "pjsip show registrations" | grep -E "^ *$1/" | grep -oE "Registered|Rejected|Unregistered" | head -1; }
reregister() { peer "pjsip send unregister $1" >/dev/null; sleep 1; peer "pjsip send register $1" >/dev/null; }
phone_registered() { [ "$(peer_reg "$1")" = Registered ]; }
phone_refused() { [ "$(peer_reg "$1")" = Rejected ]; }

log "automatic networks include the docker bridge: phones register"
check "phone ACL on the endpoint" "acl : deny/permit" "$(pbx "pjsip show endpoint kitchen" | grep -oE "^ *acl +: deny/permit" | tr -s ' ' | sed 's/^ //')"
reregister phone10
wait_for "phone 10 registers within the allowed networks" 20 phone_registered phone10

log "only another network allowed: the phone is refused, the line still works"
settings true '["10.99.0.0/16"]' 10 >/dev/null
reregister phone10
wait_for "phone 10 refused" 20 phone_refused phone10
check "refusal reported as an ACL failure" acl \
	"$(api GET /security | sed -n 's/.*"recent":\[{[^}]*"kind":"\([a-z_]*\)".*/\1/p')"
check "trunk endpoint has no ACL" 0 "$(pbx "pjsip show endpoint trunk-carrier-1" | grep -c "deny" || true)"
pbx "pjsip send register trunk-carrier-1" >/dev/null
sleep 2
wait_for "trunk still registered" 20 registered
# The phones' old contacts are still valid: a call from the carrier gets through.
peer "database put test phone10 answer" >/dev/null
peer "database put test phone11 ring" >/dev/null
peer "channel originate Local/call@carrier-out extension s@carrier-talk" >/dev/null
wait_for "inbound call through the line" 30 calls_done 1
check "inbound call answered" answered "$(last_call status)"
settings true '[]' 3 >/dev/null
reregister phone10
wait_for "phone 10 back after allowing it" 20 phone_registered phone10

log "wrong passwords: the sender is banned"
docker exec -i "$PEER" sh -c 'cat > /etc/asterisk/intruder.conf' <<'EOF'
[intruder-auth]
type = auth
auth_type = userpass
username = kitchen
password = guessed-wrong

[intruder]
type = registration
server_uri = sip:pbx:5060
client_uri = sip:kitchen@pbx
contact_user = intruder
outbound_auth = intruder-auth
retry_interval = 1
forbidden_retry_interval = 1
fatal_retry_interval = 1
auth_rejection_permanent = no
max_retries = 1000
expiration = 120
EOF
peer "module reload res_pjsip.so" >/dev/null
banned() { api GET /security | grep -q "\"ip\":\"$PEER_IP/32\""; }
wait_for "sender banned after 3 failures" 30 banned
check "ban in the configuration" 1 "$(grep -c "deny = $PEER_IP/32" "$VOIPD_GENERATED_DIR/pjsip_voipd.conf")"
check "ban recorded as a SIP event" 1 "$(sql "SELECT count(*) FROM sip_events WHERE type = 'security.banned' AND subject = '$PEER_IP/32'")"
docker exec "$PEER" rm /etc/asterisk/intruder.conf
peer "module reload res_pjsip.so" >/dev/null
reregister phone10
wait_for "banned sender cannot register a phone" 20 phone_refused phone10
check "Asterisk refuses by the ban list" 1 "$([ "$(docker logs "$PBX" 2>&1 | grep -ac "message from $PEER_IP:5060 did not pass ACL test")" -gt 0 ] && echo 1)"
check "unban" "" "$(api DELETE "/security/bans?ip=$PEER_IP/32")"
check "ban gone from the configuration" 0 "$(grep -c "voipd-bans" "$VOIPD_GENERATED_DIR/pjsip_voipd.conf" || true)"
reregister phone10
wait_for "phone 10 registers again after the unban" 20 phone_registered phone10

summary
