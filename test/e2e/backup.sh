#!/usr/bin/env bash
# End-to-end backups: scheduled-style backups with rotation, configuration
# export → edit → import reaching Asterisk, an encrypted export restoring a
# deleted phone with its password, and a full restore through a restart.
#
#   test/e2e/backup.sh        (~1.5 min)
source "$(dirname "$0")/lib.sh"

export VOIPD_VOICEMAIL_DIR=$WORK/vm VOIPD_BACKUP_DIR=$WORK/backups
mkdir -p "$VOIPD_VOICEMAIL_DIR"

build
log "start peer, voipd and pbx"
start_peer "$PEER"
seed "$(ip_of "$PEER"):5060"
export VOIPD_ADMIN_PASSWORD_HASH=$(echo "e2e-admin-password" | "$WORK/voipd" hash-password 2>/dev/null)
start_voipd
start_pbx -v "$VOIPD_VOICEMAIL_DIR:/var/spool/asterisk/recording"
wait_for "trunk registered at carrier" 40 registered
wait_for "phone 10 registered" 40 phone_up kitchen
wait_for "phone 11 registered" 40 phone_up office

API=http://127.0.0.1:$HTTP_PORT
JAR=$WORK/cookies
login() {
	curl -s -c "$JAR" -o /dev/null -H 'Content-Type: application/json' \
		-d '{"username":"admin","password":"e2e-admin-password"}' "$API/auth/local"
	CSRF=$(curl -s -b "$JAR" "$API/api/v1/me" | sed -n 's/.*"csrf_token":"\([^"]*\)".*/\1/p')
}
api() { # api METHOD PATH [JSON]
	curl -s -b "$JAR" -X "$1" -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' ${3:+-d "$3"} "$API/api/v1$2"
}
upload() { # upload PATH FILE [extra curl -F args...]
	local path=$1 file=$2
	shift 2
	curl -s -b "$JAR" -H "X-CSRF-Token: $CSRF" "$@" -F "file=@$file" "$API/api/v1$path"
}
json() { sed -n "s/.*\"$1\":\"\([^\"]*\)\".*/\1/p"; }
login

log "a voicemail on disk and in the database"
mkdir -p "$VOIPD_VOICEMAIL_DIR/vm/10"
printf 'RIFF-the-message' >"$VOIPD_VOICEMAIL_DIR/vm/10/vm-e2e.wav"
sql "INSERT INTO voicemails(id, extension_id, caller, received_at, duration_ms) VALUES('vm-e2e', 1, '+491701234567', $(now_ms), 4000)"

log "backups with rotation"
check "backup without a passphrase is refused" passphrase \
	"$(api POST /backup/run | sed -n 's/.*"fields":{"\([a-z]*\)".*/\1/p')"
api PUT /backup/settings '{"enabled":true,"time":"03:30","keep":2,"passphrase":"e2e backup passphrase"}' >/dev/null
for i in 1 2 3; do
	api POST /backup/run >/dev/null
	sleep 1.1 # file names have second resolution
