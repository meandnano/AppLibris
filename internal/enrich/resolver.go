package enrich

import (
	"context"
	"log/slog"
	"strings"

	"library/internal/storage"
)

// authorsJoin joins a resolved author list into the single string
// Resolve's values map carries for storage.FieldAuthors. It must match
// storage's own authorsSeparator — internal/storage.ApplyEnrichedFields is
// what splits it back apart — so an author list round-trips through the
// map the same way whether it came from a provider or, via the web
// layer's textarea, a person.
const authorsJoin = "\n"

// fields is every metadata field the resolver considers, in a fixed order
// so a test asserting on missing-set membership doesn't depend on map
// iteration order.
var fields = []storage.MetadataField{
	storage.FieldTitle,
	storage.FieldAuthors,
	storage.FieldPublisher,
	storage.FieldPublishedDate,
	storage.FieldLanguage,
	storage.FieldISBN,
	storage.FieldDescription,
	storage.FieldCover,
}

// isMissing is the rule the whole enrichment step exists to get right —
// docs/notes/enrichment.md's "a cleared field stays manual": a field is worth asking a
// provider for only when it is both empty and not something a person
// deliberately set (including deliberately clearing). Dropping the
// emptiness half means re-enrichment overwrites good embedded metadata
// with a guess; dropping the manual half means a deliberately-cleared
// field gets silently refilled — the exact failure field_sources exists to
// prevent, arriving through the code meant to honour it.
func isMissing(value, source string) bool {
	return value == "" && source != "manual"
}

// scalarValue reads field's current value off book — every field this
// resolver considers except authors, which the caller passes separately
// since it lives in a join table, not a books column.
func scalarValue(book storage.Book, field storage.MetadataField) string {
	switch field {
	case storage.FieldTitle:
		return book.Title
	case storage.FieldPublisher:
		return book.Publisher
	case storage.FieldPublishedDate:
		return book.PublishedDate
	case storage.FieldLanguage:
		return book.Language
	case storage.FieldISBN:
		return book.ISBN
	case storage.FieldDescription:
		return book.Description
	case storage.FieldCover:
		return book.CoverPath
	default:
		return ""
	}
}

// missingFields computes book's missing set per isMissing, over every
// field fields lists. authors is passed separately (see scalarValue) and
// sources is field_sources as it stands for this book — a field absent
// from sources reads as an empty source, which isMissing already treats as
// not-manual.
func missingFields(book storage.Book, authors []string, sources map[storage.MetadataField]string) map[storage.MetadataField]bool {
	missing := map[storage.MetadataField]bool{}
	for _, field := range fields {
		value := scalarValue(book, field)
		if field == storage.FieldAuthors {
			value = strings.Join(authors, authorsJoin)
		}
		if isMissing(value, sources[field]) {
			missing[field] = true
		}
	}
	return missing
}

// sanitizeValue makes a provider's answer safe to store in the column
// field backs: trimmed, capped, and — for every field but description —
// stripped of the line breaks that would break its single-line rendering
// everywhere downstream. An over-long value is truncated on a rune
// boundary rather than dropped: a description cut at 64 KiB is still worth
// having, and the alternative is a book silently keeping nothing because a
// provider was verbose.
//
// ApplyEnrichedFields is a second writer to the columns internal/service's
// normalizeField guards for a person's edit, and a provider's answer never
// passes through that function, so this is the only thing bounding what a
// remote source can store. The caps come from internal/storage, which sits
// below every writer of those columns: a number restated here could drift
// from the one an edit is checked against, and a value this package writes
// but normalizeField would reject is a field the app can no longer edit
func sanitizeValue(field storage.MetadataField, value string) string {
	if field != storage.FieldDescription {
		value = strings.Join(strings.Fields(value), " ")
	} else {
		value = capBlankLines(value)
	}
	value = strings.TrimSpace(value)

	limit := storage.MaxScalarBytes
	switch field {
	case storage.FieldDescription:
		limit = storage.MaxDescriptionBytes
	case storage.FieldTitle:
		limit = storage.MaxTitleBytes
	case storage.FieldAuthors:
		// One name at a time — metadataValues sanitises the list element
		// by element, so this is the per-name limit, not the list's.
		limit = storage.MaxAuthorNameBytes
	}
	if len(value) > limit {
		value = strings.ToValidUTF8(value[:limit], "")
	}
	return value
}

// capBlankLines collapses a run of three or more newlines to two, leaving a
// single newline alone: at most one blank line between paragraphs.
//
// A description is the one field that keeps its line breaks, and
// .detail__description renders them, so what a provider sends is now what a
// reader sees — including the four blank lines a scraped blurb arrives
// with. internal/googlebooks already normalises its own HTML on the way
// out; doing it here as well makes it a property of every provider's value
// rather than of one client, which is where the next provider will need it.
// Open Library's edition descriptions are plain and mostly single-block, so
// this costs nothing there today.
//
// It normalises \r\n first, so a CRLF description is not left with a lone
// carriage return in the middle of a paragraph, and strips each line's
// trailing whitespace before counting. A line of two spaces is a blank
// line to a reader, and `pre-line` collapses the spaces while keeping both
// newlines around them, so without the strip a blurb padded with spaces
// renders exactly the run of blank lines this exists to prevent.
func capBlankLines(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")

	lines := strings.Split(value, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t")
	}
	value = strings.Join(lines, "\n")

	for strings.Contains(value, "\n\n\n") {
		value = strings.ReplaceAll(value, "\n\n\n", "\n\n")
	}
	return value
}

