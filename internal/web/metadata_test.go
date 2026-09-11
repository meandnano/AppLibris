package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"library/internal/service"
	"library/internal/storage"
)

func newEditTestDB(t *testing.T) *storage.DB {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func createEditTestBook(t *testing.T, db *storage.DB, book storage.Book, authors []string) int64 {
	t.Helper()
	if book.ContentHash == "" {
		book.ContentHash = "hash-edit"
	}
	id, err := db.CreateBook(context.Background(), book, authors)
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	return id
}

func postField(handler http.Handler, id int64, field string, form url.Values, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/books/"+itoa(id)+"/metadata/"+field, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

var htmx = map[string]string{"HX-Request": "true"}

func TestMetadataHTMXEditThenSave(t *testing.T) {
	db := newEditTestDB(t)
	id := createEditTestBook(t, db, storage.Book{Title: "Old Title", Format: "epub"}, nil)
	handler := Routes(service.New(db), t.TempDir(), false, false)

	req := httptest.NewRequest(http.MethodGet, "/books/"+itoa(id)+"/metadata/title?edit=1", nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("edit fragment = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `id="book-field-title"`) || !strings.Contains(body, `name="value" value="Old Title"`) {
		t.Fatalf("edit fragment is not the title editor: %q", body)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store — an editor holding a stale value re-submits it",
			rec.Header().Get("Cache-Control"))
	}

	rec = postField(handler, id, "title", url.Values{"value": {"The New Title"}}, htmx)
	body = rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("save = %d, want 200; body = %q", rec.Code, body)
	}
	if strings.Contains(body, "<form") {
		t.Errorf("a saved field came back as an editor rather than a read view: %q", body)
	}
	if !strings.Contains(body, "The New Title") {
		t.Errorf("saved fragment does not show the new title: %q", body)
	}

	book, err := db.FindBookByID(context.Background(), id)
	if err != nil {
		t.Fatalf("FindBookByID: %v", err)
	}
	if book.Title != "The New Title" {
		t.Errorf("Title = %q, want The New Title", book.Title)
	}
	// The sort form is derived, not copied: the article goes, the display
	// title keeps it.
	if book.SortTitle != "new title" {
		t.Errorf("SortTitle = %q, want %q", book.SortTitle, "new title")
	}
}

func TestMetadataRejectedValueComesBackAsAnEditor(t *testing.T) {
	db := newEditTestDB(t)
	id := createEditTestBook(t, db, storage.Book{Title: "Keep Me", Format: "epub"}, nil)
	handler := Routes(service.New(db), t.TempDir(), false, false)

	rec := postField(handler, id, "title", url.Values{"value": {"   "}}, htmx)
	body := rec.Body.String()
	// 200 on purpose: htmx 2.0.10 does not swap a 4xx, so a rejected
	// fragment answered with 422 would leave the editor untouched and make
	// Save look like it did nothing. See metadataError.
	if rec.Code != http.StatusOK {
		t.Errorf("blank title fragment = %d, want 200 — htmx will not swap a 4xx", rec.Code)
	}
	if !strings.Contains(body, "Title is required") || !strings.Contains(body, "<form") {
		t.Errorf("rejected value did not come back as an editor with its message: %q", body)
	}

	book, _ := db.FindBookByID(context.Background(), id)
	if book.Title != "Keep Me" {
		t.Errorf("a rejected edit changed the stored title to %q", book.Title)
	}
}

func TestMetadataAcceptsFullSizeNonASCIIDescription(t *testing.T) {
	db := newEditTestDB(t)
	id := createEditTestBook(t, db, storage.Book{Title: "Книга", Format: "fb2"}, nil)
	handler := Routes(service.New(db), t.TempDir(), false, false)

	// Form-urlencoding turns each of these two-byte runes into six bytes,
	// so a value comfortably inside the service's byte limit produces a
	// request body several times its size. The body cap has to be derived
	// from the value limit, or a normal-length Russian synopsis is
	// rejected as "too large" at well under a third of the stated size.
	description := strings.TrimSpace(strings.Repeat("Это описание книги. ", 1500))
	if len(description) > service.MaxDescriptionBytes {
		t.Fatalf("test description is %d bytes, over the %d-byte limit it is meant to sit under",
			len(description), service.MaxDescriptionBytes)
	}
	encoded := len(url.Values{"value": {description}}.Encode())
	if encoded <= 70*1024 {
		t.Fatalf("encoded body is only %d bytes; this test no longer exercises a body far larger than its value", encoded)
	}

	rec := postField(handler, id, "description", url.Values{"value": {description}}, htmx)
	if rec.Code != http.StatusOK {
		t.Fatalf("description of %d bytes (%d encoded) = %d, want 200; body = %q",
			len(description), encoded, rec.Code, rec.Body.String())
	}

	book, _ := db.FindBookByID(context.Background(), id)
	if book.Description != description {
		t.Errorf("stored description is %d bytes, want %d", len(book.Description), len(description))
	}
}

func TestMetadataOverlongValueIsAFieldErrorNotABareStatus(t *testing.T) {
	db := newEditTestDB(t)
	id := createEditTestBook(t, db, storage.Book{Title: "Book", Format: "epub"}, nil)
	handler := Routes(service.New(db), t.TempDir(), false, false)

	// Past the body cap, so it never reaches the service's own check.
	rec := postField(handler, id, "description",
		url.Values{"value": {strings.Repeat("x", maxMetadataFormBody+1)}}, htmx)
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Errorf("oversized body fragment = %d, want 200", rec.Code)
	}
	if !strings.Contains(body, "editable__error") || !strings.Contains(body, "<form") {
		t.Errorf("oversized body did not come back through the form: %q", body)
	}
}

