package web

import (
	"bytes"
	"embed"
	"html/template"
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
func render(w http.ResponseWriter, name string, data any) error {
	var buf bytes.Buffer
	if err := templates.ExecuteTemplate(&buf, name, data); err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, err := buf.WriteTo(w)
	return err
}
