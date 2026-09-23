# library

@README.md

README.md, imported above, says what the app does and how it is configured.
This file maps the code and lists the conventions that belong to no single
area. Every rule an area must keep, and the reason behind it, lives in that
area's note under `docs/notes/`: read the note before changing the area,
and put a new rule or its reason there, never here. See Documentation below
for what these files may and may not say.

## Code map

- `cmd/server` — entrypoint: configuration, directory resolution, startup
  and shutdown order, wiring of the workers and the optional
  `importer.Stager`. Note: `docs/notes/design.md`.
- `internal/storage` — SQLite through `modernc.org/sqlite`; bounded read
  pool, single-connection write pool (`DB.Write`); embedded migrations;
  search sanitisation, the keyset cursor, and the derivations and limits
  every writer of a metadata column shares (`SortTitle`, `NormalizeISBN`,
  `PlainDescription`, `CapField`, `FieldLimit`). `storagetest` is the
  database every test outside this package and `cmd/server` opens. Notes:
  `docs/notes/storage.md`, `docs/notes/testing.md`.
- `internal/epub`, `internal/fb2` — embedded metadata and cover bytes from
  each format, one `Metadata` shape. `internal/cover` — thumbnail, JPEG,
  atomic write into `COVERS_DIR`; `ContentType` decides what this app calls
  an image. Note: `docs/notes/formats.md`.
- `internal/scanner` — walks `LIBRARY_DIR` into storage (`Scan`), indexes
  one path on demand (`IndexFile`), reconciles missing files, regenerates
  covers, hosts the fsnotify watcher and the startup mount checks. Note:
  `docs/notes/scanner.md`.
- `internal/resend` — one-attachment `Client.Send`. `internal/sender` —
  the `Worker` over `send_log`. Note: `docs/notes/sending.md`.
- `internal/enrich` — the enrichment `Worker`, `Provider`, `Resolve`,
  `plausibleMatch`, the decorators, `FetchCover` and the guards both
  providers share. `internal/openlibrary`, `internal/googlebooks` — the
  providers. `internal/providers` — the `METADATA_PROVIDERS` registry.
  Note: `docs/notes/enrichment.md`.
- `internal/importer` — staging, previewing and landing an upload:
  `Stager`, the three `Verdict`s, `detectSuffix`, the library-name
  derivation, and the `Fetcher` that downloads a pasted link. Indexes
  through `scanner.IndexFile`. Note: `docs/notes/import.md`.
- `internal/netguard` — the address guard every outbound fetch of a
  user- or remote-chosen URL dials through. Notes:
  `docs/notes/enrichment.md`, `docs/notes/import.md`.
- `internal/service` — validation, normalisation and page assembly beneath
  the handlers, so a future `/api/v1` is a second thin transport.
  `internal/web` — `html/template` pages, htmx fragments, CSS and vendored
  htmx, all `go:embed`ded; the route list is in the note. Note:
  `docs/notes/web.md`.
- `docs/notes/testing.md` — the fake clock, the in-memory test server, the
  test database, and what still runs on real time.
- `docs/plans/`, `docs/backlog/` — see Planning and Backlog below.

## Conventions

- Handlers parse the request, call one service method and render. Text the
  page shows is composed in the handler, never formatted in a template.
- Absent is not an error: finders return `nil, nil` and updates
  `(false, nil)` for an unknown id. The transport turns that into a 404.
- Tests follow `docs/notes/testing.md`: time runs in `synctest.Test`, HTTP
  on `httptest.NewTestServer`, the database is `storagetest.Open(t)`.
- Provider-client fixtures live under `testdata`. A fixture is a live
  capture or is labelled otherwise at the top of its test file.
- `go test ./...` must pass; CI also runs `go vet` with `-race` and builds
  the image. `.github/workflows/verify.yaml` runs on every pull request
  and on every push to `master`, the second only so a build cache exists
  in a scope pull requests can restore from.
