# Step: only `ErrNotExist` means the file is gone

## Position in the sequence

Independent of the other send plans. `internal/sender` only.

## Context

Found in the 2026-09-07 review. In `process`, both filesystem calls
collapse every error into one sentence and discard the error:

```go
info, err := os.Stat(path)
if err != nil {
	w.fail(ctx, send.ID, fileGoneReason)
	return
}
...
content, err := os.ReadFile(path)
if err != nil {
	w.fail(ctx, send.ID, fileGoneReason)
	return
}
```

`fileGoneReason` is "the file is no longer in the library". On a NAS the
common failure is not a missing file: it is `EACCES` after a permissions
change, `EIO` from a failing disk, `ESTALE` from an NFS server restart, or
"host is down" from an SMB mount that dropped. Each of these records a
false statement in `send_log`, shows it in the UI and the history page,
and logs nothing at all, so there is no line to diagnose from. The same
function already treats the index lookup differently and logs it
(`lookupFailedReason`), and CLAUDE.md's scanner section states the
posture: an unknown is not evidence.

## Scope

In scope: separating `fs.ErrNotExist` from every other filesystem error
on both calls, logging the latter, and recording a reason that invites a
retry.

Out of scope: retrying automatically. The sender's rule is "retry is a
new row" and a person presses it; an automatic retry loop over a dead
mount would spin.

## Decision 1: three reasons, not one

- `fileGoneReason`, unchanged, for `errors.Is(err, fs.ErrNotExist)` on
  either call and for `resolveFile` finding no non-missing location.
- `fileUnreadableReason = "could not read the file — try again"`, for
  every other `Stat` or `ReadFile` error. Phrased like
  `lookupFailedReason` because, like it, this plausibly succeeds next
  time.
- The existing oversized and transport reasons are untouched.

The underlying error is logged at Error with the send id and the path,
which is what `resolveFile`'s storage-error branch already does.

## Decision 2: the reason does not carry the OS error text

`EACCES` and `ESTALE` are not sentences a person acts on from a status
box, and the history page would show them for a month. The log line
carries them; the UI carries the sentence.

## Changes

- `internal/sender/sender.go`: `fileUnreadableReason`; both error
  branches split on `fs.ErrNotExist`; a `slog.Error` for the unreadable
  case.

## Tests

`internal/sender`:

- A book whose file was removed: `fileGoneReason`, as today.
- A book whose file is a directory, or unreadable (`chmod 000` on a
  non-root test runner; skip if running as root): `fileUnreadableReason`,
  and the error appears in the captured log.
- The `resolveFile` "no location" path still records `fileGoneReason`.

## CLAUDE.md

`internal/sender`'s paragraph currently says the storage error "is
reported separately ... never folded into that one". It gains the same
sentence for filesystem errors other than `ErrNotExist`, and names the
third reason.

## Verification

- Mount the library read-only for the process's uid and send a book: the
  status box says "could not read the file — try again", the log names
  the permission error, Retry works once the permission is restored.
