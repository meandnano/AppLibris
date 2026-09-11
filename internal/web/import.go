package web

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"library/internal/importer"
	"library/internal/service"
)

// multipartOverhead is how much room the upload's body limit leaves above
// the file itself: the multipart boundaries, the part headers and the
// filename. Generous, because this limit exists to stop a body nothing
// bounds rather than to be the exact cap — the importer's own count of the
// part's bytes is the cap, and it is the one a refusal names.
const multipartOverhead = 8 << 10

// minUploadWindow is the floor under the upload route's read deadline. It
// matches cmd/server's ReadTimeout, so extending the deadline can only ever
// lengthen the window a body has, never shorten it.
const minUploadWindow = 30 * time.Second

// uploadRate is the throughput the upload deadline is sized at. Deliberately
// slow: Wi-Fi to a NAS is the deployment this has to work on, and a deadline
// generous enough to be wrong about nobody is worth more here than one tuned
// to a link speed this code cannot measure.
const uploadRate = 1 << 20

// importPage is the data import.html and its fragments render against.
// Preview nil is the idle state, where the panel shows the file input;
// non-nil is a staged file waiting on a decision. Failure is the sentence a
// refused upload or a stale confirm shows above the input, composed by
// importFailureLine so the template holds no phrasing.
type importPage struct {
	Title      string
	Nav        []navItem
	HeaderNote string

	// Enabled is false when the library directory could not be written at
	// startup. The page then explains itself instead of offering a control
	// that cannot work.
	Enabled bool
	// MaxSizeLabel is the cap as the page says it ("64 MiB"), shown beside
	// the input so the limit is known before a long upload hits it.
	MaxSizeLabel string

	Preview *importPreviewView
	Failure string
	// Note is the one outcome that is neither a failure nor a redirect:
	// the file reached the library and indexing did not.
	Note string
}

// importPreviewView is one staged file shaped for the template: the three
// verdicts as three booleans the template branches on, and every line of
// prose already composed.
type importPreviewView struct {
	ID           string
	OriginalName string
	LibraryName  string
	Title        string
	AuthorLine   string
	Format       string
	SizeHuman    string
	CoverURL     string
	Description  string

	// Importable is whether the Import button is rendered at all. False
	// for content the library already holds byte for byte, where there is
	// nothing to import and the block links to the book instead.
	Importable bool
	// Warning is the title-match caveat, empty for every other verdict.
	Warning string
	// ExistingURL and ExistingTitle name the book a verdict points at,
	// empty when none does.
	ExistingURL   string
	ExistingTitle string
	// RenameNote says so when the library name will not be the name that
	// was uploaded — an FB2 offered as .epub, or a name nothing survived
	// of.
	RenameNote string

	ConfirmURL string
	DiscardURL string
}

