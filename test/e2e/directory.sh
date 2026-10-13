#!/usr/bin/env bash
# Phone directory end to end: the voipd image with the compose security
# settings (root with only NET_BIND_SERVICE/SETUID/SETGID, then dropping to
# an unprivileged user) serves LDAP on port 389 and CardDAV under /dav/.
#
#   test/e2e/directory.sh       (builds voipd:dev; ~3 min the first time)
source "$(dirname "$0")/lib.sh"

C=voipd-e2e-directory
DATA=$WORK/data
mkdir -p "$DATA"

log "build"
(cd "$ROOT" && go build -o "$WORK/voipd" ./cmd/voipd && go build -o "$WORK/ldapcheck" ./test/ldapcheck)
docker build -q -t voipd:dev "$ROOT" >/dev/null
(cd "$ROOT" && go run ./test/devseed -db "$DATA/voipd.db" -days 1 >/dev/null)

log "start the voipd image like docker compose does"
HASH=$(echo "e2e-admin-password" | "$WORK/voipd" hash-password 2>/dev/null)
docker rm -f "$C" >/dev/null 2>&1 || true
docker run -d --name "$C" --network host --user 0:0 \
	--cap-drop ALL --cap-add NET_BIND_SERVICE --cap-add SETUID --cap-add SETGID \
	--security-opt no-new-privileges:true \
	-e VOIPD_ARI_PASSWORD=x -e VOIPD_AMI_PASSWORD=x -e VOIPD_AREA_CODE=201 \
	-e VOIPD_DB_PATH=/data/voipd.db -e VOIPD_GENERATED_DIR=/data/gen -e VOIPD_VOICEMAIL_DIR=/data/vm \
	-e VOIPD_HTTP_LISTEN=127.0.0.1:$HTTP_PORT -e VOIPD_RUN_AS="$(id -u):$(id -g)" \
	-e VOIPD_ADMIN_PASSWORD_HASH="$HASH" -v "$DATA:/data" voipd:dev >/dev/null
CONTAINERS+=("$C")
up() { curl -sf "http://127.0.0.1:$HTTP_PORT/healthz" >/dev/null; }
wait_for "voipd up" 30 up
ldap_up() { ss -ltn | grep -q ':389 '; }
wait_for "LDAP on port 389" 10 ldap_up
ldap_up && ok "LDAP listening on port 389"
PID=$(docker inspect -f '{{.State.Pid}}' "$C")
check "voipd runs unprivileged after binding" "$(id -u) $(id -u)" "$(awk '/^Uid:/ {print $2, $3}' /proc/$PID/status)"
check "no capabilities left" 0000000000000000 "$(awk '/^CapEff:/ {print $2}' /proc/$PID/status)"
check "dropped privileges logged" 1 "$(docker logs "$C" 2>&1 | grep -c 'running unprivileged')"

log "directory account through the API"
JAR=$WORK/cookies
API=http://127.0.0.1:$HTTP_PORT
curl -s -c "$JAR" -o /dev/null -H 'Content-Type: application/json' \
	-d '{"username":"admin","password":"e2e-admin-password"}' "$API/auth/local"
CSRF=$(curl -s -b "$JAR" "$API/api/v1/me" | sed -n 's/.*"csrf_token":"\([^"]*\)".*/\1/p')
created=$(curl -s -b "$JAR" -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
	-d '{"username":"phones"}' "$API/api/v1/directory/accounts")
PW=$(sed -n 's/.*"password":"\([^"]*\)".*/\1/p' <<<"$created")
check "account created" 1 "$([ -n "$PW" ] && echo 1)"
check "status shows LDAP listening" 1 "$(curl -s -b "$JAR" "$API/api/v1/directory" | grep -c '"listening":true')"

lq() { "$WORK/ldapcheck" -addr 127.0.0.1:389 -user "$1" -pass "$2" -filter "$3" 2>&1 || true; }
log "LDAP searches like the phones do"
check "Yealink name search" "Mom	01701234567" "$(lq uid=phones,dc=voipd "$PW" '(|(cn=Mom*)(sn=Mom*))')"
check "reverse lookup of an incoming call" "Mom" \
	"$(lq phones "$PW" '(|(telephoneNumber=01701234567)(mobile=01701234567)(homePhone=01701234567))' | cut -f1)"
check "Gigaset filter" "Jonas" "$(lq phones "$PW" '(&(objectClass=person)(|(cn=Jon*)(sn=Jon*)))' | cut -f1)"
check "extensions are in the book" "Kitchen	10" "$(lq phones "$PW" '(telephoneNumber=10)')"
check "anonymous search refused" 1 "$("$WORK/ldapcheck" -addr 127.0.0.1:389 -filter '(cn=*)' | grep -c 'Code 50')"
check "wrong password refused" 1 "$(lq phones wrong '(cn=*)' | grep -c 'Code 49')"

log "CardDAV"
report() {
	curl -s -u "phones:$1" -X REPORT -H 'Depth: 1' -H 'Content-Type: application/xml' \
		--data '<C:addressbook-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav"><D:prop><C:address-data/></D:prop></C:addressbook-query>' \
		-o "$WORK/report.xml" -w '%{http_code}' "$API/dav/phones/books/contacts/"
}
check "addressbook-query answered" 207 "$(report "$PW")"
check "contacts as vCards" 1 "$(grep -c 'FN:Mom' "$WORK/report.xml")"
check "no access without login" 401 "$(curl -s -o /dev/null -w '%{http_code}' -X PROPFIND "$API/dav/phones/")"

summary
