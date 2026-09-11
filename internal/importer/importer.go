// Package importer takes a book offered through the web UI, stages it
// outside the library, previews what it holds, and — once a person has
// confirmed it — copies it into the library directory and indexes it
// through the scanner's own per-file path. See docs/notes/import.md.
package importer

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"library/internal/cover"
	"library/internal/epub"
	"library/internal/fb2"
	"library/internal/scanner"
	"library/internal/storage"
)

// StageTTL is how long a staged import survives unconfirmed. Long enough to
// read a preview and decide, short enough that a tab somebody forgot does
// not hold the size cap in temporary space for a day.
//
// Not configurable: nothing about a deployment changes how long a person
// takes to press a button.
const StageTTL = 30 * time.Minute

// stagingBudgetFactor sizes the staging budget from the import cap: this
// many uploads of the largest allowed size may sit unconfirmed at once.
//
// Staging is os.TempDir(), which is tmpfs in a container — so what a
// forgotten tab holds for half an hour is RAM on a machine whose whole job
// is to serve a library. Four is enough that nobody doing this by hand
// meets the limit and small enough that the worst case is a number a reader
// of MAX_IMPORT_SIZE can work out.
const stagingBudgetFactor = 4

// indexTimeout bounds the index write that follows a copy into the library.
// It runs on a context detached from the request's, so it needs a deadline
// of its own — and a long one, because indexing hashes the whole file and
// may extract and resize a cover, where internal/sender's markTimeout
// covers one small SQLite write.
const indexTimeout = 2 * time.Minute

// janitorInterval is how often expired stages are swept. Expiry is also
// rechecked on confirm, so this only bounds how long a dead file sits on
// disk, never whether a stale click is caught.
const janitorInterval = time.Minute

var (
	// ErrTooLarge is a body past the configured cap.
	ErrTooLarge = errors.New("importer: file is larger than the import size limit")
	// ErrExpired is a stage that is gone — swept by the janitor, already
	// discarded, or never there at all. One error for all three because
	// they are one thing to the person holding the stale page, and the
	// remedy is the same: stage it again.
	ErrExpired = errors.New("importer: this import has expired")
	// ErrLibraryNotWritable is import refused for the run, because the
	// startup probe could not write to the library directory.
	ErrLibraryNotWritable = errors.New("importer: the library directory is not writable")
	// ErrStagingFull is an upload refused because what is already staged
	// fills the staging budget.
	ErrStagingFull = errors.New("importer: too much is already staged")
	// ErrNotIndexed means the library file was written and the index write
	// then failed. The bytes are in the library and the next sweep picks
	// them up, so it is not the same failure as one that imported nothing.
	ErrNotIndexed = errors.New("importer: imported but not indexed")
)

// Verdict is what Stage found out about a staged file's content before
// anything was written into the library.
type Verdict string

const (
	// VerdictNew is content the index has never seen. Import is offered.
	VerdictNew Verdict = "new"
	// VerdictExists is byte-identical content already in the library.
	// Copying it would only add a second location to one book, so it is
	// refused with a link to that book.
	VerdictExists Verdict = "exists"
	// VerdictTitleMatch is different content filed under a title the
	// library already holds. Import is offered under a warning: different
	// editions of one book are a legitimate thing to own, and the person
	// is the only one who knows which this is.
	VerdictTitleMatch Verdict = "title-match"
)

// Staged is one staged import as a caller needs to see it — a value, so
// nothing outside this package holds a pointer into the registry.
type Staged struct {
	ID string
	// OriginalName is what the client called the file, shown so a person
	// can tell one upload from another. It is never what the library file
	// is called; LibraryName is.
	OriginalName string
	// LibraryName is the name the file takes in the library, suffix
	// included. A collision marker is not in it — that is decided at
	// confirm, against the directory as it stands then.
	LibraryName string

	Format        string
	Size          int64
	Title         string
	Authors       []string
	Language      string
	ISBN          string
	Publisher     string
	PublishedDate string
	Description   string
	HasCover      bool

	Verdict       Verdict
	ExistingID    int64
	ExistingTitle string
}

