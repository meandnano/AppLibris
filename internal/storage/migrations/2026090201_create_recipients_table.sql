CREATE TABLE recipients (
    id           INTEGER PRIMARY KEY,
    address      TEXT NOT NULL COLLATE NOCASE,
    label        TEXT NOT NULL DEFAULT '',
    last_used_at TIMESTAMP,
    added_at     TIMESTAMP NOT NULL
);
