#!/usr/bin/env bash
# End-to-end integration tests: real calls through Asterisk + voipd, with
# Mosquitto (MQTT), mailpit (SMTP) and a webhook receiver.
#
#   test/e2e/integrations.sh        (~1.5 min)
#
# Requires: docker, go, sqlite3, curl. Ports as for run.sh, plus 18883
# (MQTT), 11025/18025 (mailpit SMTP/API) and 18096 (webhooks).
source "$(dirname "$0")/lib.sh"

MQTT_IMAGE=${MQTT_IMAGE:-eclipse-mosquitto:2.0.20}
MAIL_IMAGE=${MAIL_IMAGE:-axllent/mailpit:v1.21.8}
MQ=voipd-e2e-mosquitto
MAIL=voipd-e2e-mailpit
HOOK_PORT=${HOOK_PORT:-18096}
export VOIPD_VOICEMAIL_DIR=$WORK/vm
mkdir -p "$VOIPD_VOICEMAIL_DIR"

calls_done() { [ "$(sql "SELECT count(*) FROM calls WHERE ended_at IS NOT NULL")" -ge "$1" ]; }
last_call() { sql "SELECT $1 FROM calls ORDER BY started_at DESC LIMIT 1"; }
mq_retained() { docker exec "$MQ" mosquitto_sub -t "$1" -C 1 -W 2 2>/dev/null; }
mq_is() { [ "$(mq_retained "$1")" = "$2" ]; }
mq_pub() { docker exec "$MQ" mosquitto_pub -t "$1" -m "$2"; }
mails_to() { curl -s "http://127.0.0.1:18025/api/v1/search?query=to:$1" | sed -n 's/.*"messages_count":\([0-9]*\).*/\1/p'; }
has_mail() { [ "$(mails_to "$1")" -ge 1 ]; }
hook_seen() { grep -q "\"event\":\"$1\"" "$WORK/hooks.jsonl" 2>/dev/null; }
mqtt_seen() { grep -q "^$1 " "$WORK/mqtt.log"; }

build
(cd "$ROOT" && go build -o "$WORK/webhookfake" ./test/webhookfake)

