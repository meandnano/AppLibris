package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"library/internal/enrich"
	"library/internal/importer"
	"library/internal/providers"
	"library/internal/resend"
	"library/internal/scanner"
	"library/internal/sender"
	"library/internal/service"
	"library/internal/storage"
	"library/internal/web"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx); err != nil {
		slog.Error("run", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	dbPath := envOrDefault("DB_PATH", "/data/library.db")
	addr := envOrDefault("ADDR", ":8080")
	libraryDir := envOrDefault("LIBRARY_DIR", "/library")
	coversDir := envOrDefault("COVERS_DIR", "/data/covers")

	var level slog.Level
	if err := level.UnmarshalText([]byte(envOrDefault("LOG_LEVEL", "INFO"))); err != nil {
		return fmt.Errorf("parse LOG_LEVEL: %w", err)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	scanInterval, err := time.ParseDuration(envOrDefault("SCAN_INTERVAL", "1h"))
	if err != nil {
		return fmt.Errorf("parse SCAN_INTERVAL: %w", err)
	}
	missingGrace, err := time.ParseDuration(envOrDefault("MISSING_GRACE", "24h"))
	if err != nil {
		return fmt.Errorf("parse MISSING_GRACE: %w", err)
	}
	if missingGrace < 0 {
		// A negative grace pulls the pruning cutoff into the future, so a
		// file marked missing this very sweep would immediately qualify for
		// deletion — silently bypassing the two-phase safeguard entirely.
		return fmt.Errorf("parse MISSING_GRACE: must not be negative: %s", missingGrace)
	}
	watchEnabled, err := strconv.ParseBool(envOrDefault("WATCH_ENABLED", "true"))
	if err != nil {
		return fmt.Errorf("parse WATCH_ENABLED: %w", err)
	}
	watchSettle, err := time.ParseDuration(envOrDefault("WATCH_SETTLE", "5s"))
	if err != nil {
		return fmt.Errorf("parse WATCH_SETTLE: %w", err)
	}
	if watchSettle < 0 {
		return fmt.Errorf("parse WATCH_SETTLE: must not be negative: %s", watchSettle)
	}
	requireFetchMetadata, err := strconv.ParseBool(envOrDefault("REQUIRE_FETCH_METADATA", "true"))
	if err != nil {
		return fmt.Errorf("parse REQUIRE_FETCH_METADATA: %w", err)
	}
	maxImportSize, err := parseByteSize(envOrDefault("MAX_IMPORT_SIZE", "64MiB"))
	if err != nil {
		return fmt.Errorf("parse MAX_IMPORT_SIZE: %w", err)
	}

	metadataProviderNames := metadataProviderNames(os.LookupEnv)

	googleBooksAPIKey := os.Getenv("GOOGLE_BOOKS_API_KEY")
	if googleBooksAPIKey == "" && slices.Contains(metadataProviderNames, "googlebooks") {
		// The anonymous quota is shared across every keyless caller of
		// this API and has been observed exhausted on every attempt, so
		// the warning has to say the provider will most likely answer
		// nothing rather than merely answer less. A Warn and not a startup
		// failure: browsing and the other provider both work without it.
		slog.Warn("GOOGLE_BOOKS_API_KEY is not set: Google Books enrichment will use the shared anonymous quota, which is routinely exhausted and answers 429")
	}

	// An unknown name fails startup outright (unlike a missing
	// RESEND_API_KEY, which only warns): asking for a specific provider
	// and silently getting fewer than requested is the kind of thing
	// nobody notices for months.
	metadataProviders, err := providers.Resolve(metadataProviderNames, googleBooksAPIKey)
	if err != nil {
		return fmt.Errorf("resolve METADATA_PROVIDERS: %w", err)
	}

	// filepath.WalkDir Lstats its root and never follows a link, so a
	// symlinked LIBRARY_DIR — ~/Books -> /volume1/books, the ordinary NAS
	// shape — is visited once as a non-directory entry and the walk ends:
	// zero books, one "library appeared empty" Warn, no error at all. The
	// resolution lives here rather than in the scanner because every
	// consumer has to agree on one root: the walk, the watcher, and the
	// sender worker resolving a book's file. Relative file_path storage is
	// unaffected, since every stored path is made relative to whatever
	// root the scanner is handed
	libraryDir, err = requireExistingDir("library directory", "LIBRARY_DIR", libraryDir)
	if err != nil {
		return err
	}
	coversDir, err = resolveDir("covers directory", coversDir)
	if err != nil {
		return err
	}
	// DB_PATH is never walked; it is resolved so the listening line names
	// the file the process actually opened. Only the directory can be
	// resolved before storage.Open, since on a first run the database file
	// itself does not exist yet — filepath.EvalSymlinks needs every
	// element of a path to be there
	dbDir, err := resolveDir("database directory", filepath.Dir(dbPath))
	if err != nil {
		return err
	}
	dbPath = filepath.Join(dbDir, filepath.Base(dbPath))

	db, err := storage.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}

	// Decided after the library directory is resolved and before anything is
	// served, so the answer is settled for the run: import is either on or
	// it is off, and no request has to rediscover it.
	importDir := filepath.Join(os.TempDir(), "applibris-imports")
	stager := newStager(db, libraryDir, coversDir, importDir, maxImportSize)
	svc := service.New(db,
		service.WithImporter(stager),
		service.WithFetcher(importer.NewFetcher(maxImportSize)),
	)
	importEnabled := svc.ImportEnabled()

	// Both RESEND_API_KEY and RESEND_FROM must be set to send anything —
	// browsing must still work on a dev machine with neither, so a missing
	// one only disables sending rather than failing startup. sendEnabled
	// is threaded into web.Routes either way: the send routes stay
	// registered so a stale open tab gets an explanation instead of a 404.
	resendAPIKey := os.Getenv("RESEND_API_KEY")
	resendFrom := os.Getenv("RESEND_FROM")
	sendEnabled := resendAPIKey != "" && resendFrom != ""

	var worker *sender.Worker
	if sendEnabled {
		worker = sender.New(db, resend.NewClient(resendAPIKey, resendFrom), libraryDir)
		svc.Notify = worker.Notify
	} else if resendAPIKey == "" {
		slog.Warn("sending disabled: RESEND_API_KEY is not set")
	} else {
		slog.Warn("sending disabled: RESEND_FROM is not set")
	}

	// Unlike sending, there's no separate config flag that disables
	// enrichment: METADATA_PROVIDERS= (empty) already resolves to an empty
	// provider list above, which makes every job a no-op, so the worker
	// always runs and its queue/worker wiring is exercised either way.
	//
	// The UI is a different question, and takes the provider count rather
	// than a config flag: a control that offers to fetch metadata from
	// nowhere would be a button that cannot do what it says, so with no
	// provider configured web.Routes gets the same disabled treatment the
	// send control shows when Resend is unconfigured, and for the same
	// reason.
	enrichWorker := enrich.New(db, metadataProviders, coversDir)
	enrichEnabled := len(metadataProviders) > 0
	if enrichEnabled {
		svc.NotifyEnrichment = enrichWorker.Notify
	} else {
		slog.Warn("enrichment disabled: METADATA_PROVIDERS is empty")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	// The UI's cross-site guard reads Sec-Fetch-Site, which browsers send
	// only to an HTTPS or localhost origin, so the service is documented as
	// requiring an HTTPS gateway in front with this listener reachable only
	// through it. Failing closed by default is what makes a deployment that
	// breaks the requirement show up in the log instead of silently running
	// without the guard; the opt-out keeps a one-line tripwire.
	if !requireFetchMetadata {
		slog.Warn("REQUIRE_FETCH_METADATA=false: state-changing requests without fetch metadata are admitted, so cross-site protection depends on the listener being unreachable from any browser except through an HTTPS gateway")
	}
	mux.Handle("/", fetchMetadataGuard(requireFetchMetadata, web.Routes(svc, coversDir, sendEnabled, enrichEnabled)))

	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
		// ReadHeaderTimeout guards a client that opens a connection and
		// never sends a request line. WriteTimeout is sized with
		// send-to-Kindle in mind: a send is a queued background job
		// (docs/notes/sending.md) precisely so a handler never holds a request
		// open reading a multi-megabyte book off disk — the worker does
		// that instead — so 60s here is headroom, not a design constraint.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		// The three paths are the resolved ones, which is the point of
		// logging them: a symlinked LIBRARY_DIR is exactly the configuration
		// whose effective root nothing else on the box makes visible
		slog.Info("listening", "addr", addr, "db_path", dbPath, "library_dir", libraryDir, "covers_dir", coversDir,
			"import_enabled", importEnabled, "import_staging_dir", importDir, "max_import_size", maxImportSize)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	// The scan goroutine gets its own cancellable context, derived from ctx
	// but stoppable independently of it: a serving failure (the address is
	// already in use, say) needs to cancel and wait for the scan just as
	// reliably as a signal does, rather than closing the database out from
	// under whatever the scan is doing. The health endpoint must answer the
	// moment the process is up, so the first sweep runs in the background
	// alongside the periodic one rather than blocking startup — a large
	// library's first scan is minutes of hashing, during which an
	// orchestrator's readiness probe would otherwise conclude the container
	// is dead and restart it.
	// The worker runs on the same independently-cancellable scanCtx as the
	// scan loop — send-to-Kindle jobs and library sweeps both want the
	// same shutdown ordering (unwind before the database closes under
	// them), so one child context and one wait pattern serves both.
	scanCtx, cancelScan := context.WithCancel(ctx)
	defer cancelScan()

	// The watcher only ever pokes this channel; the scan goroutine below is
	// still the only thing that calls Scan, so two sweeps can never overlap
	// and no lock is needed. Capacity 1 collapses a burst of pokes into one
	// pending wake-up.
	scanTrigger := make(chan struct{}, 1)
	var watcher *scanner.Watcher
	if watchEnabled {
		watcher, err = scanner.NewWatcher(libraryDir, watchSettle, scanTrigger)
		if err != nil {
			// The periodic rescan is the mechanism; the watcher only makes
			// it prompt. Losing it is a warning, not a failed startup.
			slog.Warn("filesystem watcher disabled", "error", err)
			watcher = nil
		}
	}

	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		reported := runScan(scanCtx, db, libraryDir, coversDir, missingGrace, nil)
		if watcher != nil {
			watcher.Refresh()
		}
		periodicScan(scanCtx, db, libraryDir, coversDir, scanInterval, missingGrace, scanTrigger, watcher, reported)
	}()

	var watcherDone chan struct{}
	if watcher != nil {
		watcherDone = make(chan struct{})
		go func() {
			defer close(watcherDone)
			watcher.Run(scanCtx)
		}()
	}

	var workerDone chan struct{}
	if sendEnabled {
		// A row still "sending" means the process died between handing
		// the bytes to Resend and recording the answer — which side of
		// that request it died on is unknowable, so recovery fails the
		// row rather than requeueing it (see internal/sender's doc
		// comment). Runs once, before the worker starts claiming jobs.
		if _, err := db.FailInterruptedSends(ctx, "interrupted by a restart — send again if it didn't arrive", time.Now()); err != nil {
			slog.Error("fail interrupted sends", "error", err)
		}

		workerDone = make(chan struct{})
		go func() {
			defer close(workerDone)
			worker.Run(scanCtx)
		}()
	}

	// Unlike FailInterruptedSends above, this requeues rather than fails —
	// see storage.RequeueInterruptedEnrichment's doc comment for why an
	// interrupted enrichment job is safe to simply run again, where an
	// interrupted send is not. Runs unconditionally, since the enrichment
	// worker itself always runs.
	if _, err := db.RequeueInterruptedEnrichment(ctx, time.Now()); err != nil {
		slog.Error("requeue interrupted enrichment", "error", err)
	}
	enrichDone := make(chan struct{})
	go func() {
		defer close(enrichDone)
		enrichWorker.Run(scanCtx)
	}()

	// On scanCtx like every other background loop, so a staged file is
	// never deleted out from under a confirm that is running while the
	// process shuts down. Absent with the importer, which is absent when
	// the library cannot be written.
	var janitorDone chan struct{}
	if stager != nil {
		janitorDone = make(chan struct{})
		go func() {
			defer close(janitorDone)
			stager.RunJanitor(scanCtx)
		}()
	}

	select {
	case err := <-serveErr:
		// A serving failure isn't a signal, so ctx (and scanCtx, derived
		// from it) is still live — cancel scanCtx explicitly and wait out
		// the same bounded budget the signal path uses below, so neither
		// background goroutine can still be using db when it's closed a
		// few lines down.
		deadline, cancelDeadline := context.WithTimeout(context.Background(), 10*time.Second)
		waitForBackground(cancelScan, scanDone, deadline.Done(), "scan")
		if watcherDone != nil {
			waitForBackground(cancelScan, watcherDone, deadline.Done(), "watcher")
		}
		if workerDone != nil {
			waitForBackground(cancelScan, workerDone, deadline.Done(), "sender")
		}
		waitForBackground(cancelScan, enrichDone, deadline.Done(), "enrichment")
		if janitorDone != nil {
			waitForBackground(cancelScan, janitorDone, deadline.Done(), "import janitor")
		}
		cancelDeadline()
		if closeErr := db.Close(); closeErr != nil {
			slog.Error("close database", "error", closeErr)
		}
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown", "error", err)
	}
	<-serveErr

	// Give the background goroutines the rest of the shutdown budget to
	// notice cancellation and unwind cleanly (an in-flight write returns on
	// cancellation, rolling back rather than committing a partial one)
	// before the database closes out from under them. cancelScan is
	// already implied by ctx.Done() here (scanCtx is derived from ctx),
	// but calling it explicitly costs nothing and keeps this path
	// symmetric with the serveErr one above. A goroutine stuck outside any
	// context-aware call — mid-hash on a large file, mid-upload on a
	// send — can still miss this window; the bound exists so that doesn't
	// hang shutdown.
	waitForBackground(cancelScan, scanDone, shutdownCtx.Done(), "scan")
	if watcherDone != nil {
		waitForBackground(cancelScan, watcherDone, shutdownCtx.Done(), "watcher")
	}
	if workerDone != nil {
		waitForBackground(cancelScan, workerDone, shutdownCtx.Done(), "sender")
	}
	waitForBackground(cancelScan, enrichDone, shutdownCtx.Done(), "enrichment")
	if janitorDone != nil {
		waitForBackground(cancelScan, janitorDone, shutdownCtx.Done(), "import janitor")
	}

	return db.Close()
}

// newStager builds the importer when this run can import, and returns nil
// when it cannot — which is the whole of what "importing is offered" means.
// A Stager exists exactly when importing is available: there is no disabled
// Stager, and internal/service answers a nil one with its own explanation.
//
// Two preconditions, and neither failing is a startup failure. The library
// directory must be writable, which the probe decides, and a read-only
// library is the documented deployment. The staging directory must be
// creatable, and it is os.TempDir(), which a hardened container — one run
// --read-only with no tmpfs at /tmp — does not provide; a library that can
// be read is still worth serving there. The Warn names the directory so
// TMPDIR is the obvious remedy.
func newStager(db *storage.DB, libraryDir, coversDir, importDir string, maxSize int64) *importer.Stager {
	if !probeWritable(libraryDir) {
		return nil
	}
	stager, err := importer.New(db, importer.Options{
		LibraryDir: libraryDir,
		CoversDir:  coversDir,
		TempDir:    importDir,
		MaxSize:    maxSize,
	})
	if err != nil {
		slog.Warn("importing disabled", "error", err)
		return nil
	}
	return stager
}

// writeProbeName is the file probeWritable creates and removes. It begins
// with a dot and carries no supported suffix, so a sweep that overlaps it
// walks past it: the scanner indexes neither.
//
// One fixed name rather than a unique one per run, so a crash between the
// create and the remove litters at most one file however many times the
// process restarts — which is only safe because the probe clears the name
// before claiming it.
const writeProbeName = ".applibris-write-probe"

// probeWritable reports whether the process can create a file in dir, which
// is what decides whether importing is offered for the run.
//
// A probe rather than a look at the mode bits, because a read-only mount,
// an ACL and a uid mismatch all fail at the same call and none of them
// shows in the mode. Once at startup rather than per request, because the
// answer does not change while the process runs and a confirm that fails
// anyway reports its own error.
//
// Failing is not a startup failure: the read-only library is the
// documented deployment, and everything else about the app works on one.
// It is a Warn naming both uids, the shape mkdirError already uses for the
// first thing that goes wrong in a container.
func probeWritable(dir string) bool {
	path := filepath.Join(dir, writeProbeName)

	// Cleared before it is claimed, because the open below refuses a name
	// that is already taken: a probe left behind by a crash, or by the
	// remove at the end failing, would otherwise answer "not writable" for
	// every later start of a perfectly writable library. The error is
	// dropped on purpose — a directory that may not be written fails this
	// too, and the open is the call whose failure actually describes why.
	os.Remove(path)

	// O_EXCL and not a plain create, so the open refuses to follow a
	// symlink someone left at this name rather than writing through it to
	// whatever it points at. os.Remove above unlinks such a link itself
	// rather than its target, so the pair never touches the far end.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		slog.Warn("importing disabled", "error", importProbeError(dir, err))
		return false
	}
	f.Close()

	// Worth a line of its own: the file is harmless and the scanner ignores
	// it, but it is the one piece of litter this can leave in a directory
	// the person manages by hand. Already gone is not a failure — a
	// concurrent start clearing it is doing this function's own work.
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Warn("could not remove the library write probe", "path", path, "error", err)
	}
	return true
}

