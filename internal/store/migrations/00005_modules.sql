-- +goose Up

-- Settings of the built-in modules (mqtt, webhook, notify) as JSON. The
-- module validates its own config; secrets in it are write-only via the API.
CREATE TABLE module_settings (
    module      TEXT PRIMARY KEY,
    enabled     INTEGER NOT NULL DEFAULT 0,
    config      TEXT NOT NULL DEFAULT '{}',
    updated_at  INTEGER NOT NULL DEFAULT 0     -- unix ms
);

-- Webhook receivers. events: comma separated event type prefixes
-- ("call.missed,trunk."), empty = all events.
CREATE TABLE webhook_endpoints (
    id          INTEGER PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    url         TEXT NOT NULL,
    secret      TEXT NOT NULL,
    events      TEXT NOT NULL DEFAULT '',
    enabled     INTEGER NOT NULL DEFAULT 1,
    created_at  INTEGER NOT NULL DEFAULT (unixepoch())
);

-- Delivery attempts, the most recent 200 per endpoint are kept.
CREATE TABLE webhook_deliveries (
    id           INTEGER PRIMARY KEY,
    endpoint_id  INTEGER NOT NULL REFERENCES webhook_endpoints(id) ON DELETE CASCADE,
    delivery     TEXT NOT NULL,              -- X-Voipd-Delivery, same for all attempts
    event_type   TEXT NOT NULL,
    at           INTEGER NOT NULL,           -- unix ms
    attempt      INTEGER NOT NULL,
    status_code  INTEGER NOT NULL DEFAULT 0, -- 0 = no response
    latency_ms   INTEGER NOT NULL DEFAULT 0,
    error        TEXT NOT NULL DEFAULT '',
    response     TEXT NOT NULL DEFAULT ''    -- first 512 bytes
);
CREATE INDEX webhook_deliveries_endpoint ON webhook_deliveries(endpoint_id, id);

-- +goose Down
DROP TABLE webhook_deliveries;
DROP TABLE webhook_endpoints;
DROP TABLE module_settings;
