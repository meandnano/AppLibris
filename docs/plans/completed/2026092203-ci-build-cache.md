# Let pull requests restore a build cache saved on master

## Position in the sequence

After `2026092202`, and only once that plan's effect on CI's Test step has
been measured. Both plans shorten the same step, and doing them one at a
time is what lets each saving be attributed.

## Context

`actions/setup-go@v7` caches `GOCACHE` and `GOMODCACHE` under a key built
from the OS, architecture, runner image, Go version and the hash of
`go.sum`. The two most recent PR runs (#87 and #88) both logged "Cache is
not found" and compiled everything from nothing, including the `-race`
build of every dependency. modernc's translated SQLite and libc make up
most of that build. Locally, a cold `go test -race -run '^$' ./...` takes
19 s of wall time and 70 s of CPU, and the four-core runner is slower.

The cache is never found because of how GitHub scopes caches. A run can
restore caches saved on its own ref or on the base branch, never another
PR's. `verify` runs only on `pull_request`. `publish` runs on tags, whose
caches no PR can see. So nothing ever saves a cache on `master`, and each
PR's first run starts cold. A PR's later runs do restore what its own first
run saved.

## Change

Add `push: branches: [master]` to `.github/workflows/verify.yaml`'s `on:`.
A run on `master` then saves the cache in `master`'s scope, and every PR
targeting `master` can restore it.

- **It runs the same steps as a PR run.** `go vet` fills the plain build
  and `go test -race` the race-instrumented one, so both halves a PR needs
  are in what `master` saves.
- **It is saved once per `go.sum` and Go version.** setup-go does not save
  again on a hit. Dependencies come from the cache and this module's own
  packages recompile on each run, which is the cheap part.
- **It also runs every merged commit in full.** A break that only appears
  once two PRs are combined is caught on `master` rather than by the next
  PR. The cost is one runner per merge.

The alternative is a separate workflow that only builds on `master` to fill
the cache. It uses fewer minutes, but it is a second file whose flags must
mirror `verify`'s, or the cache it saves is not the one PRs need. It is not
taken unless runner minutes start to matter.

## Verification

1. **Measure what a warm cache is worth first.** Re-run PR #88's `verify`
   before changing anything. Its first run saved a PR-scoped cache, so the
   re-run's Test and Vet steps show the warm figure against the cold one.

   **Measured** from PR #88's existing runs — no re-run was needed, its
   second and third runs already hit their own PR-scoped cache:

   | | Vet | Test |
   |---|---|---|
   | first run, cold | 23 s | 2m13s |
   | later run, warm | 0 s | 27 s |

   So a warm cache is worth about 106 s on Test and the whole of Vet.

   `2026092202`'s own effect, which this plan waited on, is the cold-to-cold
   comparison: PR #89's first run took 32 s and 1m22s against those same
   23 s and 2m13s. Test fell 38%, matching the 39% the local CPU sum
   predicted; Vet rose, because `storagetest` is one more package to compile
   and Vet is where the plain build gets filled.

   Note the two do not simply add. Most of what a warm cache saves is the
   dependency build, which `2026092202` never touched, and most of what
   `2026092202` saved is test execution, which no cache removes.
2. After merging, check that the `master` run's post-setup-go step logs
   "Cache saved with the key".
3. Check that the next PR's first run logs "Cache restored from key".
4. Compare that run's Vet and Test steps with the cold figures recorded in
   `2026092202` when it was completed.

Steps 2 to 4 are measurable only after this merges: the first `master` run
is what saves the cache, and the pull request after it is what restores it.
Record those figures in the pull request rather than here.
