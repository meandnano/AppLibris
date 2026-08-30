INSERT INTO book_files (book_id, file_path, file_size, modified_at)
SELECT id, file_path, file_size, modified_at FROM books;