// metadataValues converts a provider's answer into the same
// map[storage.MetadataField]string shape Resolve returns, so merging is a
// matter of copying keys across. An empty field in m — the normal case,
// since most providers answer only some of what they're asked — carries
// through as an empty string and is filtered out by the caller before it
// can overwrite anything.
func metadataValues(m Metadata) map[storage.MetadataField]string {
	// Author names are sanitised individually and re-joined, since
	// authorsJoin is itself a newline: sanitising the joined string would
	// collapse a three-author list into one name.
	//
	// The list is cut at storage.MaxAuthors for the same reason each name
	// is capped: a longer one is a list internal/service would refuse, so
	// keeping it whole would cost the book its editable author field.
	authors := make([]string, 0, len(m.Authors))
	for _, name := range m.Authors {
		if len(authors) == storage.MaxAuthors {
			break
		}
		if name = sanitizeValue(storage.FieldAuthors, name); name != "" {
			authors = append(authors, name)
		}
	}

	values := map[storage.MetadataField]string{
		storage.FieldTitle:         m.Title,
		storage.FieldAuthors:       strings.Join(authors, authorsJoin),
		storage.FieldPublisher:     m.Publisher,
		storage.FieldPublishedDate: m.PublishedDate,
		storage.FieldLanguage:      m.Language,
		storage.FieldISBN:          m.ISBN,
		storage.FieldDescription:   m.Description,
	}
	for field, value := range values {
		if field == storage.FieldAuthors {
			continue
		}
		values[field] = sanitizeValue(field, value)
	}
	return values
}

// Resolution is everything one Resolve call learned: Values, SourceName,
// CoverURL and CoverSource are what it found — see Resolve's own comment
// for how each is filled — while Asked and Failed are what it cost, which
// is what lets the caller tell an empty result meaning "this book is in
// neither catalogue" from one meaning "neither catalogue answered".
type Resolution struct {
	Values      map[storage.MetadataField]string
	SourceName  map[storage.MetadataField]string
	CoverURL    string
	CoverSource string

	// Asked counts providers actually called, not providers configured. A
	// chain that stops early because nothing is left missing did not ask
	// the rest, and counting them would report a run as broader than it
	// was — which matters because the caller's rule is Failed == Asked.
	Asked int
	// Failed counts, of those Asked, how many returned an error.
	Failed int
}

