CREATE TABLE send_log (
    id                  INTEGER PRIMARY KEY,
    book_id             INTEGER REFERENCES books(id) ON DELETE SET NULL,
    book_title          TEXT NOT NULL,
    recipient_address   TEXT NOT NULL,
    status              TEXT NOT NULL
                        CHECK (status IN ('queued','sending','delivered','failed')),
    provider_message_id TEXT NOT NULL DEFAULT '',
    failure_reason      TEXT NOT NULL DEFAULT '',
    queued_at           TIMESTAMP NOT NULL,
    started_at          TIMESTAMP,
    finished_at         TIMESTAMP
);
