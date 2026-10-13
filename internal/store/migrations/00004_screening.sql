-- +goose Up

-- A source of numbers: the manual lists, an uploaded file, a URL that is
-- downloaded periodically, or a CardDAV address book (e.g. PhoneBlock).
-- target says what the numbers are for: blocking, allowing, or contacts
-- (caller names; contacts are also allowed when that setting is on).
CREATE TABLE list_sources (
    id            INTEGER PRIMARY KEY,
    name          TEXT NOT NULL UNIQUE,
    kind          TEXT NOT NULL,            -- manual | file | url | carddav
    target        TEXT NOT NULL,            -- block | allow | contacts
    url           TEXT NOT NULL DEFAULT '',
    username      TEXT NOT NULL DEFAULT '',
    password      TEXT NOT NULL DEFAULT '',
    interval_min  INTEGER NOT NULL DEFAULT 360,
    enabled       INTEGER NOT NULL DEFAULT 1,
    last_sync_at  INTEGER,                  -- unix ms
    last_status   TEXT NOT NULL DEFAULT '', -- ok | failed
    last_error    TEXT NOT NULL DEFAULT '',
    failures      INTEGER NOT NULL DEFAULT 0, -- consecutive, for backoff
    created_at    INTEGER NOT NULL DEFAULT (unixepoch())
);

-- A contact or vCard of a source. href identifies it within the source
-- (the CardDAV path, or a generated id for manual contacts).
CREATE TABLE cards (
    id         INTEGER PRIMARY KEY,
    source_id  INTEGER NOT NULL REFERENCES list_sources(id) ON DELETE CASCADE,
    href       TEXT NOT NULL,
    etag       TEXT NOT NULL DEFAULT '',
    name       TEXT NOT NULL DEFAULT '',
    org        TEXT NOT NULL DEFAULT '',
    UNIQUE(source_id, href)
);

-- A number or number range. pattern is E.164 ("+492011234567") or a
-- prefix ending in "*" ("+4920112*").
CREATE TABLE list_entries (
    id          INTEGER PRIMARY KEY,
    source_id   INTEGER NOT NULL REFERENCES list_sources(id) ON DELETE CASCADE,
    card_id     INTEGER REFERENCES cards(id) ON DELETE CASCADE,
    pattern     TEXT NOT NULL,
    label       TEXT NOT NULL DEFAULT '', -- comment, or the phone type for contacts
    hits        INTEGER NOT NULL DEFAULT 0,
    last_hit_at INTEGER,                  -- unix ms
    created_at  INTEGER NOT NULL DEFAULT (unixepoch())
);
CREATE UNIQUE INDEX list_entries_loose ON list_entries(source_id, pattern) WHERE card_id IS NULL;
CREATE UNIQUE INDEX list_entries_card ON list_entries(card_id, pattern) WHERE card_id IS NOT NULL;
CREATE INDEX list_entries_pattern ON list_entries(pattern);

INSERT INTO list_sources(id, name, kind, target) VALUES
    (1, 'My blocklist', 'manual', 'block'),
    (2, 'My allowlist', 'manual', 'allow'),
    (3, 'My contacts', 'manual', 'contacts');

-- +goose Down
DROP TABLE list_entries;
DROP TABLE cards;
DROP TABLE list_sources;
