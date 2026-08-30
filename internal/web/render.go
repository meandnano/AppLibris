package web

import (
	"embed"
	"html/template"
	"net/http"
)

//go:embed templates/*.html
var templateFS embed.FS

var templates = template.Must(template.ParseFS(templateFS, "templates/*.html"))

//go:embed static
var staticFS embed.FS

// render executes the named template against data, writing straight to w.
func render(w http.ResponseWriter, name string, data any) error {
	return templates.ExecuteTemplate(w, name, data)
}