// importProbeError explains a library directory the process may not write,
// naming the uid it runs as and the uid that owns the directory — the same
// two facts mkdirError names, and for the same reason: a NAS bind mount is
// owned by the share's user, an Unraid one by nobody, and neither is
// visible from the bare permission-denied.
func importProbeError(dir string, err error) error {
	wrapped := fmt.Errorf("cannot write to library directory %s: %w", dir, err)
	if !errors.Is(err, fs.ErrPermission) {
		return wrapped
	}
	if owner, path, ok := nearestOwnerUID(dir); ok {
		return fmt.Errorf("%w (running as uid %d; %s is owned by uid %d)", wrapped, os.Getuid(), path, owner)
	}
	return fmt.Errorf("%w (running as uid %d)", wrapped, os.Getuid())
}

// byteSuffixes maps the size suffixes MAX_IMPORT_SIZE accepts to their
// multipliers. Both spellings are here because both are in circulation and
// neither reading is surprising enough to refuse: "64M" from a person
// thinking in megabytes and "64Mi" from one thinking in mebibytes should
// not differ by a factor nobody asked about, so the decimal ones are exact
// powers of ten and the binary ones exact powers of two, as written.
var byteSuffixes = []struct {
	suffix string
	unit   int64
}{
	{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30},
	{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30},
	{"KB", 1000}, {"MB", 1000 * 1000}, {"GB", 1000 * 1000 * 1000},
	{"K", 1000}, {"M", 1000 * 1000}, {"G", 1000 * 1000 * 1000},
	{"B", 1},
}

