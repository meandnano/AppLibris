CREATE VIRTUAL TABLE books_fts USING fts5(
    title, authors, description, isbn,
    tokenize='unicode61 remove_diacritics 2'
);
