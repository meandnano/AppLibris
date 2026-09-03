package service

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"library/internal/storage"
)

func newMetadataTestService(t *testing.T) (*Service, *storage.DB, int64) {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	id, err := db.CreateBook(context.Background(), storage.Book{
		ContentHash: "service-edit", Title: "Old", SortTitle: "old",
	}, nil)
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	return New(db), db, id
}

func TestUpdateBookMetadataNormalizesAndReloads(t *testing.T) {
	svc, _, id := newMetadataTestService(t)

	detail, err := svc.UpdateBookMetadata(context.Background(), id,
		MetadataUpdate{Field: "authors", Value: " First \nSecond\n\nFirst "})
	if err != nil {
		t.Fatalf("UpdateBookMetadata: %v", err)
	}
	// The response is reloaded canonical data, not an echo of the input,
	// so normalization is visible to the caller that rendered it.
	if len(detail.Authors) != 2 || detail.Authors[0] != "First" || detail.Authors[1] != "Second" {
		t.Errorf("Authors = %v, want [First Second]", detail.Authors)
	}
}

func TestUpdateBookMetadataValidation(t *testing.T) {
	svc, db, id := newMetadataTestService(t)
	ctx := context.Background()

	cases := []struct {
		name  string
		field string
		value string
		want  string
	}{
		{"blank title", "title", "   ", "Title is required"},
		{"overlong title", "title", strings.Repeat("x", maxTitleBytes+1), "Title is too long"},
		{"overlong description", "description", strings.Repeat("x", MaxDescriptionBytes+1), "Value is too long"},
		{"overlong publisher", "publisher", strings.Repeat("x", maxScalarBytes+1), "Value is too long"},
		{"unknown field", "cover_path", "x", "Unknown metadata field"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			detail, err := svc.UpdateBookMetadata(ctx, id, MetadataUpdate{Field: tc.field, Value: tc.value})
			if !errors.Is(err, ErrInvalidMetadata) {
				t.Fatalf("error = %v, want ErrInvalidMetadata", err)
			}
			if detail != nil {
				t.Errorf("a rejected update returned a detail: %#v", detail)
			}
			if msg := MetadataValidationMessage(err); !strings.Contains(msg, tc.want) {
				t.Errorf("message = %q, want it to contain %q", msg, tc.want)
			}
		})
	}

	// Nothing was written by any of them.
	book, err := db.FindBookByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if book.Title != "Old" {
		t.Errorf("Title = %q, want Old — a rejected edit must not reach storage", book.Title)
	}
}

func TestUpdateBookMetadataAuthorLimits(t *testing.T) {
	svc, _, id := newMetadataTestService(t)
	ctx := context.Background()

	tooMany := make([]string, maxAuthors+1)
	for i := range tooMany {
		tooMany[i] = "Author " + strings.Repeat("x", i%5+1) + string(rune('a'+i%26)) + itoa(i)
	}
	if _, err := svc.UpdateBookMetadata(ctx, id,
		MetadataUpdate{Field: "authors", Value: strings.Join(tooMany, "\n")}); !errors.Is(err, ErrInvalidMetadata) {
		t.Errorf("too many authors error = %v, want ErrInvalidMetadata", err)
	}

	if _, err := svc.UpdateBookMetadata(ctx, id,
		MetadataUpdate{Field: "authors", Value: strings.Repeat("x", maxAuthorNameBytes+1)}); !errors.Is(err, ErrInvalidMetadata) {
		t.Errorf("overlong author name error = %v, want ErrInvalidMetadata", err)
	}

	// Exactly at the limit is accepted — the check is off-by-one prone.
	atLimit := make([]string, maxAuthors)
	for i := range atLimit {
		atLimit[i] = "Author " + itoa(i)
	}
	detail, err := svc.UpdateBookMetadata(ctx, id, MetadataUpdate{Field: "authors", Value: strings.Join(atLimit, "\n")})
	if err != nil {
		t.Fatalf("%d authors: %v", maxAuthors, err)
	}
	if len(detail.Authors) != maxAuthors {
		t.Errorf("Authors = %d, want %d", len(detail.Authors), maxAuthors)
	}
}

func TestUpdateBookMetadataUnknownBookIsAbsentNotAnError(t *testing.T) {
	svc, _, _ := newMetadataTestService(t)
	for _, field := range []string{"title", "authors"} {
		detail, err := svc.UpdateBookMetadata(context.Background(), 99999,
			MetadataUpdate{Field: field, Value: "Something"})
		if err != nil || detail != nil {
			t.Errorf("update %s for an unknown book = %#v, %v; want nil, nil", field, detail, err)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