// parseByteSize reads a byte count with an optional unit suffix.
//
// Zero and negative are refused rather than read as "no limit": the cap is
// what bounds an upload into temporary space, and a deployment that meant
// to disable importing takes away write access to the library instead,
// which is the thing the app actually checks.
func parseByteSize(raw string) (int64, error) {
	text := strings.TrimSpace(raw)
	unit := int64(1)
	for _, s := range byteSuffixes {
		if len(s.suffix) < len(text) && strings.EqualFold(text[len(text)-len(s.suffix):], s.suffix) {
			unit = s.unit
			text = strings.TrimSpace(text[:len(text)-len(s.suffix)])
			break
		}
	}

	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a byte count", raw)
	}
	if n <= 0 {
		return 0, fmt.Errorf("must be positive: %q", raw)
	}
	if n > math.MaxInt64/unit {
		return 0, fmt.Errorf("is too large: %q", raw)
	}
	return n * unit, nil
}

// resolveDir creates dir if it is absent and returns it with every symlink
// along it resolved, so that nothing downstream is handed a path whose
// meaning depends on whether links are followed.
//
// A link that resolves nowhere is reported before MkdirAll rather than
// after: MkdirAll fails on one too (Stat follows the link and finds
// nothing, Mkdir then fails EEXIST on the link itself), but its message
// names the link it could not replace and never the target that is
// missing — which is the whole question when ~/Books points at a volume
// that did not mount
func resolveDir(label, dir string) (string, error) {
	if err := danglingLink(label, dir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", mkdirError(label, dir, err)
	}
	return evalSymlinks(label, dir)
}