- A `v*` tag publishes: `.github/workflows/publish.yaml` runs the tests,
  builds `linux/amd64` and `linux/arm64` on a runner each, pushes both to
  `ghcr.io/meandnano/applibris` by digest, joins them into one manifest
  list tagged with the version, and creates the GitHub release from the
  commits since the previous tag. A version is `vMAJOR.MINOR` with an
  optional `-suffix` for a prerelease; the image tag drops the `v`, so
  `v0.1` publishes `applibris:0.1`. The tag is matched by regex
  (`type=match`), not parsed as semver, which emits no tags for a version
  this short; the first job refuses a tag of any other shape so a typo
  cannot reach the registry.

## Documentation

CLAUDE.md, README.md and `docs/notes/` describe the **current state of the
code and why it is that way**. They are not a changelog. Git history,
`docs/plans/completed/` and pull requests already record how the code got
here, and a second copy of that record in prose goes stale, grows without
bound and buries the rules a reader actually needs.

Concretely, when writing or editing any of these files:

- Describe what the code does now and the reason it must stay that way.
  Never describe what it did before, what a plan proposed, what a review
  found, or what was tried and removed. Phrases like "used to", "no longer",
  "first built as", "the plan said", "was corrected", "before this change"
  are the signal to delete the sentence or rewrite it in the present tense.
- A rejected alternative may be mentioned only as a present-tense reason
  the current design is right, and only when the code cannot show it:
  "the comparison carries no explicit `COLLATE NOCASE`, since an explicitly
  collated expression is not the indexed one" is a rule; "the comparison
  used to carry `COLLATE NOCASE` until review found it scanned" is history.
- Do not cite plan files, PR numbers or commits from these documents. A
  backlog file may be cited for a current known limit, since it describes
  the code as it stands.
- When a change makes a sentence untrue, replace the sentence. Do not
  append a correction beneath it; that exception belongs to plans only
  (see Planning).
- A note is a list of rules, each a bold sentence followed by its one
  decisive reason, grouped under topic headings. Worked examples, second
  reasons and narrative do not belong there.

## Planning

Each implementation step is planned in its own file under
`docs/plans/<YYYYMMDDNN-description>.md` (e.g.
`docs/plans/2026083001-covers.md`), `NN` a same-day sequence number, the
same scheme as the migration filenames. Once a plan's step has been
implemented, move its file into `docs/plans/completed/`.

Plans in `docs/plans/completed/` are immutable: never edit one after it's
moved there, even to fix a mistake found later. If a problem is discovered
in a completed plan, write a new plan for the fix instead of rewriting the
old one.

The one edit a plan may take on its way *into* `completed/` is a
**correction found while implementing it**, in the same commit as the
move, because a plan whose instruction the implementation had to
contradict is misleading to anyone who later reads the two side by side.
Such an edit must **append**, never rewrite: leave the wrong instruction
standing, and add a block below it saying what was tried and what refuted
it. Rewriting it silently produces a plan that appears to have been right
all along, which git cannot distinguish from the honest version, so the
discipline is the only thing separating them.
`docs/plans/completed/2026090608-googlebooks-live-fidelity.md`'s cover-size
block is the worked example.

Where a completed plan and the code disagree, the code and `docs/notes/`
are right, and the disagreement is not recorded in the notes.

## Backlog

`docs/backlog/` holds known work that is worth doing but isn't top
priority: things that don't corrupt data, don't block another step, and
aren't visibly wrong in the shipped app today. Anything that *does* meet
one of those bars belongs in `docs/plans/` instead.

Backlog files use the same `<YYYYMMDDNN-description>.md` naming as plans,
sharing one same-day `NN` sequence with them so a number identifies exactly
one file across both directories.

A backlog item is a **problem statement, not an approved plan**: it records
what's wrong and why it was judged non-urgent, with only a sketch of a fix.
It is never implemented directly. To act on one, first re-validate it
against the current code, since the finding may have been fixed in passing,
changed shape, or become urgent, then write a real plan under `docs/plans/`
and delete the backlog file in the same change.
