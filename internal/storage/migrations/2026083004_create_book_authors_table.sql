CREATE TABLE book_authors (
    book_id     INTEGER NOT NULL REFERENCES books (id) ON DELETE CASCADE,
    author_id   INTEGER NOT NULL REFERENCES authors (id) ON DELETE CASCADE,
    position    INTEGER NOT NULL,
    PRIMARY KEY (book_id, author_id)
);
