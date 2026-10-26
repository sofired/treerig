# Review guidelines for treerig

treerig gives every Git worktree a durable identity, its own Docker Compose project and data directory, and a
routed hostname on a shared local router. Removing a worktree tears down everything it owned, and only what it can
prove it owned. It's a Go CLI: `cmd/treerig` is the entry point and `internal/` holds one package per concern.

## Where to look

- `CONTRIBUTING.md` ("What we care about", "Tests", "Go") and `AGENTS.md` ("Rules that are easy to break") hold the
  rules. If this PR changes them, judge the change against the rules as they were before it, not as it rewrites them.
- `docs/design/*.md` (`cli-1.0.md`, `config.md`, `locks.md`, `release.md`, `routing.md`, `state.md`,
  `teardown.md`): design notes. A behaviour a note described before this PR is deliberate. A note this PR adds or
  edits doesn't make a behaviour deliberate: judge the change against the note's old version.
- `docs/*.md`: the user documentation. It must match the code in the same PR.
- `internal/style`: tests that enforce some of the style rules under "Style the tooling owns" below. Don't report
  what they already catch.
- `internal/run` and `internal/run/fakerun`: the process-runner seam that unit tests mock.

## Rules a change must keep

Report a change that breaks one of these, citing the rule.

- **Ownership before removal.** Anything that removes a resource proves it belongs to the worktree first, by its
  ownership labels, never by its name.
- **Locks are never cleared by age alone.** Recovery is explicit and goes through `lock.Clear`.
- **Storage names are a contract.** Values in `internal/brand` name things on disk and in Docker. Changing one needs
  a migration. Every stored state carries a format version (`internal/stateformat`), and a build refuses a newer one.
- **Only the entry point reads the environment.** Library code gets argv, the working directory and environment
  values as parameters. A new direct read of `os.Args` or `os.Environ` outside `cmd/treerig` is a finding; the
  existing read sites are not.
- **Unit tests are hermetic:** no Docker, real Git or network. Anything real is behind `//go:build integration`.
- **Physical temp directories.** A temp directory that meets a path comparison comes from `registrytest.TempDir`,
  not `t.TempDir()` directly (macOS's temp directory is a symlink into `/private`).
- **Words:** new code, tests and docs say *worktree*, *directory* or *environment*, not *checkout* (except Git's own
  `git checkout` and `actions/checkout`).
- **Docs change with behaviour.** A PR that changes behaviour described in the README, `docs/` or `CONTRIBUTING.md`
  updates them too.

## What gets acted on here

Past reviews whose findings were fixed, roughly in order of how often:

- Docs, help text, examples and doc comments that say something the code doesn't do, or overstate a guarantee.
- Errors that are swallowed, misattributed or reported with the wrong cause, and cancellation that isn't propagated.
- Filesystem races and unsafe opens on state treerig owns: check-then-use, symlinks, truncating writes, files that
  appear before their content does.
- Tests that pass without proving their claim: skips that hide the case, waits that can't fail, unbounded waits,
  assertions on the wrong thing, missing cleanup of Docker resources.
- State that goes stale after a partial failure: an index, a route or a registration that isn't refreshed.

## What not to flag

These were raised before and rejected for good reasons.

- **Deliberate tolerance in recovery paths.** `worktree remove`, `down` and `doctor` must keep working on broken
  state, so they treat unreadable optional data as absent on purpose. Report it only if it can remove something the
  worktree doesn't own, or follows a symlink or path outside the state it owns.
- **Route files treerig didn't write.** Not validating or removing one is the design. Code that reads, parses or
  executes such a file is still reviewable.
- **Claims about Docker, Compose, Git or OS behaviour you haven't confirmed.** The maintainer checks these against real
  daemons and versions and has rejected confident but wrong claims. Include such a finding only when the repository
  shows the behaviour, such as an integration test, a comment that cites a version, or a design note. A claim with
  security impact (symlinks, permissions, escaping a directory) may still be raised as a lower-confidence question.
- **The CLI's message conventions.** Commands in messages are quoted with single quotes (`Run 'treerig urls'`), as
  `docs/design/cli-1.0.md` ("Streams") sets.
- **Hand-maintained goldens** (`internal/cli/testdata/golden`, `internal/cli/testdata/output`). A changed fixture is
  intended output. Review whether the output is right, not that the fixture changed; where the output is
  security-relevant (paths, what gets removed), check it against the code rather than trusting the fixture.
- **Style the tooling owns:** gofmt, go vet, staticcheck and `internal/style` (naked returns, spike markers and
  `TODO(#issue)`).
- **Missing doc comments.** Every new function still needs one, but CodeRabbit checks them on each pull request
  against a threshold, so a second report adds noise.
- **Release-process wording** in the PR template, CONTRIBUTING or the release docs that names a temporary state. The
  maintainer removes it when the state ends.
