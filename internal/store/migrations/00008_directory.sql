-- +goose Up

-- Read-only logins for phones and apps using the LDAP or CardDAV
-- directory (they cannot use OIDC).
CREATE TABLE directory_accounts (
    id             INTEGER PRIMARY KEY,
    username       TEXT NOT NULL UNIQUE,
    password_hash  TEXT NOT NULL,             -- argon2id
    created_at     INTEGER NOT NULL,          -- unix ms
    last_used_at   INTEGER                    -- unix ms
);

-- +goose Down
DROP TABLE directory_accounts;
