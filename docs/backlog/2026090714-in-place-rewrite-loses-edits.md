# Backlog: rewriting a book file in place deletes its edits

## Problem

Content hash is a book's identity. When a file at a known path changes
content, `scanFile` hashes it, finds no book with that hash, and calls
`CreateBookWithFile`, which reassigns the path's `book_files` row to the
new book and deletes the old book in the same transaction once it has no
locations left. `book_authors`, `field_sources` and `enrichment_jobs`
cascade; `send_log.book_id` goes `NULL`.

Every manual edit, every provenance row and every enrichment result on
the old book is gone, and the new book starts from the file's embedded
metadata.

That is the right behaviour for a file replaced by a different book. It
is the wrong behaviour for the common NAS workflows that rewrite a file
without changing which book it is:

- Calibre's metadata write-back, which rewrites the OPF inside the EPUB
  on every edit made in Calibre.
- `ebook-polish`, `kepubify`, or any re-packaging tool.
- A sync tool that re-downloads a file from a source that re-zipped it.

From the scanner's position the two cases are indistinguishable: a
different hash at a known path.

## Why this is backlog, not a plan

It is a design consequence, recorded in DESIGN.md's identity decision,
not a bug in how that decision was implemented. A person who points this
at a Calibre-managed folder and edits metadata in both places will lose
one side's edits; a person who treats the library directory as the
source of truth and edits here will not. The second is what the project
is designed for. This item exists so the trade is written down where a
future maintainer looks, and so that it is re-examined if a conversion
step (DESIGN.md's deferred list) ever writes files into the library
itself, at which point the project would be rewriting its own books.

## Re-validate before acting

- Whether `CreateBookWithFile` still prunes the orphan in the same
  transaction.
- Whether anything in the project has started writing into `LIBRARY_DIR`.

## Sketch

Two directions, neither cheap:

- **Inherit on same-path replacement.** When the path's previous book is
  about to be orphaned and the new file's embedded title matches the old
  book's (or the old book has `manual` field sources at all), carry the
  `manual` fields and their provenance across to the new book rather
  than dropping them. The heuristic is the hard part: "same path, similar
  title" is usually the same book and sometimes is not.
- **Keep the old book and add a `derived_from` link.** `books` already
  has a `derived_from` column nothing reads, reserved for exactly the
  conversion case. The replacement could be modelled as a derivation of
  the original, keeping both rows, with the UI showing one. That is a
  larger data-model change and belongs with the conversion decision.

Either way, the rule must never apply across paths: two different files
with different hashes are two books, and the mover/duplicate story
depends on that.