// requireExistingDir resolves dir the way resolveDir does but never creates
// it, naming envVar when it is not there.
//
// The library is the one configured path nothing writes: the scanner only
// reads it, so creating it is the single call that turns a legitimately
// read-only mount into a startup failure, and a library that does not exist
// is a misconfiguration rather than something to make empty. Creating it
// and warning hides that under an empty grid — which is what LIBRARY_DIR
// pointing at the wrong volume already looks like — leaving the log as the
// only place the mistake shows.
//
// filepath.EvalSymlinks refuses a path that is not there anyway; the stat
// is here so the message names the variable to fix rather than an
// ENOENT from a resolver
func requireExistingDir(label, envVar, dir string) (string, error) {
	if err := danglingLink(label, dir); err != nil {
		return "", err
	}
	info, err := os.Stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("%s %s is not there: %s must name a directory that already exists", label, dir, envVar)
	case err != nil:
		return "", fmt.Errorf("%s %s: %w", label, dir, err)
	case !info.IsDir():
		return "", fmt.Errorf("%s %s is not a directory: %s must name one", label, dir, envVar)
	}
	return evalSymlinks(label, dir)
}

func danglingLink(label, dir string) error {
	link, target, ok := brokenLink(dir)
	switch {
	case !ok:
		return nil
	case link == dir:
		return fmt.Errorf("%s %s is a symlink to %s, which is not there", label, dir, target)
	default:
		return fmt.Errorf("%s %s: %s is a symlink to %s, which is not there", label, dir, link, target)
	}
}

