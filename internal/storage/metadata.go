package storage

import (
	"context"
	"database/sql"
	"errors"
	"html"
	"slices"
	"strings"
	"time"
	"unicode"
)

var ErrInvalidMetadataField = errors.New("invalid metadata field")

type MetadataField string

const (
	FieldTitle         MetadataField = "title"
	FieldAuthors       MetadataField = "authors"
	FieldPublisher     MetadataField = "publisher"
	FieldPublishedDate MetadataField = "published_date"
	FieldLanguage      MetadataField = "language"
	FieldISBN          MetadataField = "isbn"
	FieldDescription   MetadataField = "description"
	FieldCover         MetadataField = "cover"
)

// metadataFieldOrder is every metadata field in one fixed order. Where a
// field set has to be reported rather than merely applied — the list
// ApplyEnrichedFields hands back for the "added publisher, description"
// line — iterating this instead of ranging over the caller's map keeps the
// same input reading the same way twice, since Go's map order is
// deliberately random. internal/enrich's resolver keeps the same order for
// the same reason.
var metadataFieldOrder = []MetadataField{
	FieldTitle,
	FieldAuthors,
	FieldPublisher,
	FieldPublishedDate,
	FieldLanguage,
	FieldISBN,
	FieldDescription,
	FieldCover,
}

func isKnownMetadataField(field MetadataField) bool {
	return slices.Contains(metadataFieldOrder, field)
}

var metadataFields = map[string]MetadataField{
	string(FieldTitle):         FieldTitle,
	string(FieldAuthors):       FieldAuthors,
	string(FieldPublisher):     FieldPublisher,
	string(FieldPublishedDate): FieldPublishedDate,
	string(FieldLanguage):      FieldLanguage,
	string(FieldISBN):          FieldISBN,
	string(FieldDescription):   FieldDescription,
}

// ParseMetadataField turns a request-supplied field name into a
// MetadataField. It deliberately does not accept "cover": the map is the
// gate on internal/web's per-field edit routes and internal/service's
// UpdateBookMetadata, and cover_path holds a path internal/cover.Store
// produced rather than anything a person types. Accepting it there would
// make /books/{id}/metadata/cover a route that parses, renders an empty
// field fragment and then 500s on save, instead of the 404 a name nobody
// may edit deserves. FieldCover still exists as a constant — field_sources
// records it and ApplyEnrichedFields writes it — it is just never parsed
// from a request.
func ParseMetadataField(value string) (MetadataField, bool) {
	field, ok := metadataFields[value]
	return field, ok
}

// leadingArticles are stripped from a title to derive its sort form, longest
// first so "An Ideal Husband" doesn't lose only its "A".
//
// English only. Guessing articles across languages ("Der", "La", "El")
// mis-files any title that legitimately starts with one of those words, and
// the collection is mostly English.
var leadingArticles = []string{"the ", "an ", "a "}

// SortTitle derives the form a title files under: one leading article
// removed and the rest case folded, so "The Hobbit" sorts under H and
// "apple book" sorts among the A's rather than after every capitalised
// title. Punctuation and digits are deliberately left alone — "'Salem's
// Lot" and "1984" file under "'" and "1", which is the "before A" bucket a
// reader scanning alphabetically expects.
//
// It lives here, not in internal/scanner, because two callers now derive
// the same column: the scanner on first sight of a file, and UpdateBookField
// when a title is edited. A second copy of this rule is a library that
// sorts differently depending on how a title arrived.
func SortTitle(title string) string {
	trimmed := strings.TrimSpace(title)

	stripped := trimmed
	for _, article := range leadingArticles {
		// Strictly longer, so a title that *is* an article ("A") keeps it.
		if len(stripped) > len(article) && strings.EqualFold(stripped[:len(article)], article) {
			stripped = strings.TrimSpace(stripped[len(article):])
			break
		}
	}

	// Never return empty: a blank sort_title files the book above the
	// whole library.
	if stripped == "" {
		return strings.ToLower(trimmed)
	}
	return strings.ToLower(stripped)
}

