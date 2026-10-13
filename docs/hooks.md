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
| `backup` | When you run [`berth backup`](#backup-and-restore). | environment + bundle directory as `$1` |
| `restore` | When you run [`berth restore`](#backup-and-restore). | environment + bundle directory as `$1` |

The lifecycle order is `prepare → seed → verify`. `up` asks for `prepare` and
`seed` only; `doctor` runs `verify` on its own, and never `prepare` or `seed`.
`backup` and `restore` are not part of that order: each runs alone, when its
command is run.
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

`dev` runs them exactly as `up` does, since it is `up` followed by a log
follower; the hooks finish before any log is followed, and a failing one fails
`dev` with its exit code.

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
| `BERTH_STAGE` | `prepare`, `seed`, `verify`, `backup` or `restore`. Hooks only: a [command](#commands) never has it |
| `BERTH_COMMAND` | The name of the [command](#commands) being run. Commands only: a hook never has it |
| `BERTH_PROJECT` | `project.name` from the config |
| `BERTH_REPOSITORY_ID` | UUID of the repository |
| `BERTH_WORKTREE_ID` | UUID of this worktree |
| `BERTH_SLUG` | The worktree's machine-unique slug. It changes only when [`worktree rename`](../README.md#renaming-a-worktree) renames the worktree, which needs it stopped; the next `up` runs hooks with the new one |
| `BERTH_HOST` | `<slug>.<domain>` |
| `BERTH_WORKTREE_HOME` | The worktree's private directory under the control home |
| `BERTH_CONFIG` | Absolute path of the loaded `berth.toml` |
| `BERTH_ROOT` | Directory containing `berth.toml`; also the working directory |
| `COMPOSE_PROJECT_NAME` | The worktree's Compose project, `berth-<hash>`, so `docker compose exec` and `logs` in a hook reach this worktree's containers and not the project Compose would name after the directory. Replaces an inherited value |
| `BERTH_COMPOSE_PROJECT` | The same name under berth's own prefix |
| `COMPOSE_FILE` | Absolute path of the Compose file `[compose] file` names, which Compose otherwise looks for as `compose.yaml` beside `berth.toml`. Replaces an inherited value. The extra files berth adds when it starts services (published ports, labels) are not part of it. Compose splits this variable on `:`, so a path that contains one does not work; keep the checkout where its path has none |
| `BERTH_URL` | This worktree's own URL: the `[service.<name>]` routed at the host root, with no `subdomain` and no `path` narrowing it. Unset if no service claims that route. |
| `BERTH_SERVICE_URL_<NAME>` | One per `[service.<name>]`: its own routed URL. `<NAME>` is the service's name, upper-cased, with each `-` turned into `_` |
| `BERTH_SERVICE_PORT_<NAME>` | One per `[service.<name>]` that has a pinned loopback port: the port on `127.0.0.1` the service is published on, for a tool that must bypass the router. `<NAME>` maps as for `BERTH_SERVICE_URL_<NAME>`. Unset for a service with no port yet, as in a hook that runs before the first `up` has allocated one |
| `BERTH_SHARED_ADDR_<NAME>_<PORT>` | One per container port of a [shared service](../README.md#shared-services) that has a pinned loopback port: `127.0.0.1:<host port>`, for a process on the host. `<NAME>` is the shared service's name mapped as for `BERTH_SERVICE_URL_<NAME>`, and `<PORT>` its container port as declared (`BERTH_SHARED_ADDR_MAILPIT_1025`). Read from the repository's records, so it is set whether or not the service is running. Unset for a port with no pinned port yet |

`BERTH_SHARED_ADDR_<NAME>_<PORT>` is for the host: a container reaches the
shared service by its name on its network (`mailpit:1025`) and never uses
the pinned port, and it is not passed to Compose either, so `compose.yaml`
cannot reference it. A hook that runs `berth` against another worktree does not
hand its own down: like the URL variables, an inherited one is dropped and
this worktree's set instead. The shared service is connected to the
worktree's network before `prepare` runs, so a hook may use it at once.

`BERTH_SERVICE_PORT_<NAME>` is the `port` of `urls --json`: unlike the URLs
it does not go through the router, and it is allocated before any hook runs.
`BERTH_URL` and `BERTH_SERVICE_URL_<NAME>` are the same URLs `berth urls`
prints, router port included exactly as it folds that in there. When
`doctor` runs `verify` but can't resolve the router's port, it sets none of
these three, and its Router section says why. (`BERTH_SHARED_ADDR_…` does
not depend on the router and is still set.) None of them
reaches Compose, so `compose.yaml` cannot reference `${BERTH_URL}` — read
them from a hook instead:

```sh
curl -fsS "${BERTH_URL:?no service is routed at the host root}/health"
```

Under `set -u`, an unset `BERTH_URL` aborts the script; guard it with
`${BERTH_URL:-}` or `${BERTH_URL:?…}` as above. A hook curling one of these
is a non-browser client, so the same resolver caveats apply as anywhere else
outside a browser — see [README.md's "Hostname resolution"
notes](../README.md#platform-notes).

The three Compose variables reach every hook (`prepare`, `seed`, `verify`,
`backup`, `restore`) and every [command](#commands). `BERTH_COMPOSE_PROJECT`
reaches Compose too, like the other `BERTH_*` names. A hook that runs
`docker compose exec` or `logs` finds this worktree's containers. One that runs
`docker compose up` (or `run`) starts containers in the worktree's project
**without berth's ownership labels**: `berth up --remove-orphans` may remove
them, and `worktree remove` will not reap them, since it finds containers by
label and never by name. Let `berth up` start services, and use `exec` and
`run --rm` for anything transient.

The project name is also what berth's lock recovery looks for. A process whose
environment names `COMPOSE_PROJECT_NAME=berth-<hash>` is counted as one of the
worktree's own when `worktree unlock` and stale-lock recovery scan for
survivors of a dead holder. Hook children already carried
`BERTH_WORKTREE_ID`, which marks them the same way, so this changes nothing
about which processes are counted: they are the worktree's processes.

`BERTH_HOME` and `BERTH_CONTROL_HOME` — the berth-root and control-home
overrides, if the user set either — are *not* in this list on purpose. Both
pass through from the parent unchanged, so a hook that invokes the tool
itself resolves the same berth root and control home the user did.

A hook may run the tool against another worktree, but not against its own:
`up`, `backup` and `restore` hold that worktree's lock while their hooks
run, so `up`, `down`, `dev`, `backup`, `restore`, `worktree remove`,
`worktree rename` or a locked `run` on the same worktree would wait on its own
caller. They refuse at once instead, when `BERTH_WORKTREE_ID` names the
worktree they were asked to change. A locked [command](#commands) holds the
lock too, so the same holds inside one, and inside a `lock = false` command,
which cannot be told apart from it.

## Output and exit codes

Standard output and standard error are streamed to the terminal — both to
standard error for `verify`, since `doctor`'s standard output is its report.
Exit zero is success. Any other exit code fails the stage and is preserved as the tool's
own exit code, so CI sees the same number your script produced — except for a
hook berth stopped for outliving its [`[hooks.timeout]`](#timeouts) limit,
which exits 124 whatever the hook itself was exiting with.

A hook that cannot be started fails the stage without running, with the
shell's codes: 127 when the file — or the interpreter on its `#!` line — does
not exist, 126 when it is not executable or has no `#!` line (berth never
falls back to running a script through `/bin/sh`, on any platform). A
hook ended by a signal exits 128 plus the signal number (137 for `SIGKILL`),
as a shell reports it. A hook stopped for running past its
[timeout](#timeouts) exits 124.

## Timeouts

A hook runs without a time limit unless the config gives it one. The limit
is `[hooks.timeout]`, a table beside the hooks, keyed by the hook's name:

```toml
[hooks]
prepare = "./scripts/migrate.sh"
verify  = "./scripts/check.sh"

[hooks.timeout]
prepare = "30m"
verify  = "2m"
```

Each value is a duration such as `500ms`, `30s`, `10m` or `1h30m`, greater
than zero and at most `24h`. Every hook kind has a key, `backup` and `restore`
included, and `berth config show` prints the table as written (leaving the key
out when the file has none). A limit for a hook the file does not configure is
accepted and does nothing.

**Why no default.** berth cannot know how long a project's migration or seed
takes, and a default that fires on a slow but healthy `prepare` kills it
halfway, which can leave a half-migrated database that is worse than the hang
it was meant to prevent. So a hook with no entry keeps today's behaviour and
runs until it exits or the operator interrupts berth. Set a limit where a hang
is more likely or costs more than a slow run: `verify`, which `doctor` runs
and which should answer quickly, is the obvious one, and `prepare` and `seed`
hold the worktree's operation lock the whole time. A limit on `prepare` or
`seed` longer than 25 minutes outlasts how long another command waits for
that lock, so a waiting `down` or `up` gives up before the hook does.

**What happens at the limit.** The hook is stopped through the same
process-group machinery an interrupt uses: its whole group is sent `SIGTERM`,
then `SIGKILL` ten seconds later, and berth does not go on until nothing is
left in the group — a child that ignored the `SIGTERM`, or outlived the hook
that started it, included. Then:

- The hook fails as *timed out*, not as failed: one `[berth] ERROR:` line,
  `The prepare hook timed out after 30m and was terminated.`, and exit code
  **124**, what `timeout(1)` reports, whatever the hook itself was exiting
  with when it was stopped. A hook that fails by itself says
  `The prepare hook failed (exit N).` and keeps its own code.
- In `up`, the sequencer stops there as it does for any failure: `seed` does
  not run after a `prepare` that timed out, the worktree stays pending, and
  the next `up` runs them again. The worktree's operation lock is released on
  the way out, as after any other failed hook, so the next command does not
  wait on it.
- In `doctor`, the `verify hook` line is `fail` with the same sentence, and
  `doctor` exits 124.

Stopping takes up to about ten seconds after the limit, the grace before
`SIGKILL` (longer only if something in the group will not die, see below).

**What it does not stop.** The limit covers the hook's process group *on the
host*. Work the hook handed to something outside that group keeps running: a
process started with `setsid`, a container a `docker run` started, and the
command inside a container that `docker compose exec` or `docker exec` ran —
berth stops the `docker` client, not the process the daemon started in the
container. A long migration run that way should carry its own limit inside the
container, with `[hooks.timeout]` as the slightly longer outer bound:

```toml
[hooks]
prepare = "docker compose exec -T api timeout 25m ./bin/migrate"

[hooks.timeout]
prepare = "27m"
```

`timeout` inside a container signals only the command it starts, so give it
the real binary, not a shell script that forks, or `exec` the work from the
script.

A hook that runs work in a container should also check for, and clear, a run
left over from a timed-out earlier attempt before it starts another. If
something in the hook's group will not die even from `SIGKILL`, the error says
that instead of "timed out" and names the group.

An interrupt of berth itself is a different thing and unchanged: see
[Cancellation](#cancellation).

## Backup and restore

`berth backup [dir]` runs the `backup` hook and `berth restore <dir>` runs the
`restore` hook, each with the **bundle directory** as its only added
argument: an absolute path, appended to whatever arguments the configured
command line already carries (`backup = "./scripts/backup.sh --fast"` runs
`./scripts/backup.sh --fast /path/to/bundle`). The hook gets the same
environment and working directory as `prepare`, with `BERTH_STAGE` set to
`backup` or `restore`. What goes in the bundle, and in what format, is the
hook's business; `restore` is meant to read what `backup` wrote, and nothing
checks that it does. A hook validates its own input: `restore` passes the
directory on even when it is empty (it warns), and a hook should check that
the files it needs are there before it changes anything.

A hook must not require an empty bundle directory: it is not empty when the
hook starts, because it holds the `.incomplete` marker (below). A tool that
wants a directory of its own, such as `pg_dump -Fd`, is given a subdirectory:
`pg_dump -Fd -f "$1/db"`.

### A PostgreSQL round trip

The hooks run in the project root with the worktree's own Compose project in
their environment ([`COMPOSE_PROJECT_NAME`](#environment)), so
`docker compose exec` reaches this worktree's database and no other. Keep the
data on a named volume, as below: Compose labels it, so `worktree remove`
reaps it. The anonymous volume a Postgres image declares for itself is not
reaped, and would outlive the worktree.

```yaml
services:
  db:
    image: postgres:16-alpine
    environment: { POSTGRES_USER: app, POSTGRES_PASSWORD: app, POSTGRES_DB: app }
    volumes:
      - pgdata:/var/lib/postgresql/data
volumes:
  pgdata:
```

```toml
[hooks]
backup  = "./scripts/backup.sh"
restore = "./scripts/restore.sh"
```

```sh
#!/usr/bin/env bash
# scripts/backup.sh: dump to a temporary name, then move it into place, so a
# dump that dies half way is never left under the name restore looks for.
set -euo pipefail
bundle=$1
docker compose exec -T db pg_dump -U app --clean --if-exists app > "$bundle/app.sql.tmp"
mv "$bundle/app.sql.tmp" "$bundle/app.sql"
```

```sh
#!/usr/bin/env bash
# scripts/restore.sh: stop at the first error, and apply all or nothing.
set -euo pipefail
bundle=$1
[ -f "$bundle/app.sql" ] || { echo "restore: $bundle/app.sql is missing" >&2; exit 1; }
docker compose exec -T db psql -U app -d app -v ON_ERROR_STOP=1 --single-transaction < "$bundle/app.sql"
```

`-T` is needed because the hook has no terminal. `--clean --if-exists` makes
the dump replace the tables that are there: a restore **overwrites** the
worktree's live data.

### The bundle directory

- **`berth backup`** creates the directory (parents too, mode 0700, since a
  bundle holds the worktree's data), or uses one that exists and is empty. An
  existing directory keeps its own mode; berth warns when other users can
  read it. A directory that holds anything is refused before the hook runs,
  so one backup is never mixed into another and an old bundle is never
  overwritten. A path that is a file is refused too.
- **`berth backup` with no directory** writes to
  `<worktree home>/backups/<timestamp>`, where the worktree home is
  `$BERTH_WORKTREE_HOME` and the timestamp is UTC, such as
  `20261012T153045Z`.
- **`berth restore`** requires the directory to exist already; it never creates
  one.
- A relative directory is taken from the directory you ran `berth` in, not the
  project root the hook runs in, so the hook always receives the path you
  meant.
- **The `.incomplete` marker.** Before the hook runs, `backup` writes an empty
  `.incomplete` file into the bundle, and removes it when the hook exits zero.
  A bundle that still holds it was not finished, and `restore` refuses it.
  Hooks must leave the file alone. A backup whose hook fails before writing
  anything leaves no trace: the marker is removed, and the directory too if
  berth created it. One that wrote something keeps the marker, and berth
  says so; delete the directory, or give the next backup another one.

**Why the default is inside the worktree's home.** The home is the one private
(0700) place berth already keeps a worktree's state, so a bundle holding its
data is protected the same way, with no location for the user to pick, and the
timestamped names sort in time order. The cost is that `berth worktree remove`
deletes the home, bundles included, and nothing prunes old bundles. berth
makes the loss visible rather than avoiding it: the removal plan names them
(`its home directory under the control home, including 2 backup bundle(s) in
backups/`), and a backup into the default location says that `worktree
remove` will delete it. A bundle meant to outlive the worktree, or to be
copied elsewhere, is given a directory of its own.

**Scripting `backup`.** The hook's output is the command's own standard output
and error. berth's closing `Backed up <host> to <dir>.` line, and the notices
and warnings above, go to standard error; berth prints nothing on standard
output. A script that needs the bundle's path passes an explicit directory
rather than parsing the default.

### What each command checks, and holds

Before running the hook, and in this order, both commands refuse (exit 1, a
`[berth] ERROR:` line) when:

0. `restore` is given no directory (`Restore needs a bundle directory: berth
   restore <dir>.`), checked before `berth.toml` is read;
1. the hook is not configured (`No backup hook is configured: add backup under
   [hooks] in berth.toml.`);
2. the worktree is not registered (run `berth up` first);
3. the worktree has no running service. A backup reads the services' data and
   a restore loads into them, so both need the worktree up; one running
   service is enough, and what the hook needs from the others is its own
   business;
4. the bundle directory is not usable as above, or, for `restore`, was left
   `.incomplete`.

Checks 3 and 4 run under the lock, in that order, so nothing is created for a
command that is refused. The domain and ports the hook sees are those in the
registration once the lock is won.

While the hook runs, the worktree's operation lock is held, as it is for the
hooks of `up`: `up`, `down`, `worktree remove`, a locked `run` and another
`backup` or `restore` wait for it and name `berth backup` or `berth restore`.
The development lock is not taken, so both work during a `dev` session. The
hook is not interactive and its output is streamed as for any other hook.
Inside a hook of the same worktree, both commands refuse at once rather than
wait on the lock their caller holds.

A hook that hangs, a `pg_dump` waiting on a stuck connection say, holds the
lock until it ends or you interrupt it, unless `[hooks.timeout]` gives
`backup` or `restore` a [limit](#timeouts). Past it, berth stops the hook and
its process group on the host, which does not reach a dump running inside a
container through `docker compose exec`: that keeps going there.

### Exit status and signals

The command exits with the hook's own status, as every hook does (see
[Output and exit codes](#output-and-exit-codes)): `The backup hook failed
(exit 3).` exits 3, `126` and `127` mean the hook could not be started, and a
hook ended by a signal exits 128 plus its number. A `SIGINT`, `SIGTERM` or
`SIGHUP` berth receives is delivered to the hook's whole process group as
`SIGTERM`, as in [Cancellation](#cancellation), and berth dies by that signal
once the hook's group is gone and the lock is released. An interrupted backup
keeps its `.incomplete` marker like any other that wrote something. A hook
that outlives its [`[hooks.timeout]`](#timeouts) limit is stopped and the
command exits 124, not the hook's own code; a timed-out backup is treated like
any other failed one, so a bundle it had begun keeps its marker.

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
  `BERTH_SERVICE_URL_<NAME>`, `BERTH_SERVICE_PORT_<NAME>` and
  `BERTH_SHARED_ADDR_<NAME>_<PORT>` are set as for a hook.
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
lock `up`, `down`, `worktree remove` and `worktree rename` take. Those wait for it (up to 25
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
anything that writes: `up`, `down`, `worktree remove` and `worktree rename` do not wait for
such a command, and `worktree remove` can delete the data under a running
`apply`.

- Because it carries the worktree's `BERTH_WORKTREE_ID`, stale-lock recovery
  and `worktree unlock` count it as a process to wait for, as they would any
  descendant of a dead holder.
- There is no lock to wait on, so `berth run` of another `lock = false`
  command works from inside it. But the command carries
  `BERTH_WORKTREE_ID` all the same, so it cannot drive its own worktree's
  locked verbs (`up`, `down`, `worktree remove`, `worktree rename`, a locked `run`) either: those
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
4242) after 15s; stop them with kill -KILL -4242.` If a second interrupt cuts that wait short, the message
reads instead: `The apply command left processes running (process group 4242),
and waiting for them was abandoned after a second interrupt; stop them with
kill -KILL -4242.`

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