func evalSymlinks(label, dir string) (string, error) {
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("resolve %s %s: %w", label, dir, err)
	}
	return resolved, nil
}

// mkdirError explains a directory the process was not allowed to create.
// Permission denied on a mounted volume is the first thing a container run
// meets and neither side of it is in the bare message: the container runs
// as whatever uid it was given while a NAS bind mount is owned by the
// share's user, an Unraid one by nobody, and a fresh named volume by root.
// Naming both uids turns "mkdir /data/covers: permission denied" into an
// instruction.
//
// The owner named is the nearest existing ancestor's, since the target
// directory is precisely what MkdirAll could not make
func mkdirError(label, dir string, err error) error {
	wrapped := fmt.Errorf("create %s %s: %w", label, dir, err)
	if !errors.Is(err, fs.ErrPermission) {
		return wrapped
	}
	if owner, path, ok := nearestOwnerUID(dir); ok {
		return fmt.Errorf("%w (running as uid %d; %s is owned by uid %d)", wrapped, os.Getuid(), path, owner)
	}
	return fmt.Errorf("%w (running as uid %d)", wrapped, os.Getuid())
}

// nearestOwnerUID walks dir's components from the deepest down and reports
// the owner of the first one that exists, along with the path it read.
//
// The path is made absolute for the message, since a relative COVERS_DIR
// or DB_PATH leaves the ancestor of "./data/covers" reading back as
// "data" — a name the person is then asked to go and check the ownership
// of, and which does not appear in the path they configured
func nearestOwnerUID(dir string) (uid int, path string, ok bool) {
	components := ancestors(dir)
	for i := len(components) - 1; i >= 0; i-- {
		if uid, ok := ownerUID(components[i]); ok {
			named := components[i]
			if abs, err := filepath.Abs(named); err == nil {
				named = abs
			}
			return uid, named, true
		}
	}
	return 0, "", false
}

