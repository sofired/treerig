# Contributing

Thanks for looking. A few things that make contributions land smoothly.

## Before you open a pull request

See "Go" below for the checks to run before opening a pull request. CI runs
both on Linux and macOS.

## What we care about

**Ownership is a safety property, not a feature.** Anything that removes a
resource must prove the resource belongs to the worktree first. A change that
makes teardown "work" by matching on a name will not be merged.

**Locks do not lie.** Never add a code path that clears a lock based on age
alone. Recovery is explicit and goes through `lock.Clear`.

**Library code takes values; the entry point reads the environment.** Only
`cmd/berth`'s `main` reads `os.Args` or `os.Environ` directly. Everything
else — including `cli.MainProcess` itself — receives the name, the working
directory and environment values as parameters. The runner seam is
`run.Runner` (`internal/run`), with `internal/run/fakerun` standing in for
tests.

**Storage identity is a contract.** `internal/brand` names things that end up
on disk or in Docker — see its own tests. Changing any of them is a
migration for every existing user and needs a versioned record schema or a
migration command — not a find-and-replace. Every store berth reads back
carries a format version (`internal/stateformat`, pinned by its tests), and a
build refuses a newer one; `docs/design/state.md` lists the stores, the
refusal, and how a migration is added.

## Tests

Unit tests are hermetic: no Docker, no real Git, no network. They mock at
the `run.Runner` seam (`internal/run/fakerun`), and live beside the code
they test, in the same package's own `_test.go` files.

`//go:build integration` marks a test that touches something real: a
process, a signal, or a Docker daemon — including the streams contract,
which spawns the built CLI as a process, since which stream a line lands on
is only a contract if it survives the entry point, and no mock at the
`run.Runner` seam can prove it. These are excluded from the default
`go test ./...` and run separately (see "Go" below).

Every module with security-relevant behaviour — path containment, file
modes, lock recovery, identity markers — has a dedicated test file. Keep it
that way.

A Docker suite (a `*_docker_integration_test.go` file) skips itself when no
daemon answers `docker info`, unless `BERTH_TEST_REQUIRE_DOCKER=1` turns
that into a failure instead. `BERTH_TEST_SKIP_DOCKER=1` skips it even when a
daemon answers. CI's `go` jobs set the second and its `docker` job the first,
so the Docker suites run once, in parallel with everything else. The Docker
suites sweep anything a killed run left behind, without regard to age, so
run one of them at a time on a machine.

## Go

**Toolchain:** the Go version in [`go.mod`](go.mod).

**Before you open a pull request:**

```bash
gofmt -l cmd internal                                             # must print nothing
go vet ./...
go vet -tags integration ./...
go run honnef.co/go/tools/cmd/staticcheck@2025.1.1 ./...
go run honnef.co/go/tools/cmd/staticcheck@2025.1.1 -tags integration ./...
go test -race ./...                                               # unit
go test -race -p 1 -timeout 20m -count=1 -tags integration ./...  # real git, processes, signals, Docker, goldens
CGO_ENABLED=0 go test ./... && CGO_ENABLED=0 go test -p 1 -timeout 20m -count=1 -tags integration ./...
```

The `CGO_ENABLED=0` run matters on its own: cgo changes how the Go runtime
behaves (an extra thread, notably), and releases build with it disabled.
The integration runs use `-p 1`, one package at a time: the Docker suites pick
the router's index and ping ports from a deterministic hash start, so packages
running in parallel collide on them. `-timeout 20m` replaces Go's 10-minute
per-package default: `internal/commands` takes about 5 to 7 minutes under
`-race` on CI and about 12 on a slower machine, and keeps growing, so the
default is too tight. A timeout panic skips `t.Cleanup`, which can leave Docker
resources behind: only some suites, such as the router's, sweep what an earlier
run abandoned. `-count=1` stops Go from replaying a cached pass, which would
never reach the Docker daemon.

