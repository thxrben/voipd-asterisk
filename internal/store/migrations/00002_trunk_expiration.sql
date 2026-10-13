-- +goose Up
-- Registration lifetime in seconds. Shorter means a dead registrar is
-- noticed sooner (Telekom needs no qualify; refresh is the health signal).
ALTER TABLE trunks ADD COLUMN reg_expiration INTEGER NOT NULL DEFAULT 480;

-- +goose Down
ALTER TABLE trunks DROP COLUMN reg_expiration;