func TestMetadataAuthorsRoundTripThroughTheTextarea(t *testing.T) {
	db := newEditTestDB(t)
	id := createEditTestBook(t, db, storage.Book{Title: "Book", Format: "epub"}, []string{"Old Author"})
	handler := Routes(service.New(db), t.TempDir(), false, false)

	rec := postField(handler, id, "authors", url.Values{"value": {" First \r\nSecond\n\nFirst "}}, htmx)
	if rec.Code != http.StatusOK {
		t.Fatalf("save authors = %d; body = %q", rec.Code, rec.Body.String())
	}
	// Blank lines dropped, names trimmed, a repeat linked once, order kept.
	if !strings.Contains(rec.Body.String(), "First &amp; Second") {
		t.Errorf("author byline not rendered from the saved list: %q", rec.Body.String())
	}

	authors, err := db.ListAuthorsForBook(context.Background(), id)
	if err != nil {
		t.Fatalf("ListAuthorsForBook: %v", err)
	}
	if len(authors) != 2 || authors[0] != "First" || authors[1] != "Second" {
		t.Errorf("authors = %v, want [First Second]", authors)
	}
}

func TestMetadataNoJavaScriptPathUsesWholePages(t *testing.T) {
	db := newEditTestDB(t)
	id := createEditTestBook(t, db, storage.Book{Title: "Book", Format: "epub"}, nil)
	handler := Routes(service.New(db), t.TempDir(), false, false)

	// The read view's href, followed without htmx: a fragment URL must
	// land on a whole page with that editor open, not a bare fragment.
	req := httptest.NewRequest(http.MethodGet, "/books/"+itoa(id)+"/metadata/description?edit=1", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/books/"+itoa(id)+"?edit=description" {
		t.Fatalf("edit redirect = %d %q", rec.Code, rec.Header().Get("Location"))
	}

	req = httptest.NewRequest(http.MethodGet, "/books/"+itoa(id)+"?edit=description", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `<textarea id="description-value"`) {
		t.Fatalf("full page with the editor open = %d; body = %q", rec.Code, rec.Body.String())
	}

	// And the plain form POST redirects back to the book.
	rec = postField(handler, id, "description", url.Values{"value": {"Set without JS"}}, nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/books/"+itoa(id) {
		t.Errorf("no-JS save = %d %q, want 303 to the book", rec.Code, rec.Header().Get("Location"))
	}
	book, _ := db.FindBookByID(context.Background(), id)
	if book.Description != "Set without JS" {
		t.Errorf("Description = %q", book.Description)
	}
}