// stage is the registry's own record. Everything a caller sees is in
// Staged; the rest is what Confirm needs.
type stage struct {
	// confirm serialises Confirm over one stage, so a double click waits
	// for the first call and then reads its answer instead of racing it
	// into a second copy of the same bytes.
	confirm sync.Mutex

	staged Staged
	path   string
	hash   string
	// cover is the embedded cover, and coverType the media type
	// cover.ContentType decided for it. Both are empty for a book with no
	// cover and for one whose cover is not an image this app would store.
	cover     []byte
	coverType string
	created   time.Time
	// reserved is what this stage is charged against the budget. It is the
	// cap until the copy lands and the file's own size after, and it goes
	// back to the budget wherever the staged file does.
	reserved int64
	// done marks a stage whose bytes the library has taken over, so a
	// repeated confirm answers rather than copying the file in twice.
	// bookID is the book it landed under, 0 when the copy succeeded and
	// the index write did not.
	done   bool
	bookID int64
}

// Options is everything a Stager is built with. A struct rather than seven
// positional parameters, four of which are strings that would sit next to
// each other.
type Options struct {
	// LibraryDir is the resolved library root, the directory imports are
	// copied into.
	LibraryDir string
	// CoversDir is where a cover extracted during indexing is written —
	// handed straight to the scanner.
	CoversDir string
	// TempDir is where staged files live until they are confirmed or
	// expire. New wipes it.
	TempDir string
	// MaxSize is the largest body Stage accepts, in bytes.
	MaxSize int64
	// Writable is the startup probe's answer. False disables import for
	// the run: Stage and Confirm both refuse with ErrLibraryNotWritable,
	// so a stale tab gets an explanation rather than a filesystem error.
	Writable bool
	// Now is the clock, overridden in tests.
	Now func() time.Time
}

// Stager holds the staged imports. One per process, built by cmd/server.
type Stager struct {
	db         *storage.DB
	libraryDir string
	coversDir  string
	tempDir    string
	maxSize    int64
	maxStaged  int64
	writable   bool
	now        func() time.Time

	mu sync.Mutex
	// stages is every live stage, and staged the bytes they are holding —
	// reserved pessimistically at the cap before a copy starts and
	// corrected to the real size once it lands, so two uploads racing the
	// check cannot both pass it and overshoot the budget together.
	stages map[string]*stage
	staged int64
}

// New builds a Stager and empties its temp directory.
//
// The wipe is the whole recovery story for a restart: a stage nobody
// confirmed is a file nobody asked for, and since no stage is written to
// the database there is nothing left pointing at it. That is also why the
// directory is the importer's own rather than one under /data — it holds
// nothing worth keeping and nothing worth backing up.
func New(db *storage.DB, opts Options) (*Stager, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if err := os.RemoveAll(opts.TempDir); err != nil {
		return nil, fmt.Errorf("clear import staging directory %s: %w", opts.TempDir, err)
	}
	if err := os.MkdirAll(opts.TempDir, 0o700); err != nil {
		return nil, fmt.Errorf("create import staging directory %s: %w", opts.TempDir, err)
	}
	return &Stager{
		db:         db,
		libraryDir: opts.LibraryDir,
		coversDir:  opts.CoversDir,
		tempDir:    opts.TempDir,
		maxSize:    opts.MaxSize,
		maxStaged:  opts.MaxSize * stagingBudgetFactor,
		writable:   opts.Writable,
		now:        opts.Now,
		stages:     make(map[string]*stage),
	}, nil
}

// Enabled reports whether import can do anything at all this run.
func (s *Stager) Enabled() bool { return s.writable }

// MaxSize is the configured cap, for the sentence a refusal shows.
func (s *Stager) MaxSize() int64 { return s.maxSize }