The `-race` integration run above also runs the streams contract
(`internal/cli/streams_integration_test.go`'s `TestStreamsContract`), against
a binary it builds itself.

`internal/cli/testdata/golden/` and `internal/cli/testdata/output/` are
plain, hand-maintained fixture corpora: update a fixture by hand, or by
running `go test ./internal/cli/... -run TestGolden -update` after a change
to the command tree, help wording or an error message. `-update` only writes
what `cli.Main` produced over the fixture; it never judges whether that
output is correct, so review every diff it leaves before committing it, the
same way any other testdata file in this repository is updated. See
golden_test.go's and output_integration_test.go's own package comments.

`schema/berth.schema.json` is generated, not written: `berth config schema`
prints it from the same `internal/shape` definitions that validate
`berth.toml`. After changing the config schema or a field's description,
regenerate it with `go run ./cmd/berth config schema > schema/berth.schema.json`;
`TestSchemaFileIsCurrent` fails until you do. Every field needs a
`Describe(...)`, which editors show on hover. Examples in `berth.example.toml`
and the README stay TOML 1.0 (no multi-line inline tables); a test checks them.
`berth init` writes the starter in `internal/config/starter.go`: a short
template, not a copy of the example, with its commented-out service and hooks
checked against the schema by `TestStarter`. Rename or remove a key there when
you rename or remove it in the schema.

**Go style,** beyond gofmt, go vet, and staticcheck:
- No naked returns — a function with named results still returns them
  explicitly.
- A temp directory that meets a path comparison is physical: use
  `registrytest.TempDir`, not `t.TempDir()` directly, since macOS's temp
  directory is a symlink into `/private`.
- A test that runs `up` never takes a port from the default loopback range
  (20000-29999), which a developer's own worktrees use. A unit test gets
  `registrytest.PortRange` and a probe that binds nothing from
  `commandstest.World`; a scratch berth root under a real binary or daemon
  writes its `settings.toml` with `registrytest.WriteScratchSettings`. Where
  the berth root and the control home are one directory, claim it first with
  `registrytest.ClaimControlHome`, since berth adopts only a directory it
  created.
- A helper process that must stay alive parks with a `for { time.Sleep(...) }`
  loop, never `select {}` — with nothing else runnable, Go's deadlock
  detector exits a process blocked in `select {}` unless cgo is linked, which
  hides the bug on a cgo build and reproduces it on macOS or
  `CGO_ENABLED=0`.
- No spike markers, and every TODO names an issue: `TODO(#issue)`. Both are
  enforced by `internal/style`.
- Every new function and method gets a doc comment, test helpers in
  `_test.go` files included: what it does and why, not a restatement of its
  signature. `Test*` functions are exempt, since their `t.Run` titles describe
  them. CodeRabbit checks this on each pull request, against an 80%
  threshold.

## Releases

A release is a `v*` tag. The Release workflow
(`.github/workflows/release.yml`) tests the tag and checks that its commit is
on `main`, then GoReleaser (`.goreleaser.yaml`) builds linux/darwin on amd64
and arm64 with `CGO_ENABLED=0` and `brand.Version` set from the tag, and
publishes a GitHub Release with the archives, the source archive, the `.deb`,
`.rpm` and `.apk` packages, `checksums.txt`, its cosign (keyless) bundle and
release notes from the commits since the previous tag. The job then attests the
build provenance of every file. A final job opens a pull request in
`sofired/homebrew-tap` and drives the tap's own bottle and publish workflows to
land it. The names, the signing identity and what depends on them are in
[`docs/design/release.md`](docs/design/release.md); renaming `release.yml` or a
file-name template is a migration.

**Before the first tag** (each item is a blocker, not a nicety). A source build
reports `0.0.0-dev`, so the first tag can be `v0.1.0`:

- [ ] The repository is public. Every channel downloads release assets
  anonymously, and build provenance attestations need it on this plan.
- [ ] A `release` environment (Settings, Environments) with required
  reviewers, and a deployment rule limiting it to the tag pattern `v*`. Both the
  `release` and `homebrew-tap` jobs run in it. It protects the tap token and
  adds a human approval; it is not what controls the signing identity (the tag
  ruleset below and the on-`main` check are).
- [ ] A tag ruleset for `v*` (Settings, Rules): only maintainers may create
  them, and nobody may update or delete one. The workflow also refuses a tag
  whose commit is not on `main`.
- [ ] `HOMEBREW_TAP_TOKEN`, a secret in the `release` environment: a
  fine-grained personal access token for `sofired/homebrew-tap` only, with
  Contents, Pull requests and Actions read and write, and an expiry. Put the
  expiry in a calendar; when it lapses the tap job fails on its checkout. Without
  the secret the tap job notes that it skipped and the release itself is
  unaffected.
- [ ] If you protect the tap's `main`, let GitHub Actions push to it or bypass
  the rule: the tap's `publish.yml` (`brew pr-pull`) pushes the bump and its
  bottle block straight to `main` with the workflow's `GITHUB_TOKEN`, after the
  pull request this job opens has passed the tap's checks.
- [ ] The first tap pull request starts from `packaging/homebrew/berth.rb`;
  later ones only change its `url` and `sha256`. Change the formula's body in
  the tap, and keep the template in step.

**Cutting a release:**

1. `main` is green and holds what you mean to ship.
2. Tag the commit and push the tag: `git tag vMAJOR.MINOR.PATCH && git push
   origin vMAJOR.MINOR.PATCH`. The tag is the version (`v1.2.3` reports
   `1.2.3`); it must be a semantic version. A tag with a pre-release suffix
   (`v1.2.3-rc.1`) publishes a GitHub pre-release, which is never "latest", and
   does not touch Homebrew. Release notes list the commits since the previous
   tag, so after an rc they cover only what changed since the rc; and the first
   release's notes are the whole history, long enough to trim by hand on the
   release page.
3. Approve the run in the `release` environment, and watch it. Approval is
   requested once per gated job, so twice per release: first `release`, then
   `homebrew-tap`. When it finishes, check the release page, then try `brew install sofired/tap/berth`,
   `install.sh` and one package.

**When it fails:**

- Before the GitHub Release exists: fix the cause and re-run the failed jobs.
- After GoReleaser created the release, in the same job (the attestation, say):
  re-run the failed jobs. `release.replace_existing_artifacts` makes GoReleaser
  replace the assets it already uploaded, including the signature bundle (the
  bytes are the same, since a build of one commit is reproducible), and the
  attestation step only adds one. If the repository ever turns on immutable
  releases, assets cannot be replaced: delete the release (`gh release delete
  vX.Y.Z`, keeping the tag) and re-run instead.
- Only the `homebrew-tap` job failed: re-run just that job ("Re-run failed
  jobs"). It reuses the tap's `berth-X.Y.Z` branch and its open pull request,
  and does nothing if the tap's `main` already has the version. GitHub keeps one
  pending run per concurrency group, so if two releases were tagged while one
  waited on the tap, the replaced run's tap job has to be re-run by hand.
- The tap's pull request fails its checks because the formula no longer builds,
  most often because `go.mod` asks for a newer Go than Homebrew's `go` formula
  has. Wait for Homebrew to catch up, or lower the requirement; the release
  itself is not affected.

`install.sh` is served from `main` (or a tag), not from a release: a change to
it ships when it merges.

**Trying it without publishing.** Pull requests and pushes to `main` run the
`release-snapshot` job in `go.yml`: shellcheck, `goreleaser check`, a snapshot
build, a second build whose `checksums.txt` must match the first, and
`packaging/verify-snapshot.sh` (every artifact, the files in each, how each
binary was built, the tap formula rendered into `dist/berth.rb` from the
source archive, and `install.sh` against the snapshot through a local server).
The job uploads the artifacts and the formula. To do the same locally:

```bash
go install github.com/goreleaser/goreleaser/v2@v2.18.3   # the version both workflows pin
goreleaser check
goreleaser release --snapshot --clean --skip=sign
packaging/verify-snapshot.sh dist
```

`--skip=sign` is needed because signing wants the release job's OIDC token.
Change the GoReleaser version in `go.yml` and `release.yml` together; the
actions in `release.yml` are pinned by commit, so bump those on purpose.

## Commit messages

Conventional-commit style: `feat(runtime): …`, `fix(locks): …`,
`docs: …`, `test(registry): …`. Reference an issue when one exists.