// brokenLink returns the first component of path that is a symlink whose
// target is not there, along with that target. Every component is checked
// and not just the last, since the link is as likely to be an ancestor as
// the configured path itself — /mnt/nas/books with /mnt/nas -> /volume1
// unmounted is the same failure one level up.
//
// A component that is merely absent is not a broken link: creating it is
// MkdirAll's job, and a link is the one shape MkdirAll cannot describe. A
// relative target is joined onto its link's directory, so what the message
// names is a path the reader can go and look at rather than the text of
// the link
func brokenLink(path string) (link, target string, ok bool) {
	for _, p := range ancestors(path) {
		if _, err := os.Stat(p); err == nil {
			continue
		}
		t, err := os.Readlink(p)
		if err != nil {
			return "", "", false
		}
		if !filepath.IsAbs(t) {
			t = filepath.Join(filepath.Dir(p), t)
		}
		return p, t, true
	}
	return "", "", false
}

// ancestors lists path's components from the root down, ending with path
// itself
func ancestors(path string) []string {
	var out []string
	for p := filepath.Clean(path); ; {
		out = append(out, p)
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		p = parent
	}
	slices.Reverse(out)
	return out
}

// fetchMetadataGuard picks which of internal/web's two fetch-metadata
// wrappers goes around the UI routes. A function rather than an inline if
// so the mapping from the parsed setting to the wrapper is testable on its
// own: swapping the two branches inverts the security default with every
// handler test still green, which is exactly the kind of mistake a
// verification checklist does not catch and a table test does.
func fetchMetadataGuard(require bool, next http.Handler) http.Handler {
	if require {
		return web.RequireFetchMetadata(next)
	}
	return web.WarnMissingFetchMetadata(next)
}

