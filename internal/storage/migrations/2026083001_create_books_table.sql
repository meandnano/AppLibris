CREATE TABLE books (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    content_hash    TEXT NOT NULL,
    title           TEXT NOT NULL,
    sort_title      TEXT NOT NULL COLLATE NOCASE,
    publisher       TEXT NOT NULL DEFAULT '',
    published_date  TEXT NOT NULL DEFAULT '',
    language        TEXT NOT NULL DEFAULT '',
    isbn            TEXT NOT NULL DEFAULT '',
    description     TEXT NOT NULL DEFAULT '',
    cover_path      TEXT NOT NULL DEFAULT '',
    file_path       TEXT NOT NULL,
    format          TEXT NOT NULL,
    file_size       INTEGER NOT NULL,
    added_at        TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    modified_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    derived_from    INTEGER REFERENCES books (id)
);
