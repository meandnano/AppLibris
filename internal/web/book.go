package web

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"library/internal/service"
	"library/internal/storage"
)

// bookDetailPage is the data book.html renders against. Title doubles as
// the browser tab title (via document-head) and the on-page heading — the
// book's own title, unlike libraryPage's constant "Library". Nav and
// HeaderNote are the masthead's shared fields — same contract as
// libraryPage's, composed by navFor and headerBookCount so the two pages
// can't render the masthead two different ways.
// Locations is service.FileLocation as it comes: the template reads Path
// and Missing under those names, so a per-page copy of the same two fields
// would be a rename of nothing. FileSizeHuman is empty when the book has
// no location to take a size from — the template renders an em dash there
// rather than "0 B", which is a size, and a wrong one.
//
// The seven editable fields arrive as editableFieldView rather than as
// plain strings: each renders as either a read view or its edit form, and
// which one is a property of the field, not of the page. Title, Authors
// and Description are named individually because the layout places them
// individually (heading, byline, body); MetadataFields is the definition
// list, whose rows differ only in label and value and so can be ranged
// over. Title survives alongside TitleField because document-head still
// needs a plain string for the browser tab.
//
// ID is the book id the send form posts to — removed as unread after #29,
// it comes back here with that caller. The Send* fields are also this
// struct's data for the "send-control" fragment the send POST and status
// poll routes render standalone (most other fields left zero, same reuse
// #28's book-grid fragment already makes of libraryPage). Send itself is
// service.SendState as it comes — nil when the book has never been sent,
// the idle state before any address is picked — with SendPending,
// SendButtonLabel, SendButtonPrimary, SendAt and SendPollURL as its
// derived presentation fields, computed once by applySendState so the
// template branches on state without formatting a timestamp or composing a
// label itself, the same discipline searchSummary applies to the results
// line.
type bookDetailPage struct {
	Title             string
	Nav               []navItem
	HeaderNote        string
	CoverURL          string
	Format            string
	FileSizeHuman     string
	FileSizeBytesText string
	Locations         []service.FileLocation
	AddedAt           string

	TitleField       editableFieldView
	AuthorsField     editableFieldView
	DescriptionField editableFieldView
	MetadataFields   []editableFieldView

	ID                int64
	SendEnabled       bool
	Recipients        []service.RecipientOption
	Send              *service.SendState
	SendPending       bool
	SendButtonLabel   string
	SendButtonPrimary bool
	SendAt            string
	SendPollURL       string
	SendError         string
	SendNewAddress    string
	SendNewLabel      string
}

