# Contributing

Thanks for looking. A few things that make contributions land smoothly.

## Before you open a pull request

```bash
pnpm check                # lint, typecheck, unit and CLI tests — must be clean
pnpm test:integration     # real Git and subprocesses — must pass on your platform
```

CI runs both on Linux and macOS.

## What we care about

**Ownership is a safety property, not a feature.** Anything that removes a
resource must prove the resource belongs to the worktree first. A change that
makes teardown "work" by matching on a name will not be merged.

**Locks do not lie.** Never add a code path that clears a lock based on age
alone. Recovery is explicit and goes through `clearStaleLock`.

**Library code takes values; the entry point reads the environment.** Only
`src/cli/main.ts` reads `argv` for a name. Everything else receives the name
as a parameter. The same goes for the current working directory and
`process.env` where practical.

**Storage identity is a contract.** `BRAND` in `src/brand.ts` names things
that end up on disk or in Docker. Changing any of them is a migration for
every existing user and needs a versioned record schema or a migration
command — not a find-and-replace.

**No unused parameters, no parameter properties.** The TypeScript config uses
`erasableSyntaxOnly` so the source can run on Node directly. Stub methods that
must satisfy an interface may simply omit their parameters; where the concrete
class is called directly, keep them and `void` each.

## Tests

Unit tests are hermetic: no Docker, no real Git, no network. They mock at the
`CommandRunner` seam.

The one exception is `tests/cli/`, which spawns the CLI as a process. Which
stream a line lands on is only a contract if it survives the entry point, and
no mock at the `CommandRunner` seam can prove it. Those tests need `git` but no
Docker, and they run with the unit suite. Anything that needs Docker, a real
worktree, or a cross-process lock goes under `tests/integration/` instead.

Every module with security-relevant behaviour — path containment, file modes,
lock recovery, identity markers — has a dedicated test file. Keep it that way.

The Docker suites (`*-docker.integration.test.ts`) need a daemon; without one
they skip themselves, and the run says why once, at the start. CI's Linux leg
sets `BERTH_TEST_REQUIRE_DOCKER=1`, which turns that skip into a failure.
`true`, `yes`, and `on` count as well; `0`, `false`, `no`, and `off` mean the
default; case does not matter, and any other value is refused rather than
guessed at. The probe runs once for the whole integration run, whichever
files the run includes, so a daemon-free run of just the Git and lock suites
still pays for it — and still fails under `BERTH_TEST_REQUIRE_DOCKER=1`. The
Docker suites sweep anything a killed run left behind, without regard to age,
so run one of them at a time on a machine.

## Go

berth's TypeScript build is being ported to Go (`cmd/berth`, `internal/`);
see [`docs/decisions/0001-port-to-go.md`](docs/decisions/0001-port-to-go.md)
and [`docs/port/process.md`](docs/port/process.md). Everything above still
applies to `src/` for as long as it exists. This section covers the Go side.

**Toolchain:** the Go version in [`go.mod`](go.mod), and Node 24 to run the
TypeScript build as the oracle for parity tests and oracle data.

**Before you open a pull request:**

```bash
gofmt -l cmd internal tools                                # must print nothing
go vet ./...
go vet -tags integration ./...
go run honnef.co/go/tools/cmd/staticcheck@2025.1.1 ./...
go run honnef.co/go/tools/cmd/staticcheck@2025.1.1 -tags integration ./...
go test -race ./...                                         # unit
go test -race -tags integration ./...                        # real git, processes, signals, Docker, parity
CGO_ENABLED=0 go test ./... && CGO_ENABLED=0 go test -tags integration ./...
```

The `CGO_ENABLED=0` run matters on its own: cgo changes how the Go runtime
behaves (an extra thread, notably), and releases build with it disabled.

The streams contract, against a built Go binary:

```bash
go build -o dist/berth ./cmd/berth
BERTH_CLI=$PWD/dist/berth BERTH_CLI_UNPORTED=doctor,router,unlock \
  pnpm exec vitest run tests/cli/streams.test.ts
```

`BERTH_CLI_UNPORTED` lists verbs the Go build does not have yet, whose cases
skip instead of failing.

**Regenerating oracles** needs the Node version `.node-version` pins first
on `PATH`; `-update` refuses any other. After a change to the command tree,
help wording, or an error message:

```bash
go test ./internal/cli -run TestGolden -update
```

For the corpora other packages read (config, registry slugs and locale
sorting, text), run the matching script in `tools/oracle/`, e.g.:

```bash
node tools/oracle/config-corpus.ts > internal/config/testdata/config-corpus.json
```

Each script's header comment says exactly where its output goes.

**`//go:build integration`** marks tests that touch something real: a
process, a signal, or a Docker daemon. They are excluded from the default
`go test ./...` and run separately, as above.

**Go style,** beyond gofmt, go vet, and staticcheck:
- No naked returns — a function with named results still returns them
  explicitly.
- A temp directory that meets a path comparison is physical: use
  `registrytest.TempDir`, not `t.TempDir()` directly, since macOS's temp
  directory is a symlink into `/private`.
- A helper process that must stay alive parks with a `for { time.Sleep(...) }`
  loop, never `select {}` — with nothing else runnable, Go's deadlock
  detector exits a process blocked in `select {}` unless cgo is linked, which
  hides the bug on a cgo build and reproduces it on macOS or
  `CGO_ENABLED=0`.
- No spike markers, and every TODO names an issue: `TODO(#issue)`. Both are
  enforced by `internal/style`.

**A few TypeScript-only statements above now have a Go side too:**
- The entry point is also `cmd/berth` (`cli.Main`), alongside
  `src/cli/main.ts`.
- `internal/brand` must stay in sync with `src/brand.ts` — the same
  presentation/storage-identity split, ported field for field.
- "Library code takes values; the entry point reads the environment" holds
  in Go too. Its runner seam — Go's equivalent of TypeScript's
  `CommandRunner` — is `run.Runner` (`internal/run`), with
  `internal/run/fakerun` standing in for tests.

## Commit messages

Conventional-commit style: `feat(runtime): …`, `fix(locks): …`,
`docs: …`, `test(registry): …`. Reference an issue when one exists.
