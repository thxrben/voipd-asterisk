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
| Web API: OIDC login (authentik), sessions, roles, call log, statistics, live events | done |
| Web UI: Pi-hole-style dashboard, call log with details, status page (light/dark, mobile) | done |
| Telephony configuration in the UI: lines (Telekom preset), numbers, extensions with phone credentials, ring groups, do not disturb | done |
| Call blocking: block/allow lists, number ranges, PhoneBlock/CardDAV sync, bulk import, contacts, pause, anonymous blocking | done |
| Integrations: MQTT with Home Assistant discovery, signed webhooks, e-mail notifications | done |
| Voicemail: German prompts, message waiting light, *98 phone menu, web player, personal greetings | done |
| Phone directory: LDAP (Yealink, Snom, Grandstream, Gigaset) and CardDAV (softphones, FritzBox) | done |
| Backups: encrypted full backups (scheduled, rotated), restore, configuration as YAML with import preview, call history CSV/JSON | done |
| Hardening | in progress |

## Run

```sh
cd deploy
cp .env.example .env    # set strong passwords and your area code
docker compose up -d --build
```

Both containers use host networking (SIP/RTP need real addresses; ARI/AMI
stay on 127.0.0.1). Ports: SIP 5060/udp+tcp, 5061/tcp (TLS), RTP
10000–10999/udp, web UI and CardDAV 8080/tcp, LDAP 389/tcp, LDAPS 636/tcp.
voipd starts as root with only the capabilities to open ports 389 and 636
and to switch users, then
runs as an unprivileged user without any capabilities.

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
| `VOIPD_VOICEMAIL_DIR` | voicemail directory, shared with Asterisk's recording directory (default `/var/lib/voipd/voicemail`) |
| `VOIPD_BACKUP_DIR` | scheduled backups (default `/var/lib/voipd/backups`, inside the data volume; mount another disk there) |
| `VOIPD_LDAP_LISTEN` | LDAP phone book (default `[::]:389`, empty disables it) |
| `VOIPD_LDAPS_LISTEN` | LDAPS phone book with voipd's certificate (default `[::]:636`, empty disables it) |
| `VOIPD_RUN_AS` | `uid:gid` voipd switches to when started as root (default `65532:65532`) |
| `VOIPD_HTTP_LISTEN`, `VOIPD_PUBLIC_URL` | web UI listen address (default `[::]:8080`) and the URL users open |
| `VOIPD_OIDC_ISSUER`, `…_CLIENT_ID`, `…_CLIENT_SECRET` | authentik/OIDC login; redirect URI is `<public url>/auth/callback` |
| `VOIPD_ADMIN_GROUP`, `VOIPD_USER_GROUP` | groups granting admin / user access (default `voipd-admins` / `voipd-users`) |
| `VOIPD_ADMIN_PASSWORD_HASH` | enables a break-glass local `admin` login; create with `voipd hash-password` |

## Web login (authentik)

Create an OAuth2/OpenID provider in authentik (confidential client, signing
key set, redirect URI `https://<your voipd>/auth/callback`) and an
application for it; its issuer URL is shown on the provider page. Put users
into `voipd-admins` (everything) or `voipd-users` (own extensions only:
own call history, live calls, later voicemail). The groups must be in the
token: the default `profile` scope mapping includes them. Members of
neither group are denied.

Supervisor timings (`trunks:` in YAML) default to production values:
`grace: 20s`, `first_retry: 45s`, `max_backoff: 10m`,
`reregister_attempts: 3`, `restart_after: 15m`, `restart_cooldown: 30m`.

## Call blocking

Inbound callers are checked in this order:

1. **Allowlists** and **contacts** (with "Never block contacts", the default): the call rings.
2. **Pause**: while blocking is paused, everything rings.
3. **Anonymous callers**, if "Block anonymous callers" is on.
4. **Blocklists**: the call is blocked *silently* – the caller keeps hearing
   ringing, no phone rings, and the call shows up as blocked in the log.

Numbers are stored in E.164 form, whatever format they were entered or
imported in. A trailing `*` blocks a range (`0201 123*`); ranges need the
area code and at least six digits after the country code. Lookups use an
in-memory index that is rebuilt and swapped after every change (~40 ns per
lookup with 100,000 numbers), so screening never waits for the database.

