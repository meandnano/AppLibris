INSERT INTO field_sources (book_id, field, source)
SELECT id, 'title', 'embedded' FROM books WHERE title <> ''
UNION ALL SELECT id, 'publisher', 'embedded' FROM books WHERE publisher <> ''
UNION ALL SELECT id, 'published_date', 'embedded' FROM books WHERE published_date <> ''
UNION ALL SELECT id, 'language', 'embedded' FROM books WHERE language <> ''
UNION ALL SELECT id, 'isbn', 'embedded' FROM books WHERE isbn <> ''
UNION ALL SELECT id, 'description', 'embedded' FROM books WHERE description <> ''
UNION ALL SELECT DISTINCT book_id, 'authors', 'embedded' FROM book_authors;
