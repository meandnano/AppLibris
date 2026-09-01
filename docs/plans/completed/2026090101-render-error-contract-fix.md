# Step: Correct render's post-write error contract

## Context

PR review of `2026083112-web-transport-correctness` found the fix's own
follow-up commit — only returning `render`'s pre-write `ExecuteTemplate`
error, logging a post-write `WriteTo` failure instead of returning it — had
nothing pinning it. Before that follow-up, `render` still returned a
`WriteTo` error to the caller, and `libraryHandler` reacted to it with
`http.Error`, double-writing an "internal error" body (and an implicit
second status) onto a response whose `Content-Type` and possibly a partial
body were already committed. That regression was invisible to the existing
suite: `TestRenderFailureProducesNoPartialBody` only exercises the pre-write
path (an unknown template name), never a write that fails after
`Content-Type` is set.

Also found: the handler-level "a template failure becomes a clean 500"
claim was only exercised by calling `render` directly, never by driving
`libraryHandler` itself. Deleting the `http.Error` call from the handler's
error branch left every `internal/web` test green.

## Changes

- `render` (`internal/web/render.go`) keeps returning only the pre-write
  `ExecuteTemplate` error; a `WriteTo` failure is logged inside `render`
  and never returned, since the caller can't respond any differently once
  the response is committed. This step formalizes that as a tested
  contract rather than an unpinned side effect of the earlier commit.
- Add `TestRenderWriteFailureIsNotReturned`: a fake `http.ResponseWriter`
  whose `Write` always fails after recording the call pins that `render`
  returns `nil` — not the write error — and that `Write` is attempted
  exactly once (no retry).
- Add `TestLibraryHandlerDoesNotDoubleWriteOnWriteFailure`: drives
  `libraryHandler` directly with the same fake writer and asserts no
  `WriteHeader(500)` call follows the failed write.
- Add `TestLibraryHandlerRendersCleanServerErrorOnTemplateFailure`: swaps
  the package's `templates` variable for one whose `library.html` always
  fails to execute (restored via `t.Cleanup`), driving the actual HTTP
  handler through `Routes` rather than calling `render` in isolation, and
  asserting a 500 with body exactly `"internal error\n"` — proving the
  handler boundary, not just the renderer, produces a clean error.
- `CLAUDE.md`'s `internal/web` bullet now states the no-return-after-commit
  convention explicitly: only the pre-write `ExecuteTemplate` error is ever
  returned to the handler; a write failure past that point is logged, not
  returned, because a handler reacting to it would double-write onto an
  already-committed response.

## Verification

- `go build ./...`
- `go vet ./...`
- `go test ./... -race`
