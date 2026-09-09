# Backlog: a panicking enrichment job becomes a startup crash loop

## Problem

`internal/enrich.Worker.process` and the goroutine `cmd/server` runs it
on have no `recover`. A panic anywhere inside a job (a provider client, a
decoder in `cover.Store` on hostile bytes, `plausibleMatch`) kills the
process with the job's row left `running`.

On restart, `storage.RequeueInterruptedEnrichment` puts that row back to
`queued`, and `Run` drains the queue immediately. The same job runs, the
same input panics, the process dies again. Under Docker's restart policy
that is indefinite, and the only way out is editing the row in SQLite by
hand.

The reasoning behind requeue-not-fail (an enrichment job is repeatable,
so an interrupted one is safe to run again) is exactly what turns a
one-off panic into a loop: the design assumes the interruption was
external.

## Why this is backlog, not a plan

No panic path was found. Every `[0]` in both provider clients is
length-guarded, the JSON shapes decode `null` to zero values, and the
matcher refuses oversized input before it compares. Today this depends
on a bug in a standard-library image decoder or a future provider client,
so it is structural rather than live. It is recorded because the failure
mode, when it arrives, is the worst one a background job can have: the
whole server down, repeatedly, for a feature nobody was waiting on.

`internal/sender` has the same shape with a smaller blast radius, since
`FailInterruptedSends` fails rather than requeues an interrupted row, so
a panicking send job kills the process once and then stays failed.

## Re-validate before acting

- Whether `process` still lacks a `recover`.
- Whether `RequeueInterruptedEnrichment` still requeues unconditionally.

## Sketch

A `defer` at the top of `process`:

```go
defer func() {
	if r := recover(); r != nil {
		slog.Error("enrichment job panicked", "job_id", job.ID, "book_id", job.BookID, "panic", r, "stack", string(debug.Stack()))
		w.fail(ctx, job.ID, "enrichment crashed — see the server log")
	}
}()
```

The row goes terminal with a reason, the Retry button appears, and the
process keeps serving. `w.fail` writes under `context.WithoutCancel`
already, so it lands even mid-shutdown.

Do not recover in `Run`'s loop instead: a recover there loses which job
panicked and leaves the row `running` for the requeue to loop on.

The same `defer` in `internal/sender.process` costs one more function and
removes the one-restart cost there too; do both or note why not.