// waitForBackground cancels the background goroutine driven by cancel and
// waits for done to close, up to deadline, warning (naming name as the
// task attribute) if it doesn't. Shared by the scan loop and the send-to-Kindle
// worker, which run on the same cancellable scanCtx: the caller must not
// close the database until every call sharing that ctx has returned, or a
// goroutine that missed its deadline could still write onto a closed
// connection. Calling cancel more than once (once per shared-ctx goroutine
// waited on) is safe — context.CancelFunc is idempotent.
func waitForBackground(cancel context.CancelFunc, done <-chan struct{}, deadline <-chan struct{}, name string) {
	cancel()
	select {
	case <-done:
	case <-deadline:
		slog.Warn("background task did not exit before shutdown deadline", "task", name)
	}
}

// periodicScan sweeps on a timer and whenever the watcher pokes trigger.
// Both wake-ups run the same sweep on the same goroutine, so the watcher
// changes when a sweep happens and never what one does (docs/notes/scanner.md),
// with the ticker as the safety net that runs whether or not any event ever
// arrives.
// It also carries the set of directories the previous sweep reported as
// unconfirmed from one iteration to the next, which is all the memory
// runScan's Warn-then-Info rule needs. A loop variable rather than a
// column: losing it on a restart costs one Warn per unconfirmed directory,
// which is the right thing to say to someone who has just started the
// server. reported is the startup sweep's set, so that first line is not
// repeated by the first periodic sweep.
func periodicScan(ctx context.Context, db *storage.DB, libraryDir, coversDir string, interval, missingGrace time.Duration, trigger <-chan struct{}, watcher *scanner.Watcher, reported map[string]bool) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
		case <-trigger:
		case <-ctx.Done():
			return
		}
		// Both the trigger and ctx.Done() can be ready at once — the
		// trigger holds a poke until it is taken, and shutdown cancels the
		// context under it — and select picks between ready cases at
		// random. Without this, half of those shutdowns sweep against a
		// dead context and log the resulting failure as an error.
		if ctx.Err() != nil {
			return
		}
		reported = runScan(ctx, db, libraryDir, coversDir, missingGrace, reported)
		if watcher != nil {
			// After the sweep, so a directory the sweep just discovered is
			// watched before the next change lands in it.
			watcher.Refresh()
		}
	}
}

