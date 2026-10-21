# Contributing

Thanks for looking. A few things that make contributions land smoothly.

## Before you open a pull request

See "Go" below for the checks to run before opening a pull request. CI runs
both on Linux and macOS.

Report security vulnerabilities privately through GitHub (see
[`SECURITY.md`](SECURITY.md)), not as an issue or pull request.

Everyone taking part follows the [Code of Conduct](CODE_OF_CONDUCT.md).
Report conduct problems to conduct@sofired.io.

## What we care about

**Ownership is a safety property, not a feature.** Anything that removes a
resource must prove the resource belongs to the worktree first. A change that
makes teardown "work" by matching on a name will not be merged.

**Locks do not lie.** Never add a code path that clears a lock based on age
alone. Recovery is explicit and goes through `lock.Clear`.

**Library code takes values; the entry point reads the environment.** Only
`cmd/treerig`'s `main` reads `os.Args` or `os.Environ` directly. Everything
else — including `cli.MainProcess` itself — receives the name, the working
directory and environment values as parameters. The runner seam is
`run.Runner` (`internal/run`), with `internal/run/fakerun` standing in for
tests.

**Storage identity is a contract.** `internal/brand` names things that end up
on disk or in Docker — see its own tests. Changing any of them is a
migration for every existing user and needs a versioned record schema or a
migration command — not a find-and-replace. Every store treerig reads back
carries a format version (`internal/stateformat`, pinned by its tests), and a
build refuses a newer one; `docs/design/state.md` lists the stores, the
refusal, and how a migration is added.

The pre-release rename (owner decision, 2026-10-15) changed every storage name
without a migration because nothing had shipped. From the first release
(v0.1.0-rc.2), the migration rule above applies in full.

## Repository layout

The build lives in `cmd/treerig` and `internal/`, one package per concern:

```
cmd/treerig/               entry point: argv and the environment come in here and
                          are handed down as parameters; library code never looks
                          a variable up by name itself
internal/
  cli/                    command tree, dispatch, error reporting
  commands/               the everyday verbs as plain functions over a context
  config/                 treerig.toml schema, discovery and the JSON Schema it publishes
  shape/                  the small schema library config files are validated
                          against, strict about unknown keys
  hooks/                  hook protocol and sequencer
  progress/               phase logging with an elapsed-time heartbeat
  ports/                  the lock-free loopback port allocator and its free-port probe
  checkoutindex/          per-worktree cards, the snapshot, and the rendered page
  router/                 the shared router's Compose stack per network mode, mode
                          detection, its file-provider route files and their
                          repair, and the ports it keeps
  shared/                 a repository's shared services: run once per repository in a
                          Compose project of their own, connected to each worktree
                          that uses them
  backend/                the Compose backend: labels, overrides, inventory, reap
  worktree/               what the control plane and backend both say about a worktree
  registry/               identity markers, slugs, the registry
  lock/                   flock-based locks and their checked recovery
  binstore/               the content-addressed store `worktree remove
                          --dev-teardown` runs itself from, outside any checkout
  teardown/               the store behind the pinned teardown binary: the pin file,
                          its installed binaries, the sweep of stale ones, and the
                          verified descriptor `worktree remove` executes
  releasefetch/           the bounded HTTPS fetch of a release for `teardown pin`:
                          URLs, checksums.txt, the cosign policy, archive extraction
  semver/                 a small, strict Semantic Versioning 2.0.0 comparer
  settings/               the machine's own settings.toml, read as strictly as treerig.toml
  layout/                 where everything lives under the treerig root and control home
  files/                  owner-only, symlink-refusing filesystem primitives
  text/                   the filter every printed line goes through
  brand/                  the product name and the presentation/storage split
  stateformat/            the format version of each stored state, and the
                          refusal of one a newer treerig wrote
  clierr/                 the error type a command reports and exits with
  logger/                 the stdout/stderr split every command writes through
  run/, run/osrun,
  run/fakerun/            the subprocess seam, and its real and fake implementations
  docker/                 runs the `docker` CLI and tells it apart from Docker being unavailable
  procid/                 process identity and process-table lookups, for lock recovery
  lifecycle/              signal handling and the lock-ordered mutation wrapper
  style/                  the code-style rules gofmt, vet and staticcheck don't
                          enforce, written as tests over the module's own source
```

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

