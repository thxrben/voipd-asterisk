#!/usr/bin/env bash
# Browser tests of the web UI (Playwright, local Chrome) against voipd with
# demo data and a fake OIDC provider. No Asterisk needed.
#
#   test/e2e/ui.sh
#   PW_CHANNEL=chromium test/e2e/ui.sh   # use Playwright's own browser
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
WORK=$(mktemp -d -t voipd-ui.XXXX)
UI_PORT=${UI_PORT:-18082}
IDP_PORT=${IDP_PORT:-18090}
PIDS=()
cleanup() {
	for p in "${PIDS[@]}"; do kill "$p" 2>/dev/null || true; done
	wait 2>/dev/null || true
	[ -n "${KEEP:-}" ] && echo "KEEP: $WORK" || rm -rf "$WORK"
}
trap cleanup EXIT

echo "=== build"
(cd "$ROOT/web" && npm run build --silent)
(cd "$ROOT" && go build -o "$WORK/voipd" ./cmd/voipd && go build -o "$WORK/oidcfake" ./test/oidcfake)

echo "=== seed demo data"
(cd "$ROOT" && go run ./test/devseed -db "$WORK/voipd.db" -days 30 -owner oidc:alice)

echo "=== start fake IdP and voipd"
"$WORK/oidcfake" -listen "127.0.0.1:$IDP_PORT" -issuer "http://127.0.0.1:$IDP_PORT" >"$WORK/idp.log" 2>&1 &
PIDS+=($!)
VOIPD_ARI_PASSWORD=x VOIPD_AMI_PASSWORD=x VOIPD_DB_PATH="$WORK/voipd.db" VOIPD_GENERATED_DIR="$WORK/gen" \
VOIPD_HTTP_LISTEN="127.0.0.1:$UI_PORT" VOIPD_PUBLIC_URL="http://127.0.0.1:$UI_PORT" VOIPD_AREA_CODE=201 \
VOIPD_OIDC_ISSUER="http://127.0.0.1:$IDP_PORT" VOIPD_OIDC_CLIENT_ID=voipd VOIPD_OIDC_CLIENT_SECRET=secret \
VOIPD_LOG_LEVEL=warn "$WORK/voipd" >"$WORK/voipd.log" 2>&1 &
PIDS+=($!)
for _ in $(seq 60); do
	curl -sf "http://127.0.0.1:$UI_PORT/healthz" >/dev/null && curl -sf "http://127.0.0.1:$IDP_PORT/.well-known/openid-configuration" >/dev/null && break
	sleep 0.25
done

echo "=== playwright"
cd "$ROOT/web"
BASE_URL="http://127.0.0.1:$UI_PORT" IDP_URL="http://127.0.0.1:$IDP_PORT" npx playwright test
