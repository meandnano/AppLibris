# Step: Runtime hardening — container, lifecycle, startup

## Context

Four defects in `cmd/server` and the `Dockerfile`. None is a logic bug in a
package; all four are about the binary behaving correctly as a long-running
container, which is the only way DESIGN.md expects it to run.

**1. `FROM scratch` has no CA certificates.** `internal/resend` makes an
HTTPS request to `api.resend.com`. A scratch image contains the binary and
nothing else — no `/etc/ssl/certs/ca-certificates.crt` — so Go's TLS stack
has no root store and **the first real send will fail** with
`x509: certificate signed by unknown authority`. This is invisible today
only because nothing calls `Send` yet; it will surface on day one of
send-to-Kindle, at which point it will look like a Resend problem rather
than a packaging one. Fixing it now costs one `COPY` line and removes a
guaranteed future dead end.

The same image also has no `/tmp` (anything using `os.CreateTemp` with the
default directory fails — note `2026083111-cover-regeneration` uses
`os.CreateTemp` with an explicit directory, so it is unaffected, but the
next such caller may not be), no tzdata (every `time.Local` is UTC, so
timestamps rendered to the user are wrong for anyone not on UTC), and runs
as root.

**2. `defer db.Close()` never runs.** `main` defers it at line 49, then
blocks in `ListenAndServe`, then `os.Exit(1)` — which skips deferred
functions. And on `SIGTERM`, i.e. every `docker stop`, the process is killed
outright and no defer runs either. The database is closed by process death
with the WAL un-checkpointed and any in-flight scan write cut off mid
transaction. SQLite recovers from this on next open — it is a journalled
database, not a corruption risk — but it means every restart pays WAL
recovery, and it will matter much more once a send job is in flight and its
`send_log` row's state is the record of whether a book reached someone's
Kindle.

**3. Startup blocks on a full synchronous scan.** `runScan` is called at
line 51; the mux, `/healthz` included, is not registered until line 56 and
not served until line 63. On a first run over a large library that is
minutes of hashing during which **the health endpoint does not answer at
all** — the socket isn't even listening. A container healthcheck or an
orchestrator's readiness probe will conclude the container is dead and
restart it, potentially in a loop that never completes a first scan.

**4. No HTTP server timeouts.** `http.ListenAndServe` uses a zero-value
`http.Server`: no `ReadHeaderTimeout`, no `ReadTimeout`, no `WriteTimeout`,
no `IdleTimeout`. A single client that opens a connection and never sends a
request line holds a goroutine forever. On a trusted LAN with one user this
is theoretical, but the fix is four lines and it is the kind of thing that
is never revisited once the app works.

## Scope

In scope: the Dockerfile, signal handling and graceful shutdown, moving the
first scan off the startup path, and server timeouts. Out of scope: the
`internal/resend` client's own missing HTTP timeout — that is package-level
and lives in `docs/backlog/2026083117-resend-client-hardening.md` with the
empty-`text`-field question, since neither can be validated until something
actually calls `Send`.

## Dockerfile

```dockerfile
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /server ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /server /server
EXPOSE 8080
ENTRYPOINT ["/server"]
```

`distroless/static` over `scratch` because it brings exactly the four things
missing — CA certificates, `/tmp`, tzdata, and a non-root `nonroot` user —
and nothing else. It is still a static-binary base with no shell and no
package manager, so DESIGN.md's "single container, little more than the
binary" holds.

If pulling a `gcr.io` base is unwanted, the equivalent on `scratch` is:

```dockerfile
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /usr/share/zoneinfo /usr/share/zoneinfo
USER 65534:65534
```

— plus a `/tmp` that has to be provided as a tmpfs mount at run time, since
you cannot `mkdir` in a scratch image. The distroless base is less fiddly;
pick one and note which in the file.

Non-root has a consequence worth stating: the process must be able to write
`DB_PATH`, `COVERS_DIR`, and — per DESIGN.md's "writable by the
application", for future conversion output — `LIBRARY_DIR`. Document the
uid in a comment so the volume ownership requirement isn't discovered at
runtime.