// runScan sweeps once and logs what it found, including one line per
// top-level directory whose rows it refused to prune. reported is the set
// of directories the previous sweep named; the set this sweep named is
// returned for the next one.
func runScan(ctx context.Context, db *storage.DB, libraryDir, coversDir string, missingGrace time.Duration, reported map[string]bool) map[string]bool {
	slog.Debug("scan starting", "library_dir", libraryDir)

	result, err := scanner.Scan(ctx, db, libraryDir, coversDir, missingGrace)
	if err != nil {
		slog.Error("scan", "error", err)
		// The sweep said nothing about any directory, so the previous
		// sweep's set stands: a failed scan must not make the next
		// successful one Warn about directories it has already warned about.
		return reported
	}

	attrs := []any{"scanned", result.Scanned, "new", result.New, "moved", result.Moved,
		"unchanged", result.Unchanged, "orphaned", result.Orphaned, "missing", result.Missing,
		"pruned", result.Pruned, "unconfirmed", result.Unconfirmed, "covers_regenerated", result.CoversRegenerated, "errors", result.Errors}
	if result.Errors > 0 {
		slog.Warn("scan complete", attrs...)
	} else {
		slog.Info("scan complete", attrs...)
	}

	if result.Scanned == 0 {
		// Reconciliation was skipped entirely, so this sweep reported on no
		// directory at all — an empty UnconfirmedDirs here means "did not
		// look", not "nothing to say". Replacing the set with it would make
		// the next real sweep Warn afresh about residue it has already
		// named, and a library root that blinks out is exactly when that
		// guard fires.
		return reported
	}
	return logUnconfirmedDirs(reported, result.UnconfirmedDirs)
}

// logUnconfirmedDirs reports each top-level directory whose rows this sweep
// refused to prune, and returns the set for the next sweep to compare
// against. Every directory is named on every sweep, because the count is how
// many phantom locations are waiting to be forgotten from a book's page, but
// only one that was not in the previous sweep's set is news.
func logUnconfirmedDirs(reported map[string]bool, dirs map[string]int) map[string]bool {
	now := make(map[string]bool, len(dirs))
	for dir, rows := range dirs {
		now[dir] = true
		slog.Log(context.Background(), unconfirmedLevel(reported, dir),
			"directory yielded no files, refusing to prune its rows", "dir", dir, "rows", rows)
	}
	return now
}

// unconfirmedLevel is Warn the first sweep a directory goes unconfirmed and
// Info while it stays that way. The Warn exists to point at residue nothing
// else surfaces, and a person now has an affordance for clearing it
// (POST /books/{id}/locations/forget); repeating the same warning every
// fifteen minutes for the life of a renamed folder is how a log stops being
// read. A directory that recovers and later empties again is absent from
// the set by then and Warns afresh.
func unconfirmedLevel(reported map[string]bool, dir string) slog.Level {
	if reported[dir] {
		return slog.LevelInfo
	}
	return slog.LevelWarn
}

// metadataProviderNames reads METADATA_PROVIDERS. Unlike envOrDefault's
// other uses, "set but empty" and "unset" must stay distinct here:
// METADATA_PROVIDERS= is the documented way to disable enrichment outright
// and make no outbound calls at all, while leaving it unset means "use the
// default pair" — so this takes a lookup rather than collapsing both to the
// same default. It takes that lookup as a parameter so the distinction is
// testable without a whole run().
func metadataProviderNames(lookup func(string) (string, bool)) []string {
	raw, set := lookup("METADATA_PROVIDERS")
	if !set {
		raw = "openlibrary,googlebooks"
	}
	return providers.ParseNames(raw)
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
