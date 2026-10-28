-- Verified webhook deliveries. Doubles as the idempotency record (the unique
-- key on provider + delivery_id) and the durable work queue.
-- Timestamps are Unix milliseconds (UTC).
CREATE TABLE webhook_deliveries (
    id              INTEGER PRIMARY KEY,
    provider        TEXT    NOT NULL,
    delivery_id     TEXT    NOT NULL,
    kind            TEXT    NOT NULL,
    repo            TEXT    NOT NULL,
    event           TEXT    NOT NULL, -- normalized provider.Event as JSON
    status          TEXT    NOT NULL DEFAULT 'pending'
                            CHECK (status IN ('pending', 'processing', 'done', 'failed')),
    attempts        INTEGER NOT NULL DEFAULT 0,
    last_error      TEXT,
    next_attempt_at INTEGER NOT NULL,
    received_at     INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    UNIQUE (provider, delivery_id)
);

CREATE INDEX webhook_deliveries_queue ON webhook_deliveries (status, next_attempt_at, id);
