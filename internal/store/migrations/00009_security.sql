-- +goose Up

-- Addresses refused for SIP after repeated failed logins, or by hand.
CREATE TABLE sip_bans (
    ip          TEXT PRIMARY KEY,          -- address or network, e.g. 192.0.2.10/32
    reason      TEXT NOT NULL DEFAULT '',
    failures    INTEGER NOT NULL DEFAULT 0,
    account     TEXT NOT NULL DEFAULT '',  -- last account tried
    banned_at   INTEGER NOT NULL,          -- unix ms
    expires_at  INTEGER,                   -- unix ms; NULL = until unbanned
    manual      INTEGER NOT NULL DEFAULT 0
);

-- +goose Down
DROP TABLE sip_bans;
