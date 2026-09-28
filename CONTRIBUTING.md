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
migration command — not a find-and-replace.

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
that into a failure instead — set on CI's Linux leg. The Docker suites sweep
anything a killed run left behind, without regard to age, so run one of
them at a time on a machine.

## Go

**Toolchain:** the Go version in [`go.mod`](go.mod).

**Before you open a pull request:**

```bash
gofmt -l cmd internal                                       # must print nothing
go vet ./...
go vet -tags integration ./...
go run honnef.co/go/tools/cmd/staticcheck@2025.1.1 ./...
go run honnef.co/go/tools/cmd/staticcheck@2025.1.1 -tags integration ./...
go test -race ./...                                         # unit
go test -race -tags integration ./...                        # real git, processes, signals, Docker, goldens
CGO_ENABLED=0 go test ./... && CGO_ENABLED=0 go test -tags integration ./...
```

The `CGO_ENABLED=0` run matters on its own: cgo changes how the Go runtime
behaves (an extra thread, notably), and releases build with it disabled.
`go test -race -tags integration ./...` also runs the streams contract
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
- Every new function and method gets a doc comment, test helpers in
  `_test.go` files included: what it does and why, not a restatement of its
  signature. `Test*` functions are exempt, since their `t.Run` titles describe
  them. CodeRabbit checks this on each pull request, against an 80%
  threshold.

## Commit messages

Conventional-commit style: `feat(runtime): …`, `fix(locks): …`,
`docs: …`, `test(registry): …`. Reference an issue when one exists.
