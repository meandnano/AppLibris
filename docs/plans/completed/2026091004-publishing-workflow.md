# Step: publish a release from a `v*` tag

## Position in the sequence

Independent of every other plan. Nothing under `cmd/` or `internal/` changes:
one new workflow, `.github/workflows/publish.yaml`, plus the two sentences in
`README.md` and `CLAUDE.md` that the workflow makes true.

## Context

CI is one workflow, `verify.yaml`, and it runs only on pull requests: `go vet`,
`go test -race`, and a `docker build` that is discarded. Nothing is published,
so every machine that wants to run the app builds the image itself. That is
slowest exactly where the app is meant to live — a NAS or a small arm64 box,
where a Go build competes with the disk it is supposed to be indexing.

There are no tags and no releases yet, so this step also decides what a release
is: push `vX.Y.Z`, and get an image for `linux/amd64` and `linux/arm64` on
`ghcr.io/meandnano/applibris` and a GitHub release whose notes are the commits
since the tag before it.

## Scope

In scope: the tag-triggered workflow, and the README and CLAUDE.md sentences it
makes true.

Out of scope: signing, SBOMs and SLSA provenance; a `latest` or major/minor
tag; publishing binaries beside the image; anything on `push` to `master`.

## Decision 1: one runner per architecture, pushed by digest

Each architecture builds on a runner of its own — `ubuntu-24.04` and
`ubuntu-24.04-arm`, both free for a public repository — and pushes untagged
with `push-by-digest=true`. A third job downloads the two digests and joins
them with `docker buildx imagetools create`, which is the only step that names
a tag.

The alternatives are worse for this repository. QEMU emulates the arm64 Go
compile and costs minutes per release. Cross-compiling inside the image
(`FROM --platform=$BUILDPLATFORM`, `GOARCH=$TARGETARCH`) is fast, but it makes
the Dockerfile build something different from what a person building locally
builds, and the release image would then be the only artefact never compiled
natively. Two native runners keep the Dockerfile a plain single-architecture
build that means the same thing everywhere.

`provenance: false` on the build. Left on, buildx pushes each architecture as
an index carrying an attestation manifest, and the joined list then advertises
`unknown/unknown` platforms beside the two real ones.

The image reference is written out in full, `ghcr.io/meandnano/applibris`, and
not derived from `github.repository`: ghcr rejects an uppercase reference and
the repository is `meandnano/AppLibris`.

## Decision 2: the version tag and nothing else

`v1.2.3` publishes `1.2.3` (`type=semver,pattern={{version}}`, `latest=false`).
No floating tag: a deployment names the version it runs, and an upgrade is a
deliberate edit. A `v*` tag that is not semver yields no tag at all and fails
the release job, which is the right noise for a mistyped tag.

## Decision 3: the notes are the commit range

`git describe --tags --abbrev=0 --match 'v*' "$TAG^"` finds the preceding tag
and the notes are `git log --no-merges --reverse --pretty='- %s'` over the
range; with no preceding tag the range is the whole history reachable from the
tag, which is what the first release wants. Squash merges put the PR number in
each subject, so GitHub renders every line as a link. A tag containing a hyphen
is a semver prerelease and the release is marked as one.

The release job checks out with `fetch-depth: 0`, since both the range and the
`describe` need the history and the tags.

## Decision 4: tests, but not `go vet`

The publish workflow runs `go test -race ./...` before it builds. It does not
vet: `verify.yaml` vets every pull request, and a tag is far too late to be
learning that the code does not lint. The test run is there to stop a broken
commit from being published, not to review it.

## Files

- `.github/workflows/publish.yaml`: the three jobs, `test` → `build` (matrix)
  → `release`. Top-level `permissions: contents: read`; `packages: write` on
  the two jobs that talk to ghcr and `contents: write` on the one that creates
  the release.
- `README.md`: "Running it" leads with the published image; building from the
  repository stays, for a revision that has no release.
- `CLAUDE.md`: one bullet under Conventions naming what a `v*` tag does.

## Verification

1. `git tag v0.1.0-rc.1 && git push origin v0.1.0-rc.1`.
2. `docker buildx imagetools inspect ghcr.io/meandnano/applibris:0.1.0-rc.1`
   lists exactly `linux/amd64` and `linux/arm64`.
3. `gh release view v0.1.0-rc.1` shows the commit list and is marked as a
   prerelease.
4. The container starts from the published image on both architectures.
5. Delete the tag and the release, then cut the real one.

The first push creates the ghcr package private even from a public repository.
It has to be made public once, by hand, in the package's settings.
