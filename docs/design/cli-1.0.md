# The command line for 1.0

**Status.** Proposed. Nothing here is built.

**Basis.** Two usability reviews of v0.1.0, by a CLI interface designer and a
usability specialist, each run against a live stack on a release build in
October 2026, and a terminal-UI evaluation with working prototypes. Their
reports are not in the repository; [What the reviews found](#what-the-reviews-found)
keeps what they observed.

Nothing in the command line is frozen before 1.0, so this design renames and
removes commands and changes output and exit codes where that gives a better
experience. [What changes for existing users](#what-changes-for-existing-users)
lists every break for the release notes.

**Reading guide.** The owner: the principles, the command set and [Owner
decisions](#owner-decisions). Implementers: everything, then [Order of
work](#order-of-work).

**Terms** are as in the [glossary](../glossary.md). Here, *worktree* is a
worktree treerig manages, *checkout* is its directory on disk, *slug* is its
short name (`feature-x`) and *host* is its hostname
(`feature-x.myapp.localhost`). `trig` is the same program as `treerig`;
everything here applies to it. treerig runs on Linux and macOS only, so TTY and
signal handling are Unix-only.

## What the reviews found

Each was observed on a live stack.

1. **A failed `up` hides its cause.** A crashing service produces a
   600-character `Command failed (docker compose --project-name treerig-d6d8...
   up --detach --build ...)` line and the Compose transcript, twice. The useful
   line, `container treerig-d6d8...-worker-1 exited (3)`, is in the middle and
   names the container by a hash. The service's own output (`FATAL db refused`)
   never appears. Afterwards `doctor` says `No checks failed` and exits 0.
2. **`doctor` reports configuration, not whether the worktree runs.** It
   already probes every route through a running router, but it doesn't check
   whether this worktree's services are running, and it reports a stopped
   router as `ok router  not running`, so a crashed service or a stopped router
   passes. A healthy run is 38 to 45 lines, mostly internals (`treerig root`,
   `control home`, `router state`, `router bind`, every `hook file resolves to
   ...`), with the verdict last, on stderr.
3. **URLs are printed when nothing answers them.** With the router stopped,
   `status` says `running` and `urls` prints URLs that return nothing.
4. **Worktree names don't line up.** `worktree list` shows the host and the
   removal prompt asks for it, but `worktree remove <host>` fails with `No
   worktree is registered as <host>`. A slug used in two repositories (`main`)
   is refused as ambiguous even inside one of them.
5. **Removing a worktree is two steps and says so nowhere.** The checkout stays
   after `worktree remove`. Without a terminal it fails with `Interactive
   confirmation required: "<host>".` and doesn't mention `--yes`.
6. **Three vocabularies for "is it running".** `worktree list` says `ready` for
   a worktree that is down; `status` says `running`, `stopped` or `absent`; the
   index says `[Running]` or `[Stopped]`. `status` has no URLs and `urls` has no
   state.
7. **The first screen is a flat list of 19 entries.** `up` and `status` sit
   beside `teardown`, `router`, `index` and `completion`; several one-line
   descriptions are paragraphs; the synopsis is jargon. Users' first guesses
   (`list`, `ls`, `ps`, `stop`, `rm`, `rename`, `open`, `logs`) are unknown
   commands.
8. **Commands that span the machine need a repository.** Outside one,
   `worktree list --all` and `index` fail with `No treerig.toml found ...`.
9. **Internals leak into normal output.** `config validate` prints the control
   home; `worktree remove` ends `registration, identity marker.` and, with a
   pin, prints the pinned binary's digest path; `shared down` names containers
   by hash; a second `down` prints Compose's `level=warning msg="Warning: No
   resource found to remove ..."`.
10. **Smaller traps.** `backup` with no directory writes into the worktree's
    home, which `worktree remove` deletes. `worktree rename` refuses while
    services run. `route add` prints `Routed` about two seconds before the route
    answers. A `[commands]` entry with `$VAR` in it passes `$VAR` literally. `up`
    with no services configured prints `is up` and no URL.

## Principles

1. **Daily verbs are short, top level, and act on the worktree you are in.**
   Nouns (`shared`, `route`, `router`, `config`, `teardown`) manage those things.
2. **One word, one meaning.** `list` and `remove` everywhere, with `ls` and
   `rm` as their short forms everywhere. One set of state words in every
   command, on the index page and in `--json`.
3. **A worktree is named the way treerig printed it**: UUID, host, path or slug.
4. **Quiet by default.** treerig says what happened in its own words. Compose's
   transcript, paths under the control home, IDs, digests and labels appear
   with `-v`, or when they are the evidence for a failure.
5. **A failure says what broke, shows the evidence, and gives the next step.**
   The evidence is the failing thing's own output, never the command treerig ran.
6. **Never show a URL that treerig knows does not answer.**
7. **stdout is the result, stderr is the conversation** (see
   [Streams](#streams)). `--json` is the machine contract.
8. **Contributor and recovery commands stay off the first screen**, and the
   errors that need them name them.

## The command set

| Command | What it does | Replaces | Answers |
|---|---|---|---|
| `init` | Write a starter `treerig.toml`, validate it, say what to do next | `init` | 10 |
| `up [service...] [-f]` | Start this worktree and print its URLs; `-f` then follows its logs | `up`, `dev` | 1, 3 |
| `status [worktree] [--json]` | Services, their state and URLs, and the router's state | `status`, `urls` | 3, 6 |
| `logs [service...] [-f]` | Show service logs; `-f` follows | new | 1 |
| `open [service]` | Open a service's URL in the browser | new | 7 |
| `down [worktree]` | Stop a worktree; data is kept | `down` | 9 |
| `list` (`ls`) `[--all] [--json]` | Worktrees with their state and URL; every repository's outside one | `worktree list`, `index` (text) | 6, 8 |
| `remove` (`rm`) `[worktree]` | Remove a worktree's containers, volumes and data, and its checkout | `worktree remove` | 4, 5 |
| `remove --gone [--all]` | Remove the worktrees whose checkout Git says is gone | new | 5 |
| `rename <new-slug>` | Change this worktree's slug and host, and restart it if it runs | `worktree rename` | 10 |
| `run <name> [-- args]` | Run a command declared in `treerig.toml` | `run` | |
| `backup [dir]`, `restore <dir>` | Run the backup or restore hook | same | 10 |
| `route add\|list\|remove` | Route a process on your machine through a worktree URL | `route add\|ls\|rm` | 2 |
| `doctor [--json]` | Does this machine and this worktree work, and if not, why | `doctor`, `router status` | 1, 2 |
| `shared up\|down\|status` | Shared services | same | |
| `router down` | Stop the shared router, to free its port | `router down` | |
| `unlock [worktree] [--router\|--registry\|--shared]` | Clear a lock left by a crashed command | `worktree unlock` | |
| `config validate\|show\|schema` | Check the file without Docker; print it; print its JSON Schema | same | 9 |
| `teardown pin\|status` | Pin a verified build for removal; show what removal will run | same | |
| `completion <shell>` | A shell completion script | same | |

`-v` (`--verbose`) is accepted by every command (see [Verbose output](#verbose-output)).
`--json` is accepted by `status`, `list`, `doctor`, `shared status`, `route
list` and `teardown status`, as today plus `list --all`.

### Removed, and where each job goes

- **`dev`** becomes `up -f` (see [`up -f`](#up--f)).
- **`urls`** goes into `status`, whose stdout is the service table with URLs;
  scripts read `status --json`.
- **`index`** goes into `list --all` (read-only text and JSON, including the
  shared services' web UIs and the Docker error fields `index --json` carries
  today). The page at `http://treerig.localhost` stays; `up`, `down` and
  `remove` keep refreshing it, `doctor` reports a stale page, and `list --all
  --refresh` rebuilds it.
- **`worktree list|remove|rename|unlock`** become `list`, `remove`, `rename` and
  `unlock`. `unlock` is not a worktree verb: it also clears the registry's, the
  router's and a repository's shared-services locks.
- **`router up`** goes into `up`, which already starts the router. **`router
  status`** goes into `doctor` and `status`.
- **`--domain`** (on `remove` and `unlock`) goes: the host names the domain.
- **`--label-namespace` on `remove`** goes: removal reads the namespace from the
  registration, then the index card, then this repository's `treerig.toml`, and
  never from the resources it is about to delete. If none has it, the error
  says to run `treerig remove` from a checkout of the repository that created
  the worktree. `shared down` keeps the flag, hidden, until the shared stack's
  state records its namespace (a [state.md](state.md) format change).

For one release, each removed command is a hidden stub that names its
replacement (`'treerig urls' is now 'treerig status'.`) and exits with the
usage-error code. The stubs go in the next release.

`ps`, `stop` and `start` are not aliases (each would be a second word for one
meaning); an unknown command suggests the right one: `Unknown command 'stop'.
Did you mean 'down'?`

## The first screen

```
Give every Git worktree its own services, data and URL.

Usage: treerig <command> [flags]

Start here
  init        Write treerig.toml for this repository
  up          Start this worktree and print its URLs
  open        Open a service in your browser

Day to day
  status      What is running here, and its URLs
  logs        Show service logs (-f to follow)
  down        Stop this worktree; data is kept
  list        List worktrees, with their state and URLs
  remove      Remove a worktree: its services, data and checkout
  rename      Change this worktree's name and URL

Project tasks
  run         Run a command from treerig.toml
  backup      Back up this worktree's data
  restore     Restore a backup into this worktree
  route       Route a process on your machine through this worktree's URL

Problems
  doctor      Check that your machine and this worktree work

Flags:
  -h, --help       Show help
  -v, --verbose    Show what treerig runs, and every detail
  -V, --version    Print the version

Run 'treerig help --all' for shared services, the router, locks, configuration,
the teardown pin and completion. Docs: https://github.com/sofired/treerig/tree/main/docs
```

`help --all` adds:

```
More
  shared      Run services once per repository and share them across worktrees
  router      Stop the shared router, to free its port
  unlock      Clear a lock left by a crashed command
  config      Check the configuration, print it, or print its JSON Schema
  teardown    Pin the build that removes worktrees, and show what removal runs
  completion  Print a shell completion script
```

The docs URL comes from `internal/brand`. `internal/cli/help.go` renders help
by hand and ignores cobra's groups, so it learns groups (`cmd.Groups()` and each
command's `GroupID`) and `--all`. `--config` stops being a root persistent flag
and is added to each command that reads `treerig.toml`, so help, completion and
`init`'s refusal all follow from where it is declared. `-V` is on the root only.

## Naming a worktree

Wherever a worktree is an argument (`status`, `down`, `remove`, `unlock`), it
is matched in this order, and the first match wins:

1. **A UUID** (the 36-character form). Always unique; every recovery command
   treerig prints uses it.
2. **A host**: a registered host, matched exactly.
3. **A path**: an argument that starts with `/`, `./`, `../` or `~`, or that
   names an existing directory. It resolves (after `EvalSymlinks`) to the
   worktree whose recorded root is its longest prefix, so a checkout nested in
   another resolves to the inner one. A path that names a directory inside a
   checkout, not its root, is accepted only by read-only verbs (`status`); a
   destructive verb asks for the root or another form. A path that also
   matches a host or slug is refused as ambiguous, listing both.
4. **A slug**: this repository's first. Outside a repository, or for a slug
   this repository doesn't have, it must be unique on the machine; otherwise
   the error lists the matching hosts and UUIDs.

No argument means the worktree you are in. The resolver lives in
`internal/registry`, beside `FindGlobal`, and completion uses the same one.

`remove`'s confirmation phrase is the slug when the slug names one worktree on
the machine, and the host otherwise. The prompt always shows the repository
and the checkout path.

## State words

One function in `internal/worktree` derives every state, from the registration,
Git's list of worktrees, and one `docker ps -a` per label namespace (the
inventory `internal/checkoutindex` runs today). `status`, `list`, `doctor`, the
index page and every `--json` document use it, and nothing derives state any
other way.

A worktree is in exactly one of these:

| Text | JSON | Meaning |
|---|---|---|
| `running` | `running` | Every service is running (and healthy, where it has a health check) |
| `partly running` | `partly_running` | Some services run; at least one has exited, is restarting or is unhealthy |
| `starting` | `starting` | Containers are being created, or a health check hasn't passed yet |
| `stopped` | `stopped` | No service is running; data is kept |
| `not started` | `not_started` | Registered, but no `up` has finished |
| `removing` | `removing` | A removal was interrupted; `treerig remove` finishes it |
| `checkout missing` | `checkout_missing` | Git, asked successfully, no longer lists its checkout |
| `unknown` | `unknown` | Docker or Git didn't answer, so the state can't be told |

A service is `running`, `starting`, `unhealthy`, `restarting`, `exited`
(JSON adds `exitCode`), `stopped`, `not created` or `unknown`. A host route is
`listening` or `not listening`, from a TCP connect to its port on `127.0.0.1`
with a short budget.

- `removing` needs a marker written when removal starts. That is a stored-state
  change, made as [state.md](state.md) says.
- `checkout missing` needs positive evidence: Git ran against a reachable
  repository and lists the entry as prunable and not locked. A failed `git`, a
  missing main checkout or an unmounted volume gives `unknown`, never
  `checkout missing`.
- Every text word maps to one JSON value, pinned by one test.
- The index page shows when it was generated, since it is refreshed only by
  `up`, `down` and `remove`.

`ready`, `pending`, `absent`, `Partial` and `[Running]` go. `docs/commands.md`
already says none of the three vocabularies was frozen before 1.0.

## Streams

- **stdout** carries only the result: the table of `status` and `list`, the URL
  table and the `is running` line of `up`, `doctor`'s verdict and problems, the
  path `backup` wrote, every `--json` document.
- **stderr** carries everything else: progress, prompts, notices, hints,
  footers, the router notice under `status`, the `verify` hook's output.
- Errors and warnings keep their prefix (`[treerig] ERROR:`, `[treerig]
  WARNING:`), and evidence and next steps follow as indented lines. Plain
  progress and notices lose the `[treerig]` prefix (see [Owner
  decisions](#owner-decisions)).
- Commands in messages are in single quotes (`run 'treerig up' again`), and
  messages are ASCII: separators are commas, not middle dots.

## Rendering

A terminal-UI evaluation built the same `up` progress view, failure summary,
removal prompt and a dashboard twice: with Bubble Tea v2, bubbles, lipgloss and
huh, and with the standard library and `golang.org/x/sys`. The Charm build
added 2.6 MB to a 2.5 MB binary (+102%) and about 23 modules to treerig's 18;
off a terminal both needed the same hand-written line output; and huh's
accessible mode emitted colour under `NO_COLOR=1` and `TERM=dumb`. treerig 1.0
uses an in-repo renderer.

- **One decision, at the entry point.** `cmd/treerig` decides the mode from
  whether stderr is a terminal, `TERM`, `NO_COLOR`, `FORCE_COLOR`, `CI` and
  `--progress`, and passes it in. Library code never reads the environment or
  probes the terminal. The colour rule is the existing `colourFor`.
- **Live mode**: stderr is a terminal, `TERM` isn't `dumb`, `CI` is unset and
  `--progress` isn't `plain`. Per-service lines are redrawn in place; the cursor
  is hidden and always restored, on exit, Ctrl-C and SIGTERM.
- **Plain mode**, everywhere else and with `--progress=plain`: one line per
  change (`worker: exited (code 3)`), plus a heartbeat (`still starting web,
  40s`) every 10 seconds, so CI and agents never see a silent minute. Every
  state is carried by words, never by colour alone. Agents that run treerig on
  a pseudo-terminal pass `--progress=plain`.
- **Cooked mode only.** Nothing puts the terminal in raw mode, so Ctrl-C stays a
  SIGINT to the foreground process group and the signal handling and exit
  codes in [output-and-exit-codes.md](../output-and-exit-codes.md) hold.
- **The live view is torn down** before a hook runs and before `up -f` and
  `logs -f` stream, and their output passes through untouched.
- **A small `Progress` interface** where `commands` uses it, with the plain
  writer as the default and in every in-process test, so the goldens stay
  byte-exact; the live writer is tested through `internal/run/ptytest`.
  `logger.Logger` is not widened. Untrusted text (service names, container
  output, branch names) goes through `text.Printable` before either writer.
- **The data source** for per-service progress is a poll of `docker compose ps
  --all --format json` while `up` runs, which works on every Compose treerig
  supports (2.21 and newer). `--progress json` from Compose is faster but its
  schema is unverified across versions; it can replace the poll later.
- **A full-screen dashboard** (`treerig ui`) is out of scope for 1.0. If one is
  added it is an optional command, it may use Bubble Tea, and it changes no
  other command's output.

## Verbose output

`-v` is a persistent flag on `Env`, passed to the backend, hooks, progress and
logger. It adds:

- Compose's transcript, as today's default prints it;
- each Docker and Compose command treerig runs, with treerig's own arguments
  only;
- paths under the control home and the treerig root, IDs and digests;
- for `doctor`, every check (today's full report).

It never prints the environment, arguments after `--`, hook or `[commands]`
arguments, or `docker compose config` output, and it redacts `KEY=value` pairs
and URLs with userinfo. `TREERIG_DEBUG` stays, meaning only stack traces on
errors.

## Command by command

### `up`

```
$ treerig up
Starting feature-x.myapp.localhost
  web     running
  worker  running
  mail    running (shared)

feature-x.myapp.localhost is running.
  web     http://feature-x.myapp.localhost
  mail    http://mail.shared.myapp.localhost   (shared)
```

The lines before the blank one are stderr; the rest is stdout. Before printing
a URL, `up` waits for the router to serve the route (one helper, shared with
`route add`): a reply other than the router's own 404, within 10 seconds,
after the locks are released. A URL that isn't served yet is printed as
`http://... (not answering yet)`. A worktree with no routed service prints `No
service is routed: add a [service.<name>] table to treerig.toml.` on stderr
and no table.

A failed `up`:

```
$ treerig up
Starting crash.myapp.localhost
  web     running
  worker  exited (code 3)

[treerig] ERROR: crash.myapp.localhost did not start: worker exited with code 3.
  Last output from worker:
    connecting to db
    FATAL db refused
  web is running: http://crash.myapp.localhost
  Fix worker, then run 'treerig up' again.
  Full log: treerig logs worker
```

Each kind of failure has its own evidence:

| Failure | Evidence |
|---|---|
| A service exits | its exit code and last 10 lines of output |
| A service is unhealthy, or still starting at Compose's wait timeout | its health check's last result and last 10 lines |
| A service keeps restarting | its restart count and last 10 lines |
| A dependency failed | the dependency's own evidence, as above |
| An image fails to build or pull | the last 20 lines of the build or pull output |
| A port is taken | the port and the service |
| A hook fails | the hook's output, already on stderr; its exit code is passed through (124 on timeout) |
| Docker doesn't answer | `Docker isn't answering. Start Docker, then run 'treerig up' again.` |

Evidence lines pass through `text.Printable`, are cut at 200 characters, go to
stderr only and never into `--json`. Collecting them (`ps --all` for exit
codes, `logs --no-log-prefix --tail N` per failed service) has a 10-second
budget in all; past it, `up` says `Service logs unavailable` and still reports
the original failure. Logs can contain secrets, and treerig can't redact them;
[output-and-exit-codes.md](../output-and-exit-codes.md) says so. `up` exits 1,
or with the hook's own code. `-v` adds Compose's transcript and the command.

### `up -f`

`up -f` (`--follow`) is `dev`: `up`, then follow the services' logs until
Ctrl-C. It is the same code path as `up`, and keeps `dev`'s lock model: the
operation lock during `up`'s work and the development-session lock for the whole
session, so `remove` and `rename` refuse while someone is attached and a second
`up -f` refuses ([locks.md](locks.md#the-development-session-dev-5)). A plain
`up` during a session still works. If `up` fails, nothing is followed. Ending
the session exits 130, 143 or 129 for SIGINT, SIGTERM or SIGHUP as `dev` does,
and a closed stdout (`up -f | head`) releases the lock too. The notice is
reworded: `Following the logs of feature-x. Press Ctrl-C to stop following;
the services keep running.`

`logs -f` takes no lock: it is a viewer, and it ends when the project has no
containers left.

### `status`

```
$ treerig status
feature-x.myapp.localhost   partly running
  SERVICE           STATE            URL
  web               running          http://feature-x.myapp.localhost
  worker            exited (code 3)  -
  app (host route)  listening        http://app.feature-x.myapp.localhost
  mail (shared)     running          http://mail.shared.myapp.localhost
```

`status` probes nothing over HTTP. A URL is shown when its service is running
and the router's container is up; with the router stopped the URL column is
`-` and stderr says `The router is stopped, so no URL loads. Run 'treerig up'
to start it.` `status --json` always carries each service's `url`, `state` and
`reachable`, so scripts that read `urls` can read it instead. `status` exits 0
whatever the state, since it reports and `doctor` is the check that fails, and
1 when Docker doesn't answer and the state is `unknown`. In a worktree that
isn't registered it prints `feature-x is not registered. Run 'treerig up' to
create its environment.` on stderr and exits 0.

### `list`

```
$ treerig list
   WORKTREE   STATE             URL                                  CHECKOUT
*  main       running           http://main.myapp.localhost          /home/sam/src/myapp
   feature-x  stopped           -                                    /home/sam/src/myapp.feature-x
   old-spike  checkout missing  -                                    /home/sam/src/myapp.old-spike
```

On stderr, only when stderr is a terminal:

```
1 checkout is gone: 'treerig remove --gone' lists what it would remove and asks.
Every repository: treerig list --all, or http://treerig.localhost
```

- The URL column shows the first routed service, with `+2 more` when there are
  others; `list --json` carries all of them.
- Paths are absolute.
- Outside a repository, or with `--all`, a REPOSITORY column comes first and
  every registered worktree on the machine is listed. Neither needs a
  `treerig.toml`: they use `ContextWithoutConfig`.
- The cost is one `docker ps -a` per label namespace and one `git worktree
  list` per repository, each with a timeout; a repository that doesn't answer
  shows `unknown`.
- The index address in the footer is the real one (with the router's port when
  it isn't 80), and the line is left out when the router isn't running.
- `list` is read-only: it takes no router lock and writes nothing.

### `doctor`

Healthy, by default, on stdout:

```
$ treerig doctor
Everything works.
  Docker 29.7, Compose 5.5, router running on 127.0.0.1:80
  feature-x.myapp.localhost: 3 of 3 services running, verify hook passed
```

With problems:

```
$ treerig doctor
2 problems.

  worker is not running: it exited with code 3.
    See why: treerig logs worker

  The router is not running, so no worktree URL loads.
    Port 80 on 127.0.0.1 is not available.
    Free it, or set [router] port in /home/sam/.treerig/settings.toml.

Everything else works. Every check: treerig doctor -v
```

- **New runtime checks**, for this worktree and the shared services it uses:
  each service is running and healthy; each routed URL answers through the
  router; the router runs when any worktree on the machine runs. Machine-wide
  checks stay cheap; runtime checks don't reach other worktrees.
- **What fails.** A service that exited with a non-zero code, is unhealthy or
  keeps restarting; a worktree that is `partly running`; the router stopped
  while a worktree runs; a URL of a running service that doesn't answer. A
  worktree stopped with `down` is fine, and so is a stopped router when no
  worktree runs.
- **Causes before symptoms.** Checks declare what they depend on. When Docker
  doesn't answer, every check that needs it is `skip` ("not checked: Docker
  isn't answering"), and the first Docker timeout short-circuits the rest. In
  text, a symptom is listed under its cause; in `--json` every check stays,
  with a new `causedBy` field and a `fix` field. Check IDs don't change.
- **Locks.** A runtime check on a worktree whose operation lock is held (an
  `up` in progress) is `skip`.
- **Pin.** A pin that fails or warns (`registry.teardown-pin`) is a problem in
  the default output; a development build with no pin is a warning naming
  `treerig teardown pin --latest`.
- **Time.** The runtime section has one deadline and runs its probes
  concurrently; a slow Docker is a warning, not a hang.
- **Exit.** 1 when there is a problem; the `verify` hook's own code when the
  hook ran and failed (124 on timeout); otherwise 0. The hook's output stays on
  stderr. OS-specific hints (`ss`, `lsof`) appear with `-v` only.
- **The index page** is reported stale when it is older than the registry.

[doctor.md](../doctor.md) is rewritten around this.

### `remove`

```
$ treerig remove feature-x
Remove feature-x (feature-x.myapp.localhost) in myapp?
  This deletes 2 containers, 1 network, 2 volumes and its data,
  and its checkout at /home/sam/src/myapp.feature-x, including 214 ignored
  files (.env, node_modules/, db.sqlite3, ...). The branch feature-x is kept.
Type feature-x to confirm: feature-x
Removed feature-x (feature-x.myapp.localhost): 2 containers, 1 network, 2 volumes, data, checkout.
```

**Before the prompt**, under the operation lock, treerig inventories what it
will delete and checks the checkout with `git status --porcelain --ignored` and
`git worktree list --porcelain`. The checkout is kept, and the prompt says so
and why, when it has modified or untracked files, is on a detached HEAD whose
commits no branch holds, is locked (`git worktree lock`), has submodules, or is
the repository's main worktree. Ignored files don't keep it, but the prompt
names them, since they are deleted with it. `--keep-checkout` keeps it anyway.
The decision is made before anything is deleted, and the inventory is checked
again after confirmation.

**The pinned teardown** ([teardown.md](teardown.md)) does what it does today:
containers, networks, volumes, data and registration, proven by ownership
labels. The gate no longer passes the caller's argv. The invoking process
resolves the worktree, confirms, and then runs the pinned binary with a stable
internal form: a hidden `treerig teardown run <uuid>` for pins of a release that
has it, and `worktree remove <uuid> --yes` for older ones (0.1.x). `teardown
run` never changes shape, so a pin keeps working across releases. A pin too old
to remove the way it was asked says so, instead of failing as an unknown
command.

**The Git step** runs in the invoking process after the pinned teardown exits
0, never inside it, so the pin's contract stays "what treerig created". It runs
`git -C <main worktree> -c core.fsmonitor=false -c core.hooksPath=/dev/null
worktree remove <checkout>`, without `--force`, with `GIT_DIR`, `GIT_WORK_TREE`,
`GIT_COMMON_DIR` and `GIT_INDEX_FILE` removed from its environment, and never
from inside the checkout. The preflight `git status` uses the same options. The
target's checkout path comes from Git's own list for its repository, not from
the index card. [teardown.md](teardown.md) gains this scope before it is built.

- **Exit.** 0 when everything in the plan was done, including a checkout the
  plan said it would keep. 1 when the teardown fails, or when Git refuses
  something the preflight didn't foresee; then: `[treerig] ERROR: Removed
  feature-x, but Git kept its checkout: <Git's reason>.` and the command to
  finish it.
- **A crash between teardown and the Git step** leaves a checkout with no
  record. That is safe, and `git worktree remove` finishes it.
- **Without a terminal and without `--yes`**: `[treerig] ERROR: Cannot ask for
  confirmation without a terminal. Pass --yes to remove feature-x.` With
  `--yes`, the plan is still printed to stderr, as a record.
- **With a pin**, one line says so: `Removing with the pinned treerig 0.1.0.`
  The digest path and the second pinned-binary line go to `-v`.
- **The worktree you are in**: the remaining steps run from the main worktree,
  and the last line is `You were in it. Run: cd /home/sam/src/myapp`.
- **One worktree per call.** `remove` takes one argument.

### `remove --gone`

`remove --gone` removes every worktree of this repository that Git lists as
prunable and not locked, in a repository Git could read. Each goes through the
same proof, teardown and pin as `remove <worktree>`, one at a time, holding one
lock at a time. It lists them with their volumes and data size, asks once
(type the count), continues past a failure, and exits 1 if any failed. `--all`
does the same for every repository on the machine and never accepts `--yes`.
A repository Git couldn't read is listed as skipped.

### Everything else

- **`down [worktree]`** with nothing to stop: `feature-x is not running.
  Nothing to stop.` (or `is not registered`), exit 0. It still runs `compose
  down` while any container or network remains, and drops only Compose's
  `No resource found to remove` warning, matched exactly.
- **`rename <new-slug>`** runs under the operation and development locks as
  today; if the services run, it then runs `up`, so Compose recreates the
  containers whose environment changed. The Compose project name and volumes
  don't change. It prints the plan first, refuses during an `up -f` session,
  and `--no-restart` leaves the services stopped. If the `up` fails, the rename
  stands and the message says to run `treerig up`. This is written into
  [locks.md](locks.md) before it is built. It prints the new URL only when the
  worktree runs and the router does: `feature-x is now login:
  http://login.myapp.localhost`.
- **`route add`** waits for the route as `up` does. On timeout it keeps the
  route, warns, and exits 0.
- **`open [service]`** opens the first routed service, or the named one, but
  only a URL treerig built for a route the router serves, with scheme `http`, a
  host ending in `.localhost` or a loopback address, and the router's port. The
  opener (`xdg-open` or `open`) gets the URL as an argument, never through a
  shell. With no opener (SSH, CI, an agent) it prints the URL on stdout and
  exits 0. `open` reads `BROWSER` and `DISPLAY` only through the entry point.
- **`backup [dir]`** with no directory writes `<treerig root>/backups/<repo>/<slug>/<timestamp>/`,
  created with mode 0700 and refusing an existing path, outside every checkout
  and kept by `remove`. It prints the path on stdout. An explicit directory
  inside the worktree's home still warns that `remove` deletes it.
- **`unlock`** with no argument clears this worktree's lock as `worktree unlock`
  does; `--router`, `--registry` and `--shared` name the machine's locks, and
  `unlock --list` lists the held ones. The checks before clearing are unchanged
  ([lock-recovery.md](../lock-recovery.md)).
- **`router down`**: `Router stopped. 2 worktrees keep running, but their URLs
  don't load until you run 'treerig up'.` No Compose lines.
- **`config validate`** stays, for CI and pre-commit hooks: it needs no Docker.
  It stops printing the control home. It and `doctor` warn about a
  `[commands]` entry with `$` followed by a name or `{`: `commands.<name>: runs
  without a shell, so $VAR is passed as written; put it in a script.`
- **`init`** validates what it wrote and says what to do next.
- **`shared`** messages name services (`mail`), never containers.
- **The missing-config error** is one sentence: `No treerig.toml in this
  directory or above it. Run 'treerig init' in your repository, or pass
  --config <path>.`
- **`--dev-teardown`** stays a flag on `remove`, hidden from help. It is never
  a setting or an environment variable (`internal/style` checks), and the
  refusal on a development build names it.
- **`teardown`** is listed by `help --all` on every build.
- **Help** gets examples for `route add`, `remove`, `run`, `backup`, `restore`,
  `list`, `down` and `shared up`. Long help shrinks to a paragraph and a link.

## Exit codes

| Command | 0 | 1 | Other |
|---|---|---|---|
| `up` | running | did not start | a hook's own code; 130/143/129 ending `up -f` |
| `status` | any state | Docker didn't answer | |
| `down` | stopped, or nothing to stop | failed | |
| `remove` | the plan was done | teardown failed, or Git refused unexpectedly | |
| `remove --gone` | every one removed | any failed | |
| `doctor` | no problem | a problem | the `verify` hook's own code |
| `route add` | routed (even if not answering yet) | not routed | |
| removed command stubs | | | the usage-error code |

[output-and-exit-codes.md](../output-and-exit-codes.md) gains this table.

## What this design does not change

`treerig.toml` keys, the hook protocol and its exit codes, `run`'s exit codes,
the meaning of `--config`, storage names, the ownership rule, the lock rules,
and `--json` key names except the state values and the added fields.

## What changes for existing users

For the release notes. Before 1.0 nothing is frozen, so these are breaking
changes without a deprecation period beyond the one-release stubs.

- Removed: `dev` (`up -f`), `urls` (`status`), `index` (`list --all`),
  `worktree list|remove|rename|unlock` (`list`, `remove`, `rename`, `unlock`),
  `router up`, `router status`.
- Renamed: `route ls|rm` to `route list|remove`, with `ls` and `rm` as short
  forms.
- Removed flags: `--domain`, and `--label-namespace` on `remove`.
- New: `-v`, `--progress`, `up -f`, `logs`, `open`, `list --all`, `remove
  --gone`, `--keep-checkout`, `rename --no-restart`, `unlock --list`.
- `remove` also removes the checkout, as the plan in its prompt says.
- `backup` without a directory writes under the treerig root.
- State words change in text and in JSON.
- The default text of `up`, `status`, `list`, `doctor`, `remove`, `down`,
  `router down` and the errors changes; plain notices lose `[treerig]`.
- Exit codes: `down` with nothing to stop exits 0; `doctor` fails on a crashed
  service, a stopped router with worktrees running, and a URL that doesn't
  answer; `status` exits 1 when Docker doesn't answer.

## Owner decisions

Recommendations; the owner confirms or changes each.

1. **`remove` removes the checkout**, after a preflight shown in the prompt,
   keeping dirty, detached, locked and main checkouts and the branch.
   Recommended: yes.
2. **`[commands]` stay shell-free**, with a warning for `$VAR`, not an error.
   Recommended: yes.
3. **`dev` becomes `up -f`.** Recommended: yes.
4. **An in-repo renderer, not Bubble Tea**, for 1.0. Recommended: yes.
5. **Plain notices lose the `[treerig]` prefix**; errors and warnings keep it.
   It touches nearly every output fixture, in one mechanical pull request.
   Recommended: yes.
6. **`doctor` fails on runtime problems** (a crashed service, a stopped router
   while worktrees run, a dead URL), not on a deliberate `down`.
   Recommended: yes.
7. **State values change in `--json`.** Recommended: yes.
8. **`backup` defaults under the treerig root.** Recommended: yes.

## Order of work

One concern per pull request. Each pull request changes the docs, goldens,
integration tests, completion goldens and `.github/scripts/release-smoke.sh`
that its behaviour touches, and lists the deleted and rewritten fixtures in its
description. `release-smoke.sh` picks its expectations from the release under
test, because it runs from `main` against older tags, and a release-smoke leg
runs a new build with the previous release pinned.

1. **This design**, merged once the owner decides.
2. **Prefix** (decision 5): plain notices lose `[treerig]`. Mechanical.
3. **Help mechanism**: groups, `help --all`, `--config` per command, `-v` (a
   no-op at first), `--progress`, hidden flags. Wording unchanged.
4. **Config-free context** for `list`, `unlock`, `remove <worktree>` and
   completion.
5. **Name resolver** in `internal/registry`, used by completion.
6. **State function** in `internal/worktree`, the `removing` marker, and the
   new words in `status`, `list`, the index page and JSON.
7. **`status`** with URLs and the router's state; `urls` becomes a stub.
8. **`list`** at top level, outside repositories, `--all`; `worktree list` and
   `index` become stubs.
9. **Quiet output and `-v`**: the backend's output mode, redaction, `down`,
   `shared` and `router down` wording, the `Progress` interface with the plain
   writer.
10. **`up` failure summary**: exit codes from `ps`, `logs --tail`, the evidence
    budget, the failure table.
11. **`logs`**, then **`up -f`** with `dev` as a stub, and the locks.md update.
12. **Live progress writer.**
13. **Route wait** for `up` and `route add`.
14. **Teardown gate contract**: `teardown run`, the argv by pin version, the
    teardown.md update, the release-smoke leg.
15. **`remove`** at top level, its prompt and preflight, then the Git step,
    then `--gone`.
16. **`rename`** restarting, with locks.md first.
17. **`unlock`** at top level with `--list`.
18. **`doctor`**: `causedBy` and `fix`, then the runtime checks, then the new
    default, then the exit rule.
19. **The rest**: `init` validation, `open`, `backup`'s default, the `$`
    warning, `--domain` and `--label-namespace` removal.
20. **First-screen wording** and one-line descriptions, once the set is final.
21. **Stub removal**, a release later.

Docs that quote changed commands or output change in the pull request that
changes them: the README; getting-started, commands, worktrees, doctor,
output-and-exit-codes, json-output, troubleshooting, host-routes,
checkout-index, teardown-pin, concepts, glossary, install, hooks, routing,
shared-services, platforms, compose, settings, lock-recovery, trust-model,
configuration and upgrading-from-a-pre-rename-build under `docs/`;
`treerig.example.toml`, CONTRIBUTING.md and AGENTS.md. Design notes (locks,
routing, teardown) get a one-line note that the command names they use were
replaced in 1.0, besides the changes above. A final grep for every removed name
comes back empty outside design notes and release notes.
