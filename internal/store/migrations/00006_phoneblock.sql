-- +goose Up

-- PhoneBlock API sources: cursor is the blocklist version of the last sync
-- (empty = next sync downloads the full list); min_votes is the threshold
-- (0 = the PhoneBlock account's setting).
ALTER TABLE list_sources ADD COLUMN cursor TEXT NOT NULL DEFAULT '';
ALTER TABLE list_sources ADD COLUMN min_votes INTEGER NOT NULL DEFAULT 0;

-- Mirror of the whole PhoneBlock community blocklist with its votes. The
-- source's list_entries are derived from it (threshold, personal lists),
-- so changing those never needs a new full download.
CREATE TABLE phoneblock_numbers (
    source_id      INTEGER NOT NULL REFERENCES list_sources(id) ON DELETE CASCADE,
    phone          TEXT NOT NULL,
    votes          INTEGER NOT NULL,
    rating         TEXT NOT NULL DEFAULT '',
    last_activity  INTEGER NOT NULL DEFAULT 0, -- unix ms
    PRIMARY KEY (source_id, phone)
) WITHOUT ROWID;

-- +goose Down
DROP TABLE phoneblock_numbers;
ALTER TABLE list_sources DROP COLUMN min_votes;
ALTER TABLE list_sources DROP COLUMN cursor;
