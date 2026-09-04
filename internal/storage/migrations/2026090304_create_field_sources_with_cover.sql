CREATE TABLE field_sources_new (
    book_id INTEGER NOT NULL REFERENCES books(id) ON DELETE CASCADE,
    field   TEXT NOT NULL CHECK (field IN ('title', 'authors', 'publisher', 'published_date', 'language', 'isbn', 'description', 'cover')),
    source  TEXT NOT NULL CHECK (source <> ''),
    PRIMARY KEY (book_id, field)
);
