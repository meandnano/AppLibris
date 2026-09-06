package enrich

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"library/internal/cover"
	"library/internal/storage"
)

// pollInterval is the safety-net tick that catches anything a Notify poke
// missed — a job left queued by a crash between insert and notify, most
// obviously. Same shape as internal/sender's, for the same reason: the
// poke is the optimisation, the tick is the mechanism.
const pollInterval = 1 * time.Minute

// markTimeout bounds the write that records a job's terminal outcome —
// same value and same reasoning as internal/sender's: one local SQLite
// write, possibly happening inside the process's shutdown budget.
const markTimeout = 5 * time.Second

// bookGoneReason is recorded when a job's book no longer exists by the
// time it's claimed — the book was pruned between enqueue and claim.
// enrichment_jobs.book_id cascades on delete, so the ordinary case is the
// row vanishing with the book rather than ever reaching this; it exists
// for the narrow race where a claim and a deletion interleave.
const bookGoneReason = "the book was removed from the library before enrichment could run"

// lookupFailedReason is recorded when a read the job needs (the book, its
// authors, its field sources) fails for a reason that isn't the book being
// gone — a storage error, not a statement about the book's existence.
const lookupFailedReason = "could not read the library index — try again"

// applyFailedReason is recorded when Resolve succeeded but writing its
// result back failed.
const applyFailedReason = "could not save enriched metadata"

// allProvidersFailedReason is recorded when every provider the run asked
// returned an error — a throttle, a 5xx, a timeout. "Could answer" rather
// than "could be reached": a 429 and a 5xx are the provider being reached
// and refusing, and this is the one sentence the whole step exists to make
// true. It is deliberately not
// a done job with an empty result: that row is indistinguishable from an
// honest "this book is in neither catalogue", and internal/web renders it
// in the success treatment as "Nothing to add", telling a person there is
// nothing left to find on a day nobody would answer.
//
// failed rather than a fourth terminal state because it is the job going
// wrong in the same sense the other three reasons are — the job's whole
// purpose is to get an answer out of a provider, and a run that got none
// did not do it —
// and because failed already renders with a Retry button, which is the one
// thing a person can usefully do about it.
const allProvidersFailedReason = "no metadata provider could answer — try again"

// coverLostReason is recorded when a run's only result was a cover it could
// not download or store. It names the cover rather than reusing
// allProvidersFailedReason, which would be false — a provider did answer,
// and the failure is entirely on this side of the call.
//
// It deliberately does not say "try again", unlike the reasons above. Most
// of what reaches here is deterministic — a refused scheme, bytes that are
// not a decodable image, an image past maxPixels, a body past
// MaxCoverBytes — and pressing Fetch again reproduces each exactly. In a
// step whose whole subject is the control not overstating what happened,
// promising a retry that cannot work is the same fault one level down. The
// button is still there for the cases that are transient.
const coverLostReason = "found a cover but could not save it"

// coverFetchTimeout bounds one cover download. The worker owns this client
// rather than borrowing a provider's, because the download is the worker's
// step (see Metadata.CoverURL) and the URL may name a host — Open Library's
// separate covers domain, for one — that has nothing to do with whichever
// provider answered. It is the same order as a provider's own Timeout, for
// the same reason: enrichment is a background nicety, and a slow image is
// skipped rather than waited out.
const coverFetchTimeout = 8 * time.Second

// Worker claims and processes enrichment_jobs rows one at a time, in queue
// order — internal/sender's shape, with providers in place of a Transport.
type Worker struct {
	db          *storage.DB
	providers   []Provider
	coversDir   string
	coverClient *http.Client
	notify      chan struct{}
}

// New returns a Worker that reads jobs from db and asks providers, in
// order, for whatever's missing. A nil or empty providers is valid: every
// job then resolves to "nothing missing, no providers", which is what
// METADATA_PROVIDERS= configures and what keeps this worker's wiring
// exercised on a deployment that makes no outbound calls at all. coversDir
// is where a fetched cover is stored, via internal/cover.Store, under the
// book's content hash — the same directory the scanner and internal/web
// already share.
func New(db *storage.DB, providers []Provider, coversDir string) *Worker {
	return &Worker{
		db:          db,
		providers:   providers,
		coversDir:   coversDir,
		coverClient: &http.Client{Timeout: coverFetchTimeout, CheckRedirect: CheckCoverRedirect},
		notify:      make(chan struct{}, 1),
	}
}

// Notify pokes the worker to check the queue immediately, instead of
// waiting for the next pollInterval tick. Non-blocking, capacity-1 channel:
// a burst of enqueues coalesces into one wake-up.
func (w *Worker) Notify() {
	select {
	case w.notify <- struct{}{}:
	default:
	}
}