func TestMetadataUnknownFieldAndBookAre404(t *testing.T) {
	db := newEditTestDB(t)
	id := createEditTestBook(t, db, storage.Book{Title: "Book", Format: "epub"}, nil)
	handler := Routes(service.New(db), t.TempDir(), false, false)

	if rec := postField(handler, id, "cover_path", url.Values{"value": {"x"}}, htmx); rec.Code != http.StatusNotFound {
		t.Errorf("POST to a field that is not editable = %d, want 404", rec.Code)
	}
	// "cover" is a real storage.MetadataField — field_sources records it and
	// ApplyEnrichedFields writes it — but it is not one a person may edit,
	// so it must not parse here. If it did, this POST would reach
	// UpdateBookField, come back with ErrInvalidMetadataField (which is not
	// service.ErrInvalidMetadata), and answer 500; the GET would render an
	// empty field fragment pointing at /books/0/metadata/.
	if rec := postField(handler, id, "cover", url.Values{"value": {"x"}}, htmx); rec.Code != http.StatusNotFound {
		t.Errorf("POST to the cover field = %d, want 404", rec.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/books/"+itoa(id)+"/metadata/cover", nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET the cover field = %d, want 404", rec.Code)
	}
	if rec := postField(handler, 99999, "title", url.Values{"value": {"x"}}, htmx); rec.Code != http.StatusNotFound {
		t.Errorf("POST for an unknown book = %d, want 404", rec.Code)
	}
}

func TestMetadataUnknownEditQueryStillRendersTheBook(t *testing.T) {
	db := newEditTestDB(t)
	id := createEditTestBook(t, db, storage.Book{Title: "Book", Format: "epub"}, nil)
	handler := Routes(service.New(db), t.TempDir(), false, false)

	req := httptest.NewRequest(http.MethodGet, "/books/"+itoa(id)+"?edit=not-a-field", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unknown edit target = %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "<form method=\"post\"") {
		t.Errorf("an unrecognised edit target opened an editor: %q", rec.Body.String())
	}
}

func TestMetadataRejectsCrossSitePost(t *testing.T) {
	db := newEditTestDB(t)
	id := createEditTestBook(t, db, storage.Book{Title: "Original", Format: "epub"}, nil)
	handler := Routes(service.New(db), t.TempDir(), false, false)

	rec := postField(handler, id, "title", url.Values{"value": {"Vandalised"}},
		map[string]string{"Sec-Fetch-Site": "cross-site"})
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-site metadata POST = %d, want 403", rec.Code)
	}
	book, _ := db.FindBookByID(context.Background(), id)
	if book.Title != "Original" {
		t.Errorf("a cross-site POST rewrote the title to %q", book.Title)
	}
}

func TestMetadataEditIsSearchableImmediately(t *testing.T) {
	db := newEditTestDB(t)
	id := createEditTestBook(t, db, storage.Book{Title: "Piranesi", Format: "epub"}, nil)
	handler := Routes(service.New(db), t.TempDir(), false, false)

	if rec := postField(handler, id, "title", url.Values{"value": {"Jonathan Strange"}}, htmx); rec.Code != http.StatusOK {
		t.Fatalf("save = %d", rec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/?q=Strange", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "Jonathan Strange") {
		t.Errorf("an edited title is not searchable; body = %q", rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/?q=Piranesi", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "Jonathan Strange") {
		t.Errorf("the pre-edit title still matches in search; body = %q", rec.Body.String())
	}
}

func TestMetadataFullPageErrorKeepsThe422(t *testing.T) {
	db := newEditTestDB(t)
	id := createEditTestBook(t, db, storage.Book{Title: "Keep Me", Format: "epub"}, nil)
	handler := Routes(service.New(db), t.TempDir(), false, false)

	// Nothing swallows a status on the navigation path, so the honest code
	// survives there even though the fragment above has to answer 200.
	rec := postField(handler, id, "title", url.Values{"value": {"   "}}, nil)
	body := rec.Body.String()
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("blank title, no JS = %d, want 422", rec.Code)
	}
	if !strings.Contains(body, "Title is required") || !strings.Contains(body, "detail__meta") {
		t.Errorf("no-JS rejection is not the whole page with the message: %q", body)
	}
}

func TestMetadataUnknownBookIs404OnBothPaths(t *testing.T) {
	db := newEditTestDB(t)
	handler := Routes(service.New(db), t.TempDir(), false, false)

	// The redirect must not be chosen before the book is known to exist,
	// or an ordinary GET answers 303 for a book that isn't there while the
	// htmx GET answers 404.
	for _, tc := range []struct {
		name    string
		headers map[string]string
	}{
		{"htmx", htmx},
		{"ordinary", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/books/99999/metadata/title?edit=1", nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Errorf("GET for an unknown book = %d, want 404", rec.Code)
			}
		})
	}
}

