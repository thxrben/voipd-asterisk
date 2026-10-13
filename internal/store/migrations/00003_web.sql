-- +goose Up

-- People who logged in to the web UI (OIDC subject or "local:admin").
CREATE TABLE users (
    subject     TEXT PRIMARY KEY,
    username    TEXT NOT NULL DEFAULT '',
    name        TEXT NOT NULL DEFAULT '',
    email       TEXT NOT NULL DEFAULT '',
    role        TEXT NOT NULL,              -- admin | user (from groups at last login)
    created_at  INTEGER NOT NULL,           -- unix ms
    last_login  INTEGER NOT NULL
);

-- Server-side sessions. Only a hash of the cookie value is stored.
CREATE TABLE sessions (
    id_hash     TEXT PRIMARY KEY,
    subject     TEXT NOT NULL REFERENCES users(subject) ON DELETE CASCADE,
    csrf        TEXT NOT NULL,
    created_at  INTEGER NOT NULL,
    last_seen   INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL,           -- absolute expiry
    id_token    TEXT NOT NULL DEFAULT ''    -- for RP-initiated logout
);
CREATE INDEX sessions_subject ON sessions(subject);

-- Who changed what. detail is JSON.
CREATE TABLE audit_log (
    id       INTEGER PRIMARY KEY,
    at       INTEGER NOT NULL,
    subject  TEXT NOT NULL,
    action   TEXT NOT NULL,                 -- e.g. extension.update, list.import
    target   TEXT NOT NULL DEFAULT '',
    detail   TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX audit_log_at ON audit_log(at DESC);

-- Which list entry blocked a call (lists arrive in the next migration).
ALTER TABLE calls ADD COLUMN block_entry_id INTEGER;

CREATE INDEX calls_status_started ON calls(status, started_at);
CREATE INDEX calls_direction_started ON calls(direction, started_at);
CREATE INDEX calls_block_entry ON calls(block_entry_id);
CREATE INDEX call_legs_endpoint ON call_legs(endpoint);

-- +goose Down
DROP INDEX call_legs_endpoint;
DROP INDEX calls_block_entry;
DROP INDEX calls_direction_started;
DROP INDEX calls_status_started;
ALTER TABLE calls DROP COLUMN block_entry_id;
DROP TABLE audit_log;
DROP TABLE sessions;
DROP TABLE users;