// importHandler serves GET /import: the page holding the file input, or the
// panel alone for an htmx caller — so it names both headers in Vary, like
// every other route whose body depends on them.
func importHandler(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Vary", "HX-Request, HX-History-Restore-Request")

		page, err := newImportPage(r, svc)
		if err != nil {
			slog.Error("build import page failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		renderImport(w, r, http.StatusOK, page)
	}
}

// importUploadHandler serves POST /import/file: the upload itself.
//
// The body is read straight into staging through r.MultipartReader rather
// than through ParseMultipartForm, which would spool the whole file to a
// second temporary copy before this handler saw a byte of it.
//
// A rejected upload answers 200 to an htmx caller and 422 to everyone else,
// the split the metadata editors already make: htmx 2.0.10 does not swap a
// 4xx, so a refusal nobody can see is a broken button, while a full-page
// rejection is a real one and says so.
func importUploadHandler(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Vary", "HX-Request, HX-History-Restore-Request")

		page, err := newImportPage(r, svc)
		if err != nil {
			slog.Error("build import page failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		// Bounded before anything can refuse, not after: the read-only
		// refusal below answers a request whose body is still arriving, and
		// draining it is what lets the answer be read — which needs the
		// drain to have a limit.
		limit := svc.MaxImportBytes() + multipartOverhead
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		extendReadDeadline(w, limit)

		if !page.Enabled {
			page.Failure = importFailureLine(service.ErrImportDisabled, svc.MaxImportBytes())
			renderImportRejection(w, r, page)
			return
		}

		name, body, err := uploadedFile(r)
		if err != nil {
			page.Failure = importFailureLine(err, svc.MaxImportBytes())
			if page.Failure == "" {
				slog.Warn("import upload failed", "error", err)
				page.Failure = "That upload could not be read. Try again."
			}
			renderImportRejection(w, r, page)
			return
		}

		preview, err := svc.StageImport(r.Context(), name, body)
		if err != nil {
			page.Failure = importFailureLine(err, svc.MaxImportBytes())
			if page.Failure == "" {
				slog.Error("stage import failed", "error", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			renderImportRejection(w, r, page)
			return
		}

		// Without htmx the upload is an ordinary form post, so it answers
		// with a redirect to the preview's own URL: a reload of the result
		// then re-reads the stage instead of re-sending the file.
		if !isHTMXFragment(r) {
			http.Redirect(w, r, "/import/"+preview.ID, http.StatusSeeOther)
			return
		}
		page.Preview = importPreviewViewOf(preview)
		renderImport(w, r, http.StatusOK, page)
	}
}

// importPreviewHandler serves GET /import/{id}: the staged file's preview,
// as a fragment or as the whole page.
func importPreviewHandler(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Vary", "HX-Request, HX-History-Restore-Request")

		page, err := newImportPage(r, svc)
		if err != nil {
			slog.Error("build import page failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		preview, err := svc.StagedImport(r.Context(), r.PathValue("id"))
		if err != nil && !errors.Is(err, service.ErrImportDisabled) {
			slog.Error("read staged import failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if preview == nil {
			// An expired stage is not a missing page: the person is
			// looking at a URL that was right a moment ago, and the input
			// they need is the one this page carries anyway.
			page.Failure = importFailureLine(importer.ErrExpired, svc.MaxImportBytes())
			renderImport(w, r, http.StatusOK, page)
			return
		}
		page.Preview = importPreviewViewOf(preview)
		renderImport(w, r, http.StatusOK, page)
	}
}

// importConfirmHandler serves POST /import/{id}/confirm: the copy into the
// library and the index write.
//
// It answers an htmx caller with HX-Redirect and everyone else with a 303,
// both to the book's own page — the import is finished, and there is
// nothing left on this page to swap.
func importConfirmHandler(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Vary", "HX-Request, HX-History-Restore-Request")

		page, err := newImportPage(r, svc)
		if err != nil {
			slog.Error("build import page failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		bookID, indexed, err := svc.ConfirmImport(r.Context(), r.PathValue("id"))
		if err != nil {
			page.Failure = importFailureLine(err, svc.MaxImportBytes())
			if page.Failure == "" {
				slog.Error("confirm import failed", "error", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			renderImportRejection(w, r, page)
			return
		}
		if !indexed {
			page.Note = "Imported, but indexing failed — the next scan will pick it up."
			renderImport(w, r, http.StatusOK, page)
			return
		}

		bookURL := "/books/" + strconv.FormatInt(bookID, 10)
		if isHTMXFragment(r) {
			w.Header().Set("HX-Redirect", bookURL)
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, bookURL, http.StatusSeeOther)
	}
}

// importDiscardHandler serves POST /import/{id}/discard: dropping a staged
// file. It answers with the panel back in its idle state, so the next
// upload starts from the same place the first one did.
func importDiscardHandler(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Vary", "HX-Request, HX-History-Restore-Request")

		page, err := newImportPage(r, svc)
		if err != nil {
			slog.Error("build import page failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if err := svc.DiscardImport(r.Context(), r.PathValue("id")); err != nil && !errors.Is(err, service.ErrImportDisabled) {
			slog.Error("discard import failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		if !isHTMXFragment(r) {
			http.Redirect(w, r, "/import", http.StatusSeeOther)
			return
		}
		renderImport(w, r, http.StatusOK, page)
	}
}

// importCoverHandler serves GET /import/{id}/cover: the cover a staged file
// had embedded, straight from the stage.
//
// The type is the one the service carries, decided by a decoder that read
// the image's header, and never http.DetectContentType over these bytes:
// they are an uploaded file's own choice of what to call a cover, and
// neither format reader checks that the thing is an image. Sniffing a
// "cover" that is really an HTML document would answer text/html from this
// app's origin, which is the origin sameSiteOnly admits. nosniff is the
// second half of the same rule — the browser must not re-decide a type the
// server has named.
//
// no-store rather than a max-age: the id is single-use and the bytes are
// gone within half an hour either way, so anything a cache kept would only
// ever be served back to the one tab that asked.
func importCoverHandler(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cover, contentType := svc.StagedCover(r.PathValue("id"))
		if len(cover) == 0 {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(cover)
	}
}

// newImportPage builds the page's shared chrome. The book count is the
// masthead's, the same one every other page shows.
func newImportPage(r *http.Request, svc *service.Service) (importPage, error) {
	total, err := svc.CountBooks(r.Context())
	if err != nil {
		return importPage{}, err
	}
	enabled := svc.ImportEnabled()
	return importPage{
		Title:        "Import",
		Nav:          navFor("import", enabled),
		HeaderNote:   headerBookCount(total),
		Enabled:      enabled,
		MaxSizeLabel: humanSize(svc.MaxImportBytes()),
	}, nil
}

// renderImport answers with the panel for an htmx caller and the whole page
// for everyone else.
func renderImport(w http.ResponseWriter, r *http.Request, status int, page importPage) {
	name := "import.html"
	if isHTMXFragment(r) {
		name = "import-panel"
	}
	if err := renderStatus(w, status, name, page); err != nil {
		slog.Error("render template failed", "template", name, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// renderImportRejection answers a refused upload or confirm: 200 with the
// panel for htmx, which does not swap a 4xx, and 422 with the whole page
// for everyone else.
//
// It drains first, so the connection is never left with a request body
// nobody consumed — the condition under which Go's server stops reading and
// closes, which can cost the client the response it was about to be given.
// The drain needs no bound of its own: the upload route has already wrapped
// the body in http.MaxBytesReader.
//
// It is hygiene rather than a cure, and the shape of the limit is why.
// copyIn stops at the cap plus one byte of the *file part* and reads no
// further, so an over-cap upload never trips MaxBytesReader at all — which
// leaves this with at most multipartOverhead to consume, too little to have
// blocked the client and well inside the window Go's lingering close
// already covers. The case that could actually lose the refusal is a body
// far past the limit, where megabytes are still in flight; MaxBytesReader
// refuses to hand them over, so nothing here can drain them and nothing
// here can improve on the lingering close.
func renderImportRejection(w http.ResponseWriter, r *http.Request, page importPage) {
	io.Copy(io.Discard, r.Body)

	if isHTMXFragment(r) {
		renderImport(w, r, http.StatusOK, page)
		return
	}
	renderImport(w, r, http.StatusUnprocessableEntity, page)
}

// uploadedFile finds the file part of a multipart body and hands back its
// client-offered name and its bytes, unread.
//
// The part is returned rather than copied, so the body streams into staging
// under the importer's own cap instead of being held here first.
func uploadedFile(r *http.Request) (string, *multipartFile, error) {
	reader, err := r.MultipartReader()
	if err != nil {
		return "", nil, err
	}
	for {
		part, err := reader.NextPart()
		if err != nil {
			return "", nil, err
		}
		if part.FormName() != "file" {
			part.Close()
			continue
		}
		if part.FileName() == "" {
			part.Close()
			return "", nil, errNoFileChosen
		}
		return part.FileName(), &multipartFile{part}, nil
	}
}

// errNoFileChosen is a form submitted with the input left empty, which a
// browser sends as a file part with no filename.
var errNoFileChosen = errors.New("web: no file was chosen")

// multipartFile is the part as an io.Reader, named so the importer's
// signature reads as what it is rather than as a mime detail.
type multipartFile struct{ io.Reader }

// uploadWindow is how long a body of the given size is given to arrive.
//
// A pure function so the sizing is testable: the call below runs against a
// real server and not against httptest's recorder, which has no
// SetReadDeadline at all — so a rule written only inside that call would be
// asserted by nothing.
func uploadWindow(bytes int64) time.Duration {
	window := time.Duration(bytes/uploadRate) * time.Second
	if window < minUploadWindow {
		return minUploadWindow
	}
	return window
}

// extendReadDeadline gives this one request long enough to receive a body
// the size of the cap.
//
// cmd/server's ReadTimeout covers the body and is sized for a page request;
// sixty-four megabytes over Wi-Fi to a NAS routinely takes longer. Extended
// per request rather than globally so every other route keeps the tight
// timeout, and extended rather than removed so a stalled upload still ends.
// A server that does not support the control is left alone: the global
// timeout then applies, which is the behaviour this replaces.
func extendReadDeadline(w http.ResponseWriter, bytes int64) {
	if err := http.NewResponseController(w).SetReadDeadline(time.Now().Add(uploadWindow(bytes))); err != nil {
		slog.Debug("could not extend the upload read deadline", "error", err)
	}
}

// importFailureLine composes the sentence a refused import shows, or ""
// for an error the page cannot explain — which the caller turns into a 500
// rather than guessing at.
//
// Composed here rather than in the template, the convention searchSummary
// and enrichmentResultLine already follow.
func importFailureLine(err error, maxBytes int64) string {
	var tooBig *http.MaxBytesError
	switch {
	case errors.Is(err, importer.ErrTooLarge), errors.As(err, &tooBig):
		return "That file is larger than the " + humanSize(maxBytes) + " import limit."
	case errors.Is(err, importer.ErrUnsupportedFormat):
		return "That is not an EPUB or FB2 file."
	case errors.Is(err, importer.ErrExpired):
		return "This import has expired — choose the file again."
	case errors.Is(err, importer.ErrStagingFull):
		return "Another import is still waiting. Finish or discard it, then try again."
	case errors.Is(err, errNoFileChosen):
		return "Choose a file first."
	case errors.Is(err, service.ErrImportDisabled), errors.Is(err, importer.ErrLibraryNotWritable):
		return "The library directory is read-only, so importing is disabled."
	default:
		return ""
	}
}

// importPreviewViewOf shapes one staged import for the template, composing
// every line of prose the three verdicts need.
func importPreviewViewOf(preview *service.ImportPreview) *importPreviewView {
	view := &importPreviewView{
		ID:           preview.ID,
		OriginalName: preview.OriginalName,
		LibraryName:  preview.LibraryName,
		Title:        preview.Title,
		AuthorLine:   authorLine(preview.Authors),
		Format:       preview.Format,
		SizeHuman:    humanSize(preview.Size),
		Description:  preview.Description,
		Importable:   preview.Verdict != string(importer.VerdictExists),
		ConfirmURL:   "/import/" + preview.ID + "/confirm",
		DiscardURL:   "/import/" + preview.ID + "/discard",
	}
	if preview.HasCover {
		view.CoverURL = "/import/" + preview.ID + "/cover"
	}
	if preview.ExistingID != 0 {
		view.ExistingURL = "/books/" + strconv.FormatInt(preview.ExistingID, 10)
		view.ExistingTitle = preview.ExistingTitle
	}
	if preview.Verdict == string(importer.VerdictTitleMatch) {
		view.Warning = "The library already holds a book called " + preview.ExistingTitle + ". Import anyway if this is a different edition."
	}
	if preview.LibraryName != preview.OriginalName {
		view.RenameNote = "Will be saved as " + preview.LibraryName
	}
	return view
}