func TestMetadataAcceptsAFullSizeAuthorList(t *testing.T) {
	db := newEditTestDB(t)
	id := createEditTestBook(t, db, storage.Book{Title: "Anthology", Format: "epub"}, nil)
	handler := Routes(service.New(db), t.TempDir(), false, false)

	// The author list, not the description, is the largest value the
	// service accepts. A body cap sized off the description limit rejects
	// this before normalizeAuthors ever applies its own limits.
	names := make([]string, 100)
	for i := range names {
		// Near the 1 KiB-per-name limit, in two-byte runes, so the list as
		// a whole lands between a description-sized cap and the real one.
		names[i] = "Автор " + itoa(int64(i)) + " " + strings.Repeat("я", 495)
	}
	value := strings.Join(names, "\n")
	if encoded := len(url.Values{"value": {value}}.Encode()); encoded <= 3*service.MaxDescriptionBytes {
		t.Fatalf("encoded body is %d bytes; this no longer exceeds a description-sized cap", encoded)
	}

	rec := postField(handler, id, "authors", url.Values{"value": {value}}, htmx)
	if rec.Code != http.StatusOK {
		t.Fatalf("full-size author list = %d, want 200; body = %q", rec.Code, rec.Body.String())
	}
	authors, err := db.ListAuthorsForBook(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(authors) != 100 {
		t.Errorf("stored %d authors, want 100", len(authors))
	}
}

func TestMetadataRejectsLineBreaksInSingleLineFields(t *testing.T) {
	db := newEditTestDB(t)
	id := createEditTestBook(t, db, storage.Book{Title: "Book", Format: "epub"}, nil)
	handler := Routes(service.New(db), t.TempDir(), false, false)

	// A browser text input cannot produce these; a scripted request can.
	for _, field := range []string{"title", "publisher", "published", "language", "isbn"} {
		name := field
		if name == "published" {
			name = "published_date"
		}
		rec := postField(handler, id, name, url.Values{"value": {"one\ntwo"}}, htmx)
		if !strings.Contains(rec.Body.String(), "cannot contain line breaks") {
			t.Errorf("%s accepted an embedded newline; body = %q", name, rec.Body.String())
		}
	}

	// Description is the multiline field and must keep its newlines.
	rec := postField(handler, id, "description", url.Values{"value": {"one\ntwo"}}, htmx)
	if rec.Code != http.StatusOK {
		t.Fatalf("multiline description = %d, want 200", rec.Code)
	}
	book, _ := db.FindBookByID(context.Background(), id)
	if book.Description != "one\ntwo" {
		t.Errorf("Description = %q, want the newline preserved", book.Description)
	}
}

func TestMetadataEmptyFieldsHaveDistinctAccessibleNames(t *testing.T) {
	db := newEditTestDB(t)
	id := createEditTestBook(t, db, storage.Book{Title: "Sparse", Format: "epub"}, nil)
	handler := Routes(service.New(db), t.TempDir(), false, false)

	req := httptest.NewRequest(http.MethodGet, "/books/"+itoa(id), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	body := rec.Body.String()

	// With every optional value empty, the visible text of each link is
	// just an em dash — the accessible name is the only thing telling a
	// screen-reader user which field a link edits.
	for _, label := range []string{"Edit title", "Edit authors", "Edit description",
		"Edit publisher", "Edit published", "Edit language", "Edit isbn"} {
		if !strings.Contains(body, `aria-label="`+label+`"`) {
			t.Errorf("missing accessible name %q", label)
		}
	}
}