// Field length limits, in bytes of UTF-8 rather than runes: they exist to
// bound what reaches these columns, and bytes are the unit the database and
// an HTTP request body are both measured in.
//
// They live here, below every writer, because three writers cap the same
// columns and all three have to agree: internal/service for a person's
// edit, internal/enrich for a provider's answer, and internal/scanner for
// what a file had embedded in it. A value one writer stores but another's
// validation would reject is a field the app can no longer edit — opening
// the editor and pressing Save unchanged fails on a value nobody typed
const (
	MaxTitleBytes       = 1024
	MaxAuthorNameBytes  = 1024
	MaxScalarBytes      = 4096
	MaxDescriptionBytes = 64 * 1024
	MaxAuthors          = 100
)

// NormalizeISBN returns the first ISBN-shaped run in raw as bare digits
// with an upper-cased check digit, or "" when raw holds none.
//
// One derivation for every reader of an ISBN — internal/epub's three
// identifier forms, internal/fb2's <isbn>, and both providers on the way
// into a lookup — because the value is the key the whole provider chain is
// asked with, it is what the detail page shows, and nothing reconsiders it
// once the field is filled. A publisher's "ISBN 978-0-00-000000-0 (ebook)"
// stored as written is a lookup nobody answers.
//
// An "ISBN" or "urn:isbn:" marker is stripped first, groups may be
// separated by single hyphens or spaces, and a trailing X counts only as an
// ISBN-10's tenth character. Text before or after the run is ignored, so
// "ISBN 978-0-00-000000-0 (ebook)" yields the digits and "Not available"
// yields nothing.
//
// Every caller reads a slot that already claims to hold an ISBN — an
// opf:scheme="ISBN" or urn:isbn: identifier, FB2's <isbn> element, a
// provider's ISBN array — so a run found there is an ISBN by declaration and
// needs no corroborating shape. internal/epub's bare branch is the one
// reader with no such claim, and it holds its own guard (bareISBN) rather
// than making every other caller pay for it
//
// The check digit is deliberately not validated: a malformed ISBN in a file
// is still the best identifier it offers, and a wrong check digit still
// keys a provider lookup that answers no-match cleanly
func NormalizeISBN(raw string) string {
	rest := strings.TrimSpace(cutISBNMarker(strings.TrimSpace(raw)))

	for i := 0; i < len(rest); {
		if !isISBNRunByte(rest[i]) {
			i++
			continue
		}
		end := i
		for end < len(rest) && isISBNRunByte(rest[end]) {
			end++
		}
		if isbn, ok := isbnFromRun(rest[i:end]); ok {
			return isbn
		}
		i = end
	}
	return ""
}

// cutISBNMarker removes an ISBN marker from the front of v, so the digits
// behind it are not read as part of the run
func cutISBNMarker(v string) string {
	for _, marker := range []string{"urn:isbn:", "isbn:", "isbn"} {
		if len(v) >= len(marker) && strings.EqualFold(v[:len(marker)], marker) {
			return v[len(marker):]
		}
	}
	return v
}

// isISBNRunByte reports whether c can appear inside an ISBN-shaped run.
// Membership is deliberately wider than the grammar isbnFromRun accepts, so
// that a run is always maximal: "030640615X7" is one eleven-character run
// that is refused, not a valid ISBN-10 with a stray digit after it
func isISBNRunByte(c byte) bool {
	return c >= '0' && c <= '9' || c == '-' || c == ' ' || c == 'X' || c == 'x'
}

// isbnFromRun validates one maximal run and returns it as bare digits
func isbnFromRun(run string) (string, bool) {
	trimmed := strings.Trim(run, "- ")

	var digits strings.Builder
	for i := 0; i < len(trimmed); i++ {
		switch c := trimmed[i]; {
		case c >= '0' && c <= '9':
			digits.WriteByte(c)
		case c == '-' || c == ' ':
			// A separator only ever sits between two characters of the
			// run — trailing ones are already gone — so a doubled one is
			// not an ISBN's grouping
			if next := trimmed[i+1]; next == '-' || next == ' ' {
				return "", false
			}
		default:
			// X, in the one position an ISBN-10's check digit occupies
			if digits.Len() != 9 || i != len(trimmed)-1 {
				return "", false
			}
			digits.WriteByte('X')
		}
	}

	isbn := digits.String()
	return isbn, len(isbn) == 10 || len(isbn) == 13
}