// Resolve decides which of book's metadata fields are missing, asks
// providers for them in order, and merges the answers field by field. It
// takes no database and no clock — everything it needs about the book's
// current state is passed in — which is what makes ordering and merging
// testable without a real provider, per docs/notes/enrichment.md.
//
// If nothing is missing, no provider is called at all. Otherwise providers
// are asked in the given order. Each is asked by ISBN when book has one and
// by title and author otherwise — and, when an ISBN lookup comes back a
// clean no-match, by title as well, on the same provider before the chain
// moves on: a catalogue that does not hold that edition may still hold the
// book. An ISBN lookup that *errors* is not followed up, since a 5xx says
// nothing about whether the ISBN is right.
//
// Only the fields still missing that a provider actually answered are kept,
// each recorded under that provider's Name() in SourceName — a provider
// cannot supply a value for a field that isn't missing, or overwrite a field
// an earlier provider in the same run already answered. Two rules narrow
// that further, and both apply to answers reached by title rather than by
// ISBN, since those are whatever a remote ranking put first for a title that
// is frequently the filename:
//
//   - the answer must clear plausibleMatch (see match.go) before any of it
//     is merged, and one that does not is treated as a no-match — nothing
//     kept, no provenance recorded, no cover taken, and the chain carries
//     on to the next provider;
//   - isbn is never filled from such an answer, which is enforced by
//     dropping it from the missing set as soon as the title path is taken.
//     That one line also lets the early stop fire for a book with no ISBN,
//     so moving it re-opens the write as well as the wasted call.
//
// Once nothing is left missing, the loop stops without calling the
// remaining providers — docs/notes/enrichment.md's "the chain stops early and saves the
// API calls" — which is why a two-provider test where the first answers
// everything must show the second is never called.
//
// A provider returning an error is logged and skipped; the chain continues
// to the next one. That is deliberately not this function's failure to
// report: Resolve itself only fails if it never even gets to ask (nothing
// here can, today, since no database or network call happens inside it),
// and a provider having nothing to say is the ordinary case, not an
// error — the caller decides what job status a partial or empty result
// earns, using Resolution's Asked and Failed to tell the two empty results
// apart.
//
// A cover is handled apart from the other six fields: Values only ever
// carries strings that go straight into a column, and a cover's does not
// exist until the image has been downloaded and passed through
// internal/cover.Store — I/O Resolve deliberately never performs itself,
// per the "no database, no clock" contract above. So a provider's cover
// comes back separately, as CoverURL and CoverSource, for the worker to
// fetch, store and fold into Values once it has a path. It is still
// subject to the same missing-set membership, first-answer-wins and
// early-stop rules as every other field: a book whose cover_path is
// already set never has a cover URL returned for it, so nothing downloads
// an image the book has no use for.
func Resolve(ctx context.Context, book storage.Book, authors []string,
	sources map[storage.MetadataField]string, providers []Provider,
) (Resolution, error) {
	missing := missingFields(book, authors, sources)
	res := Resolution{
		Values:     map[storage.MetadataField]string{},
		SourceName: map[storage.MetadataField]string{},
	}
	if len(missing) == 0 {
		return res, nil
	}

	for _, p := range providers {
		if len(missing) == 0 {
			break
		}

		var (
			answer    Metadata
			perr      error
			viaSearch bool
		)
		// Asked counts providers this run actually called, so it is
		// incremented inside each calling branch rather than above them: a
		// book with neither an ISBN nor a title calls nobody, and counting
		// it would report a run as broader than it was — the invariant the
		// worker's Asked > 0 && Failed == Asked rule rests on.
		if book.ISBN != "" {
			res.Asked++
			answer, perr = p.ByISBN(ctx, book.ISBN)
			// A clean no-match means this catalogue does not hold that
			// edition, and the title is still worth asking about — subject
			// to the gate below. An *error* is not a no-match: it says
			// nothing about the ISBN, and falling back on it would accept
			// a fuzzy answer because a host was briefly unreachable.
			if perr == nil && answer.IsEmpty() && book.Title != "" {
				answer, perr = p.Search(ctx, book.Title, authors)
				viaSearch = true
			}
		} else if book.Title != "" {
			res.Asked++
			answer, perr = p.Search(ctx, book.Title, authors)
			viaSearch = true
		}
		if viaSearch {
			// Dropping isbn from the missing set is what withholds it, and
			// it does two jobs at once — so moving or removing this line
			// re-opens the write, not just the early stop.
			//
			// A search answer must never supply an ISBN, even having passed
			// the gate below. Every other field is a description that is
			// roughly right or roughly wrong; an ISBN is an identifier that
			// either names this book or names a different one, and it is the
			// lookup key every later run would use — so a wrong one compounds
			// instead of sitting still. And because the field can never be
			// filled from here, leaving it in the set would mean the set
			// never empties for a book without an ISBN: every such book would
			// spend a call and a rate-limit token on every remaining provider,
			// on every run, for a field none of them may answer.
			//
			// The consequence is worth stating because it reads as an
			// oversight otherwise: enrichment cannot write isbn by any route.
			// The field is only ever missing for a book that has none, such a
			// book can only reach a provider through Search, and a book that
			// has one does not need it.
			delete(missing, storage.FieldISBN)
		}
		if perr != nil {
			res.Failed++
			slog.Warn("enrichment provider failed", "provider", p.Name(), "book_id", book.ID, "error", perr)
			continue
		}

		// An ISBN names one edition, so a ByISBN answer is about this book
		// by construction. A search answer is whatever the provider's
		// ranking put first for a title that is often the filename, so it
		// has to earn the merge. A rejected answer is treated as no match —
		// the ordinary zero Metadata — so the chain continues to the next
		// provider with the missing set otherwise intact — isbn has already
		// left it above, which is why "intact" is not quite the word.
		if viaSearch && !answer.IsEmpty() {
			if ok, reason := plausibleMatch(book.Title, authors, answer); !ok {
				// The reason matters: an author veto shows two titles that
				// look like a fine match and says nothing about why it was
				// refused, so without it the two rejection causes are
				// indistinguishable in the record.
				slog.Debug("enrichment search answer rejected", "provider", p.Name(),
					"book_id", book.ID, "reason", reason, "query_title", book.Title,
					"candidate_title", answer.Title, "candidate_authors", answer.Authors)
				continue
			}
		}

		before := len(res.Values)
		for field, value := range metadataValues(answer) {
			if !missing[field] || value == "" {
				continue
			}
			res.Values[field] = value
			res.SourceName[field] = p.Name()
			delete(missing, field)
		}

		tookCover := false
		if missing[storage.FieldCover] && answer.CoverURL != "" {
			res.CoverURL = answer.CoverURL
			res.CoverSource = p.Name()
			delete(missing, storage.FieldCover)
			tookCover = true
		}

		// Logged after the merge rather than before it, and only when the
		// answer actually contributed: Decision 5 justifies this line as
		// "the line that explains a field's value", and an accepted answer
		// that filled nothing explains none.
		if viaSearch && (len(res.Values) > before || tookCover) {
			slog.Info("enrichment matched by search", "provider", p.Name(),
				"book_id", book.ID, "query_title", book.Title, "matched_title", answer.Title)
		}
	}
	return res, nil
}
