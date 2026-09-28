# The hook contract

Hooks are how a project tells the tool what to do without the tool learning
what the project is. They are ordinary executables. The tool spawns them,
hands them an environment, and reads their exit code. That is the whole
interface, and it is deliberately small: everything here is a compatibility
promise.

## Stages

| Stage | When it runs | Receives |
| ----- | ------------ | -------- |
| `prepare` | Services are up and healthy. Nothing has been seeded. | environment only |
| `seed` | **Only after `prepare` exited zero.** | environment only |
| `verify` | When `berth doctor` checks a registered worktree. Exit zero means healthy. | environment only |
| `backup` | Accepted and validated now; nothing invokes it yet. A future `berth backup` command will. | environment + bundle directory as `$1` |
| `restore` | Accepted and validated now; nothing invokes it yet. A future `berth restore` command will. | environment + bundle directory as `$1` |

The lifecycle order is `prepare → seed → verify`. `up` asks for `prepare` and
`seed` only; `doctor` runs `verify` on its own, and never `prepare` or `seed`.
The sequencer stops at the first non-zero exit and reports which stage failed.
A stage that is not configured is skipped, not failed.

The `prepare`/`seed` ordering is enforced by the runner, not by convention.
Put migrations in `prepare` and data loading in `seed`, and it is impossible
to load data into an unmigrated schema.

## When `up` runs them

`up` runs `prepare` every time, once the services report healthy. It runs
`seed` only the first time a worktree comes up — more precisely, while the
worktree's registration is still *pending*, which it stays until `prepare`
and `seed` have both exited zero. A failed first `up` therefore seeds on the
retry; a successful one never seeds again until the environment is removed.
An interrupted `worktree remove` also returns the registration to pending,
since its volumes may already be gone, so the next `up` seeds again rather
than starting on empty stores. Write `prepare` to be idempotent; write `seed`
assuming an empty, migrated store.

## Resolution

A command that starts with `.` or contains `/` is resolved relative to the
directory containing `berth.toml`. Anything else is looked up on `PATH`.

```toml
[hooks]
prepare = "./scripts/migrate.sh --fast"   # ./scripts/migrate.sh, relative to berth.toml
backup  = "pg_dump"                       # from PATH
```

Splitting is on whitespace, with double-quoted segments kept whole. If you
need anything richer than that, write a script.

## Environment

Every hook inherits the parent environment plus these. The prefix is a
storage-contract name — it does **not** change when the binary is renamed, so
hook scripts keep working however the tool is invoked.

| Variable | Value |
| -------- | ----- |
| `BERTH_STAGE` | `prepare`, `seed` or `verify` today; `backup` and `restore` once something invokes them |
| `BERTH_PROJECT` | `project.name` from the config |
| `BERTH_REPOSITORY_ID` | UUID of the repository |
| `BERTH_WORKTREE_ID` | UUID of this worktree |
| `BERTH_SLUG` | The worktree's machine-unique slug |
| `BERTH_HOST` | `<slug>.<domain>` |
| `BERTH_WORKTREE_HOME` | The worktree's private directory under the control home |
| `BERTH_CONFIG` | Absolute path of the loaded `berth.toml` |
| `BERTH_ROOT` | Directory containing `berth.toml`; also the working directory |

`BERTH_HOME` and `BERTH_CONTROL_HOME` — the berth-root and control-home
overrides, if the user set either — are *not* in this list on purpose. Both
pass through from the parent unchanged, so a hook that invokes the tool
itself resolves the same berth root and control home the user did.

A hook may run the tool against another worktree, but not against its own:
`up` holds that worktree's lock while its hooks run, so `up`, `down` or
`worktree remove` on the same worktree would wait on its own caller. They
refuse at once instead, when `BERTH_WORKTREE_ID` names the worktree they
were asked to change.

## Output and exit codes

Standard output and standard error are streamed to the terminal — both to
standard error for `verify`, since `doctor`'s standard output is its report.
Exit zero is success. Any other exit code fails the stage and is preserved as the tool's
own exit code, so CI sees the same number your script produced.

A hook that cannot be started fails the stage without running, with the
shell's codes: 127 when the file — or the interpreter on its `#!` line — does
not exist, 126 when it is not executable or has no `#!` line (berth never
falls back to running a script through `/bin/sh`, on any platform). A
hook ended by a signal exits 128 plus the signal number (137 for `SIGKILL`),
as a shell reports it.

## `doctor`'s own check

`doctor` predicts this without running anything: for every configured hook
— `prepare`, `seed`, `verify`, `backup` and `restore` alike, whether or not
this worktree is registered — it resolves the command exactly as above and
reports one `<stage> hook file` line, checking that the file exists, is
executable, and is something exec(2) can run directly: a native ELF or
Mach-O executable, or a script whose `#!` line names an interpreter that
itself exists, is executable, and is itself checked the same way — a native
binary always works, and a script naming another script is followed on
Linux up to the kernel's own interpreter-chain limit and refused outright
on Darwin, which never recurses at all (a relative interpreter path
resolves against the hook's own working directory, never `PATH`, exactly
like execve(2); a shebang saved with CRLF line endings is caught the same
way a real run would fail on it, glued-on `\r` and all — see "Output and
exit codes" above). A file whose execute bit is set but cannot be opened to check
its format (mode `0111`, say) is left `ok`: execute-only is not a format
problem. A hook file that would not start names the exit code a real run
would give, 127 or 126, in its own line — a prediction, not something
`doctor` itself exits with (see the README's own `doctor` section). This is
separate from, and runs whether or not, `doctor` goes on to actually run
`verify`, reported on its own unprefixed `verify hook` line; `config
validate` checks only the file, not what its hooks point at.

## Cancellation

Hooks are spawned through the tool's process runner, which means an interrupt
(`SIGINT`/`SIGTERM`) is delivered as `SIGTERM` to the hook and its descendants,
escalating to `SIGKILL` after ten seconds. Write hooks that can be interrupted.

## An example, in three languages

Each of these is a valid `prepare` hook. The tool cannot tell them apart.

```bash
#!/usr/bin/env bash
set -euo pipefail
echo "migrating ${BERTH_SLUG} against ${BERTH_HOST}"
exec ./bin/rails db:migrate
```

```python
#!/usr/bin/env python3
import os, subprocess, sys
print(f"migrating {os.environ['BERTH_SLUG']}")
sys.exit(subprocess.call(["python", "manage.py", "migrate"]))
```

```go
// go run ./cmd/prepare
package main

import ("fmt"; "os"; "os/exec")

func main() {
	fmt.Println("migrating", os.Getenv("BERTH_SLUG"))
	cmd := exec.Command("goose", "up")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil { os.Exit(1) }
}
```