// Run drains the queue, then blocks waiting for a Notify poke or the next
// pollInterval tick, until ctx is done. A job in flight when ctx is
// cancelled is left running — see process's ctx.Err() handling — for
// RequeueInterruptedEnrichment to recover at next startup. Unlike
// internal/sender's equivalent recovery, that recovery requeues rather
// than fails: see storage.RequeueInterruptedEnrichment's doc comment for
// why an enrichment job can safely be re-run where a send cannot.
func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		w.drain(ctx)

		select {
		case <-w.notify:
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
}

// drain processes claimed jobs until the queue is empty, ctx is done, or a
// claim fails outright.
func (w *Worker) drain(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}

		job, err := w.db.ClaimNextEnrichment(ctx, time.Now())
		if err != nil {
			if ctx.Err() == nil {
				slog.Error("claim next enrichment", "error", err)
			}
			return
		}
		if job == nil {
			return
		}

		w.process(ctx, job)
	}
}

// process resolves and applies one already-claimed job, marking it done or
// failed. Every read and write below that can fail is checked against
// ctx.Err() first: because Decision 2 (see package doc) makes a running
// job safe to re-run from scratch, a failure that coincides with ctx
// already being cancelled is treated as an abandoned attempt, not a
// verdict — the row is left running for recovery rather than recorded as
// failed, which would otherwise need its own retry path that doesn't exist
// here. A failure with ctx still live is a real one and always ends in a
// terminal Mark call, so drain moves on to the next job.
func (w *Worker) process(ctx context.Context, job *storage.EnrichmentJob) {
	book, err := w.db.FindBookByID(ctx, job.BookID)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		w.fail(ctx, job.ID, lookupFailedReason)
		return
	}
	if book == nil {
		w.fail(ctx, job.ID, bookGoneReason)
		return
	}

	authors, err := w.db.ListAuthorsForBook(ctx, job.BookID)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		w.fail(ctx, job.ID, lookupFailedReason)
		return
	}

	sources, err := w.db.FieldSourcesForBook(ctx, job.BookID)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		w.fail(ctx, job.ID, lookupFailedReason)
		return
	}

	res, err := Resolve(ctx, *book, authors, sources, w.providers)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		w.fail(ctx, job.ID, err.Error())
		return
	}
	if ctx.Err() != nil {
		// Resolve logs and skips a provider error rather than returning
		// one — cancellation included, since from inside Resolve a
		// provider call abandoned by a shutdown looks exactly like one
		// that failed outright. So a cancelled ctx can reach here with
		// err == nil and an empty or partial values map that says
		// nothing about what a full run would have found. Leave the job
		// running rather than recording that partial result as the
		// answer; RequeueInterruptedEnrichment picks it up at next
		// startup and Resolve runs again from scratch.
		return
	}

	// A run nobody answered is not a run that found nothing, and the
	// difference is the whole question the control exists to answer — see
	// allProvidersFailedReason. Failed == Asked rather than Failed > 0: if
	// one provider was throttled and another answered cleanly and had
	// nothing, the run did learn something about the book, and failing it
	// would hide a real answer behind a flaky neighbour and invite a retry
	// that cannot improve on it. Asked > 0 keeps the two legitimate
	// zero-provider cases — nothing missing, and METADATA_PROVIDERS= —
	// honest successes.
	//
	// The last two clauses say "and produced nothing at all", which is
	// implied by Failed == Asked today: a provider that errored is skipped
	// before its answer is read, so it contributes neither a value nor a
	// cover URL. They are written for the day that stops holding, and the
	// cover half is the one easy to leave out — a cover-only answer has an
	// empty Values and lives entirely in CoverURL, so a check that
	// inspected Values alone would discard exactly the partial result this
	// is meant to protect.
	if res.Asked > 0 && res.Failed == res.Asked && len(res.Values) == 0 && res.CoverURL == "" {
		// Guarded here rather than relying on the ctx.Err() check above
		// staying immediately above: every other terminal write in process
		// pairs its own guard with the write, and a verdict this one
		// reaches from a snapshot must not be recorded once ctx is
		// already cancelled — during a shutdown every provider "fails",
		// which is indistinguishable here from every provider being
		// unable to answer, and a permanent failed row would deny the job the
		// retry RequeueInterruptedEnrichment exists to give it.
		if ctx.Err() != nil {
			return
		}
		w.fail(ctx, job.ID, allProvidersFailedReason)
		return
	}

	// Resolve hands back a cover URL rather than a path — see its doc
	// comment — because both the download and turning it into a path are
	// this worker's I/O to do. Resolve only ever returns one for a book
	// whose cover_path is empty, so nothing is downloaded for a book that
	// already has a cover. Storing is exactly the scanner's own cover.Store
	// call: resized, JPEG, named by the book's content hash, never a remote
	// URL. A fetch or store failure only loses the cover — it is logged and
	// the field left out of values, the same tolerance the scanner gives an
	// embedded cover that fails to store, since it must not fail a job
	// whose text fields already resolved.
	coverLost := false
	if res.CoverURL != "" {
		if path, ok := w.storeCover(ctx, *book, res.CoverURL); ok {
			res.Values[storage.FieldCover] = path
			res.SourceName[storage.FieldCover] = res.CoverSource
		} else {
			coverLost = true
		}
	}

	// The tolerance above justifies itself with "must not fail a job whose
	// text fields already resolved" — and for a book whose only missing
	// field was its cover there are none, so the reason for the tolerance
	// is absent while the tolerance still applies. Such a run found a cover,
	// dropped it, and would report "Nothing to add" in the success
	// treatment: the same lie step 01 removed, in the one corner its own
	// classification cannot see, since Failed == 0 because the provider
	// answered perfectly well.
	//
	// Narrower than "a failed cover fails the job", deliberately. A run that
	// resolved text fields and lost its cover stays done, naming what it
	// wrote — it really did add something, which is what
	// TestWorkerCoverFailureStillFinishesTheJob pins. And a run where the
	// provider offered no cover at all is still an honest "Nothing to add";
	// coverLost is false there, which is what keeps it out.
	if coverLost && len(res.Values) == 0 {
		if ctx.Err() != nil {
			return
		}
		w.fail(ctx, job.ID, coverLostReason)
		return
	}

	// One call, one transaction, whatever Resolve found — SourceName
	// carries each field's own provider, so a job that pulled fields from
	// more than one provider still records each under the one that
	// actually answered it, and ApplyEnrichedFields's re-check of every
	// field against the book's *current* state (not the snapshot Resolve
	// saw) is what keeps this safe against an edit racing the provider
	// calls above.
	var written []storage.MetadataField
	if len(res.Values) > 0 {
		fields, applied, err := w.db.ApplyEnrichedFields(ctx, job.BookID, res.Values, res.SourceName, time.Now())
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("apply enriched fields", "job_id", job.ID, "error", err)
			w.fail(ctx, job.ID, applyFailedReason)
			return
		}
		// A book that vanished between the claim above and this write is
		// the same failed job the claim-time check records, not a done one
		// with nothing to enrich — calling a run that wrote nothing
		// successful is exactly what MarkEnrichmentDone must not mean.
		if !applied {
			w.fail(ctx, job.ID, bookGoneReason)
			return
		}
		// What ApplyEnrichedFields actually wrote, not what Resolve
		// proposed: its own re-check skips a field a concurrent edit has
		// since filled, and the result line exists to say what *this* run
		// did.
		written = fields
	}

	w.done(ctx, job.ID, written)
}