// blockTags are the tags whose boundary is a line break in the plain text
// a description column holds. The markup a blurb arrives with is shallow —
// paragraphs, line breaks and the odd list — so the rest carry no structure
// worth preserving and are simply dropped
var blockTags = map[string]bool{
	"br": true, "p": true, "div": true, "li": true, "tr": true, "h1": true,
	"h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
}

// PlainDescription renders an HTML-formatted description as the plain text
// books.description holds: a block tag becomes a line break, every other tag
// is dropped, and a '<' that starts nothing tag-shaped is left where it is.
//
// One derivation for every reader that can be handed markup — internal/epub,
// where dc:description legally holds escaped HTML, and internal/googlebooks,
// whose Volumes API documents volumeInfo.description as HTML ("simple
// formatting elements, such as b, i and br tags"). Nothing downstream renders
// a description as markup: html/template escapes the detail page's, so a tag
// left in shows a reader a literal "<p>" and the edit textarea then offers
// them the same markup to hand-fix.
//
// internal/enrich's sanitizeValue deliberately does not call it. Open
// Library's description is plain to begin with, and a blanket strip across
// every provider would answer a question that source never asks.
//
// Entities are unescaped only after the tags are gone, so text that was
// itself escaped markup ("&lt;b&gt;") survives as the literal characters an
// author wrote rather than being stripped as a tag
func PlainDescription(raw string) string {
	if !strings.ContainsAny(raw, "<&") {
		// Trimmed even on the fast path, so every return from this
		// function is trimmed. A caller testing the result against "" to
		// decide whether a source said anything — internal/googlebooks'
		// enrichVolume does — would otherwise read a description of "   "
		// as an answer and overwrite a real one with whitespace that
		// internal/enrich's sanitizeValue then trims to nothing, losing the
		// field outright
		return trimBlank(raw)
	}

	var b strings.Builder
	b.Grow(len(raw))
	for i := 0; i < len(raw); {
		if raw[i] != '<' {
			b.WriteByte(raw[i])
			i++
			continue
		}
		// A '<' that starts nothing tag-shaped is a character in the
		// description, not markup: "a < b" must survive intact
		end, name := tagAt(raw, i)
		if end < 0 {
			b.WriteByte(raw[i])
			i++
			continue
		}
		if blockTags[name] {
			b.WriteByte('\n')
		}
		i = end
	}

	text := html.UnescapeString(b.String())
	return trimBlank(collapseBlankLines(text))
}

// zeroWidth are the characters that carry no ink and that unicode.IsSpace
// does not call space, so strings.TrimSpace leaves them behind. A
// description of nothing but one of them — "&#8203;" unescapes to exactly
// that — would otherwise read as an answer to every "is this empty" test
// between here and the column, and overwrite a real description with a
// value that renders as nothing
const zeroWidth = "\u200b\u200c\u200d\ufeff"

// trimBlank is strings.TrimSpace widened to the zero-width characters, so
// "blank" here means "renders as nothing" rather than "is Unicode
// whitespace"
//
// Composed with unicode.IsSpace rather than written as a cutset, which is
// the distinction that matters: strings.Trim with a hand-listed cutset is
// not TrimSpace, and spelling out the ASCII spaces plus a couple of
// favourites drops the other seventeen runes IsSpace accepts — U+3000, the
// ordinary CJK ideographic space, among them. Widening a trim by narrowing
// it is an easy trade to make by accident, and this library holds Chinese
// and Japanese books.
func trimBlank(s string) string {
	return strings.TrimFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune(zeroWidth, r)
	})
}