// Stage writes r to a temporary file, decides what it is, and answers what
// a preview needs — without touching the library directory.
//
// name is the filename the client offered. It chooses neither the parser
// nor the stored extension, both of which come out of the content
// (detectSuffix); it only seeds what the library file is called.
func (s *Stager) Stage(ctx context.Context, name string, r io.Reader) (Staged, error) {
	if !s.writable {
		return Staged{}, ErrLibraryNotWritable
	}

	// Reserved at the cap rather than at the body's own size, which is not
	// known until it has been written: a reservation made after the copy
	// would be a budget that admits everything and reports afterwards.
	if !s.reserve(s.maxSize) {
		return Staged{}, ErrStagingFull
	}
	reserved := s.maxSize
	defer func() {
		// Released unless the stage below takes ownership of it, which it
		// signals by zeroing this.
		if reserved > 0 {
			s.release(reserved)
		}
	}()

	id, err := newStageID()
	if err != nil {
		return Staged{}, err
	}
	path := filepath.Join(s.tempDir, id)

	size, hash, err := s.copyIn(path, r)
	if err != nil {
		os.Remove(path)
		return Staged{}, err
	}

	suffix, err := detectSuffix(path)
	if err != nil {
		os.Remove(path)
		return Staged{}, err
	}

	// Renamed onto the suffix because epub.ReadMetadata and
	// fb2.ReadMetadata are picked by it and read the path they are given,
	// so the staged file has to be named the way the library file will be
	// for the same parse to happen twice.
	staged := path + suffix
	if err := os.Rename(path, staged); err != nil {
		os.Remove(path)
		return Staged{}, err
	}

	meta := capMetadata(readMetadata(staged, suffix, name))

	// Decided here rather than where the bytes are served, so HasCover
	// means "a cover this app would keep" and the preview stops promising
	// one the import would then drop. It is also what stops the preview
	// route handing a browser a media type an uploaded file chose: see
	// cover.ContentType.
	coverType := ""
	if len(meta.Cover) > 0 {
		var err error
		if coverType, err = cover.ContentType(meta.Cover); err != nil {
			slog.Debug("staged cover is not a usable image", "name", name, "error", err)
			meta.Cover = nil
		}
	}

	record := &stage{
		staged: Staged{
			ID:            id,
			OriginalName:  name,
			LibraryName:   libraryName(libraryStem(name, meta.Title, id, suffix), suffix, 1),
			Format:        bookFormat(suffix),
			Size:          size,
			Title:         meta.Title,
			Authors:       meta.Authors,
			Language:      meta.Language,
			ISBN:          meta.ISBN,
			Publisher:     meta.Publisher,
			PublishedDate: meta.PublishedDate,
			Description:   meta.Description,
			HasCover:      len(meta.Cover) > 0,
		},
		path:      staged,
		hash:      hash,
		cover:     meta.Cover,
		coverType: coverType,
		created:   s.now(),
		reserved:  size,
	}

	if err := s.decideVerdict(ctx, record); err != nil {
		os.Remove(staged)
		return Staged{}, err
	}

	s.mu.Lock()
	// The record now owns what it holds, charged at the file's real size
	// rather than at the cap it was admitted under.
	s.staged -= s.maxSize - record.reserved
	s.stages[id] = record
	s.mu.Unlock()
	reserved = 0

	return record.staged, nil
}

// reserve charges n against the staging budget, reporting false when it
// will not fit.
func (s *Stager) reserve(n int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.staged+n > s.maxStaged {
		return false
	}
	s.staged += n
	return true
}

// release gives n back.
func (s *Stager) release(n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.staged -= n
}

// copyIn writes r to path under the size cap, hashing as it goes, and
// reports what it wrote.
//
// The cap is enforced by reading one byte past it: a body that yields
// maxSize+1 bytes is over, and one that yields exactly maxSize is not, so
// the boundary needs no Content-Length to be believed. The hash is taken
// on the way in rather than by re-reading the file, since the verdict needs
// it before anything else happens.
func (s *Stager) copyIn(path string, r io.Reader) (int64, string, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()

	h := sha256.New()
	size, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, s.maxSize+1))
	if err != nil {
		return 0, "", err
	}
	if size > s.maxSize {
		return 0, "", ErrTooLarge
	}
	if err := f.Close(); err != nil {
		return 0, "", err
	}
	return size, hex.EncodeToString(h.Sum(nil)), nil
}

