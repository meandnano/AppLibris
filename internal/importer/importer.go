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
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

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

	staged  Staged
	path    string
	hash    string
	cover   []byte
	created time.Time
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
	writable   bool
	now        func() time.Time

	mu     sync.Mutex
	stages map[string]*stage
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

	meta := readMetadata(staged, suffix, name)
	record := &stage{
		staged: Staged{
			ID:            id,
			OriginalName:  name,
			LibraryName:   libraryName(libraryStem(name, meta.Title, id, suffix), suffix),
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
		path:    staged,
		hash:    hash,
		cover:   meta.Cover,
		created: s.now(),
	}

	if err := s.decideVerdict(ctx, record); err != nil {
		os.Remove(staged)
		return Staged{}, err
	}

	s.mu.Lock()
	s.stages[id] = record
	s.mu.Unlock()

	return record.staged, nil
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

// Cover returns the cover bytes a staged file had embedded, for the
// preview's own image, or nil when it had none.
//
// The bytes are held in memory rather than stored: they are the same ones
// internal/scanner holds per file while a sweep runs, each format package
// has already capped what it read, and writing a thumbnail into COVERS_DIR
// for a book that may never be imported would put a file there that nothing
// owns.
func (s *Stager) Cover(id string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.stages[id]
	if !ok || s.expired(record) || len(record.cover) == 0 {
		return nil, false
	}
	return record.cover, true
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

	bookID, err := scanner.IndexFile(ctx, s.db, s.libraryDir, filepath.Join(s.libraryDir, name), s.coversDir)
	if err != nil {
		// The file stays in the library. Deleting it would discard the
		// bytes over a database error, where the next sweep is the
		// recovery the scanner already promises.
		slog.Warn("imported file could not be indexed; the next scan will pick it up", "name", name, "error", err)
		return 0, fmt.Errorf("%w: %w", ErrNotIndexed, err)
	}

	s.landed(record, bookID)
	return bookID, nil
}

// copyIntoLibrary writes the staged bytes to a claimed <name>.part and
// renames it onto <name>, which is the only way anything in this package
// creates a supported suffix in the library directory.
//
// A copy and not a rename from the staging directory: those are different
// filesystems in every deployment that matters, and os.Rename across them
// fails. Sync before the rename, so a machine that loses power between the
// two has either no file or the whole one.
func (s *Stager) copyIntoLibrary(record *stage) (string, error) {
	src, err := os.Open(record.path)
	if err != nil {
		return "", err
	}
	defer src.Close()

	suffix := suffixOf(record.path)
	stem := libraryStem(record.staged.OriginalName, record.staged.Title, record.staged.ID, suffix)

	dst, name, err := createPart(s.libraryDir, stem, suffix)
	if err != nil {
		return "", err
	}
	part := filepath.Join(s.libraryDir, name+partSuffix)

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
	if err := os.Rename(part, filepath.Join(s.libraryDir, name)); err != nil {
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

// remove drops a stage and deletes its file. Callers hold s.mu.
func (s *Stager) remove(id string, record *stage) {
	if record.path != "" {
		os.Remove(record.path)
	}
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
