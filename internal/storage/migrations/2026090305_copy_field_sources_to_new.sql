INSERT INTO field_sources_new (book_id, field, source)
SELECT book_id, field, source FROM field_sources;