// decideVerdict looks the staged content up and fills the record's verdict.
//
// A byte-identical match deletes the staged file immediately: there is
// nothing left to confirm, so holding the bytes for half an hour would be
// holding them for a button that is not offered. The record survives so the
// preview page can still render and be discarded.
func (s *Stager) decideVerdict(ctx context.Context, record *stage) error {
	existing, err := s.db.FindBookByContentHash(ctx, record.hash)
	if err != nil {
		return err
	}
	if existing != nil {
		record.staged.Verdict = VerdictExists
		record.staged.ExistingID = existing.ID
		record.staged.ExistingTitle = existing.Title
		os.Remove(record.path)
		record.path = ""
		// Nothing left on disk, so it holds none of the budget either —
		// the record survives only so the page can render and be
		// discarded.
		record.reserved = 0
		record.done = true
		record.bookID = existing.ID
		return nil
	}

	titled, err := s.db.FindBookBySortTitle(ctx, storage.SortTitle(record.staged.Title))
	if err != nil {
		return err
	}
	if titled != nil {
		record.staged.Verdict = VerdictTitleMatch
		record.staged.ExistingID = titled.ID
		record.staged.ExistingTitle = titled.Title
		return nil
	}

	record.staged.Verdict = VerdictNew
	return nil
}

// Get returns one staged import, or false if it is gone.
func (s *Stager) Get(id string) (Staged, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.stages[id]
	if !ok || s.expired(record) {
		return Staged{}, false
	}
	return record.staged, true
}

// Cover returns the cover a staged file had embedded, with the media type
// cover.ContentType decided for it, or ok false when it had none this app
// would keep.
//
// The type is returned rather than left to whoever serves the bytes,
// because sniffing them is the mistake: they are an uploaded file's choice,
// and a "cover" that is really an HTML document sniffs as text/html. Stage
// has already established that these bytes decode as an image, so the type
// comes from the decoder that read the header.
//
// The bytes are held in memory rather than stored: they are the same ones
// internal/scanner holds per file while a sweep runs, each format package
// has already capped what it read, and writing a thumbnail into COVERS_DIR
// for a book that may never be imported would put a file there that nothing
// owns.
func (s *Stager) Cover(id string) (data []byte, contentType string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, found := s.stages[id]
	if !found || s.expired(record) || len(record.cover) == 0 {
		return nil, "", false
	}
	return record.cover, record.coverType, true
}

// Confirm copies the staged file into the library and indexes it, returning
// the book the library now holds it under.
//
// The preview's verdict is not consulted. It was a snapshot, and a sweep
// can index the same bytes between preview and confirm; IndexFile's answer
// is the truth. If the book it names already had another location, the copy
// just written is a second location of a known book — which the detail page
// renders as "2 paths", the correct outcome for the race — and the redirect
// lands on that book.
//
// A repeated confirm answers the same book id as the first, so a double
// click is a slip rather than an error. A confirm of content that was
// already in the library at preview time answers the existing book for the
// same reason: the bytes are in the library and indexed, which is what the
// caller was asking for.
func (s *Stager) Confirm(ctx context.Context, id string) (int64, error) {
	if !s.writable {
		return 0, ErrLibraryNotWritable
	}

	s.mu.Lock()
	record, ok := s.stages[id]
	if ok && s.expired(record) {
		s.remove(id, record)
		ok = false
	}
	s.mu.Unlock()
	if !ok {
		return 0, ErrExpired
	}

	record.confirm.Lock()
	defer record.confirm.Unlock()

	if record.done {
		if record.bookID == 0 {
			return 0, ErrNotIndexed
		}
		return record.bookID, nil
	}

	name, err := s.copyIntoLibrary(record)
	if err != nil {
		return 0, err
	}

	// Marked before the index write, not after: from here the library owns
	// the bytes, and whichever way indexing goes a second confirm must
	// answer rather than copy them in again. The staged file goes with it;
	// the record itself stays until it expires, which is what lets a
	// double click get the first call's answer.
	s.taken(record)

	// Deliberately not ctx: the library owns the bytes from the line above,
	// and a browser that closed its tab in this gap would otherwise cancel
	// the index write, mark the stage done with no book, and make every
	// later confirm report a failure that did not happen — about a file
	// sitting in the library that the next sweep will index anyway. The
	// same rule internal/sender applies once Resend has accepted a message.
	indexCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), indexTimeout)
	defer cancel()

	bookID, created, err := scanner.IndexFile(indexCtx, s.db, s.libraryDir, filepath.Join(s.libraryDir, name), s.coversDir)
	if err != nil {
		// The file stays in the library. Deleting it would discard the
		// bytes over a database error, where the next sweep is the
		// recovery the scanner already promises.
		slog.Warn("imported file could not be indexed; the next scan will pick it up", "name", name, "error", err)
		return 0, fmt.Errorf("%w: %w", ErrNotIndexed, err)
	}
	if !created {
		// The preview's verdict was a snapshot and a sweep can index the
		// same bytes between it and this call, so landing on a book that
		// already existed is a correct outcome and not an error — but it
		// is a surprising one, since the person is about to be sent to a
		// book they did not think they were importing.
		slog.Info("imported file joined a book the library already had", "name", name, "book_id", bookID)
	}

	s.landed(record, bookID)
	return bookID, nil
}

