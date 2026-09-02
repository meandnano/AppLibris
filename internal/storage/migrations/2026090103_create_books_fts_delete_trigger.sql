CREATE TRIGGER books_fts_after_delete AFTER DELETE ON books
BEGIN
    DELETE FROM books_fts WHERE rowid = old.id;
END;