// tagAt reports the index just past the tag starting at raw[i] (which the
// caller has already checked is '<') along with its lower-cased name, or
// -1 when what follows is not tag-shaped
func tagAt(raw string, i int) (int, string) {
	j := i + 1
	if j < len(raw) && raw[j] == '/' {
		j++
	}
	start := j
	for j < len(raw) && isTagNameByte(raw[j]) {
		j++
	}
	if j == start {
		return -1, ""
	}
	name := strings.ToLower(raw[start:j])
	for ; j < len(raw); j++ {
		if raw[j] == '>' {
			return j + 1, name
		}
	}
	// An unterminated '<' runs to the end of the string, which is a
	// truncated description rather than a tag
	return -1, ""
}

func isTagNameByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// collapseBlankLines caps a run of newlines at two, since an opening and a
// closing block tag each contribute one and a paragraph break needs only
// the pair
func collapseBlankLines(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	newlines := 0
	for i := 0; i < len(text); i++ {
		if text[i] == '\n' {
			newlines++
			if newlines > 2 {
				continue
			}
		} else {
			newlines = 0
		}
		b.WriteByte(text[i])
	}
	return b.String()
}

func setFieldSourceTx(ctx context.Context, tx *sql.Tx, bookID int64, field MetadataField, source string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO field_sources (book_id, field, source)
		VALUES (?, ?, ?)
		ON CONFLICT(book_id, field) DO UPDATE SET source = excluded.source`, bookID, field, source)
	return err
}

// scalarFieldValues maps each scalar metadata field to the value b holds
// for it. Shared so provenance-writing and provenance-reading callers
// cannot disagree about which column a field names; cover is absent, since
// its value is a path rather than metadata and no caller here may write it.
func scalarFieldValues(b Book) map[MetadataField]string {
	return map[MetadataField]string{
		FieldTitle:         b.Title,
		FieldPublisher:     b.Publisher,
		FieldPublishedDate: b.PublishedDate,
		FieldLanguage:      b.Language,
		FieldISBN:          b.ISBN,
		FieldDescription:   b.Description,
	}
}

func setEmbeddedFieldSourcesTx(ctx context.Context, tx *sql.Tx, bookID int64, b Book, authorNames []string) error {
	for field, value := range scalarFieldValues(b) {
		if value != "" {
			if err := setFieldSourceTx(ctx, tx, bookID, field, "embedded"); err != nil {
				return err
			}
		}
	}
	if len(authorNames) > 0 {
		return setFieldSourceTx(ctx, tx, bookID, FieldAuthors, "embedded")
	}
	return nil
}

// updateBookColumnTx writes value to the single books column field backs,
// bumping modified_at. It does not touch field_sources or books_fts —
// those are the caller's job, so a caller writing several fields in one
// transaction (ApplyEnrichedFields) can sync the FTS row once at the end
// rather than once per field. It must be called from inside a DB.Write
// callback — see DB.Write's contract.
func updateBookColumnTx(ctx context.Context, tx *sql.Tx, bookID int64, field MetadataField, value string, modifiedAt time.Time) error {
	var err error
	switch field {
	case FieldTitle:
		_, err = tx.ExecContext(ctx, `UPDATE books SET title = ?, sort_title = ?, modified_at = ? WHERE id = ?`, value, SortTitle(value), formatTime(modifiedAt), bookID)
	case FieldPublisher:
		_, err = tx.ExecContext(ctx, `UPDATE books SET publisher = ?, modified_at = ? WHERE id = ?`, value, formatTime(modifiedAt), bookID)
	case FieldPublishedDate:
		_, err = tx.ExecContext(ctx, `UPDATE books SET published_date = ?, modified_at = ? WHERE id = ?`, value, formatTime(modifiedAt), bookID)
	case FieldLanguage:
		_, err = tx.ExecContext(ctx, `UPDATE books SET language = ?, modified_at = ? WHERE id = ?`, value, formatTime(modifiedAt), bookID)
	case FieldISBN:
		_, err = tx.ExecContext(ctx, `UPDATE books SET isbn = ?, modified_at = ? WHERE id = ?`, value, formatTime(modifiedAt), bookID)
	case FieldDescription:
		_, err = tx.ExecContext(ctx, `UPDATE books SET description = ?, modified_at = ? WHERE id = ?`, value, formatTime(modifiedAt), bookID)
	case FieldCover:
		// cover_retry is cleared with the path, exactly as
		// UpdateBookCoverPath does: it marks "a cover store failed, try
		// again next sweep", and a book that now has a cover has nothing
		// left to retry. Leaving it set would make the scanner skip its
		// stat check and act with no evidence about the file — re-extracting
		// the embedded cover over this one for a book that has one, and
		// forgetting a perfectly good cover for a book that does not.
		_, err = tx.ExecContext(ctx, `UPDATE books SET cover_path = ?, cover_retry = 0, modified_at = ? WHERE id = ?`, value, formatTime(modifiedAt), bookID)
	default:
		return ErrInvalidMetadataField
	}
	return err
}

func (db *DB) UpdateBookField(ctx context.Context, bookID int64, field MetadataField, value string, modifiedAt time.Time) (exists bool, err error) {
	// authors has no column of its own (see UpdateBookAuthors); cover_path
	// holds a path internal/cover.Store produced, not text a person types,
	// so it is only ever written by ApplyEnrichedFields.
	if field == FieldAuthors || field == FieldCover {
		return false, ErrInvalidMetadataField
	}
	err = db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM books WHERE id = ?)`, bookID).Scan(&exists); err != nil || !exists {
			return err
		}
		if err := updateBookColumnTx(ctx, tx, bookID, field, value, modifiedAt); err != nil {
			return err
		}
		if err := setFieldSourceTx(ctx, tx, bookID, field, "manual"); err != nil {
			return err
		}
		return syncBookFTSTx(ctx, tx, bookID)
	})
	return exists, err
}

