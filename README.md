# voipd

A self-hosted home PBX for a Raspberry Pi 4 (arm64; x86-64 works too).
Asterisk does SIP and media; **voipd** (Go) owns all logic: routing,
screening, call history, trunk supervision and integrations.

```
phones / FritzBox ──SIP──▶ Asterisk (PJSIP) ◀──SIP──▶ carrier (e.g. Telekom)
                              ▲  ARI + AMI (127.0.0.1 only)
                              ▼
                            voipd ──▶ SQLite, event bus (MQTT/webhooks/UI)
```

## Status

| Area | State |
|---|---|
| Asterisk image, generated PJSIP config, ARI/AMI control | done |
| Inbound DID routing, parallel ringing, internal + outbound calls | done |
| German number normalization (`+49`, `0049`, `0…`, local, short codes) | done |
| Call history with per-leg quality (codec, loss, jitter, RTT, MOS) | done |
| Trunk supervisor: outage detection, escalation, IP/DNS watchers | done |
| Web UI + OIDC, blocklists, modules (MQTT, webhooks), voicemail, directory, backups | planned |

## Run

```sh
cd deploy
cp .env.example .env    # set strong passwords and your area code
docker compose up -d --build
```

Both containers use host networking (SIP/RTP need real addresses; ARI/AMI
stay on 127.0.0.1). Ports: SIP 5060/udp+tcp, 5061/tcp (TLS), RTP
10000–10999/udp.

Building for the Pi from another machine needs `docker buildx` (`--platform
linux/arm64`); without it, build on the Pi itself.

## Configuration

Bootstrap settings come from a YAML file (`-config`, `VOIPD_CONFIG`) and/or
environment variables; trunks, numbers, extensions and routing live in the
database.

| Env | Meaning |
|---|---|
| `VOIPD_ARI_PASSWORD`, `VOIPD_AMI_PASSWORD` | required, shared with the Asterisk container |
| `VOIPD_AREA_CODE` | local area code without 0 (e.g. `201`), for numbers dialed without one |
| `VOIPD_LOCAL_NETS` | trusted non-NAT networks, comma separated (LAN, WireGuard) |
| `VOIPD_EXTERNAL_V4` | static public IPv4 when behind NAT … |
| `VOIPD_PUBLIC_IP_URL` | … or a URL returning it (e.g. `https://api.ipify.org`), checked periodically |
| `VOIPD_DNS_SERVER` | `host:port` resolver for voipd's registrar DNS checks |
| `VOIPD_DB_PATH`, `VOIPD_LOG_LEVEL` | database path, `debug`/`info`/`warn` |

Supervisor timings (`trunks:` in YAML) default to production values:
`grace: 20s`, `first_retry: 45s`, `max_backoff: 10m`,
`reregister_attempts: 3`, `restart_after: 15m`, `restart_cooldown: 30m`.

## Trunk reliability

Asterisk's registration retries are configured never to give up (also after
403 and timeouts). On top, voipd:

1. tracks every registration (events + polling) and announces `trunk.down`
   after a grace period, `trunk.up` (with downtime) on recovery;
2. escalates while down: forced re-register with backoff → reload of the
   registration module → engine restart, only when no calls are active;
3. forces re-registration immediately when local addresses, the IPv6
   prefix, the public IP or the registrar's DNS targets (NAPTR/SRV/A) change.

## Tests

```sh
go test -race ./...     # unit tests (call flows run against a fake engine)
test/e2e/run.sh         # real Asterisk: calls between carrier, PBX and phones (~1 min)
test/e2e/chaos.sh       # trunk outages, auth rejection, DNS failure, carrier IP move,
                        # engine restarts; asserts detection and bounded recovery (~12 min)
```

The e2e suites need Docker, Go and `sqlite3`, build `deploy/asterisk` first
(`docker build -t voipd-asterisk:22.11.0 deploy/asterisk`), and use ports
5060/5061/8088/5038 on the host. `KEEP=1` leaves containers running.
