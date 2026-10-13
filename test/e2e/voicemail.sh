#!/usr/bin/env bash
# End-to-end voicemail: German prompts in the image, a caller leaves a
# message after nobody answers, the message waiting light goes on, the phone
# listens with *98 and deletes it, the light goes off.
#
#   test/e2e/voicemail.sh        (~1.5 min)
source "$(dirname "$0")/lib.sh"

calls_done() { [ "$(sql "SELECT count(*) FROM calls WHERE ended_at IS NOT NULL")" -ge "$1" ]; }
last_call() { sql "SELECT $1 FROM calls ORDER BY started_at DESC LIMIT 1"; }
export VOIPD_VOICEMAIL_DIR=$WORK/vm
mkdir -p "$VOIPD_VOICEMAIL_DIR"

build
log "image ships the German prompts"
check "prompts in the image" 139 "$(docker run --rm --entrypoint sh "$IMAGE" -c 'ls /var/lib/asterisk/sounds/voipd | wc -l')"

log "start peer, voipd and pbx (recordings shared with voipd)"
start_peer "$PEER"
seed "$(ip_of "$PEER"):5060"
sql "UPDATE ring_groups SET fallback = 'voicemail', fallback_extension_id = 1 WHERE id = 1"
start_voipd
start_pbx -v "$VOIPD_VOICEMAIL_DIR:/var/spool/asterisk/recording"
wait_for "trunk registered at carrier" 40 registered
wait_for "phone 10 registered" 40 phone_up kitchen
wait_for "phone 11 registered" 40 phone_up office
peer "pjsip set logger on" >/dev/null
mwi() { docker logs "$PEER" 2>&1 | grep -ac "Messages-Waiting: $1" || true; }
mwi_seen() { [ "$(mwi "$1")" -gt "$2" ]; }
check "mailbox on the phone's AOR and endpoint" 2 \
	"$( (pbx "pjsip show aor kitchen"; pbx "pjsip show endpoint kitchen") | grep -c 'mailboxes.*10@voipd')"

n=0
log "scenario: nobody answers, the caller leaves a message"
peer "database put test phone10 ring" >/dev/null
peer "database put test phone11 ring" >/dev/null
peer "database put test callerid 01701234567" >/dev/null
yes_before=$(mwi yes)
peer "channel originate Local/call@carrier-out extension s@carrier-talk" >/dev/null
recording() { ls "$VOIPD_VOICEMAIL_DIR"/vm/10/*.wav >/dev/null 2>&1; }
wait_for "recording started" 30 recording
sleep 4 # the caller "speaks" (a tone)
peer "channel request hangup all" >/dev/null
n=$((n + 1))
wait_for "call finished" 20 calls_done $n
check "call status" voicemail "$(last_call status)"
check "message stored" 1 "$(sql "SELECT count(*) FROM voicemails WHERE caller = '+491701234567' AND NOT heard")"
dur=$(sql "SELECT duration_ms FROM voicemails LIMIT 1")
check "message is about 4 s long" 1 "$([ "$dur" -ge 2500 ] && [ "$dur" -le 7000 ] && echo 1 || echo "0 ($dur ms)")"
VM=$(sql "SELECT id FROM voicemails LIMIT 1")
check "audio file in the shared directory" 1 "$(ls "$VOIPD_VOICEMAIL_DIR/vm/10/$VM.wav" 2>/dev/null | wc -l)"
check "file readable and deletable by voipd" 1 "$([ -r "$VOIPD_VOICEMAIL_DIR/vm/10/$VM.wav" ] && [ -w "$VOIPD_VOICEMAIL_DIR/vm/10" ] && echo 1)"
wait_for "message waiting light on" 15 mwi_seen yes "$yes_before"
mwi_seen yes "$yes_before" && ok "message waiting light on (NOTIFY Messages-Waiting: yes)"

log "scenario: phone 10 dials *98, listens and deletes the message"
no_before=$(mwi no)
# 1 = listen (interrupts the menu), 7 = delete (interrupts the message).
peer "database put test dtmf wwwwwwww1wwwwww7" >/dev/null
peer "channel originate Local/*98@phone-out application Wait 40" >/dev/null
deleted() { [ "$(sql "SELECT count(*) FROM voicemails")" = 0 ]; }
within "message deleted through the phone menu" 30 deleted
check "audio file removed" 0 "$(ls "$VOIPD_VOICEMAIL_DIR"/vm/10/*.wav 2>/dev/null | wc -l)"
wait_for "message waiting light off" 15 mwi_seen no "$no_before"
mwi_seen no "$no_before" && ok "message waiting light off (NOTIFY Messages-Waiting: no)"
peer "channel request hangup all" >/dev/null
n=$((n + 1))
wait_for "menu call finished" 20 calls_done $n
check "menu call recorded as *98" "*98|answered" "$(last_call "callee || '|' || status")"
check "prompts played without errors" 0 "$(docker logs "$PBX" 2>&1 | grep -ci 'unable to open\|file does not exist\|no such file' || true)"

summary
