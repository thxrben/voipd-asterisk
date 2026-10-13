#!/bin/sh
# Render the static Asterisk config templates with secrets from the
# environment. Asterisk config files cannot read env vars themselves.
set -eu

: "${VOIPD_ARI_PASSWORD:?VOIPD_ARI_PASSWORD must be set}"
: "${VOIPD_AMI_PASSWORD:?VOIPD_AMI_PASSWORD must be set}"
export VOIPD_ARI_USER="${VOIPD_ARI_USER:-voipd}"
export VOIPD_AMI_USER="${VOIPD_AMI_USER:-voipd}"
export VOIPD_ARI_APP="${VOIPD_ARI_APP:-voipd}"

vars='$VOIPD_ARI_USER $VOIPD_ARI_PASSWORD $VOIPD_AMI_USER $VOIPD_AMI_PASSWORD $VOIPD_ARI_APP'
for f in /usr/share/voipd/asterisk-conf/*.conf; do
    envsubst "$vars" < "$f" > "/etc/asterisk/$(basename "$f")"
done
chmod 640 /etc/asterisk/ari.conf /etc/asterisk/manager.conf

# PJSIP and Asterisk look up the machine's own hostname on hot paths. With
# host networking that is the host's name, which the container often cannot
# resolve locally (e.g. hosts relying on nss-myhostname). Every lookup then
# waits for upstream DNS timeouts, and a slow DNS server stalls SIP
# processing entirely. Make it resolve locally.
h=$(hostname)
if ! grep -qw -- "$h" /etc/hosts 2>/dev/null; then
    echo "127.0.1.1 $h" >> /etc/hosts || echo "WARNING: could not add $h to /etc/hosts"
fi

# voipd writes generated config into /etc/asterisk/voipd (a volume shared
# with the voipd container). voipd owns it; Asterisk only reads it.
VOIPD_UID="${VOIPD_UID:-65532}"
mkdir -p /etc/asterisk/voipd
chown "$VOIPD_UID":asterisk /etc/asterisk/voipd
chmod 2775 /etc/asterisk/voipd
find /etc/asterisk -maxdepth 1 -type f -exec chown asterisk:asterisk {} +

# PJSIP enables DNS resolution per transport type once, at startup, so the
# transports voipd generates must exist before Asterisk starts. voipd writes
# its config immediately on start; wait for it (bounded, so a missing voipd
# never blocks Asterisk forever).
wait=${VOIPD_CONFIG_WAIT:-60}
if [ ! -s /etc/asterisk/voipd/pjsip_voipd.conf ]; then
    echo "waiting up to ${wait}s for voipd to write pjsip_voipd.conf ..."
fi
while [ ! -s /etc/asterisk/voipd/pjsip_voipd.conf ] && [ "$wait" -gt 0 ]; do
    sleep 1
    wait=$((wait - 1))
done
[ -s /etc/asterisk/voipd/pjsip_voipd.conf ] || echo "WARNING: starting without voipd config; restart Asterisk once voipd runs"
chown -R asterisk:asterisk /var/lib/asterisk /var/spool/asterisk /var/log/asterisk /var/run/asterisk

exec "$@"