// bookDetailHandler serves GET /books/{id}. A non-numeric id and an
// unknown id both 404 identically (http.NotFound) — deliberately
// indistinguishable, since neither is a client error worth its own page.
func bookDetailHandler(svc *service.Service, sendEnabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}

		// ?edit=<field> is how the no-JavaScript path opens an editor: the
		// read view's link is a plain href, and htmx only intercepts it
		// when it has loaded. An unrecognised field opens nothing rather
		// than 400ing — it names no resource, and a mistyped query string
		// should still show the book.
		edit := r.URL.Query().Get("edit")
		if _, ok := storage.ParseMetadataField(edit); !ok {
			edit = ""
		}

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

		page, err := makeBookDetailPage(r, svc, detail, sendEnabled, edit)
		if err != nil {
			slog.Error("build book detail page failed", "id", id, "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		if err := render(w, "book.html", page); err != nil {
			slog.Error("render template failed", "template", "book.html", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
	}
}

// makeBookDetailPage assembles the whole detail page from a book already
// loaded, with edit naming the field whose editor should be open (empty
// for none). Takes the loaded detail rather than an id because both
// callers have already fetched it — the handler to decide between 404 and
// 200, the metadata error path to render the rest of the page around a
// rejected value.
func makeBookDetailPage(r *http.Request, svc *service.Service, detail *service.BookDetail, sendEnabled bool, edit string) (*bookDetailPage, error) {
	count, err := svc.CountBooks(r.Context())
	if err != nil {
		return nil, err
	}

	var sizeHuman, sizeBytes string
	if detail.HasFileSize {
		sizeHuman = humanSize(detail.FileSize)
		sizeBytes = fmt.Sprintf("%d bytes", detail.FileSize)
	}

	page := bookDetailPage{
		Title:             detail.Title,
		Nav:               navFor("library"),
		HeaderNote:        headerBookCount(count),
		CoverURL:          coverURL(detail.CoverPath),
		Format:            detail.Format,
		FileSizeHuman:     sizeHuman,
		FileSizeBytesText: sizeBytes,
		Locations:         detail.Locations,
		AddedAt:           detail.AddedAt.Format("2006-01-02"),
		ID:                detail.ID,
	}
	applyFieldViews(&page, detail, edit)
	if err := populateSendControl(r.Context(), svc, sendEnabled, &page); err != nil {
		return nil, err
	}
	return &page, nil
}

// editableFieldView is one field's state for the templates: Value is what
// an editor's control holds, Display what the read view shows, and the two
// differ wherever the stored form isn't the readable one — authors are
// newline-separated for a textarea but "A, B & C" in the byline. Edit
// decides which of the two is rendered. EditURL and ReadURL are the htmx
// swap targets; BookURL is the same intent as a plain href, for the path
// where htmx never loaded.
type editableFieldView struct {
	BookID      int64
	Field       string
	Label       string
	Value       string
	Display     string
	Edit        bool
	Error       string
	EditURL     string
	ReadURL     string
	BookURL     string
	Placeholder string
	Multiline   bool
	Mono        bool
}

// applyFieldViews fills page's four editable slots from detail.
func applyFieldViews(page *bookDetailPage, detail *service.BookDetail, edit string) {
	views := makeFieldViews(detail, edit)
	page.TitleField = views[storage.FieldTitle]
	page.AuthorsField = views[storage.FieldAuthors]
	page.DescriptionField = views[storage.FieldDescription]
	page.MetadataFields = []editableFieldView{
		views[storage.FieldPublisher], views[storage.FieldPublishedDate],
		views[storage.FieldLanguage], views[storage.FieldISBN],
	}
}

// makeFieldViews builds every editable field's view, keyed by field so
// both the whole page and a single-field fragment draw from one place and
// cannot drift. edit names at most one field to open.
//
// An empty optional field still gets a Display of "" and is rendered as a
// visible em dash or an invitation rather than dropped: a field that isn't
// on the page cannot be filled in, and sparse metadata is the common FB2
// case. PublishedDate is passed through exactly as stored, never parsed —
// it is free text from embedded metadata, sometimes a year and sometimes a
// full date, and parsing it would lie confidently.
func makeFieldViews(detail *service.BookDetail, edit string) map[storage.MetadataField]editableFieldView {
	views := map[storage.MetadataField]editableFieldView{}
	add := func(field storage.MetadataField, label, value, display, placeholder string, multiline, mono bool) {
		name := string(field)
		views[field] = editableFieldView{
			BookID: detail.ID, Field: name, Label: label, Value: value, Display: display,
			Edit:    edit == name,
			EditURL: fmt.Sprintf("/books/%d/metadata/%s?edit=1", detail.ID, name),
			ReadURL: fmt.Sprintf("/books/%d/metadata/%s", detail.ID, name),
			BookURL: fmt.Sprintf("/books/%d", detail.ID),
			// The read view's plain href carries the same intent as the
			// hx-get, so the no-JavaScript path opens the same editor.
			Placeholder: placeholder, Multiline: multiline, Mono: mono,
		}
	}
	add(storage.FieldTitle, "Title", detail.Title, detail.Title, "Book title", false, false)
	add(storage.FieldAuthors, "Authors", strings.Join(detail.Authors, "\n"), fullAuthorLine(detail.Authors), "One author per line", true, false)
	add(storage.FieldDescription, "Description", detail.Description, detail.Description, "Add a description", true, false)
	add(storage.FieldPublisher, "publisher", detail.Publisher, detail.Publisher, "Publisher", false, false)
	add(storage.FieldPublishedDate, "published", detail.PublishedDate, detail.PublishedDate, "Published date", false, false)
	add(storage.FieldLanguage, "language", detail.Language, detail.Language, "Language", false, false)
	add(storage.FieldISBN, "isbn", detail.ISBN, detail.ISBN, "ISBN", false, true)
	return views
}

// populateSendControl fills page's send-control fields: the saved
// recipients (skipped when sending is unconfigured — there is nothing to
// pick from) and this book's most recent send, if any. Shared by the full
// detail-page render and the standalone send-control fragment handlers in
// send.go, so the three routes derive the picker and status box the same
// way.
func populateSendControl(ctx context.Context, svc *service.Service, sendEnabled bool, page *bookDetailPage) error {
	page.SendEnabled = sendEnabled
	if !sendEnabled {
		return nil
	}

	recipients, err := svc.Recipients(ctx)
	if err != nil {
		return err
	}
	page.Recipients = recipients

	send, err := svc.LatestSend(ctx, page.ID)
	if err != nil {
		return err
	}
	applySendState(page, send)
	return nil
}

// applySendState fills page's derived send-status fields from send (nil
// for a book that has never been sent). Computed once here — the button
// label, whether the control is mid-send, and the formatted timestamp —
// rather than in the template, the same discipline searchSummary applies
// to the results line.
func applySendState(page *bookDetailPage, send *service.SendState) {
	page.Send = send
	if send == nil {
		page.SendButtonLabel = "Send"
		page.SendButtonPrimary = true
		return
	}

	page.SendAt = send.At.Format("2006-01-02 15:04")
	switch send.Status {
	case "queued", "sending":
		page.SendPending = true
		page.SendButtonLabel = "Sending"
		page.SendPollURL = fmt.Sprintf("/books/%d/sends/%d", page.ID, send.ID)
	case "delivered":
		page.SendButtonLabel = "Send again"
	case "failed":
		page.SendButtonLabel = "Retry"
		page.SendButtonPrimary = true
	default:
		page.SendButtonLabel = "Send"
		page.SendButtonPrimary = true
	}
}

// humanSize formats n bytes as e.g. "12.3 MB", binary (1024-based) units.
// Picking the unit off the truncated ratio isn't quite enough on its own:
// a value like 1023.97 is < 1024 but rounds to "1024.0" at one decimal
// place, so a size a handful of bytes under 1 GiB would otherwise print as
// "1024.0 MB". Bumping the unit once more when the rounded value would
// reach the next tier's threshold keeps the displayed number and its unit
// in agreement.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := "KMGTPE"
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	value := float64(n) / float64(div)
	if value >= unit-0.05 && exp < len(units)-1 {
		div *= unit
		exp++
		value = float64(n) / float64(div)
	}
	return fmt.Sprintf("%.1f %cB", value, units[exp])
}
