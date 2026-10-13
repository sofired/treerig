# The hook contract

Hooks are how a project tells the tool what to do without the tool learning
what the project is. They are ordinary executables. The tool spawns them,
hands them an environment, and reads their exit code. That is the whole
interface, and it is deliberately small: everything here is a compatibility
promise.

[Commands](#commands), the project's own named tasks that `berth run` runs on
demand, are resolved, split and given their environment exactly as hooks are;
everything below applies to them except where that section says otherwise.

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
directory containing `berth.toml`. Anything else is looked up on `PATH`. The
same goes for a `[commands]` entry.

```toml
[hooks]
prepare = "./scripts/migrate.sh --fast"   # ./scripts/migrate.sh, relative to berth.toml
backup  = "pg_dump"                       # from PATH
```

Splitting is on whitespace, with double-quoted segments kept whole. If you
need anything richer than that, write a script.

## Environment

Every hook, and every command, inherits the parent environment plus these. The prefix is a
storage-contract name — it does **not** change when the binary is renamed, so
hook scripts keep working however the tool is invoked.

| Variable | Value |
| -------- | ----- |
| `BERTH_STAGE` | `prepare`, `seed` or `verify` today; `backup` and `restore` once something invokes them. Hooks only: a [command](#commands) never has it |
| `BERTH_COMMAND` | The name of the [command](#commands) being run. Commands only: a hook never has it |
| `BERTH_PROJECT` | `project.name` from the config |
| `BERTH_REPOSITORY_ID` | UUID of the repository |
| `BERTH_WORKTREE_ID` | UUID of this worktree |
| `BERTH_SLUG` | The worktree's machine-unique slug |
| `BERTH_HOST` | `<slug>.<domain>` |
| `BERTH_WORKTREE_HOME` | The worktree's private directory under the control home |
| `BERTH_CONFIG` | Absolute path of the loaded `berth.toml` |
| `BERTH_ROOT` | Directory containing `berth.toml`; also the working directory |
| `BERTH_URL` | This worktree's own URL: the `[service.<name>]` routed at the host root, with no `subdomain` and no `path` narrowing it. Unset if no service claims that route. |
| `BERTH_SERVICE_URL_<NAME>` | One per `[service.<name>]`: its own routed URL. `<NAME>` is the service's name, upper-cased, with each `-` turned into `_` |
| `BERTH_SERVICE_PORT_<NAME>` | One per `[service.<name>]` that has a pinned loopback port: the port on `127.0.0.1` the service is published on, for a tool that must bypass the router. `<NAME>` maps as for `BERTH_SERVICE_URL_<NAME>`. Unset for a service with no port yet, as in a hook that runs before the first `up` has allocated one |
| `BERTH_SHARED_ADDR_<NAME>_<PORT>` | One per container port of a [shared service](../README.md#shared-services) that has a pinned loopback port: `127.0.0.1:<host port>`, for a process on the host. `<NAME>` is the shared service's name mapped as for `BERTH_SERVICE_URL_<NAME>`, and `<PORT>` its container port as declared (`BERTH_SHARED_ADDR_MAILPIT_1025`). Read from the repository's records, so it is set whether or not the service is running. Unset for a port with no pinned port yet |
| `BERTH_SHARED_URL_<NAME>` | One per shared service with a routed `port`: the URL of its web UI, `http://<name>.shared.<domain>` with the router's port folded in as for `BERTH_SERVICE_URL_<NAME>`. `<NAME>` maps as there. Read from the repository's records, so it is set whether or not the service is running. Unset for a service with no routed port, or one that has not been started yet |

`BERTH_SHARED_ADDR_<NAME>_<PORT>` and `BERTH_SHARED_URL_<NAME>` are for the
host: a container reaches the shared service by its name on its network
(`mailpit:1025`) and never uses the pinned port or the URL, and neither is
passed to Compose, so `compose.yaml` cannot reference them. A hook that runs
`berth` against another worktree does not hand its own down: like the URL
variables, an inherited one of either is dropped and this worktree's set
instead. The shared service is connected to the worktree's network before
`prepare` runs, so a hook may use it at once.

`BERTH_SERVICE_PORT_<NAME>` is the `port` of `urls --json`: unlike the URLs
it does not go through the router, and it is allocated before any hook runs.
`BERTH_URL` and `BERTH_SERVICE_URL_<NAME>` are the same URLs `berth urls`
prints, router port included exactly as it folds that in there. When
`doctor` runs `verify` but can't resolve the router's port, it sets none of
these three, nor `BERTH_SHARED_URL_<NAME>`, and its Router section says why.
(`BERTH_SHARED_ADDR_…` does not depend on the router and is still set.)
None of these variables reaches Compose, so `compose.yaml` cannot reference `${BERTH_URL}` — read
them from a hook instead:

```sh
curl -fsS "${BERTH_URL:?no service is routed at the host root}/health"
```

Under `set -u`, an unset `BERTH_URL` aborts the script; guard it with
`${BERTH_URL:-}` or `${BERTH_URL:?…}` as above. A hook curling one of these
is a non-browser client, so the same resolver caveats apply as anywhere else
outside a browser — see [README.md's "Hostname resolution"
notes](../README.md#platform-notes).

`BERTH_HOME` and `BERTH_CONTROL_HOME` — the berth-root and control-home
overrides, if the user set either — are *not* in this list on purpose. Both
pass through from the parent unchanged, so a hook that invokes the tool
itself resolves the same berth root and control home the user did.

A hook may run the tool against another worktree, but not against its own:
`up` holds that worktree's lock while its hooks run, so `up`, `down`,
`worktree remove` or a locked `run` on the same worktree would wait on its
own caller. They refuse at once instead, when `BERTH_WORKTREE_ID` names the
worktree they were asked to change. A locked [command](#commands) holds the
lock too, so the same holds inside one, and inside a `lock = false` command,
which cannot be told apart from it.

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

## Commands

`berth run <name> [-- args...]` runs one of the project's named commands: the
on-demand tasks that are not part of coming up, such as `terraform plan` and
`apply` against the worktree's Keycloak, without the tool learning what they
do. They are declared under `[commands]` in `berth.toml`. Each is a command
line on its own, or a table:

```toml
[commands]
plan = "./scripts/keycloak.sh plan"

[commands.apply]
command    = "./scripts/keycloak.sh apply"
lock       = true   # the default
kill-after = "60s"  # the default is 10s
```

The name is lower-case letters, digits and hyphens. `command` is resolved and
split as [above](#resolution) and runs with the directory containing
`berth.toml` as its working directory. `lock` and `kill-after` are described
below; `kill-after` is a duration such as `30s`, `5m` or `1m30s`, greater
than zero and at most `1h`. A name is at most 63 characters. `berth config show` prints `commands` as an object, in the order
the file wrote it, with both options filled in; a file with no `[commands]`
leaves the key out.

`berth run` with no name, or an unknown one, is a `[berth] ERROR:` line on
stderr, `No command "nope": berth.toml defines plan, apply.` (in file order),
or `…: berth.toml has no [commands].`, and exits 1. Everything after `--` is appended to the command
line as separate arguments, and reaches the command untouched, `-h`
included; anything between the name and `--` is an error, so a mistyped name
is never taken for an argument or the other way round:

```sh
berth run apply -- -auto-approve -var "region=eu west"
```

Flags after the command name are still berth's own: `--config`, `-V` and
`-h` there are read by berth, not passed on, and any other flag is an error
that says so. The command's own flags go after `--`.

### What a command gets

- The environment above, with `BERTH_COMMAND` set to its name and no
  `BERTH_STAGE`, whatever the caller had in its own environment. `BERTH_URL`,
  `BERTH_SERVICE_URL_<NAME>`, `BERTH_SERVICE_PORT_<NAME>`,
  `BERTH_SHARED_ADDR_<NAME>_<PORT>` and `BERTH_SHARED_URL_<NAME>` are set as
  for a hook.
- The terminal. A command runs interactively, in a process group of its own,
  and holds the terminal's foreground when berth's stdin is a terminal and
  berth holds it: `terraform apply` can ask for its approval, and Ctrl-C and
  Ctrl-Z work as they would without berth. Without a terminal (a pipe, CI,
  `ssh` without `-t`) it still gets its own process group, and stdin is
  whatever berth's is.
- Its own output. Its standard output and standard error are berth's own, not
  copied or captured, so `berth run plan > out` holds only what `plan`
  wrote, and `berth run plan | cat` works. berth prints nothing of its own
  while the command runs: no start line, no heartbeat. When the command
  fails, one `[berth] ERROR:` line on stderr says so.
- Its exit code as berth's own, as for a hook: `The apply command failed
  (exit 2).` exits 2, `126` and `127` mean it could not be started, and a
  command ended by a signal exits 128 plus its number.

`berth run` needs the worktree to be registered (run `berth up` first) and
never starts services: a command against stopped ones fails in its own
script, with a clearer message than berth could give.

### Locking

By default a command holds the worktree's operation lock while it runs, the
lock `up`, `down` and `worktree remove` take. Those wait for it (up to 25
minutes) and, after a couple of seconds, say what they are waiting on:
`berth run apply`, without the arguments after `--`, which may carry a secret.
A second `run` waits too, and one inside the same worktree's own hooks or
commands refuses at once, since its caller holds the lock. The development
lock is not taken, so an `apply` can run during a `dev` session.

The lock is released once the command's whole process group has exited, and
not before: when the command returns, whatever it left in its group is sent
`SIGTERM`, then `SIGKILL` after `kill-after`. So the lock is never handed to a
process, as it is to `docker`: a command cannot keep the worktree locked by
outliving `berth run`.

`lock = false` skips the lock and nothing else. berth still catches
`SIGINT`, `SIGTERM` and `SIGHUP`, forwards them to the command, and waits for
its group. It is *trusted, not enforced*, and meant for read-only commands
such as a `plan`, the way `doctor` runs `verify` unlocked. Do not use it for
anything that writes: `up`, `down` and `worktree remove` do not wait for
such a command, and `worktree remove` can delete the data under a running
`apply`.

- Because it carries the worktree's `BERTH_WORKTREE_ID`, stale-lock recovery
  and `worktree unlock` count it as a process to wait for, as they would any
  descendant of a dead holder.
- There is no lock to wait on, so `berth run` of another `lock = false`
  command works from inside it. But the command carries
  `BERTH_WORKTREE_ID` all the same, so it cannot drive its own worktree's
  locked verbs (`up`, `down`, `worktree remove`, a locked `run`) either: those
  refuse at once, as they do inside a hook, because the refusal cannot know
  whether the caller holds the lock.

### Interrupting

A `SIGINT` or `SIGTERM` berth receives, from `kill` or CI cancelling the job,
is forwarded to the command's process group as itself (`SIGINT` stays
`SIGINT`). A `SIGHUP`, which means the operator's terminal is gone, is
forwarded as `SIGTERM`, since terraform and docker wind down on that and not
on a hangup. What follows, in order:

1. The command has `kill-after` to exit. A **second** `SIGINT` or `SIGTERM`
   ends that grace at once: the command's group is sent `SIGKILL`. Any two
   count, a `SIGINT` immediately followed by a `SIGTERM` from a supervisor
   included. (A second
   `SIGHUP` does not count: a hangup often arrives twice within milliseconds.)
2. Once the command has exited, anything it left in its group is sent
   `SIGTERM`, then `SIGKILL` after another `kill-after`. A second signal
   during this wait gives up on it and says which process group is left.
3. The lock is released, and berth dies by the first signal it was sent, so
   a shell sees that signal's status even when the command was killed with
   `SIGKILL` (137) on the way.

So the worst wait after one signal is about twice `kill-after`, and one more
`SIGINT` or `SIGTERM` cuts it short. CI systems that cancel a job give it only
seconds before they kill everything (GitHub Actions, about 7.5 seconds), which
a large `kill-after` outlasts: the runner's own `SIGKILL` will arrive first.

Ctrl-C typed at a terminal the command holds goes to the command's group
directly, so berth reports how the command ended and exits with its code (130
for `SIGINT`). The same holds for a closed terminal: while the command holds
the terminal, the kernel delivers the terminal's `SIGHUP` to the command
itself, as `SIGHUP`, and berth cannot change that. So a long `apply` over SSH
belongs in `tmux` or `screen`, or under `nohup` or `setsid`. `nohup berth run
…` makes berth, and the command it starts, ignore `SIGHUP`: closing the
terminal then stops neither, output goes to `nohup.out` unless redirected, and
because `nohup` also detaches stdin from the terminal, the command cannot
prompt: pass its own no-prompt flag after `--`.

A backgrounded `berth run … &` (or one run without a terminal) never gets the
terminal's foreground. If its stdin is still a terminal, a command that reads
it is stopped by `SIGTTIN`, and holds the lock while it is stopped, until
`fg` brings berth back or a signal arrives (berth sends the command
`SIGCONT` along with it). In CI, or anywhere stdin might be open but never
written to, pass the command's own no-prompt flag after `--`
(`-input=false`, `-auto-approve`) or redirect stdin from `/dev/null`;
otherwise a prompt waits forever, holding the lock.

Size `kill-after` for the command's *own* graceful stop, since `SIGKILL` is
what follows it. The default of 10 seconds can end `terraform apply` before
it has written its state or released its state lock, or `docker compose run`
before it has asked the daemon to stop its container; give those a minute or
more, up to an hour, the most the config accepts:

```toml
[commands.apply]
command    = "terraform apply"
kill-after = "5m"
```

Interrupting `berth run` stops the local CLI, not necessarily the work it
started elsewhere: a container the CLI asked a daemon to start, an apply
already under way against a remote API, or a Terraform state lock held in a
remote backend can outlive it. Check for them, and clear a lock with the
tool's own command (`terraform force-unlock`), before running the command
again.

If a command leaves processes behind that outlast the wait, berth says so
by name and group: `The apply command left processes running (process group
4242) after 15s; stop them with kill -KILL -4242.`

## `doctor`'s own check

`doctor` predicts this without running anything: for every configured hook
— `prepare`, `seed`, `verify`, `backup` and `restore` alike, whether or not
this worktree is registered — and then for every `[commands]` entry, in file
order, it resolves the command exactly as above and reports one `<stage> hook
file` or `<name> command file` line, checking that the file exists, is
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
problem. A hook or command file that would not start names the exit code a real run
would give, 127 or 126, in its own line — a prediction, not something
`doctor` itself exits with (see the README's own `doctor` section). This is
separate from, and runs whether or not, `doctor` goes on to actually run
`verify`, reported on its own unprefixed `verify hook` line; `config
validate` checks only the file, not what its hooks point at.

## Cancellation

Hooks are spawned through the tool's process runner, which means an interrupt
(`SIGINT`, `SIGTERM` or `SIGHUP`) is delivered as `SIGTERM` to the hook and
its descendants, escalating to `SIGKILL` after ten seconds. Write hooks that
can be interrupted. A hook is never interactive, so it always keeps the plain
`SIGTERM` contract, whatever stopped berth.

A [command](#commands) is different, because it runs interactively: the
signal berth received is forwarded as itself (`SIGINT` stays `SIGINT`), and
the command gets the terminal's own job control, Ctrl-Z included, only when
its stdin is a terminal and berth currently holds that terminal's foreground
— not when berth is backgrounded, its stdin is piped, it runs over `ssh`
without `-t`, or it runs in CI, where the command still gets its own process
group but never the terminal itself. Either way, once the command returns,
whatever is left in its process group is sent `SIGTERM`, then `SIGKILL` after
its `kill-after` grace (ten seconds unless the command sets its own),
whether or not berth was itself signalled.

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