`worktree remove` on a development build (a plain `go build`, which reports
`0.0.0-dev`) refuses without a pinned teardown binary, while a release build
(`semver.IsPublishedRelease`) removes with itself (`docs/design/teardown.md`,
#146). So every suite that runs it through a built binary pins that binary first,
in its own scratch root, with `pinWithEnv`
(`internal/cli/teardown_gate_integration_test.go`), unless it is the test of the
release path, which builds with `-X …/brand.Version=0.1.0-rc.3` (`buildVersioned`)
and pins nothing. The gate is skipped by
`cli.Main` and by an `Env` built by hand (its `Executable` and `Exec` are nil), so
in-process tests need nothing. The frozen `worktree remove` goldens under
`internal/cli/testdata/output` carry the gate's two lines, with the stored
binary's name scrubbed to `DIGEST`. `--dev-teardown` is a flag and nothing
else: `internal/style` fails if a setting, a `treerig.toml` key or an
environment variable names it.

Every module with security-relevant behaviour — path containment, file
modes, lock recovery, identity markers — has a dedicated test file. Keep it
that way.

A Docker suite (a `*_docker_integration_test.go` file) skips itself when no
daemon answers `docker info`, unless `TREERIG_TEST_REQUIRE_DOCKER=1` turns
that into a failure instead. `TREERIG_TEST_SKIP_DOCKER=1` skips it even when a
daemon answers. CI's `go` jobs set the second and its `docker` job the first,
so the Docker suites run once, in parallel with everything else. The Docker
suites sweep anything a killed run left behind, without regard to age, so
run one of them at a time on a machine.

CI splits the `docker` job into ten jobs, the `-race` and no-cgo legs times
five shards, each on its own runner and daemon: `internal/commands` in three,
`internal/router` and `internal/cli` in a fourth, and every other package in
the fifth. A shard is selected by
`.github/scripts/commands-shards.sh run <1|2|3|slow|rest>`. It names the slow
`internal/commands` tests for shards 1 and 2 and sends every other test, new
ones included, to shard 3; it names the two packages for `slow` and computes
`rest` with `go list`, so a new package needs nothing. Its `check`, run on
the three commands shards, fails them unless every top-level test in
`internal/commands` is in exactly one shard and every package is in exactly
one, so a renamed test or package, or an overlapping name, cannot drop one or
run it twice. When a slow Docker test
makes a shard the longest, move its name into the shorter of the first two
lists.

## Go

**Toolchain:** the Go version in [`go.mod`](go.mod).

The brand SVGs in `docs/brand/` come from `docs/brand/source/build.py`, which
needs Python 3 and its standard library only. Go remains the project language.

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

`schema/treerig.schema.json` is generated, not written: `treerig config schema`
prints it from the same `internal/shape` definitions that validate
`treerig.toml`. After changing the config schema or a field's description,
regenerate it with `go run ./cmd/treerig config schema > schema/treerig.schema.json`;
`TestSchemaFileIsCurrent` fails until you do. Every field needs a
`Describe(...)`, which editors show on hover. Examples in `treerig.example.toml`,
the README and `docs/` stay TOML 1.0 (no multi-line inline tables); a test checks them.
`treerig init` writes the starter in `internal/config/starter.go`: a short
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
  `commandstest.World`; a scratch treerig root under a real binary or daemon
  writes its `settings.toml` with `registrytest.WriteScratchSettings`. Where
  the treerig root and the control home are one directory, claim it first with
  `registrytest.ClaimControlHome`, since treerig adopts only a directory it
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
- [ ] The first tap pull request starts from `packaging/homebrew/treerig.rb`;
  later ones only change its `url` and `sha256`. Change the formula's body in
  the tap, and keep the template in step.
- [ ] The docs no longer say that nothing has shipped. In `README.md`, update
  the `Pre-release` line under the title, the status
  box ("Nothing is tagged yet") and the Install text ("No release exists
  yet"). In `docs/install.md`, update the "No release exists yet" note at the
  top and each "None has shipped yet" line. `grep -n "yet" README.md
  docs/install.md` finds them.

**Cutting a release:**

1. `main` is green and holds what you mean to ship.
2. Tag the commit and push the tag: `git tag vMAJOR.MINOR.PATCH && git push
   origin vMAJOR.MINOR.PATCH`. The tag is the version (`v1.2.3` reports
   `1.2.3`); it must be a strict semantic version that treerig reads as a release
   (no leading zeros, no `+build`, not `0.0.0`, no `git describe` tail), which
   `.github/scripts/validate-tag.sh` checks and `internal/semver`'s tests hold to
   `IsPublishedRelease`. A tag with a pre-release suffix
   (`v1.2.3-rc.1`) publishes a GitHub pre-release, which is never "latest", and
   does not touch Homebrew. Release notes list the commits since the previous
   tag, so after an rc they cover only what changed since the rc; and the first
   release's notes are the whole history, long enough to trim by hand on the
   release page.
3. Approve the run in the `release` environment, and watch it. Approval is
   requested once per gated job, so twice per release: first `release`, then
   `homebrew-tap`. When it finishes, check the release page, then try `brew install sofired/tap/treerig`,
   `install.sh` and one package.
4. Look at the **Release smoke** run that follows (Actions, "Release smoke"; see
   below). It is not part of the release and does not hold it up.

   The smoke workflow, and the script it runs, come from `main` (a
   `workflow_run` workflow runs from the default branch), not from the tag. So
   merge any change to `release-smoke.yml` or `.github/scripts/release-smoke.sh`
   before you tag, or dispatch it afterwards with "Use workflow from: main".

**Release smoke** (`.github/workflows/release-smoke.yml`, #147) checks the
published release the way a user meets it. It has two kinds of job: "Read and
validate the tag", and a `smoke` job on each of `ubuntu-latest`,
`ubuntu-24.04-arm` and `macos-latest` (linux/amd64, linux/arm64 and darwin/arm64;
darwin/amd64, which the release also ships, has no standard hosted runner, so
nothing runs that archive). It starts when the Release workflow finishes
successfully, and shows in the Actions tab as "Release smoke vX.Y.Z". Each
runner:

- downloads `checksums.txt`, its Sigstore bundle and that platform's archive from
  `releases/download/<tag>/`, checks `checksums.txt` with `cosign verify-blob`
  against the identity in `docs/design/release.md` (the real keyless signature,
  which nothing else exercises before a release), checks that it lists every
  asset the release contract names, and checks the archive against it;
- on Linux, downloads the `.deb`, `.rpm` and `.apk` (a pre-release's names differ
  from its `checksums.txt` lines; `docs/design/release.md` says how) and checks
  that `/usr/bin/treerig` in each is the archive's `treerig`;
- runs the archive's `treerig` through three legs, each with its own scratch
  `TREERIG_HOME` and `TREERIG_CONTROL_HOME` and no terminal, ending in a
  `worktree remove` that removes nothing (the checkout is not a registered
  worktree): **pin** (`teardown pin --release` with the real cosign,
  `teardown status`, and both "pinned teardown binary" lines of the gate),
  **no pin** (a release removes with itself: no refusal and neither line, and
  `teardown status --json` says `"removal": "this-build"`) and **corrupt pin**
  (the stored binary overwritten, which the gate refuses);
- at a stable tag that is the latest release, also checks
  `releases/latest/download/checksums.txt` and `teardown pin --latest`.

**Running it again.** Use Actions, "Release smoke", Run workflow ("Use workflow
from: main"), and give the tag with its `v`, as `v0.1.0-rc.3` (`validate-tag.sh`
checks it, and a tag without the `v` is refused). The workflow starts only when
the Release run *succeeded*. A re-run of
a failed Release job or run makes a new attempt of the same run (the same run id,
a higher `run_attempt`); when that attempt succeeds the run's conclusion becomes
`success` and GitHub sends a new `workflow_run` `completed` event, so the smoke
run starts by itself, once, for the tag. GitHub's documentation says that the
`requested` activity does not occur on a re-run and does not spell out
`completed`, so if no smoke run appears within a minute of the re-run finishing,
dispatch it by hand. A Release run that fails and stays failed (a failed
`homebrew-tap` job, the last one, and for a stable tag the slowest: it waits for
the tap's bottles) also starts no smoke run; dispatch it. The smoke runs for one
tag are serialised (`concurrency`), never cancelled.

**When Release smoke fails.** The log of the failed job says what; read, in
order, the `release-smoke: …` line that ends it (and, in the run's summary, its
`::error::` annotation), and for a leg the
`release-smoke: FAILED (<leg> leg): …` line, above which each command is shown as
`[<leg>] $ <command>` with its `out|` and `err|` lines and `exit N`. A failure
means the *published release* has a problem; nothing was unpublished.

| What it says | What it means | What to do |
| --- | --- | --- |
| `checksums.txt is not signed by …; do not trust this release` | The signature is missing, or isn't from `release.yml` at that tag. | Don't trust the release. Mark it pre-release or delete it (`gh release delete`, keeping the tag), and find out why: was `release.yml` renamed, was it run from a fork or another ref, was `checksums.txt` replaced? |
| `could not download …` or a 404 | An asset is not there (yet), or GitHub had a hiccup. | Re-run the failed job; the downloads are retried already. If it stays, the release is missing that asset: look at the release page and the Release run. |
| `checksums.txt does not list: …` | A release asset the contract names (`docs/design/release.md`) is missing or renamed. | A file-name template changed (a migration) or GoReleaser skipped a package. Fix forward with a new tag. |
| `… has sha256 …, but checksums.txt lists …` or `… does not match its line` | A download differs from what was signed: the asset was replaced, or the upload was cut. | Re-run once. If it persists, treat the release as untrustworthy, as for a signature failure. |
| `/usr/bin/treerig in … is not the treerig in …`, or `no /usr/bin/treerig in …` | A package carries a different binary from the archive's, or can't be read (the message has the tools' last words). | The release is inconsistent: don't point people at the package, and fix forward. |
| `release-smoke: FAILED (pin leg) …`, `(no-pin leg)`, `(corrupt leg)` | The release's own binary doesn't do what the gate and pin should. | Reproduce it with the same binary: unpack the archive and run `.github/scripts/release-smoke.sh legs --binary ./treerig --version X.Y.Z --keep` (the `--keep` leaves the scratch directory, named at the top of the output, with each command's output). Then fix forward; it is a bug in #146 or the pin. |

**The homebrew-tap job is checked by hand**: the tap's pull request is merged or
closed by its publish workflow, and an anonymous API call is rate-limited on
shared runners, so a check there would be flaky. At a stable tag, confirm in
`sofired/homebrew-tap` that:

- the `treerig-X.Y.Z` pull request is merged;
- the tap's `main` has the new `url` and `sha256` in `Formula/treerig.rb` and a
  `bottle do` block;
- the tap's `publish.yml` run for it is green; and
- `brew install sofired/tap/treerig` installs `X.Y.Z` and `treerig --version` says so.

If any of that is missing, re-run the failed `homebrew-tap` job of the Release
run (see "When the release fails" below); it reuses the tap's branch and pull
request.

The legs are a script, so they run locally against a binary you have, before any
release exists (`.github/scripts/release-smoke.sh help`, and its header, for the
rest). A GoReleaser snapshot is a development build, so give it `--dev-build`
(the no-pin leg then expects the gate's refusal), and `--pin file`, which pins
the snapshot's own `treerig` instead of downloading a release. `--dev-build`,
`--pin file`, `--base-url`, `--no-signature-check` and `--no-packages` are for
local testing only, and the script refuses them under GitHub Actions. The one
exception is `--ci-snapshot`, which allows `--dev-build` and `--pin file` (and
nothing else) for the `release-snapshot` job in `go.yml`, which runs the legs on
every pull request. `release-smoke.yml` uses none of them.

```bash
goreleaser release --snapshot --clean --skip=sign
.github/scripts/release-smoke.sh legs --pin file --dev-build \
  --binary dist/treerig_linux_amd64_v1/treerig \
  --version "$(sed -n 's/.*"version":"\([^"]*\)".*/\1/p' dist/metadata.json)"
```

A build with a release version (`-ldflags "-X
github.com/sofired/treerig/internal/brand.Version=0.1.0-rc.3"`) takes `--pin
file` without `--dev-build`. Both use scratch homes under `$TMPDIR` and leave
your own `~/.treerig` and pin alone. `fetch` and `latest` run the same way
against a real tag (cosign 3, curl, and on Linux `dpkg-deb`, `rpm2cpio` and
`cpio` needed).

**When the release fails:**

- Before the GitHub Release exists: fix the cause and re-run the failed jobs.
- After GoReleaser created the release, in the same job (the attestation, say):
  re-run the failed jobs. `release.replace_existing_artifacts` makes GoReleaser
  replace the assets it already uploaded, including the signature bundle (the
  bytes are the same, since a build of one commit is reproducible), and the
  attestation step only adds one. If the repository ever turns on immutable
  releases, assets cannot be replaced: delete the release (`gh release delete
  vX.Y.Z`, keeping the tag) and re-run instead.
- Only the `homebrew-tap` job failed: re-run just that job ("Re-run failed
  jobs"). It reuses the tap's `treerig-X.Y.Z` branch and its open pull request,
  and does nothing if the tap's `main` already has the version. GitHub keeps one
  pending run per concurrency group, so if two releases were tagged while one
  waited on the tap, the replaced run's tap job has to be re-run by hand.
- The tap's pull request fails its checks because the formula no longer builds,
  most often because `go.mod` asks for a newer Go than Homebrew's `go` formula
  has. Wait for Homebrew to catch up, or lower the requirement; the release
  itself is not affected.

`install.sh` is served from `main` (or a tag), not from a release: a change to
it ships when it merges.

**The `trig` alias.** Every install route except the archive links `trig` to
`treerig` (Homebrew, `install.sh`, the packages); `docs/design/release.md`
("The `trig` alias") says how each treats a name clash. The user docs for it
are in `docs/install.md` ("The `trig` alias"). The tap's `Formula/treerig.rb`
is only rewritten for its url and sha256 once it exists, so a formula
published before the alias needs the `install` and `test` changes from
`packaging/homebrew/treerig.rb` applied to the tap by hand, in a pull request
there.

**Trying it without publishing.** Pull requests and pushes to `main` run the
`release-snapshot` job in `go.yml`: shellcheck, `goreleaser check`, a snapshot
build, a second build whose `checksums.txt` must match the first, and
`packaging/verify-snapshot.sh` (every artifact, the files in each, how each
binary was built, that `/usr/bin/treerig` in the host's `.deb` and `.rpm` is the
`treerig` in the host's archive, that both packages link `/usr/bin/trig` to it,
the tap formula rendered into `dist/treerig.rb` from the source archive, and
`install.sh` against the snapshot through a local server, including the `trig`
alias and when it must be skipped). The job also runs the teardown pin flow:
`teardown pin --release` against the snapshot with a real cosign (`TestTeardownPinSnapshot`,
`TestTeardownPinSnapshotTampering`, `TestTeardownPinWithRealCosign` and
`TestTeardownPinWithRealCosignAndNoTrustRoot`). It also pins the snapshot's own
binary and runs it through `worktree remove`'s gate against an empty directory
(`TestSnapshotBinaryThroughTheGate`, with the same `TREERIG_SNAPSHOT_DIST` and
`TREERIG_SNAPSHOT_VERSION`, under `-tags integration`), so the CGO_ENABLED=0,
trimpath release binary proves the exec through `/proc/self/fd`. It uploads the
artifacts and the formula. To do the same locally:

```bash
go install github.com/goreleaser/goreleaser/v2@v2.18.3   # the version both workflows pin
goreleaser check
goreleaser release --snapshot --clean --skip=sign
packaging/verify-snapshot.sh dist

# The pin flow; cosign 3.0.0 or newer on PATH (the job installs v3.0.6). Run it
# after the snapshot build, and after any second build, which `--clean` would wipe.
TREERIG_REQUIRE_COSIGN=1 \
TREERIG_SNAPSHOT_DIST="$PWD/dist" \
TREERIG_SNAPSHOT_VERSION="$(sed -n 's/.*"version":"\([^"]*\)".*/\1/p' dist/metadata.json)" \
  go test -race -count=1 -run '^(TestTeardownPinSnapshot|TestTeardownPinSnapshotTampering|TestTeardownPinWithRealCosign|TestTeardownPinWithRealCosignAndNoTrustRoot)$' ./internal/commands/

# The snapshot binary through worktree remove's gate; no cosign needed.
TREERIG_SNAPSHOT_DIST="$PWD/dist" \
TREERIG_SNAPSHOT_VERSION="$(sed -n 's/.*"version":"\([^"]*\)".*/\1/p' dist/metadata.json)" \
  go test -race -count=1 -tags integration -run '^TestSnapshotBinaryThroughTheGate$' ./internal/cli/
```

`--skip=sign` is needed because signing wants the release job's OIDC token.

The job sets `TREERIG_REQUIRE_COSIGN=1` and checks the test log, so a missing
cosign or snapshot fails the step instead of skipping, and so does any skipped
test. Leave the variable out locally to skip those tests where cosign is absent;
the tests use a throwaway key and a local server, and make no request to the
internet. The package-hash check reads the `.deb` with `dpkg-deb` and the `.rpm`
with `rpm2cpio` and `cpio`; locally it says "skipped" where a tool is missing,
and under `CI` it fails instead.

Change the GoReleaser version in `go.yml` and `release.yml` together; the
actions in `release.yml` are pinned by commit, and Dependabot
(`.github/dependabot.yml`) moves each pin together with its `# vX.Y.Z` comment.
It opens a weekly pull request for minor and patch updates, a separate one for
each major update, and its own security updates.

The `release-signing` group (cosign-installer, goreleaser-action and attest)
signs and publishes only on a release tag. A pull request's CI installs
cosign-installer and goreleaser-action in the `release-snapshot` job but never
signs. `go.yml`, `release.yml` and `release-smoke.yml` all pin cosign-installer,
by the same commit, and all set `cosign-release:`; they change together, and
`TestCosignPinnedTheSameInAllWorkflows` (`internal/style`) fails if they differ.
Before merging an update to the group, check by hand:

- `goreleaser release --snapshot --clean --skip=sign` still passes;
- the new installer still takes `cosign-release:` and can still install
  `v3.0.6` (read its `action.yml` at the new commit and its release notes).

`cosign-release:` is an input, not an action, so Dependabot leaves it alone;
raise it by hand in all three workflows (`docs/design/release.md`).

## Commit messages

Conventional-commit style: `feat(runtime): …`, `fix(locks): …`,
`docs: …`, `test(registry): …`. Reference an issue when one exists.
