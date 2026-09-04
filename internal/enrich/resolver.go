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
// DESIGN.md's "a cleared field stays manual": a field is worth asking a
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

// A provider's answer goes into the same columns internal/service's
// normalizeField guards for a person's edit, but never passes through it —
// ApplyEnrichedFields is a second writer to those columns. These bound
// what a remote source can put there, mirroring that function's own limits
// rather than importing them: internal/service sits above this package,
// and a background worker asking it to validate would invert the layering.
// They have to match internal/service's numbers, not merely be of the same
// kind: a value this package writes but that one would reject is a field
// the app itself can no longer edit — opening the editor and pressing Save
// unchanged fails validation on a value nobody typed.
const (
	maxEnrichedScalarBytes      = 4096
	maxEnrichedTitleBytes       = 1024
	maxEnrichedAuthorNameBytes  = 1024
	maxEnrichedAuthors          = 100
	maxEnrichedDescriptionBytes = 64 * 1024
)

// sanitizeValue makes a provider's answer safe to store in the column
// field backs: trimmed, capped, and — for every field but description —
// stripped of the line breaks that would break its single-line rendering
// everywhere downstream. An over-long value is truncated on a rune
// boundary rather than dropped: a description cut at 64 KiB is still worth
// having, and the alternative is a book silently keeping nothing because a
// provider was verbose.
func sanitizeValue(field storage.MetadataField, value string) string {
	if field != storage.FieldDescription {
		value = strings.Join(strings.Fields(value), " ")
	}
	value = strings.TrimSpace(value)

	limit := maxEnrichedScalarBytes
	switch field {
	case storage.FieldDescription:
		limit = maxEnrichedDescriptionBytes
	case storage.FieldTitle:
		limit = maxEnrichedTitleBytes
	case storage.FieldAuthors:
		// One name at a time — metadataValues sanitises the list element
		// by element, so this is the per-name limit, not the list's.
		limit = maxEnrichedAuthorNameBytes
	}
	if len(value) > limit {
		value = strings.ToValidUTF8(value[:limit], "")
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
	// The list is cut at maxEnrichedAuthors for the same reason each name
	// is capped: a longer one is a list internal/service would refuse, so
	// keeping it whole would cost the book its editable author field.
	authors := make([]string, 0, len(m.Authors))
	for _, name := range m.Authors {
		if len(authors) == maxEnrichedAuthors {
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

// Resolve decides which of book's metadata fields are missing, asks
// providers for them in order, and merges the answers field by field. It
// takes no database and no clock — everything it needs about the book's
// current state is passed in — which is what makes ordering and merging
// testable without a real provider, per DESIGN.md.
//
// If nothing is missing, no provider is called at all. Otherwise providers
// are asked in the given order; each is asked by ISBN when book has one,
// by title and author otherwise. Only the fields still missing that a
// provider actually answered are kept, each recorded under that provider's
// Name() in sourceName — a provider cannot supply a value for a field that
// isn't missing, or overwrite a field an earlier provider in the same run
// already answered. Once nothing is left missing, the loop stops without
// calling the remaining providers — DESIGN.md's "the chain stops early and
// saves the API calls" — which is why a two-provider test where the first
// answers everything must show the second is never called.
//
// A provider returning an error is logged and skipped; the chain continues
// to the next one. That is deliberately not this function's failure to
// report: Resolve itself only fails if it never even gets to ask (nothing
// here can, today, since no database or network call happens inside it),
// and a provider having nothing to say is the ordinary case, not an
// error — the caller decides what job status a partial or empty result
// earns.
//
// A cover is handled apart from the other six fields: values only ever
// carries strings that go straight into a column, and a cover's does not
// exist until the image has been downloaded and passed through
// internal/cover.Store — I/O Resolve deliberately never performs itself,
// per the "no database, no clock" contract above. So a provider's cover
// comes back separately, as coverURL and coverSource, for the worker to
// fetch, store and fold into values once it has a path. It is still
// subject to the same missing-set membership, first-answer-wins and
// early-stop rules as every other field: a book whose cover_path is
// already set never has a cover URL returned for it, so nothing downloads
// an image the book has no use for.
func Resolve(ctx context.Context, book storage.Book, authors []string,
	sources map[storage.MetadataField]string, providers []Provider,
) (values map[storage.MetadataField]string, sourceName map[storage.MetadataField]string, coverURL string, coverSource string, err error) {
	missing := missingFields(book, authors, sources)
	values = map[storage.MetadataField]string{}
	sourceName = map[storage.MetadataField]string{}
	if len(missing) == 0 {
		return values, sourceName, "", "", nil
	}

	for _, p := range providers {
		if len(missing) == 0 {
			break
		}

		var (
			answer Metadata
			perr   error
		)
		if book.ISBN != "" {
			answer, perr = p.ByISBN(ctx, book.ISBN)
		} else {
			answer, perr = p.Search(ctx, book.Title, authors)
		}
		if perr != nil {
			slog.Warn("enrichment provider failed", "provider", p.Name(), "book_id", book.ID, "error", perr)
			continue
		}

		for field, value := range metadataValues(answer) {
			if !missing[field] || value == "" {
				continue
			}
			values[field] = value
			sourceName[field] = p.Name()
			delete(missing, field)
		}

		if missing[storage.FieldCover] && answer.CoverURL != "" {
			coverURL = answer.CoverURL
			coverSource = p.Name()
			delete(missing, storage.FieldCover)
		}
	}
	return values, sourceName, coverURL, coverSource, nil
}