Add a `.dockerignore` while here (`data/`, `library/`, `bin/`, `.git/`):
`COPY . .` currently copies a developer's local database and book collection
into the build context and then into an image layer.

## Lifecycle

Restructure `main` around a signal-aware context:

```go
ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
defer stop()
```

- Pass `ctx` into the scan goroutine and the periodic scanner, replacing the
  `context.Background()` the scanner is handed today. A sweep in progress
  then unwinds on shutdown instead of being killed mid-write.
- `periodicScan` selects on `ctx.Done()` alongside the ticker.
- Build an explicit `http.Server` and run `ListenAndServe` in a goroutine;
  the main goroutine waits on `ctx.Done()`, then calls `srv.Shutdown` with
  its own bounded context (`context.WithTimeout(context.Background(),
  10*time.Second)` — deliberately not the cancelled `ctx`, or shutdown
  aborts instantly).
- Then `db.Close()`, explicitly, not deferred. Order matters: HTTP first so
  no request is mid-query, then the database.
- Startup failures keep their `slog.Error` + `os.Exit(1)` shape, but move
  the `db.Close()` on those paths to explicit calls, since the defer is
  known not to fire.

**`main` is getting long.** Split the body into `run(ctx) error` with `main`
doing only logger setup, the `signal.NotifyContext`, and
`if err := run(ctx); err != nil { slog.Error(...); os.Exit(1) }`. That gives
every failure path one exit point and makes the defers actually meaningful.

## Startup scan

Register routes and start listening **first**, then run the initial sweep in
the same goroutine as the periodic one:

```go
go func() {
	runScan(ctx, db, libraryDir, coversDir)   // initial
	periodicScan(ctx, db, libraryDir, coversDir, scanInterval)
}()
```

The library page during that first sweep shows an empty grid filling in
across refreshes, which is strictly better than a connection refused, and
matches DESIGN.md's posture that enrichment never blocks a book from
appearing.

`/healthz` answering immediately is the point. Do **not** make it report
scan state — liveness and "has finished its first sweep" are different
questions, and conflating them recreates the restart loop. If a readiness
signal is ever wanted, it is a second endpoint.

## Server timeouts

```go
srv := &http.Server{
	Addr:              addr,
	Handler:           mux,
	ReadHeaderTimeout: 10 * time.Second,
	ReadTimeout:       30 * time.Second,
	WriteTimeout:      60 * time.Second,
	IdleTimeout:       120 * time.Second,
}
```

`WriteTimeout` at 60s is chosen with send-to-Kindle in mind: a handler that
reads a 28MB book off disk and hands it to Resend inside a request would
blow a shorter one. DESIGN.md makes sends a queued background job precisely
so that doesn't happen, so 60s is headroom, not a design constraint —
mention that in a comment so it isn't tuned down without noticing.

## Tests

`cmd/server` has no test file and this step does not add one — the
behaviours here (signal handling, container base, listener lifecycle) are
integration properties, and a test that spawns the binary and sends it a
signal costs more to maintain than it catches at this project's size.
Extracting `run(ctx) error` does make that possible later, which is reason
enough to do the extraction now.

The one thing worth an automated check is the image, and that belongs in
whatever CI exists rather than in `go test`.

## CLAUDE.md

Update the `cmd/server` bullet: serves immediately and scans in the
background (first sweep included), shuts down gracefully on SIGTERM/SIGINT
closing the HTTP server then the database, and runs with explicit HTTP
timeouts. Note the image base is distroless/static and the process runs
non-root, so mounted volumes must be writable by that uid.

## Verification

- `go build ./...`, `go vet ./...`, `go test ./...` clean.
- Manual: start against a large library and `curl localhost:8080/healthz`
  immediately — it must answer while the first sweep is still running.
- Manual: `docker run` the image, `docker stop` it, and confirm the log
  shows the shutdown sequence and the container exits well inside the 10s
  grace rather than being SIGKILLed at the end of Docker's timeout.
- Manual: inside the built image, confirm the TLS root store is present —
  the cheapest proof is a temporary `main` that dials `api.resend.com:443`,
  or simply deferring this check to the first real send once
  send-to-Kindle exists. Note in the commit which was done, because "the
  CA fix is present" and "the CA fix works" are different claims.