// storeCover downloads coverURL and stores it as book's cover thumbnail,
// reporting the stored path. It reports false — logging why — for every
// failure, since a cover is normally the one field whose absence costs
// nothing but a dashed box in the grid. Normally: the caller decides
// whether this run had anything else to show for itself, and for one whose
// only missing field was the cover a failure here is the whole job.
func (w *Worker) storeCover(ctx context.Context, book storage.Book, coverURL string) (string, bool) {
	data, err := FetchCover(ctx, w.coverClient, coverURL)
	if err != nil {
		slog.Warn("fetch enriched cover failed", "book_id", book.ID, "error", err)
		return "", false
	}
	if len(data) == 0 {
		// Logged like the other two exits: this one can now end a job, and
		// a failure on the page with nothing in the log is the operator's
		// worst version of it.
		slog.Warn("fetch enriched cover returned nothing", "book_id", book.ID, "url", coverURL)
		return "", false
	}
	path, err := cover.Store(w.coversDir, book.ContentHash, data)
	if err != nil {
		slog.Warn("store enriched cover failed", "book_id", book.ID, "error", err)
		return "", false
	}
	return path, true
}

// done records job's successful completion, naming the fields written so
// the UI can report them. Like internal/sender's terminal writes, it runs
// on a context detached from ctx: process has already established there is
// nothing left to lose by writing the verdict, so a shutdown landing in
// this exact gap shouldn't cost it.
func (w *Worker) done(ctx context.Context, jobID int64, written []storage.MetadataField) {
	markCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), markTimeout)
	defer cancel()
	if err := w.db.MarkEnrichmentDone(markCtx, jobID, written, time.Now()); err != nil {
		slog.Error("mark enrichment done", "job_id", jobID, "error", err)
	}
}

// fail records job's terminal failure with reason. Same detached-context
// reasoning as done.
func (w *Worker) fail(ctx context.Context, jobID int64, reason string) {
	markCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), markTimeout)
	defer cancel()
	if err := w.db.MarkEnrichmentFailed(markCtx, jobID, reason, time.Now()); err != nil {
		slog.Error("mark enrichment failed", "job_id", jobID, "error", err)
	}
}