**PhoneBlock**: *Blocking → Add list → PhoneBlock*, then enter a PhoneBlock
**API key** (create one in your PhoneBlock account settings). voipd syncs the
complete community blocklist through the PhoneBlock API: once the full list,
after that only the changes since the last version, once a day (PhoneBlock
asks for no more). Numbers are blocked from a number of votes you choose
(2–100) or from your PhoneBlock account's setting; changing the threshold
needs no new download. Your personal PhoneBlock block- and allowlist apply
too, and the caller's rating becomes the name on the phone
(`SPAM: Fraud (20)`). A failed sync (wrong key, server down, or a full list
that suddenly shrinks) keeps the previous list and is shown on the list's
card.

PhoneBlock's CardDAV address book still works as a regular CardDAV list,
but it may contain only part of the blocklist.

Other sources: any CardDAV address book (also for contacts, e.g. Nextcloud),
a URL with a CSV/text/vCard list downloaded periodically, or a one-off
import of a file (checked first, then imported as its own list so it can be
removed again as a whole).

## Voicemail

A ring group whose fallback is *Voicemail* (Telephony → Ring groups) takes a
message when nobody answers: the caller hears the mailbox's personal greeting
(or the standard one) and a beep; `#`, 5 s of silence, hanging up or 3 minutes
end the message. Phones of the extension get the message waiting light.

- **On the phone:** dial `*98` for your own mailbox (no PIN: the phone is
  logged in). `1` listen, `0` record your greeting; while listening `1`
  repeat, `2`/`#` next, `7` delete. All codes are listed under *Phone codes*
  in the web UI.
- **In the web UI** (*Voicemail*): play, download, mark as heard, delete;
  upload a greeting (WAV, mono, 16 bit, 8 or 16 kHz, up to 2 minutes).
  Users see the mailboxes of their own extensions, admins all.

The German prompts are generated once with [Piper](https://github.com/rhasspy/piper)
(voice "Thorsten") when the Asterisk image is built; texts are in
`deploy/asterisk/prompts/de.txt`. Messages and greetings live in the
`voicemail` volume shared by both containers.

## Phone directory

Phones look up names in voipd's contacts (all contact lists, synced ones
included) and the extensions, and show caller names on incoming calls.
Create an account under *Directory*; the page shows the values to enter on
each kind of phone.

- **LDAP** (Yealink, Snom, Grandstream, Gigaset): server = voipd's address,
  port 389, base `dc=voipd`, simple bind with the account (`phones` or
  `uid=phones,dc=voipd`). Entries are `inetOrgPerson` with `cn`, `sn`,
  `givenName`, `o`, `telephoneNumber`, `mobile`, `homePhone`; numbers are
  given as dialed (`01701234567`, `0044…`). Number searches match however
  the phone writes the number (`0170…`, `+49170…`, `0049170…`).
- **CardDAV** (softphones, DAVx5, iOS, FritzBox online phone book):
  `https://<voipd>/dav/` with the account; the address books are
  `/dav/<account>/books/contacts/` and `…/extensions/`.

