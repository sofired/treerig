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

## Commit messages

Conventional-commit style: `feat(runtime): …`, `fix(locks): …`,
`docs: …`, `test(registry): …`. Reference an issue when one exists.
