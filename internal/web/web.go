// Package web is the HTTP transport for the browser UI: thin handlers that
// parse a request, call into internal/service, and render — no business
// logic lives here, per DESIGN.md's "Layering for a future API."
package web

import (
	"log"
	"net/http"

	"library/internal/service"
)

// Routes builds the web UI's HTTP handler.
func Routes(svc *service.Service) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", libraryHandler(svc))
	mux.Handle("GET /static/", http.FileServerFS(staticFS))
	return mux
}

func libraryHandler(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		books, err := svc.ListBooks(r.Context())
		if err != nil {
			log.Printf("web: list books: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if err := render(w, "library.html", books); err != nil {
			log.Printf("web: render library.html: %v", err)
		}
	}
}
