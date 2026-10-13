-- +goose Up

-- Voicemail messages. The audio is <voicemail dir>/vm/<extension number>/<id>.wav
-- (recorded by Asterisk into the shared recording directory).
CREATE TABLE voicemails (
    id            TEXT PRIMARY KEY,
    extension_id  INTEGER NOT NULL REFERENCES extensions(id) ON DELETE CASCADE,
    call_id       TEXT NOT NULL DEFAULT '',
    caller        TEXT NOT NULL DEFAULT '',   -- E.164, "anonymous" or ''
    caller_name   TEXT NOT NULL DEFAULT '',
    received_at   INTEGER NOT NULL,           -- unix ms
    duration_ms   INTEGER NOT NULL DEFAULT 0,
    heard         INTEGER NOT NULL DEFAULT 0,
    heard_at      INTEGER
);
CREATE INDEX voicemails_box ON voicemails(extension_id, received_at);

-- A personal greeting was recorded or uploaded (greet/<number>.wav).
ALTER TABLE extensions ADD COLUMN voicemail_greeting INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE extensions DROP COLUMN voicemail_greeting;
DROP TABLE voicemails;