log "start broker, mail server, webhook receiver"
docker rm -f "$MQ" "$MAIL" >/dev/null 2>&1 || true
docker run -d --name "$MQ" -p 127.0.0.1:18883:1883 "$MQTT_IMAGE" mosquitto -c /mosquitto-no-auth.conf >/dev/null
CONTAINERS+=("$MQ")
docker run -d --name "$MAIL" -p 127.0.0.1:11025:1025 -p 127.0.0.1:18025:8025 "$MAIL_IMAGE" >/dev/null
CONTAINERS+=("$MAIL")
: >"$WORK/secret"
"$WORK/webhookfake" -listen "127.0.0.1:$HOOK_PORT" -secret-file "$WORK/secret" -log "$WORK/hooks.jsonl" >"$WORK/hook.log" 2>&1 &
BG_PIDS+=($!)
mail_up() { curl -sf http://127.0.0.1:18025/api/v1/info; }
wait_for "mailpit up" 30 mail_up
mq_up() { docker exec "$MQ" mosquitto_pub -t ping -m 1; }
wait_for "mosquitto up" 30 mq_up
docker exec "$MQ" mosquitto_sub -v -t 'voipd/#' >"$WORK/mqtt.log" 2>&1 &
BG_PIDS+=($!)

log "start peer, voipd and pbx"
start_peer "$PEER"
seed "$(ip_of "$PEER"):5060"
sql "INSERT INTO users(subject, username, name, email, role, created_at, last_login)
       VALUES ('oidc:alice', 'alice', 'Alice', 'alice@example.org', 'user', 0, 0);
     UPDATE extensions SET owner_subject = 'oidc:alice' WHERE id = 1;"
export VOIPD_ADMIN_PASSWORD_HASH=$(echo "e2e-admin-password" | "$WORK/voipd" hash-password 2>/dev/null)
export VOIPD_PUBLIC_URL=http://pbx.example.org
start_voipd
start_pbx -v "$VOIPD_VOICEMAIL_DIR:/var/spool/asterisk/recording"
wait_for "trunk registered at carrier" 40 registered
wait_for "phone 10 registered" 40 phone_up kitchen
wait_for "phone 11 registered" 40 phone_up office

JAR=$WORK/cookies
API=http://127.0.0.1:$HTTP_PORT
curl -s -c "$JAR" -o /dev/null -H 'Content-Type: application/json' \
	-d '{"username":"admin","password":"e2e-admin-password"}' "$API/auth/local"
CSRF=$(curl -s -b "$JAR" "$API/api/v1/me" | sed -n 's/.*"csrf_token":"\([^"]*\)".*/\1/p')
api() { curl -s -b "$JAR" -X "$1" -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' ${3:+-d "$3"} "$API/api/v1$2"; }

log "configure integrations through the API"
api PUT /modules/mqtt '{"enabled":true,"config":{"url":"mqtt://127.0.0.1:18883"}}' >/dev/null
api PUT /modules/notify '{"enabled":true,"config":{"host":"127.0.0.1","port":11025,"security":"none","from":"voipd <pbx@example.org>","missed_calls":true,"trunk_alerts":true}}' >/dev/null
created=$(api POST /webhooks '{"name":"receiver","url":"http://127.0.0.1:'"$HOOK_PORT"'/hook","events":["call.missed","call.blocked"],"enabled":true}')
sed -n 's/.*"secret":"\([^"]*\)".*/\1/p' <<<"$created" >"$WORK/secret"
check "webhook secret shown" 1 "$(grep -c '^whsec_' "$WORK/secret")"
api PUT /modules/webhook '{"enabled":true,"config":{}}' >/dev/null
module_ok() { api GET /status | grep -q "\"$1\":{\"state\":\"ok\""; }
wait_for "mqtt module connected" 20 module_ok mqtt
wait_for "voipd online on MQTT" 20 mq_is voipd/status online
check "line state on MQTT" up "$(mq_retained voipd/line/492011111111/state)"
check "phone 10 registered on MQTT" ON "$(mq_retained voipd/extension/10/registered)"
check "Home Assistant discovery published" 1 \
	"$(mq_retained homeassistant/switch/voipd/anonymous_block/config | grep -c '"command_topic":"voipd/blocking/anonymous/set"')"

n=0
log "scenario: missed call -> MQTT event, signed webhook, e-mail to the phone's owner"
peer "database put test phone10 ring" >/dev/null
peer "database put test phone11 ring" >/dev/null
peer "database put test callerid 01701234567" >/dev/null
peer "channel originate Local/call@carrier-out application Wait 15" >/dev/null
n=$((n + 1))
wait_for "unanswered call finished" 30 calls_done $n
check "status" missed "$(last_call status)"
wait_for "call.missed on MQTT" 10 mqtt_seen voipd/event/call.missed
mqtt_seen voipd/event/call.missed && ok "call.missed on MQTT"
wait_for "call.missed webhook" 10 hook_seen call.missed
check "webhook signature valid" 1 "$(grep '"event":"call.missed"' "$WORK/hooks.jsonl" | grep -c '"valid":true')"
check "webhook carries the extensions that rang" 1 "$(grep -c '"extensions":\["10","11"\]' "$WORK/hooks.jsonl")"
wait_for "missed-call mail" 15 has_mail alice@example.org
check "mail to the owner of extension 10" 1 "$(mails_to alice@example.org)"
check "mail subject" 1 "$(curl -s "http://127.0.0.1:18025/api/v1/search?query=to:alice@example.org" | grep -c 'Verpasster Anruf von 0170 1234567')"
stats_missed() { mq_retained voipd/stats/today | grep -q '"missed":1'; }
within "stats on MQTT count the missed call" 5 stats_missed

log "scenario: MQTT command switches on anonymous blocking"
mq_pub voipd/blocking/anonymous/set ON
wait_for "anonymous blocking on" 10 mq_is voipd/blocking/anonymous ON
check "setting stored" true "$(sql "SELECT value FROM settings WHERE key='anonymous_block'")"
check "audited as mqtt" 1 "$(sql "SELECT count(*) FROM audit_log WHERE subject='mqtt' AND action='screening.settings'")"
peer "database put test callerid anonymous" >/dev/null
peer "channel originate Local/call@carrier-out application Wait 6" >/dev/null
n=$((n + 1))
sleep 6
peer "channel request hangup all" >/dev/null
wait_for "blocked call finished" 20 calls_done $n
check "anonymous call blocked" "blocked|anonymous" "$(last_call "status || '|' || block_reason")"
wait_for "call.blocked webhook" 10 hook_seen call.blocked
hook_seen call.blocked && ok "call.blocked webhook"

log "scenario: MQTT command hangs up a call in progress"
mq_pub voipd/blocking/anonymous/set OFF
wait_for "anonymous blocking off" 10 mq_is voipd/blocking/anonymous OFF
peer "database put test callerid 01701234567" >/dev/null
peer "database put test phone10 ring" >/dev/null
peer "database put test phone11 answer:1" >/dev/null
peer "database put test talk 40" >/dev/null
peer "channel originate Local/call@carrier-out extension s@carrier-talk" >/dev/null
answered() { [ -n "$(sql "SELECT id FROM calls WHERE ended_at IS NULL AND status='answered'")" ]; }
wait_for "call answered" 20 answered
wait_for "active call on MQTT" 10 mq_is voipd/calls/active 1
CALL=$(sql "SELECT id FROM calls WHERE ended_at IS NULL AND status='answered'")
t0=$(now_ms)
mq_pub "voipd/call/$CALL/hangup" ""
n=$((n + 1))
within "call ended after the command" 10 calls_done $n
check "status" answered "$(sql "SELECT status FROM calls WHERE id='$CALL'")"
check "talked less than the caller wanted" 1 "$(sql "SELECT count(*) FROM calls WHERE id='$CALL' AND ended_at - answered_at < 30000")"
check "hangup audited" 1 "$(sql "SELECT count(*) FROM audit_log WHERE subject='mqtt' AND action='call.hangup' AND target='$CALL'")"
wait_for "no active calls on MQTT" 10 mq_is voipd/calls/active 0

log "scenario: voicemail -> e-mail with the recording, count on MQTT"
sql "UPDATE ring_groups SET fallback = 'voicemail', fallback_extension_id = 1 WHERE id = 1"
peer "database put test phone10 ring" >/dev/null
peer "database put test phone11 ring" >/dev/null
peer "database put test callerid 01701234567" >/dev/null
peer "channel originate Local/call@carrier-out extension s@carrier-talk" >/dev/null
recording() { ls "$VOIPD_VOICEMAIL_DIR"/vm/10/*.wav >/dev/null 2>&1; }
wait_for "voicemail recording started" 30 recording
sleep 3
peer "channel request hangup all" >/dev/null
n=$((n + 1))
wait_for "voicemail call finished" 20 calls_done $n
check "call went to voicemail" voicemail "$(last_call status)"
vm_mail() { curl -s "http://127.0.0.1:18025/api/v1/search?query=subject:Sprachnachricht" | grep -q '"messages_count":1'; }
wait_for "voicemail mail" 15 vm_mail
VM_MAIL=$(curl -s "http://127.0.0.1:18025/api/v1/search?query=subject:Sprachnachricht")
check "voicemail mail to the mailbox owner" 1 "$(grep -c 'alice@example.org' <<<"$VM_MAIL")"
check "recording attached" 1 "$(grep -c '"Attachments":1' <<<"$VM_MAIL")"
wait_for "voicemail count on MQTT" 10 mq_is voipd/extension/10/voicemail 1
check "voicemail count on MQTT" 1 "$(mq_retained voipd/extension/10/voicemail)"

log "scenario: voipd stops -> offline on MQTT"
stop_voipd
wait_for "offline on MQTT" 10 mq_is voipd/status offline
mq_is voipd/status offline && ok "status offline after shutdown"

summary
