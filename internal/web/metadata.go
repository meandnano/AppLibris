package web

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"library/internal/service"
	"library/internal/storage"
)

// maxMetadataFormBody bounds the request body a metadata edit may post. It
// is derived rather than picked, for two reasons that both bite.
//
// The units differ: the service limits a value in bytes of *decoded* text,
// while this limits the *encoded* body, and form-urlencoding escapes every
// byte outside a small unreserved set as %XX. Cyrillic and CJK text is
// three body bytes per value byte, so a cap set near the decoded limit
// rejects a description at under a third of the advertised size.
//
// And description is not the largest accepted value: an author list is up
// to MaxAuthors names of MaxAuthorNameBytes each, which is larger. Sizing
// off description alone would reject a valid author list before
// normalizeAuthors ever got to apply its own documented limits. The +1KiB
// covers the field name, the separators and a little slack.
const maxMetadataFormBody = 3*service.MaxMetadataValueBytes + 1024

// metadataHandler serves GET and POST /books/{id}/metadata/{field} — one
// field's read view, its edit form, and the submission that saves it.
//
// The GET is htmx-only in practice: without JavaScript the read view's
// link is an ordinary href to /books/{id}?edit={field}, and this route
// redirects there rather than answering a bare fragment, so a fragment URL
// pasted into the address bar lands on a whole page. The POST mirrors it:
// an htmx caller gets the swapped-in field, everyone else a 303 back to
// the book. A rejected value comes back as the form with its message and a
// 422 — never http.Error, which would swap the editor away and lose what
// the user typed.
func metadataHandler(svc *service.Service, sendEnabled, enrichEnabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Vary", "HX-Request, HX-History-Restore-Request")
		// An edit form carries the current value, and a stale one would
		// re-submit data the user already changed.
		w.Header().Set("Cache-Control", "no-store")

		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		field, fieldOK := storage.ParseMetadataField(r.PathValue("field"))
		if err != nil || !fieldOK {
			http.NotFound(w, r)
			return
		}
		fragment := isHTMXFragment(r)

		if r.Method == http.MethodGet {
			metadataFragment(w, r, svc, id, field, fragment)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxMetadataFormBody)
		if err := r.ParseForm(); err != nil {
			// Too large is still a rejected value, not a broken request,
			// so it comes back through the form like any other.
			var toolarge *http.MaxBytesError
			if errors.As(err, &toolarge) {
				metadataError(w, r, svc, id, field, sendEnabled, enrichEnabled, "", "That is too long to save", fragment)
				return
			}
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}

		submitted := r.FormValue("value")
		detail, err := svc.UpdateBookMetadata(r.Context(), id, service.MetadataUpdate{Field: string(field), Value: submitted})
		if err != nil && !errors.Is(err, service.ErrInvalidMetadata) {
			slog.Error("update book metadata failed", "id", id, "field", field, "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if err != nil {
			metadataError(w, r, svc, id, field, sendEnabled, enrichEnabled, submitted, service.MetadataValidationMessage(err), fragment)
			return
		}
		if detail == nil {
			http.NotFound(w, r)
			return
		}
		if !fragment {
			http.Redirect(w, r, fmt.Sprintf("/books/%d", id), http.StatusSeeOther)
			return
		}
		renderField(w, field, makeFieldViews(detail, "")[field])
	}
}

// metadataFragment answers the GET: the field's read view, or its edit
// form when ?edit=1. A non-htmx caller is redirected to the whole page
// carrying the same intent in its own query string.
func metadataFragment(w http.ResponseWriter, r *http.Request, svc *service.Service, id int64, field storage.MetadataField, fragment bool) {
	// The book is loaded before the fragment/navigation split, not after,
	// so a missing book is the same plain 404 on both paths. Redirecting
	// first would answer 303 for a book that does not exist and leave the
	// 404 to the page it points at.
	detail, err := svc.GetBook(r.Context(), id)
	if err != nil {
		slog.Error("get book failed", "id", id, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if detail == nil {
		http.NotFound(w, r)
		return
	}

	editing := r.URL.Query().Get("edit") == "1"
	if !fragment {
		target := fmt.Sprintf("/books/%d", id)
		if editing {
			target += "?edit=" + string(field)
		}
		http.Redirect(w, r, target, http.StatusSeeOther)
		return
	}

	edit := ""
	if editing {
		edit = string(field)
	}
	renderField(w, field, makeFieldViews(detail, edit)[field])
}

// metadataError re-renders field as an open editor holding the rejected
// value and the reason, at 422. The htmx caller gets just the field; the
// no-JavaScript caller gets the whole page with that field open, since it
// has no way to swap one in.
func metadataError(w http.ResponseWriter, r *http.Request, svc *service.Service, id int64, field storage.MetadataField, sendEnabled, enrichEnabled bool, value, message string, fragment bool) {
	detail, err := svc.GetBook(r.Context(), id)
	if err != nil {
		slog.Error("get book failed", "id", id, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if detail == nil {
		http.NotFound(w, r)
		return
	}

	if fragment {
		// 200, not 422: htmx 2.0.10's default response policy does not swap
		// a 4xx, so an honest status here would leave the editor untouched
		// and make Save look like it did nothing. The alternative — opting
		// 422 in from the client — buys the status code at the cost of the
		// whole interaction depending on one listener still being loaded
		// and still matching. The full-page path below keeps the 422,
		// where nothing silently swallows it.
		view := makeFieldViews(detail, string(field))[field]
		view.Value = value
		view.Error = message
		renderField(w, field, view)
		return
	}

	page, err := makeBookDetailPage(r, svc, detail, sendEnabled, enrichEnabled, string(field))
	if err != nil {
		slog.Error("build book detail page failed", "id", id, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	setPageFieldError(page, field, value, message)
	if err := renderStatus(w, http.StatusUnprocessableEntity, "book.html", page); err != nil {
		slog.Error("render template failed", "template", "book.html", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// renderField writes one field's fragment. Each of the three individually
// placed fields has its own template because each sits in different markup
// — a heading, a byline, a body paragraph — while every definition-list row
// shares one. Always 200: a fragment is only ever rendered for an htmx
// caller, and see metadataError for why a rejected value is not a 4xx here.
func renderField(w http.ResponseWriter, field storage.MetadataField, view editableFieldView) {
	name := "book-field-meta"
	switch field {
	case storage.FieldTitle:
		name = "book-field-title"
	case storage.FieldAuthors:
		name = "book-field-authors"
	case storage.FieldDescription:
		name = "book-field-description"
	}
	if err := render(w, name, view); err != nil {
		slog.Error("render template failed", "field", field, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// setPageFieldError opens field's editor on a whole-page render and fills
// it with the rejected value — the no-JavaScript equivalent of swapping
// one field's fragment back in.
func setPageFieldError(page *bookDetailPage, field storage.MetadataField, value, message string) {
	set := func(view *editableFieldView) {
		view.Edit = true
		view.Value = value
		view.Error = message
	}
	switch field {
	case storage.FieldTitle:
		set(&page.TitleField)
	case storage.FieldAuthors:
		set(&page.AuthorsField)
	case storage.FieldDescription:
		set(&page.DescriptionField)
	default:
		for i := range page.MetadataFields {
			if page.MetadataFields[i].Field == string(field) {
				set(&page.MetadataFields[i])
			}
		}
	}
}

// fullAuthorLine joins every author name for display — unlike authorLine's
// grid-card "and N others" collapse, the detail page has room to name
// everyone.
func fullAuthorLine(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	default:
		return strings.Join(names[:len(names)-1], ", ") + " & " + names[len(names)-1]
	}
}
