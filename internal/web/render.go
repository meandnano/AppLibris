package web

import (
	"bytes"
	"embed"
	"html/template"
	"log/slog"
	"net/http"
)

//go:embed templates/*.html
var templateFS embed.FS

var templates = template.Must(template.ParseFS(templateFS, "templates/*.html"))

//go:embed static
var staticFS embed.FS

// render executes the named template into a buffer before writing anything
// to w, so a template error (a nil field access, a range over a bad type)
// becomes a clean error return instead of a response truncated mid-stream.
// Content-Type is set explicitly rather than left to sniffing, since an htmx
// partial can start with a bare tag that sniffing won't reliably call HTML.
//
// Only the pre-write ExecuteTemplate error is returned to the caller: once
// Content-Type is set and WriteTo starts sending bytes, the response is
// already committed, so a caller that reacted by calling http.Error would
// double-write onto it. A WriteTo failure at that point (almost always the
// client disconnecting) is logged here instead, since the caller can no
// longer respond any differently.
func render(w http.ResponseWriter, name string, data any) error {
	var buf bytes.Buffer
	if err := templates.ExecuteTemplate(&buf, name, data); err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if _, err := buf.WriteTo(w); err != nil {
		slog.Warn("write rendered response", "template", name, "error", err)
	}
	return nil
}
