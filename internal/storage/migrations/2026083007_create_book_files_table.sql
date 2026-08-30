CREATE TABLE book_files (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    book_id         INTEGER NOT NULL REFERENCES books (id),
    file_path       TEXT NOT NULL,
    file_size       INTEGER NOT NULL,
    modified_at     TIMESTAMP NOT NULL,
    added_at        TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
