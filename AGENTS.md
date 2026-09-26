# AGENTS.md

Guidance for AI coding agents working on berth. Human contributors should
start with [`CONTRIBUTING.md`](CONTRIBUTING.md), which this file builds on;
where the two disagree, `CONTRIBUTING.md` wins.

## What this project is

berth gives every Git worktree a durable identity, its own Docker Compose
project and data directory, and a routed hostname on a shared local router.
Removing a worktree tears down everything it owned, and only what it can prove
it owned.

Two builds live in this repository:

- **TypeScript** (`src/`, `tests/`) is the build users run today, and the
  specification for the Go port. It is frozen: fixes only, no new features.
- **Go** (`cmd/berth`, `internal/`) is the port that will replace it. The
  process is in [`docs/port/process.md`](docs/port/process.md); the decision is
  in [`docs/decisions/0001-port-to-go.md`](docs/decisions/0001-port-to-go.md).

## Setup and checks

Toolchains: Node 24 (pinned in `.node-version`), pnpm through Corepack, and
the Go version in `go.mod`. Docker is needed only for the Docker integration
suites.

```bash
corepack enable && pnpm install
pnpm check                  # lint, typecheck, unit and CLI tests
pnpm test:integration       # real Git, subprocesses, locks; Docker suites skip without a daemon

gofmt -l cmd internal tools # must print nothing
go vet ./... && go vet -tags integration ./...
go test -race ./...
go test -race -tags integration ./...
CGO_ENABLED=0 go test ./...
```

`CONTRIBUTING.md` has the full Go list (staticcheck, the `CGO_ENABLED=0`
integration run, the streams contract against a built binary). Run the checks
for every build you touched before saying a change is done, and report what
you ran and what it printed.

## Repository map

- `src/`: the TypeScript build. `src/cli/main.ts` is its entry point.
- `cmd/berth`, `internal/`: the Go build. One package per concern, named after
  its TypeScript counterpart where there is one (`internal/checkoutindex` for
  `src/checkout-index/`).
- `tests/unit`, `tests/cli`, `tests/integration`: TypeScript tests. What goes
  where is in `CONTRIBUTING.md`.
- `tools/oracle/`: Node scripts that write expected data from the TypeScript
  build into Go packages' `testdata/`.
- `docs/port/divergences.md`: every accepted difference between the two builds.
- `docs/design/locks.md`, `docs/hooks.md`: design notes and the hook protocol.

## Rules that are easy to break

These come from `CONTRIBUTING.md` and `docs/port/process.md`. Changes that
break them are not merged.

- **Ownership before removal.** Anything that removes a resource proves it
  belongs to the worktree first, by its ownership labels, never by its name.
- **Locks are never cleared by age alone.** Recovery is explicit and checked.
- **Storage names are a contract.** `src/brand.ts` and `internal/brand` name
  things on disk and in Docker. Changing one is a migration, not a rename.
- **Only the entry point reads the environment.** Library code receives argv
  names, the working directory and environment values as parameters.
- **TypeScript is the specification.** Go ports behaviour and every
  user-facing string exactly. A difference that cannot be avoided gets a row
  in `docs/port/divergences.md` with a reason and a pinning test. Only a
  maintainer approves a row; leave it marked as proposed.
- **Test titles carry over.** Each TypeScript `it` becomes a Go
  `t.Run("<the exact title>", …)`.
- **Expected data comes from the oracle.** Regenerate golden files and corpora
  with the commands in `CONTRIBUTING.md`, never by hand.
- **A TypeScript fix ships to Go too**, in the same pull request or a prompt
  follow-up, so the two builds never drift apart.
- **Go style:** no naked returns; no spike markers, and every `TODO` names an
  issue, as `TODO(#issue)`; temp directories that meet path comparisons come
  from `registrytest.TempDir`; helper processes park with a sleep loop, never
  `select {}`; every new function, test helpers included, has a doc comment
  (`Test*` functions are exempt). `internal/style` enforces the first three.

## Running things safely

- The Docker integration suites remove whatever a killed run left behind.
  Run one of them at a time on a machine.
- Don't use the Go build on a machine where the TypeScript build is in use:
  their locks don't exclude each other.
- To try a build by hand, give it a scratch home so it doesn't touch the
  registered worktrees of the person you're working for. For the TypeScript
  build that is `BERTH_HOME`, its control home directly. For the Go build,
  `BERTH_HOME` is the berth root — `settings.toml` (#79), the
  router's own state and lock, and (unless `BERTH_CONTROL_HOME` overrides
  it) the control home all live under it (#80). For the Go build, a scratch
  `BERTH_HOME` also moves the router's state and lock, but not the router
  itself: there is one router container per Docker daemon, whichever build
  or home starts it, and `router up` (or `up`) from any build or home
  replaces the one that is running. Ask before starting or stopping it by
  hand.

## Commits and pull requests

- Conventional commits: `feat(runtime): …`, `fix(locks): …`, `docs: …`,
  `test(registry): …`. Reference an issue when one exists.
- One concern per pull request. Say in the description what you ran, and
  which divergence-ledger rows, if any, need a maintainer's approval.
- Update the README, `docs/hooks.md` or `CONTRIBUTING.md` in the same pull
  request when the behaviour they describe changes.
- CI (`ci.yml`, `go.yml`) runs only on pull requests and on pushes to
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
