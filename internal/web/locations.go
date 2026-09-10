package web

import (
	"log/slog"
	"net/http"
	"strconv"

	"library/internal/service"
)

// forgetLocationHandler serves POST /books/{id}/locations/forget, the one
// affordance for a path that is gone for good. The scanner marks such a row
// and then refuses to prune it whenever its top-level directory yielded no
// files, because that is equally what an offline sub-mount looks like; a
// renamed folder therefore leaves every book under it with a dead location
// for the life of the deployment. Only a person can tell the two apart, so
// only a person clears it.
//
// The row is named by its book_files id in the "file" field. The button is
// rendered beside a location that is currently marked missing and nowhere
// else, and the same rule is a condition on the delete, so a stale page
// cannot forget a path that has since come back.
//
// Forgetting a book's last location prunes the book, through the same
// pruneOrphanedBookTx every other path uses, and the response then has
// nowhere to go: the reader lands on the library grid instead of a page
// whose subject no longer exists. htmx gets HX-Redirect for that, since a
// fragment cannot be swapped into it either.
func forgetLocationHandler(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Vary", "HX-Request, HX-History-Restore-Request")

		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		fileID, err := strconv.ParseInt(r.FormValue("file"), 10, 64)
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		_, bookDeleted, err := svc.ForgetLocation(r.Context(), id, fileID)
		if err != nil {
			slog.Error("forget location failed", "id", id, "file_id", fileID, "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		fragment := isHTMXFragment(r)
		if bookDeleted {
			if fragment {
				w.Header().Set("HX-Redirect", "/")
				w.WriteHeader(http.StatusOK)
				return
			}
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		if !fragment {
			http.Redirect(w, r, "/books/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
			return
		}

		// A forget that matched nothing lands here too, and re-renders the
		// list as it stands: a double click, or a row a sweep has cleared
		// under the reader, is a slip, and the honest answer to it is the
		// current list rather than an error page.
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
		page := bookDetailPage{ID: id, Locations: detail.Locations}
		if err := render(w, "book-locations", page); err != nil {
			slog.Error("render template failed", "template", "book-locations", "error", err)
		}
	}
}