The directory is read-only. Plain LDAP is unencrypted: keep it to the LAN
and WireGuard, or use LDAPS on port 636 (import voipd's CA on the phone).

## Backups

Under *System → Backup*:

- **Full backups** hold the database (configuration, call history,
  contacts, lists) and all voicemail, encrypted with a passphrase
  ([age](https://age-encryption.org), scrypt). voipd makes one daily at
  the configured time and keeps the newest N in `VOIPD_BACKUP_DIR`; a
  failed backup is reported by e-mail (admins), MQTT and webhooks
  (`backup.failed`). The default directory is on the same disk as the data:
  mount a USB stick or NAS share there (owned by uid 65532), or download
  backups from the page.
- **Restore** a stored or uploaded backup from the page: voipd checks the
  passphrase and the archive, then restarts and swaps the data in at start.
  The replaced data stays in `pre-restore/` (database) and
  `voicemail/.pre-restore/` until the next restore. Without the web UI:
  `docker compose exec voipd voipd restore /var/lib/voipd/backups/<file>`
  (asks for the passphrase or reads `VOIPD_BACKUP_PASSPHRASE`), then
  `docker compose restart voipd`.
- **Configuration** export as readable YAML (lines, phones, ring groups,
  routes, manual lists and contacts, synced list sources, integrations,
  directory accounts, settings), without passwords, or with them as an
  encrypted file. Importing shows a preview of every addition, change and
  removal first; sections present in the file replace the current ones,
  absent sections and passwords missing from the file are kept, and
  settings are merged.
- **Call history** downloads as CSV or JSON from the call log, with the
  current filters.

## SIP access

Under *System → Security*:

- **Phones only from allowed networks.** Phone endpoints get an ACL; a
  REGISTER or call from anywhere else is refused before the password is
  checked. By default the allowed networks are this host's own networks
  (the LAN), `VOIPD_LOCAL_NETS` (e.g. WireGuard) and loopback; the page
  shows the effective list and can replace it with your own. The carrier
  line is not affected.
- **Bans after failed logins.** voipd follows Asterisk's security events
  (wrong password, unknown account). A sender with 10 failures within 10
  minutes is banned for an hour (all adjustable); bans apply to every SIP
  request from that address, survive restarts, can be lifted or added by
  hand, and are reported by e-mail (admins), MQTT and webhooks
  (`security.banned`).

## TLS and SRTP for phones

voipd runs its own certificate authority. On first start it creates the CA
(20 years) and a server certificate for this host's name, its IPv4
addresses and any extra names set under *System → Security*; it renews the
server certificate a month before it expires or when the addresses change,
and Asterisk picks it up without a restart. Phones connect with TLS on port
5061: import the CA from `http://<voipd>/ca.pem` (or `/ca.crt`, DER) as a
trusted certificate, or switch off server verification on the phone. An
extension with SRTP must use the TLS transport (SDES keys travel in the SIP
messages). `VOIPD_TLS_CERT_FILE`/`VOIPD_TLS_KEY_FILE` still replace the
certificate for SIP if set. The CA is part of the configuration export (its
key only in the encrypted file), so phones keep trusting a restored voipd.

LDAPS (port 636, `VOIPD_LDAPS_LISTEN`) serves the phone book with the same
certificate.

## Integrations

Configured under *Integrations* in the web UI; each one can be switched on
and off separately and shows its connection state there and on the status
page.

**MQTT** (MQTT 5, e.g. the Mosquitto add-on of Home Assistant): broker URL
`mqtt://host:1883`, `mqtts://host:8883` or `ws(s)://…`. With *Home Assistant
discovery* on, voipd appears as a device with switches (block anonymous
calls, pause blocking, do not disturb per phone) and sensors (lines, phones,
calls/missed/blocked today, active calls, last caller). Topics, below the
base topic `voipd`:

| Topic | Payload |
|---|---|
| `status` | `online` / `offline` (last will) |
| `line/<number>/state`, `…/attributes` | `up` / `down`, JSON details |
| `extension/<ext>/dnd`, `extension/<ext>/registered` | `ON` / `OFF` |
| `extension/<ext>/voicemail` | number of new voicemail messages |
| `calls/active`, `calls/active/list`, `calls/last_caller` | count, JSON, name or number |
| `stats/today` | JSON: `total`, `inbound`, `answered`, `missed`, `blocked`, … |
| `blocking/anonymous`, `blocking/paused` | `ON` / `OFF` |
| `event/<type>` | every event as JSON (`call.incoming`, `call.missed`, `trunk.down`, …), not retained |

Commands (can be switched off): `blocking/anonymous/set` (`ON`/`OFF`),
`blocking/pause/set` (minutes, `0` resumes), `blocklist/add` and
`allowlist/add` (a number, or `{"number": …, "label": …}`),
`extension/<ext>/dnd/set` (`ON`/`OFF`), `call/<id>/hangup`. Commands are
recorded in the audit log as `mqtt`; a failed one is reported on `error`.

**Webhooks**: every endpoint gets the events it subscribed to as a JSON POST
(`{"type", "at", "call_id", "data"}`), in order, retried 4 times (1 s … 2 min).
Each endpoint has its own secret (shown once when created):

```
X-Voipd-Event: call.missed
X-Voipd-Delivery: <id, the same for every attempt>
X-Voipd-Timestamp: <unix seconds>
X-Voipd-Signature: sha256=<hex HMAC-SHA256(secret, timestamp + "." + body)>
```

```python
expected = "sha256=" + hmac.new(secret, f"{ts}.".encode() + body, hashlib.sha256).hexdigest()
ok = hmac.compare_digest(expected, request.headers["X-Voipd-Signature"]) and abs(time.time() - int(ts)) < 300
```

**E-mail** (SMTP with STARTTLS, TLS or plain on a trusted network): missed
calls go to the owners of the phones that rang (their e-mail address comes
from authentik) and to extra addresses; new voicemail messages go to the
mailbox owner, with the recording attached (optional, up to 10 MB); line
outages/recovery and, optionally, blocked calls go to the admins. Mails are in German and link to
the call in the web UI (`VOIPD_PUBLIC_URL`).

## Trunk reliability

Asterisk's registration retries are configured never to give up (also after
403 and timeouts). On top, voipd:

1. tracks every registration (events + polling) and announces `trunk.down`
   after a grace period, `trunk.up` (with downtime) on recovery;
2. escalates while down: forced re-register with backoff → reload of the
   registration module → engine restart, only when no calls are active;
3. forces re-registration immediately when local addresses, the IPv6
   prefix, the public IP or the registrar's DNS targets (NAPTR/SRV/A,
   including a change of server preference) change.

Running calls are never disturbed: re-registering waits while a call is up
on that trunk (de-registering can tear down calls, e.g. at Telekom), module
reloads and engine restarts wait until no call at all is up. Outages are
still announced immediately; only recovery actions wait, and nothing that
is waiting gets dropped.

Registrars are resolved per RFC 3263 (NAPTR → SRV → A/AAAA), so a carrier
with several servers (e.g. `tel.t-online.de`) works out of the box,
including failover to the next SRV target when the preferred one dies.

## Tests

```sh
go test -race ./...     # unit tests (call flows run against a fake engine)
test/e2e/run.sh         # real Asterisk: calls between carrier, PBX and phones, config via the API,
                        # PhoneBlock-like CardDAV blocklist (9,000 contacts) and the PhoneBlock API
                        # (30,000 numbers, then changes only) blocking real calls (~2.5 min)
test/e2e/dns.sh         # Telekom-like NAPTR/SRV with two servers and two numbers: resolution,
                        # per-number routing, failover, no re-registration during calls (~6 min)
test/e2e/chaos.sh       # trunk outages, auth rejection, DNS failure, carrier IP move,
                        # engine restarts; asserts detection and bounded recovery (~12 min)
test/e2e/ui.sh          # web UI in Chrome (Playwright) with demo data and a fake OIDC provider (~30 s)
test/e2e/integrations.sh # Mosquitto, mailpit, webhook receiver: events, e-mail and MQTT commands
                        # with real calls (~1.5 min)
test/e2e/voicemail.sh   # leave a message, message waiting light, *98 listen and delete (~1 min)
test/e2e/backup.sh      # backups with rotation, config export/edit/import reaching Asterisk, encrypted
                        # export restoring a deleted phone, full restore through a restart (~1 min)
test/e2e/security.sh    # phone over TLS verifying voipd's CA, SRTP call; phone ACL (refused outside
                        # allowed networks, line unaffected); ban after wrong passwords, unban (~2 min)
test/e2e/directory.sh   # voipd image with the compose security settings: LDAP on 389 as the phones
                        # search, LDAPS on 636 verified against voipd's CA, unprivileged
                        # afterwards, CardDAV (~3 min with image build)
```

UI development: `go run ./test/devseed -db /tmp/demo.db` creates demo data;
run voipd against it (`VOIPD_HTTP_LISTEN=127.0.0.1:18080`) and
`npm run dev` in `web/` (proxies the API to that port).

The e2e suites need Docker, Go and `sqlite3`, build `deploy/asterisk` first
(`docker build -t voipd-asterisk:22.11.0 deploy/asterisk`), and use ports
5060/5061/8088/5038 on the host. `KEEP=1` leaves containers running.
