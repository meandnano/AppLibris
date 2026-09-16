# AppLibris — review calibration

## What this is

A self-hosted ebook library server for one person on a home network. One Go
binary, one container, an embedded SQLite database (`modernc.org/sqlite`,
pure Go), and no other services. The UI is server-rendered `html/template`
with a little vendored htmx; there is no JavaScript build step and every page
works with JavaScript off.

It points at a directory of EPUB and FB2 files, indexes them, and offers a
cover grid, search, a detail page, metadata enrichment from two providers,
and a button that emails a book to a Kindle through Resend.

There is no login. The deployment is documented as sitting behind an HTTPS
gateway with the plain listener reachable only through it.

## What a real failure looks like here

Ordered by what it costs. A finding that lands in the first group is worth
reporting even if it is unlikely; one in the last is worth reporting only if
it is certain.

1. **A library file is modified, replaced or lost.** Those are the person's
   books and the app does not own them. The rule the code enforces is that
   writes into `LIBRARY_DIR` only ever create new paths — never replace, never
   truncate, never rename over. Anything that can clobber a path is the most
   serious class of defect in this repo.
2. **An index row is deleted for a file that still exists.** Mounts go
   offline, sub-mounts fail, directories become unreadable. The codebase's
   standing rule is "an unknown is not evidence": only `fs.ErrNotExist` means
   gone, and every other error leaves the row alone. A path that erases a
   person's hand-edited metadata on the strength of an `EACCES` is a real bug.
3. **A person's edit is overwritten by something automatic.** A field a human
   set is `manual` and neither the scanner nor enrichment may touch it. A
   cleared field stays `manual`; emptiness is never read as provenance.
4. **A value is stored that the editor would then refuse.** Every writer of a
   metadata column caps through `storage.FieldLimit`/`CapField`. A writer that
   restates a limit, or stores past one, produces a field the app can no
   longer save unchanged — the editor opens, Save fails on a value nobody
   typed.
5. **A cross-site write succeeds.** With no login, `Sec-Fetch-Site` is the
   only thing between the collection and any page the person visits. Every
   state-changing route is wrapped in `sameSiteOnly`, and the whole handler
   sits behind a fetch-metadata guard.
6. **A send reports an outcome it does not know.** Delivered means Resend
   accepted it. A row left `sending` after a restart is failed, never
   requeued, because which side of the request the process died on is
   unknowable and a duplicate send is worse than a false negative.
7. **An untrusted file exhausts memory.** EPUB and FB2 are attacker-shaped
   input here — a person imports what they downloaded. Every read from one is
   capped before its bytes are held.

## Blast radius

One person's library on a LAN. No multi-tenancy, no accounts, and the only
personal data is a Kindle email address. The index is rebuildable from the
files; the files are not rebuildable from anything.

So: a bug that loses or corrupts a file in `LIBRARY_DIR` is severe out of
proportion to its likelihood. A bug that costs a re-scan is minor. A
performance finding needs a measurement behind it — the workload is one
person browsing a few thousand books.

## Reporting bar

This codebase is invariant-dense on purpose. `CLAUDE.md` carries a long list
of rules under the heading "Each of these is a rule the code will not tell you
about and a plausible tidy-up would break", and `docs/notes/*.md` carries the
reasoning behind each. **Read the invariant list before deciding something is
wrong.** A great many things that look like oversights are load-bearing and
documented as such.

Report:

- a violation of a stated invariant, naming the invariant
- a defect in executable logic, with the inputs that produce it
- a test that would pass against a broken implementation
- a document that contradicts the code it describes
- a comment that asserts something the code does not do

Do not report:

- a restatement of a decision the notes already justify
- a style preference, or a suggestion to shorten a comment
- "consider extracting" without a second caller that exists today
- a hypothetical concurrent access on a path only one goroutine reaches

## Conventions that are deliberate, not defects

Flagging any of these as a problem is a false positive.

**Comments and documentation**

- Comments never end with a full stop.
- Comments explain *why*, not *what* — the code shows the behaviour. Long,
  prose-heavy rationale comments are the house style, not noise.
- No section-divider comments, no step-by-step narration, no emoji.
- Trivial functions carry no doc comment; the reasoning goes on the
  non-obvious ones.
- A comment must describe only the current state of the code. A comment
  saying what the code used to do is itself a defect here.
- `CLAUDE.md`, `README.md` and `docs/notes/` describe the current design and
  why it must stay that way. They are not a changelog. Past-tense narration
  ("used to", "no longer", "was corrected") is a defect in those files.
- `docs/plans/completed/` is the one place history is kept deliberately, and
  those files are immutable. A completed plan disagreeing with the code is
  expected; the code and `docs/notes/` are right, and the disagreement is not
  recorded.

**Go and structure**

- Absent is not an error: finders return `nil, nil` and updates return
  `(false, nil)` for an unknown id. The transport turns that into a 404.
- Handlers parse the request, call one service method, and render. Text the
  page shows is composed in the handler, never formatted in a template.
- Send and enrichment are two parallel surfaces — state, latest, shaping —
  and stay that way. "Do not abstract over exactly two cases" is a written
  rule; an abstraction over them has no third instance to test its shape
  against.
- `epub.Metadata` and `fb2.Metadata` are structurally identical and
  deliberately distinct types. There is no shared interface and should not be.
- Function fields (`Service.Notify`) rather than interfaces where there is one
  nullary call, to keep a package dependency from becoming a cycle.
- `internal/service` rejects an over-long edit where `internal/scanner` and
  `internal/enrich` truncate. That asymmetry is deliberate: a person's value
  is theirs and is not rewritten behind them.

**Storage**

- A `DB.Write` callback never calls an exported `*DB` method — the write pool
  has one connection and the outer call holds it.
- Every exported write method owns exactly one transaction.
- No backfill migrations. A development database older than a table is reset
  by deleting the file.
- The keyset cursor comparison carries **no** explicit `COLLATE NOCASE`, since
  an explicitly collated expression is not the indexed one and the seek would
  become a scan.

**Web**

- A rejection answers **422** wherever it has a body to show, and the form
  that posted opts that status in with `hx-status:422="swap:outerHTML"`; the
  disabled send and enrich controls answer **503** and their forms carry
  `hx-status:503="swap:outerHTML"`. The `htmx-config` meta tag keeps every
  other `4xx` and `5xx` in `noSwap`, so a route that gains an error body,
  4xx or 5xx, without the matching `hx-status` on its form is a refusal
  nobody can see. The opt-in names the exact status: `noSwap` is consulted
  before the element at each step, so an opt-in on a wildcard `noSwap`
  itself lists is unreachable.
- A fragment is answered when `HX-Request` is present and
  `HX-History-Restore-Request` is absent, and `Vary` names both.
- Every read affordance carries both `href` and `hx-get`, every editor both
  `action` and `hx-post`. One markup path; there is no separate no-JS path.

**Tests**

- Provider fixtures under `testdata` are live captures unless the test file
  says otherwise. Never hand-edit a fixture to make a test pass.
- `export_test.go` plus an external `_test` package is the accepted answer to
  a test-only import cycle.

## Verification

CI runs exactly this, and nothing else:

```
go vet ./...
go test -race ./...
```

There is no golangci-lint configuration. `gofmt` cleanliness is expected.
A finding that depends on a linter this project does not run is not
actionable.
