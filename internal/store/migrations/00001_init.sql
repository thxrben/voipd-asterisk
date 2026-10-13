-- +goose Up

-- Key/value runtime settings (e.g. anonymous_block = "true").
CREATE TABLE settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at INTEGER NOT NULL DEFAULT (unixepoch())
);

-- An upstream SIP account. A Telekom line with several MSNs is one trunk
-- with one registration per number (see trunk_numbers).
CREATE TABLE trunks (
    id              INTEGER PRIMARY KEY,
    name            TEXT NOT NULL UNIQUE,         -- also the PJSIP object id prefix
    enabled         INTEGER NOT NULL DEFAULT 1,
    profile         TEXT NOT NULL DEFAULT 'generic', -- generic | telekom
    registrar       TEXT NOT NULL,                -- host or domain, e.g. tel.t-online.de
    outbound_proxy  TEXT NOT NULL DEFAULT '',
    transport       TEXT NOT NULL DEFAULT 'udp',  -- udp | tcp | tls
    address_family  TEXT NOT NULL DEFAULT 'auto', -- auto | v4 | v6
    auth_username   TEXT NOT NULL DEFAULT '',
    auth_password   TEXT NOT NULL DEFAULT '',
    srtp            INTEGER NOT NULL DEFAULT 0,
    dial_format     TEXT NOT NULL DEFAULT 'e164', -- e164 | national
    codecs          TEXT NOT NULL DEFAULT 'g722,alaw,ulaw',
    created_at      INTEGER NOT NULL DEFAULT (unixepoch())
);

-- A phone number (DID/MSN) owned on a trunk. Each one registers separately.
CREATE TABLE trunk_numbers (
    id          INTEGER PRIMARY KEY,
    trunk_id    INTEGER NOT NULL REFERENCES trunks(id) ON DELETE CASCADE,
    number      TEXT NOT NULL UNIQUE,  -- canonical E.164, e.g. +492011234567
    sip_user    TEXT NOT NULL DEFAULT '', -- registration user if it differs from the number
    register    INTEGER NOT NULL DEFAULT 1,
    label       TEXT NOT NULL DEFAULT ''
);

CREATE TABLE extensions (
    id               INTEGER PRIMARY KEY,
    number           TEXT NOT NULL UNIQUE,  -- internal number, e.g. "10"
    name             TEXT NOT NULL,
    enabled          INTEGER NOT NULL DEFAULT 1,
    sip_username     TEXT NOT NULL UNIQUE,
    sip_password     TEXT NOT NULL,
    transport        TEXT NOT NULL DEFAULT 'udp', -- udp | tcp | tls
    srtp             INTEGER NOT NULL DEFAULT 0,
    codecs           TEXT NOT NULL DEFAULT 'g722,alaw,ulaw',
    max_contacts     INTEGER NOT NULL DEFAULT 3,
    outbound_number_id INTEGER REFERENCES trunk_numbers(id) ON DELETE SET NULL, -- caller ID / trunk for outbound
    clir             INTEGER NOT NULL DEFAULT 0,
    dnd              INTEGER NOT NULL DEFAULT 0,
    voicemail_pin    TEXT NOT NULL DEFAULT '',
    owner_subject    TEXT NOT NULL DEFAULT '',  -- OIDC subject of the owning user
    created_at       INTEGER NOT NULL DEFAULT (unixepoch())
);

CREATE TABLE ring_groups (
    id            INTEGER PRIMARY KEY,
    name          TEXT NOT NULL UNIQUE,
    ring_timeout  INTEGER NOT NULL DEFAULT 30, -- seconds
    fallback      TEXT NOT NULL DEFAULT 'hangup', -- hangup | voicemail | keep_ringing
    fallback_extension_id INTEGER REFERENCES extensions(id) ON DELETE SET NULL -- whose voicemail
);