// updateBookAuthorsTx replaces bookID's whole author list and records
// source as the provenance for FieldAuthors — "manual" from
// UpdateBookAuthors, a provider's name from ApplyEnrichedFields. It does
// not sync books_fts; see updateBookColumnTx for why that's the caller's
// job. It must be called from inside a DB.Write callback — see DB.Write's
// contract.
func updateBookAuthorsTx(ctx context.Context, tx *sql.Tx, bookID int64, names []string, source string, modifiedAt time.Time) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM book_authors WHERE book_id = ?`, bookID); err != nil {
		return err
	}
	// Deduplicated here, not just by the caller: book_authors is keyed
	// on (book_id, author_id), so a name credited twice would violate
	// the primary key and roll the whole update back. createBookTx
	// applies the same first-occurrence rule, and this is the operation
	// that mirrors it — a caller must not be able to fail the write with
	// input the creation path accepts. Positions stay contiguous, since
	// they are the order the book lists its authors in, not the index of
	// the submitted line.
	seen := make(map[string]bool, len(names))
	position := 0
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true

		authorID, err := findOrCreateAuthor(ctx, tx, name)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO book_authors (book_id, author_id, position) VALUES (?, ?, ?)`, bookID, authorID, position); err != nil {
			return err
		}
		position++
	}
	if _, err := tx.ExecContext(ctx, `UPDATE books SET modified_at = ? WHERE id = ?`, formatTime(modifiedAt), bookID); err != nil {
		return err
	}
	return setFieldSourceTx(ctx, tx, bookID, FieldAuthors, source)
}

