# AGENTS.md

Guidance for AI coding agents working on treerig. Human contributors should
start with [`CONTRIBUTING.md`](CONTRIBUTING.md), which this file builds on;
where the two disagree, `CONTRIBUTING.md` wins.

## What this project is

treerig gives every Git worktree a durable identity, its own Docker Compose
project and data directory, and a routed hostname on a shared local router.
Removing a worktree tears down everything it owned, and only what it can prove
it owned.

The build lives in `cmd/treerig` (the entry point) and `internal/` (everything
else).

## Setup and checks

Toolchains: the Go version in `go.mod`. Docker is needed only for the Docker
integration suites.

```bash
gofmt -l cmd internal # must print nothing
go vet ./... && go vet -tags integration ./...
go test -race ./...
go test -race -p 1 -timeout 20m -count=1 -tags integration ./...
CGO_ENABLED=0 go test ./...
```

The integration runs use `-p 1` (one package at a time): the Docker suites pick
the router's index and ping ports from a deterministic hash start, so packages
running in parallel collide. `-timeout 20m` replaces Go's 10-minute per-package
default: `internal/commands` takes about 5 to 7 minutes under `-race` on CI and
about 12 on a slower machine, and keeps growing, so the default is too tight. A
timeout panic skips `t.Cleanup`, which can leave Docker resources behind: only
some suites, such as the router's, sweep what an earlier run abandoned.
`-count=1` stops Go from replaying a cached pass, which would never reach the
Docker daemon.

`CONTRIBUTING.md` has the full list (staticcheck, the `CGO_ENABLED=0`
integration run, the streams contract against a built binary). Run these
checks before saying a change is done, and report what you ran and what it
printed.

## Repository map

- `cmd/treerig`, `internal/`: the build. One package per concern.
- `docs/design/state.md`: which stored state carries a format version, the
  newer-format refusal, and how a migration is added.
- `docs/design/teardown.md`: the teardown pin, a trusted teardown binary
  installed and verified by `treerig teardown pin`. The removal gate that runs it
  is pending (#146).
- `docs/design/locks.md`, `docs/design/config.md`, `docs/design/release.md`,
  `docs/hooks.md`: design notes (locks; the config file, its JSON Schema and
  examples; the release assets and signing identity) and the hook protocol.

## Rules that are easy to break

These come from `CONTRIBUTING.md`. Changes that
break them are not merged.

- **Ownership before removal.** Anything that removes a resource proves it
  belongs to the worktree first, by its ownership labels, never by its name.
- **Locks are never cleared by age alone.** Recovery is explicit and checked.
- **Storage names are a contract.** `internal/brand`'s values name things on
  disk and in Docker — see its own tests. Changing one is a migration, not a
  rename.
- **Only the entry point reads the environment.** Library code receives argv
  names, the working directory and environment values as parameters.
- **The frozen goldens are hand-maintained fixtures.**
  `internal/cli/testdata/golden` and `internal/cli/testdata/output` pin
  exact CLI output; a difference from them is a change to review, not
  something to record elsewhere.
- **Go style:** no naked returns; no spike markers, and every `TODO` names an
  issue, as `TODO(#issue)`; temp directories that meet path comparisons come
  from `registrytest.TempDir`; helper processes park with a sleep loop, never
  `select {}`; every new function, test helpers included, has a doc comment
  (`Test*` functions are exempt). `internal/style` enforces the first three.

## Running things safely

- The Docker integration suites remove whatever a killed run left behind.
  Run one of them at a time on a machine.
- To try a build by hand, give it a scratch home so it doesn't touch the
  registered worktrees of the person you're working for. `TREERIG_HOME` is the
  treerig root — `settings.toml` (#79), the router's own state and lock, and
  (unless `TREERIG_CONTROL_HOME` overrides it) the control home all live under
  it (#80). A scratch `TREERIG_HOME` also moves the router's state and lock,
  but not the router itself: there is one router container per Docker
  daemon, whichever home starts it. `up` and `router up` refuse to touch a
  router another treerig root started (#82), but `router down` stops it from
  anywhere. Ask before starting or stopping it by hand.
- A scratch `TREERIG_HOME` has its own registry, which cannot see the pinned
  loopback ports (#119) of the real worktrees. Before the first `up` there,
  write `[ports]` / `range = "30000-30999"` into its `settings.toml`, so the
  trial never takes a port a real worktree holds.

## Commits and pull requests

- Conventional commits: `feat(runtime): …`, `fix(locks): …`, `docs: …`,
  `test(registry): …`. Reference an issue when one exists.
- One concern per pull request. Say in the description what you ran.
- Update the README, `docs/hooks.md` or `CONTRIBUTING.md` in the same pull
  request when the behaviour they describe changes.
- CI (`go.yml`) runs only on pull requests and on pushes to
  `main`. A pushed branch without a pull request gets no CI run.
- CodeRabbit reviews every pull request. Before marking one ready, answer
  each of its findings with a fix or a reasoned reply on the pull request,
  and bring the branch up to date with `main`. When a pull request merges,
  check whether other open ones now need updating or retargeting.
- Pull requests are usually squash-merged, and GitHub deletes the head
  branch on merge. Delete your local branch and its worktree yourself, and
  run `git fetch --prune` to drop the remote-tracking ref. `git branch
  --merged` and `git cherry` can't tell that a squash-merged branch has
  landed, so check its pull request's state instead.