CREATE TABLE ring_group_members (
    ring_group_id INTEGER NOT NULL REFERENCES ring_groups(id) ON DELETE CASCADE,
    extension_id  INTEGER NOT NULL REFERENCES extensions(id) ON DELETE CASCADE,
    PRIMARY KEY (ring_group_id, extension_id)
);

-- Inbound routing is by called number only.
CREATE TABLE did_routes (
    trunk_number_id INTEGER PRIMARY KEY REFERENCES trunk_numbers(id) ON DELETE CASCADE,
    ring_group_id   INTEGER NOT NULL REFERENCES ring_groups(id) ON DELETE RESTRICT
);

CREATE TABLE calls (
    id            TEXT PRIMARY KEY,  -- voipd call id (uuid)
    direction     TEXT NOT NULL,     -- inbound | outbound | internal | notify
    status        TEXT NOT NULL,     -- ringing | answered | missed | blocked | cancelled | failed | voicemail
    caller        TEXT NOT NULL,     -- E.164, extension number or "anonymous"
    caller_name   TEXT NOT NULL DEFAULT '',
    callee        TEXT NOT NULL,     -- DID (inbound), dialed number (outbound) or extension
    trunk_id      INTEGER REFERENCES trunks(id) ON DELETE SET NULL,
    answered_by   TEXT NOT NULL DEFAULT '', -- extension number
    block_reason  TEXT NOT NULL DEFAULT '',
    started_at    INTEGER NOT NULL, -- unix ms
    answered_at   INTEGER,
    ended_at      INTEGER,
    hangup_cause  TEXT NOT NULL DEFAULT ''
);
CREATE INDEX calls_started ON calls(started_at DESC);
CREATE INDEX calls_caller ON calls(caller);

-- One row per Asterisk channel involved in a call, with quality stats.
CREATE TABLE call_legs (
    id            TEXT PRIMARY KEY, -- Asterisk channel id
    call_id       TEXT NOT NULL REFERENCES calls(id) ON DELETE CASCADE,
    kind          TEXT NOT NULL,    -- trunk | extension
    endpoint      TEXT NOT NULL,
    created_at    INTEGER NOT NULL,
    answered_at   INTEGER,
    ended_at      INTEGER,
    hangup_cause  TEXT NOT NULL DEFAULT '',
    codec         TEXT NOT NULL DEFAULT '',
    transport     TEXT NOT NULL DEFAULT '',
    srtp          INTEGER NOT NULL DEFAULT 0,
    remote_addr   TEXT NOT NULL DEFAULT '',
    rx_packets    INTEGER,
    tx_packets    INTEGER,
    rx_lost       INTEGER,
    tx_lost       INTEGER,
    jitter_ms     REAL,
    rtt_ms        REAL,
    mos           REAL
);
CREATE INDEX call_legs_call ON call_legs(call_id);

CREATE TABLE call_events (
    id       INTEGER PRIMARY KEY,
    call_id  TEXT NOT NULL REFERENCES calls(id) ON DELETE CASCADE,
    at       INTEGER NOT NULL, -- unix ms
    type     TEXT NOT NULL,
    detail   TEXT NOT NULL DEFAULT '{}' -- JSON
);
CREATE INDEX call_events_call ON call_events(call_id, at);

-- General SIP/system events (trunk state, registrations, ...).
CREATE TABLE sip_events (
    id      INTEGER PRIMARY KEY,
    at      INTEGER NOT NULL, -- unix ms
    type    TEXT NOT NULL,
    subject TEXT NOT NULL DEFAULT '', -- trunk name, extension, ...
    detail  TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX sip_events_at ON sip_events(at DESC);

-- +goose Down
DROP TABLE sip_events;
DROP TABLE call_events;
DROP TABLE call_legs;
DROP TABLE calls;
DROP TABLE did_routes;
DROP TABLE ring_group_members;
DROP TABLE ring_groups;
DROP TABLE extensions;
DROP TABLE trunk_numbers;
DROP TABLE trunks;
DROP TABLE settings;