func (db *DB) UpdateBookAuthors(ctx context.Context, bookID int64, names []string, modifiedAt time.Time) (exists bool, err error) {
	err = db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM books WHERE id = ?)`, bookID).Scan(&exists); err != nil || !exists {
			return err
		}
		if err := updateBookAuthorsTx(ctx, tx, bookID, names, "manual", modifiedAt); err != nil {
			return err
		}
		return syncBookFTSTx(ctx, tx, bookID)
	})
	return exists, err
}

// ClearProviderCover forgets a provider-supplied cover whose stored file has
// gone: it empties cover_path, clears cover_retry and deletes the cover row
// from field_sources, so the book reads as having no cover and the next
// enrichment run offers to fetch one again.
//
// It reports false when the book is gone, and equally when its cover_path is
// no longer observedPath — the value the caller saw before deciding. Both
// are "nothing to do here" rather than errors, the finders'
// absent-isn't-an-error contract.
//
// cover_retry is cleared alongside the path — see updateBookColumnTx's
// FieldCover branch, which owns that pairing and is what this composes.
// Here it matters twice over: the marker makes the scanner skip its stat
// check entirely, so a book left carrying it would reach the forget branch
// again on the next sweep with no evidence its file had gone.
//
// Deleting the field_sources row is not incidental either: a row naming a
// provider beside an empty cover_path is a claim about a value that no
// longer exists, and it is what enrich.Resolve's isMissing would consult.
//
// It deliberately does not check provenance itself. The caller has already
// read field_sources to decide it is looking at a provider's cover, and a
// method re-deriving that decision would either duplicate the predicate or
// invite a second, differently-wrong copy of it — see internal/scanner's
// maybeRegenerateCover for why "a row exists" is the whole test.
func (db *DB) ClearProviderCover(ctx context.Context, bookID int64, observedPath string, at time.Time) (cleared bool, err error) {
	err = db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		cleared, err = forgetCoverTx(ctx, tx, bookID, observedPath, at)
		return err
	})
	return cleared, err
}

// RecordUnusableCover records that a book's embedded cover can never be
// stored: cover.Store refused the image itself (cover.ErrUnsupportedCover)
// rather than the filesystem, so storing the same bytes again fails the same
// way. It writes the state a book with no embedded cover has — cover_path
// empty, cover_retry clear — and the scanner's first guard then passes the
// book by on every later sweep instead of re-parsing it to reach the same
// refusal. The retry marker is the point: it was designed for a store that
// failed on I/O, and left set here it re-opens the loop this ends.
//
// It is ClearProviderCover's write under a second name, guard included, and
// not by coincidence: a book reaches this with either the marker set and no
// cover at all, or a provider cover whose file has gone and an embedded
// original that cannot replace it — and in the second case the provider's
// row beside an empty cover_path is the same stale claim ClearProviderCover
// removes. The guard on observedPath matters here for the same reason too:
// the scanner re-parses the whole book between reading its snapshot and
// writing, and an enrichment run finishing inside that window has a fresh
// cover this must not blank.
//
// recorded is false when the book is gone or its cover_path has moved on
// from observedPath, the finders' absent-isn't-an-error contract
func (db *DB) RecordUnusableCover(ctx context.Context, bookID int64, observedPath string, at time.Time) (recorded bool, err error) {
	err = db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		recorded, err = forgetCoverTx(ctx, tx, bookID, observedPath, at)
		return err
	})
	return recorded, err
}

// forgetCoverTx is the one write ClearProviderCover and RecordUnusableCover
// share: guarded on the cover_path the caller observed, it empties the path,
// clears cover_retry and deletes the cover's field_sources row. It reports
// false, and writes nothing, when the guard refuses
func forgetCoverTx(ctx context.Context, tx *sql.Tx, bookID int64, observedPath string, at time.Time) (bool, error) {
	// Guarded on the path the caller actually looked at, not merely on the
	// book existing. The scanner decides to clear from a snapshot and then
	// does a stat, a full EPUB/FB2 parse and a provenance round trip before
	// this write lands; in that window an enrichment run can write a fresh
	// cover and its row, and an unguarded blank would throw it away. Same
	// staleness fieldIsStillMissingTx closes for every other field — no
	// reason for this one to opt out
	var matches bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM books WHERE id = ? AND cover_path = ?)`,
		bookID, observedPath).Scan(&matches); err != nil || !matches {
		return false, err
	}
	// Through the shared helper rather than a second copy of its statement:
	// its FieldCover branch is this write with a value bound, and it already
	// carries the cover_retry pairing that would otherwise be stated in two
	// places and drift
	if err := updateBookColumnTx(ctx, tx, bookID, FieldCover, "", at); err != nil {
		return false, err
	}
	_, err := tx.ExecContext(ctx,
		`DELETE FROM field_sources WHERE book_id = ? AND field = ?`, bookID, FieldCover)
	return err == nil, err
}