done
check "only the newest two kept" 2 "$(ls "$VOIPD_BACKUP_DIR" | grep -c '^voipd-backup-.*\.tar\.gz\.age$')"
check "no temporary files left" 0 "$(ls -A "$VOIPD_BACKUP_DIR" | grep -vc '^voipd-backup-' || true)"
check "backup is age-encrypted" "age-encryption.org/v1" "$(head -c 21 "$(ls -d "$VOIPD_BACKUP_DIR"/* | tail -1)")"
BACKUP=$(ls "$VOIPD_BACKUP_DIR" | tail -1)

log "configuration export, edit, preview, import"
curl -s -b "$JAR" "$API/api/v1/backup/config" -o "$WORK/config.yaml"
check "plain export has no passwords" 0 "$(grep -c 'office-pw\|secrets:' "$WORK/config.yaml" || true)"
sed -i 's/name: Office/name: Büro/' "$WORK/config.yaml"
PLAN=$(upload /backup/config/plan "$WORK/config.yaml")
check "preview lists the change" 1 "$(echo "$PLAN" | grep -c '"key":"11","fields":\["name"\]')"
TOKEN=$(echo "$PLAN" | json token)
upload /backup/config/apply "$WORK/config.yaml" -F "token=$TOKEN" >/dev/null
check "new name in the database" "Büro" "$(sql "SELECT name FROM extensions WHERE number = '11'")"
check "new name reached Asterisk" 1 "$(pbx "pjsip show endpoint office" | grep -c 'callerid.*Büro')"
check "password kept" office-pw "$(sql "SELECT sip_password FROM extensions WHERE number = '11'")"

log "encrypted export brings back a deleted phone with its password"
curl -s -b "$JAR" "$API/api/v1/backup/config?secrets=1" -o "$WORK/config.yaml.age"
check "export with passwords is encrypted" "age-encryption.org/v1" "$(head -c 21 "$WORK/config.yaml.age")"
api DELETE /extensions/2 >/dev/null
endpoint_gone() { ! pbx "pjsip show endpoints" | grep -q "Endpoint:  office/"; }
wait_for "phone 11 removed from Asterisk" 10 endpoint_gone
check "wrong passphrase refused" "wrong passphrase" \
	"$(upload /backup/config/plan "$WORK/config.yaml.age" -F "passphrase=nope nope nope" | json passphrase)"
PLAN=$(upload /backup/config/plan "$WORK/config.yaml.age" -F "passphrase=e2e backup passphrase")
TOKEN=$(echo "$PLAN" | json token)
upload /backup/config/apply "$WORK/config.yaml.age" -F "passphrase=e2e backup passphrase" -F "token=$TOKEN" >/dev/null
check "phone 11 back with its password" office-pw "$(sql "SELECT sip_password FROM extensions WHERE number = '11'")"
wait_for "phone 11 registers again" 60 phone_up office

log "full restore through a restart"
api POST /extensions '{"id":0,"number":"12","name":"Garage","enabled":true,"sip_username":"garage","password":"","transport":"udp","srtp":false,"codecs":["g722","alaw"],"max_contacts":2,"outbound_number_id":0,"clir":false,"dnd":false,"owner_subject":""}' >/dev/null
check "extension 12 added after the backup" 1 "$(sql "SELECT count(*) FROM extensions WHERE number = '12'")"
rm "$VOIPD_VOICEMAIL_DIR/vm/10/vm-e2e.wav"
sql "DELETE FROM voicemails"
check "restore with a wrong passphrase refused" "wrong passphrase" \
	"$(api POST "/backup/files/$BACKUP/restore" '{"passphrase":"nope nope nope"}' | json passphrase)"
api POST "/backup/files/$BACKUP/restore" '{"passphrase":"e2e backup passphrase"}' >/dev/null
voipd_gone() { ! kill -0 "$VOIPD_PID" 2>/dev/null; }
within "voipd exits for the restart" 10 voipd_gone
wait "$VOIPD_PID" 2>/dev/null || true
VOIPD_PID=
start_voipd # the container's restart policy does this in production
api_up() { [ "$(curl -s "$API/healthz")" = ok ]; }
wait_for "voipd back" 20 api_up
check "restore logged" 1 "$(grep -c 'backup restored' "$WORK/voipd.log")"
check "extension added after the backup is gone" 0 "$(sql "SELECT count(*) FROM extensions WHERE number = '12'")"
check "name from the time of the backup" Office "$(sql "SELECT name FROM extensions WHERE number = '11'")"
check "voicemail row restored" 1 "$(sql "SELECT count(*) FROM voicemails WHERE id = 'vm-e2e'")"
check "voicemail audio restored" RIFF-the-message "$(cat "$VOIPD_VOICEMAIL_DIR/vm/10/vm-e2e.wav" 2>/dev/null)"
check "replaced database kept aside" 1 "$(ls "$WORK/pre-restore/voipd.db" 2>/dev/null | wc -l)"
wait_for "phone 10 still registered" 60 phone_up kitchen
wait_for "trunk registered after the restore" 60 registered
login
check "restored settings: schedule on" true "$(api GET /backup | sed -n 's/.*"enabled":\([a-z]*\).*/\1/p')"

summary
