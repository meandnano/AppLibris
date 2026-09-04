CREATE TABLE enrichment_jobs (
    id             INTEGER PRIMARY KEY,
    book_id        INTEGER NOT NULL REFERENCES books(id) ON DELETE CASCADE,
    status         TEXT NOT NULL
                   CHECK (status IN ('queued','running','done','failed')),
    failure_reason TEXT NOT NULL DEFAULT '',
    queued_at      TIMESTAMP NOT NULL,
    started_at     TIMESTAMP,
    finished_at    TIMESTAMP
);