// copyIntoLibrary writes the staged bytes to a claimed <name>.part and
// publishes it under a name nothing else holds, which is the only way
// anything in this package creates a supported suffix in the library
// directory. A sweep or the watcher meeting the copy sees only the .part,
// which matchedSuffix answers "" for.
//
// A copy and not a rename out of staging: those are different filesystems
// in every deployment that matters, and os.Rename across them fails. Sync
// before publishing, so a machine that loses power between the two has
// either no file or the whole one.
func (s *Stager) copyIntoLibrary(record *stage) (string, error) {
	src, err := os.Open(record.path)
	if errors.Is(err, fs.ErrNotExist) {
		// Expiry is checked under s.mu and the file is opened after the
		// lock is dropped, so a janitor sweep crossing the time to live in
		// that window takes the file out from under this call. It is the
		// expiry the caller already has a sentence for, not a filesystem
		// error nobody can name.
		return "", ErrExpired
	}
	if err != nil {
		return "", err
	}
	defer src.Close()

	suffix := suffixOf(record.path)
	stem := libraryStem(record.staged.OriginalName, record.staged.Title, record.staged.ID, suffix)

	dst, part, err := claimPart(s.libraryDir, stem, suffix)
	if err != nil {
		return "", err
	}

	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		os.Remove(part)
		return "", err
	}
	if err := dst.Sync(); err != nil {
		dst.Close()
		os.Remove(part)
		return "", err
	}
	if err := dst.Close(); err != nil {
		os.Remove(part)
		return "", err
	}

	name, err := publish(s.libraryDir, part, stem, suffix)
	if err != nil {
		os.Remove(part)
		return "", err
	}
	return name, nil
}

// Discard drops a staged import and its file. An id that is already gone is
// not an error: discarding is what the person asked for and it has
// happened.
func (s *Stager) Discard(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if record, ok := s.stages[id]; ok {
		s.remove(id, record)
	}
}

// RunJanitor sweeps expired stages until ctx is cancelled.
func (s *Stager) RunJanitor(ctx context.Context) {
	ticker := time.NewTicker(janitorInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweep()
		}
	}
}

func (s *Stager) sweep() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, record := range s.stages {
		if s.expired(record) {
			s.remove(id, record)
		}
	}
}

// expired reports whether record is past its time to live. Callers hold s.mu.
func (s *Stager) expired(record *stage) bool {
	return s.now().Sub(record.created) >= StageTTL
}

// remove drops a stage, deletes its file and gives its share of the budget
// back. Callers hold s.mu.
func (s *Stager) remove(id string, record *stage) {
	if record.path != "" {
		os.Remove(record.path)
	}
	s.staged -= record.reserved
	record.reserved = 0
	delete(s.stages, id)
}

// taken marks a stage whose bytes the library now holds and deletes the
// staging copy.
func (s *Stager) taken(record *stage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if record.path != "" {
		os.Remove(record.path)
		record.path = ""
	}
	// The record outlives the confirm so a double click gets its answer,
	// but it is holding no bytes any more and must not hold budget either.
	s.staged -= record.reserved
	record.reserved = 0
	record.done = true
}

// landed records the book an import ended up as.
func (s *Stager) landed(record *stage, bookID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record.bookID = bookID
}

// newStageID returns 128 random bits in lower-case base32 without padding.
//
// Random rather than sequential so one browser tab cannot reach another
// person's stage by guessing the number next to its own. It is not a
// credential — every state-changing route is same-site-only regardless —
// it is what keeps two people's imports apart on a server that has no idea
// who either of them is.
func newStageID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw[:])), nil
}

// meta is the subset of a format package's Metadata the preview shows.
// epub.Metadata and fb2.Metadata are structurally identical but distinct
// types, the same reason internal/scanner keeps its own bookMeta.
type meta struct {
	Title         string
	Authors       []string
	Language      string
	ISBN          string
	Publisher     string
	PublishedDate string
	Description   string
	Cover         []byte
}

// readMetadata parses the staged file for the preview alone. What is
// actually stored comes from the scanner's own extraction during
// IndexFile — including its caps — so a parse failure here costs a
// populated preview and nothing else.
//
// The filename fallback is the client's name rather than the staged path,
// which is an opaque id: a file with no embedded title previews under the
// name the person recognises.
func readMetadata(path, suffix, original string) meta {
	fallback := stripSuffix(original, suffix)

	switch suffix {
	case ".epub":
		m, err := epub.ReadMetadata(path)
		if err != nil {
			slog.Info("read staged metadata failed", "name", original, "error", err)
			return meta{Title: fallback}
		}
		return meta{
			Title:         orFallback(m.Title, fallback),
			Authors:       m.Authors,
			Language:      m.Language,
			ISBN:          m.ISBN,
			Publisher:     m.Publisher,
			PublishedDate: m.PublishedDate,
			Description:   m.Description,
			Cover:         m.Cover,
		}
	default:
		m, err := fb2.ReadMetadata(path)
		if err != nil {
			slog.Info("read staged metadata failed", "name", original, "error", err)
			return meta{Title: fallback}
		}
		return meta{
			Title:         orFallback(m.Title, fallback),
			Authors:       m.Authors,
			Language:      m.Language,
			ISBN:          m.ISBN,
			Publisher:     m.Publisher,
			PublishedDate: m.PublishedDate,
			Description:   m.Description,
			Cover:         m.Cover,
		}
	}
}

// capMetadata bounds a preview to what the import would actually store.
//
// Two reasons, and the second is the one that bites. A preview is rendered
// into a page, and what the parsers hand back is bounded only by the
// document caps in internal/epub and internal/fb2 — four megabytes of
// description for an EPUB, and for a .fb2.zip a single text node may run to
// maxZipDocumentBytes, which html/template then escapes on its way to a
// browser. And the title is what the title-match verdict compares, through
// storage.SortTitle, against a sort_title the scanner derived from a
// *capped* title: uncapped here, a long title could never match the row it
// is a duplicate of.
//
// storage.CapField and not a copy of it, so the preview and internal/scanner
// cannot disagree about what will be kept. No logging: a sweep's Info line
// names a file in a directory, where this is one upload a person is looking
// at, and the preview showing the capped value is the report
func capMetadata(m meta) meta {
	m.Title, _ = storage.CapField(storage.FieldTitle, m.Title)
	m.Language, _ = storage.CapField(storage.FieldLanguage, m.Language)
	m.ISBN, _ = storage.CapField(storage.FieldISBN, m.ISBN)
	m.Publisher, _ = storage.CapField(storage.FieldPublisher, m.Publisher)
	m.PublishedDate, _ = storage.CapField(storage.FieldPublishedDate, m.PublishedDate)
	m.Description, _ = storage.CapField(storage.FieldDescription, m.Description)

	for i, name := range m.Authors {
		m.Authors[i], _ = storage.CapField(storage.FieldAuthors, name)
	}
	if len(m.Authors) > storage.MaxAuthors {
		m.Authors = m.Authors[:storage.MaxAuthors]
	}
	return m
}

func orFallback(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// bookFormat maps a sniffed suffix to the badge the preview shows, the same
// collapse internal/scanner makes: how a book is packaged on disk is not
// something the format badge should surface.
func bookFormat(suffix string) string {
	if suffix == ".epub" {
		return "epub"
	}
	return "fb2"
}

// suffixOf recovers the sniffed suffix from a staged path, which is the id
// with the suffix on it.
func suffixOf(path string) string {
	base := filepath.Base(path)
	for _, s := range []string{".fb2.zip", ".epub", ".fb2"} {
		if strings.HasSuffix(base, s) {
			return s
		}
	}
	return ""
}
