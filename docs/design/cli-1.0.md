# The command line for 1.0

**Status.** Proposed. Nothing here is built.

**Basis.** Two usability reviews of v0.1.0, by a CLI interface designer and a
usability specialist, each run against a live stack on a release build in
October 2026, and a terminal-UI evaluation with working prototypes. Their
reports are not in the repository; [What the reviews
found](#what-the-reviews-found)
keeps what they observed.

Nothing in the command line is frozen before 1.0, so this design renames and
removes commands and changes output and exit codes where that gives a better
experience. [What changes for existing users](#what-changes-for-existing-users)
lists every break for the release notes.

**Reading guide.** The owner: the principles, the command set and [Owner
decisions](#owner-decisions). Implementers: everything, then [Order of
work](#order-of-work).

**Terms.** [Words](#words) sets the words users see: *worktree*, *directory*
and *environment*. This note uses them the same way; *slug* is a worktree's
short name (`feature-x`) and *host* its hostname (`feature-x.myapp.localhost`).
Other terms are as in the [glossary](../glossary.md). `trig` is the same
program as `treerig`; everything here applies to it. treerig runs on Linux and
macOS only, so TTY and signal handling are Unix-only.

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
5. **Removing a worktree is two steps and says so nowhere.** The directory stays
   after `worktree remove`. Without a terminal it fails with `Interactive
   confirmation required: "<host>".` and doesn't mention `--yes`.
6. **Three vocabularies for "is it running".** `worktree list` says `ready` for
   a worktree that is down and `pending` for one whose first `up` failed;
   `status` says `running`, `stopped` or `absent`; the index says `[Running]` or
   `[Stopped]`. `status` has no URLs and `urls` has no state.
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
   `rm` as their short forms everywhere, because those are what users typed
   first. One set of state words in every command, on the index page and in
   `--json`.
3. **A worktree is named the way treerig printed it**: UUID, host, path or slug.
4. **Quiet by default.** treerig says what happened in its own words. Compose's
   transcript, paths under the control home, IDs, digests and labels appear
   with `-v`, or when they are the evidence for a failure.
5. **A failure says what broke, shows the evidence, and gives the next step.**
   The evidence is the failing thing's own output, never the command treerig ran.
6. **Never show a URL that treerig knows does not answer without saying so.**
7. **stdout is the result, stderr is the conversation** (see
   [Streams](#streams)). `--json` is the machine contract.
8. **Contributor and recovery commands stay off the first screen**, and the
   errors that need them name them.

## Words

A treerig worktree and a Git worktree are one-to-one: `git worktree add`
creates one, and treerig gives it a slug, a URL and services of its own. So the
CLI keeps Git's word, and uses three words everywhere a user sees them:

| Word | Means | Examples |
|---|---|---|
| **worktree** | The whole thing: what `list` shows and `remove` removes | `remove feature-x`, `list` |
| **directory** | The worktree's files on disk, which Git created | `directory missing`, `--keep-directory`, the DIRECTORY column |
| **environment** | What treerig adds: containers, networks, volumes, data and the URL | `down` stops the environment; `remove --keep-directory` removes the environment and keeps the directory |

*Checkout* is phased out, in user-facing text and internally: in Git it
usually means switching branches, treerig used it inconsistently (the "checkout
index", `doctor`'s "Git no longer lists its checkout"), and the usability review
found users don't need it. A word kept in code and design notes leaks back out
through the messages, docs and JSON written from them, so it goes everywhere
by 1.0. It is not contained (about 1,180 lines in 172 Go files, 223 string
literals, 31 goldens and 25 docs pages), and most of the user-visible uses are
output this design rewrites anyway, so the order is:

- From now on, new code, tests, docs and design notes don't use it, and each
  pull request in the order of work removes it from the strings, docs and
  goldens it rewrites. Two renames land with the steps that rework them: the
  `checkout.*` doctor check IDs become `worktree.*` (step 19), and
  `internal/checkoutindex` and `docs/checkout-index.md` become
  `internal/overview` and `docs/overview.md` (step 8).
- Once the redesign has landed, one pull request removes what is left
  (identifiers, comments, test names, docs), before 1.0, while no other pull
  request is in flight. It adds a test in `internal/style` that fails on the
  word anywhere in the repository, so it can't come back. Its allow-list is
  explicit and short: Git's own commands (`git checkout`, `actions/checkout`);
  design notes under `docs/design/`, which keep their history; release notes;
  the sentences in CONTRIBUTING.md and AGENTS.md that state this rule; and the
  test's own file.

No storage name or JSON key uses the word, so none of this needs a migration.
The page at
`http://treerig.localhost` is *the overview*. *Control home*, *registration*,
*identity marker*, *label namespace* and *pin* stay out of default output too
(principle 4); where an error needs one, it says what it is. The
[glossary](../glossary.md) and the user docs follow these words in the pull
requests that change each behaviour.

## The command set

| Command | What it does | Replaces | Answers |
|---|---|---|---|
| `init` | Write a starter `treerig.toml`, validate it, say what to do next | `init` | 10 |
| `up [service...] [-f]` | Start this worktree and print its URLs; `-f` then follows its logs | `up`, `dev` | 1, 3 |
| `status [worktree] [--json]` | Services, their state and URLs, and the router's state | `status`, `urls` | 3, 6 |
| `logs [service...] [-f]` | Show service logs; `-f` follows | new | 1 |
| `open [service]` | Open a service's URL in the browser | new | 7 |
| `down [worktree]` | Stop a worktree's environment; data is kept | `down` | 9 |
| `list` (`ls`) `[--all [--refresh]] [--json]` | Worktrees with their state and URL; every repository's outside one | `worktree list`, `index` (text) | 6, 8 |
| `remove` (`rm`) `[worktree] [--yes] [--keep-directory]` | Remove a worktree: its environment and its directory | `worktree remove` | 4, 5 |
| `remove --gone [--all]` | Remove the worktrees whose directory Git says is gone | new | 5 |
| `rename <new-slug>` | Change this worktree's name and URL, and restart it if it runs | `worktree rename` | 10 |
| `run <name> [-- args]` | Run a command declared in `treerig.toml` | `run` | |
| `backup [dir]`, `restore <dir>` | Run the backup or restore hook | same | 10 |
| `route add\|list\|remove` | Route a process on your machine through a worktree URL | `route add\|ls\|rm` | 10 |
| `doctor [--json]` | Does this machine and this worktree work, and if not, why | `doctor`, `router status` | 1, 2 |
| `shared up\|down\|status` | Shared services | same | |
| `router down` | Stop the shared router, to free its port | `router down` | |
| `unlock [worktree] [--router\|--registry\|--shared\|--list]` | Clear a lock left by a crashed command | `worktree unlock` | |
| `config validate\|show\|schema` | Check the file without Docker; print it; print its JSON Schema | same | 9 |
| `teardown pin\|status` | Pin a verified build for removal; show what removal will run | same | |
| `completion <shell>` | A shell completion script | same | |

`-v` (`--verbose`) is accepted by every command (see [Verbose
output](#verbose-output)).
`--json` is accepted by `status`, `list`, `doctor`, `shared status`, `route
list` and `teardown status`.

### Removed, and where each job goes

- **`dev`** becomes `up -f` (see [`up -f`](#up--f)).
- **`urls`** goes into `status`, whose stdout is the service table with URLs;
  scripts read `status --json`.
- **`index`** goes into `list --all` (text and JSON, including the shared
  services' web UIs and the Docker error fields `index --json` carries today).
  The overview page at `http://treerig.localhost` stays; `up`, `down`,
  `remove` and `rename` keep refreshing it, `doctor` reports a stale page, and
  `list --all --refresh`
  rebuilds it.
- **`worktree list|remove|rename|unlock`** become `list`, `remove`, `rename` and
  `unlock`. `unlock` is not a worktree verb: it also clears the registry's, the
  router's and a repository's shared-services locks.
- **`router up`** goes into `up`, which already starts the router. **`router
  status`** goes into `doctor` and `status`.
- **`--domain`** (on `remove` and `unlock`) goes: the host names the domain.
- **`--label-namespace` on `remove`** goes: removal reads the namespace from the
  registration, then the index card, then the target repository's
  `treerig.toml`, and never from the resources it is about to delete. The
  namespace only selects the label key; the identity labels must still match in
  full. If none has it, the error says to run `treerig remove` from inside
  the repository that created the worktree. `shared down` keeps the flag,
  hidden, until the shared stack's state records its namespace (a
  [state.md](state.md) format change).

For one release, each removed command is a hidden stub that prints its
replacement (`'treerig urls' is now 'treerig status'.`) and exits 1, as an
unknown command does. Stubs disable flag parsing, so `index --json` gets the
pointer and not a flag error. They go in the next release.

`ps`, `stop` and `start` are not aliases, since each would be a second word for
one meaning; an unknown command suggests the right one: `Unknown command
'stop'. Did you mean 'down'?`

Restarting one service and running a shell in a container stay with Docker
Compose; `status -v` prints the Compose project name to use.

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
  down        Stop this worktree's environment; data is kept
  list        List worktrees, with their state and URLs
  remove      Remove a worktree: its environment and directory
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
      --progress   'plain' prints one line per change and never redraws
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
A command that reads no `treerig.toml` refuses `--config` and says why (`'treerig
router status' reads no treerig.toml, so it does not take --config.`). Written
before the command's name, `--config` still reaches a command that takes it
(cobra skips the flag it does not know while it finds the command), but it is
refused when that command takes none.

## Naming a worktree

Wherever a worktree is an argument (`status`, `down`, `remove`, `unlock`):

The forms cannot be confused with each other, so the order does not change an
answer:

- **A UUID** (the 36-character form) names that worktree. Every recovery command
  treerig prints uses it.
- **A host** names the registered worktree with exactly that host (compared
  without regard to case).
- **A path** is an argument written as one: it is `.` or `..`, or it starts
  with `/`, `./`, `../` or `~`. It resolves (after `EvalSymlinks`) to the
  worktree whose recorded root is its longest prefix, matched whole path
  component by whole path component, so `/repo/app2/src` never matches the
  root `/repo/app`, and a directory nested in another resolves to the inner
  one. The resolver's tests include sibling roots that share a prefix. A path
  inside a directory, not its root, is accepted only by `status`; a
  destructive verb asks for the root or another form. Roots are matched by file
identity where the directory exists, so a case-insensitive filesystem's second
spelling still finds it. A worktree whose index card is oversized or does not
parse has no recorded root and is skipped, so a read-only caller's path inside
its directory can resolve to an outer worktree; a destructive verb cannot be
misled that way, since it accepts only a root. The resolver returns the
recorded root with the worktree (`Resolved.Root`), so a caller that needs the
directory (`status`, #203) does not read the card again.
- **Anything else is a slug**: this repository's first. Outside a repository,
  or for a slug this repository doesn't have, it must be unique on the machine;
  otherwise the error lists the matching hosts and UUIDs.

No argument means the worktree you are in. A bare word is never read as a
directory, so a directory called `feature-x` can't change what `treerig remove
feature-x` means. The resolver lives in `internal/registry`, and
completion uses the same one.

For a worktree of another repository, `status` and `down` read that
repository's `treerig.toml` and Compose file from its recorded root, and its
namespace from the registration. When its directory is missing they say so and
stop.

`remove`'s prompt always shows the worktree's slug and host, its repository
and its directory, so the answer is about one named worktree (see
[Confirmations](#confirmations)).

## State words

A worktree is in exactly one of these:

| Text | JSON | Meaning |
|---|---|---|
| `running` | `running` | At least one service runs, and every service that hasn't exited cleanly is running (and healthy, where it has a health check) |
| `starting` | `starting` | A container is being created, or a health check hasn't passed yet |
| `partly running` | `partly_running` | Some services run; at least one has exited with an error, is restarting or is unhealthy |
| `failed` | `failed` | No service runs, and at least one exited with an error, is restarting or is unhealthy |
| `stopped` | `stopped` | An `up` has finished before, and no service runs (after `down`, or every service exited cleanly); data is kept |
| `not started` | `not_started` | Registered, and no `up` has ever finished |
| `removing` | `removing` | A removal was interrupted; `treerig remove` finishes it |
| `directory missing` | `directory_missing` | Git, asked successfully, no longer has its directory (below) |
| `unknown` | `unknown` | Docker or Git didn't answer, so the state can't be told |

The first that applies wins: `removing`, `directory missing`, `unknown`,
`starting`, `failed`, `partly running`, `running`, `stopped`, `not started`.
Containers decide over history, so a first `up` that fails with one service
running is `partly running`, and one whose only service crashed is `failed`. A
service that exited with code 0, such as a one-shot `migrate` or `seed`, has
done its job: it counts as neither running nor failed, so `web` running beside
a finished `migrate` is `running`, and a worktree whose services all exited
cleanly is `stopped`. Every combination of service states maps to exactly one
worktree state, which the mapping test enumerates.
Whether an `up` has ever finished is what the registration records today as
`ready` against `pending`; `down` removes containers, so `stopped` can't depend
on them existing.

A service is `running`, `starting`, `unhealthy`, `restarting`, `exited`
(JSON adds `exitCode`), `stopped`, `not created` or `unknown`. A host route is
`listening` or `not listening`, from a TCP connect to its port on `127.0.0.1`
with a short budget.

- **Where it is computed.** A pure `Classify` function in `internal/worktree`
  maps plain inputs to these words. A new `internal/worktreestate` package
  gathers the inputs (the registration, `git worktree list`, and one `docker ps
  -a` per label namespace, as `internal/checkoutindex` does today) and is what
  `status`, `list`, `doctor` and the index page call. Across worktrees the
  inputs come from `docker ps`'s `State` and the health suffix of its `Status`,
  parsed against a table of Docker's wordings; for one worktree (`status`,
  `doctor`, `up`) they come from `docker compose ps --all --format json`, which
  carries `ExitCode`.
- **`removing`** needs a marker written when removal starts: a stored-state
  change, made as [state.md](state.md) says. A removal by a 0.1.x pin writes
  none, so an interrupted one shows `not started` or `stopped`.
- **`directory missing` needs positive evidence**: Git ran against a reachable
  repository and lists the entry as prunable and not locked; the directory
  itself doesn't exist (Git also reports an existing directory as prunable when
  its administrative files are gone, and that one is `unknown`, not missing, as
  is a path that can't be checked); and the directory's parent exists, is not
  empty, and is not a mount point or below one that the mount table
  (`/proc/self/mountinfo` on Linux, `getmntinfo` on macOS)
  doesn't list as mounted. An unmounted volume's mount point is an ordinary
  empty directory, so emptiness is the signal. Otherwise the state is
  `unknown`, with a hint that names the cause: `Its parent directory is
  missing or empty: is the volume mounted?` when the parent or mount check
  failed, `Git's records for it are damaged: 'git worktree repair' may fix
  them.` when the directory exists but Git calls it prunable, and `treerig
  couldn't check it: <the error>.` for a permission error or a Git failure.
  Git can't tell a directory moved without `git worktree move` from a
  deleted one; `remove --gone` shows each path so the user can run `git
  worktree repair` first.
- Every text word maps to one JSON value, pinned by one test.
- The overview page shows when it was generated, since it is refreshed only by
  `up`, `down`, `remove`, `rename` and `list --all --refresh`.

`ready`, `pending`, `absent`, `Partial` and `[Running]` go. `docs/commands.md`
already says none of the three vocabularies was frozen before 1.0.

## Streams

- **stdout** carries only the result: the table of `status` and `list`, the
  `is running` line and URL table of `up`, `doctor`'s verdict and problems, the
  path `backup` wrote, `open`'s URL when there is no browser, every `--json`
  document, the output of `run`'s own command, and the logs of `logs` and `up
  -f`: a container's stdout on stdout and its stderr on stderr, as `dev` does
  today.
- **stderr** carries everything else: progress, prompts, notices, hints,
  footers, the router notice under `status`, and the output of every hook
  (`up`'s `prepare` and `seed`, `backup`, `restore`, `verify`), so `dir=$(treerig
  backup)` gets only the path.
- Errors and warnings keep their prefix (`[treerig] ERROR:`, `[treerig]
  WARNING:`), and evidence and next steps follow as indented lines. Plain
  progress and notices lose the `[treerig]` prefix: they aren't errors, and the
  prefix made every line look like one.
- Message texts in this note are shown without the `[treerig] ERROR:` or
  `[treerig] WARNING:` prefix that every error and warning carries, except
  where a mock shows a whole run.
- Commands in messages are in single quotes (`run 'treerig up' again`), and
  messages are ASCII: separators are commas.

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
  `--progress`, and passes it in. Library code never reads environment
  variables or
  probes the terminal. The colour rule is the existing `colourFor`.
- **Live mode**: stderr is a terminal, `TERM` isn't `dumb`, `CI` is unset and
  `--progress` isn't `plain`. Per-service lines are redrawn in place; the cursor
  is hidden and always restored, on exit, Ctrl-C and SIGTERM.
- **Plain mode**, everywhere else and with `--progress=plain`: one line per
  change (`worker: exited (code 3)`), plus a heartbeat (`still starting web,
  40s`) every 10 seconds, so CI and agents never see a silent minute. Every
  state is carried by words, never by colour alone. Agents that run treerig on
  a pseudo-terminal pass `--progress=plain`.
- **Prompts** need stdin and stderr to be terminals and `CI` to be unset;
  otherwise the command says why and to pass `--yes` (`Cannot ask for
  confirmation: CI is set. Pass --yes to remove feature-x.`, or `... without a
  terminal ...`).
- **Cooked mode only.** Nothing puts the terminal in raw mode, so Ctrl-C stays a
  SIGINT to the foreground process group and the signal handling and exit
  codes in [output-and-exit-codes.md](../output-and-exit-codes.md) hold.
- **The live view is torn down** before a hook runs and before `up -f` and
  `logs -f` stream, and their output passes through untouched.
- **A small `Progress` interface** where `commands` uses it, with the plain
  writer as the default and in every in-process test, so the goldens stay
  byte-exact; the live writer is tested through `internal/run/ptytest`. The
  live writer writes to stderr directly, never through `logger.Logger`, whose
  `text.Printable` filter would turn its control sequences into spaces; the
  logger is not widened. Untrusted text (service names, container output,
  branch names) goes through `text.Printable` before either writer.
- **The data source** for per-service progress is a poll of `docker compose ps
  --all --format json` while `up` runs, which works on every Compose treerig
  supports (2.21 and newer). The poll runs at most every 2 seconds, each call
  is bounded, a failed poll keeps the last state, and the poll stops when the
  `up` child exits and sets no deadline of its own (`up` has none, #14).
  Compose's `--progress json` is faster but its schema is unverified across
  versions; it can replace the poll later.
- **A full-screen dashboard** (`treerig ui`) is out of scope for 1.0. If one is
  added it is an optional command, it may use Bubble Tea, and it changes no
  other command's output.

## Verbose output

`-v` is a persistent flag stored on `Env` and passed to `backend.Options`, the
hooks runner, progress and the logger. It adds:

- Compose's transcript, as today's default prints it;
- each Docker and Compose command treerig runs, with treerig's own arguments
  only;
- paths under the control home and the treerig root, IDs and digests;
- for `doctor`, every check (today's full report).

It never prints environment variables, arguments after `--`, hook or `[commands]`
arguments, or `docker compose config` output, and it redacts `KEY=value` pairs
and URLs with userinfo, through one tested function in `internal/text`.
`TREERIG_DEBUG` stays, meaning only stack traces on errors.

## Command by command

### `up`

```
$ treerig up
Starting feature-x.myapp.localhost
  web            running
  worker         running
  mail (shared)  running

feature-x.myapp.localhost is running.
  web            http://feature-x.myapp.localhost
  mail (shared)  http://mail.shared.myapp.localhost
```

The lines before the blank one are stderr; the rest is stdout, and its URLs are
plain. Before printing them, after the locks are released, `up` waits up to 10
seconds for the router to have loaded each route, by the rule `doctor`'s route
probe uses (a 404 counts as the router's own only when the target answers
differently when asked directly). A route that is loaded but whose service
doesn't answer yet is still printed, so stdout stays a stable table for
scripts; this is the one case principle 6 allows, and stderr adds `web isn't
answering yet: http://feature-x.myapp.localhost`. A worktree with no routed
service prints `No
service is routed: add a [service.<name>] table to treerig.toml.` on stderr and
no table.

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
  Full log: 'treerig logs worker'
```

Each kind of failure has its own evidence:

| Failure | Evidence |
|---|---|
| A service exits | its exit code and last 10 lines of output |
| A service is unhealthy, or still starting at Compose's wait timeout | its health check's last result and last 10 lines |
| A service keeps restarting | its restart count and last 10 lines |
| A dependency failed | the dependency's own evidence, as above |
| An image fails to build or pull | the last 20 lines of the build or pull output, kept in a ring buffer while `up` runs |
| A port is taken | the port and the service |
| A hook fails | the hook's output, already on stderr; its exit code is passed through (124 on timeout) |
| Docker doesn't answer | `Docker isn't answering. Start Docker, then run 'treerig up' again.` |

Evidence lines pass through `text.Printable`, are cut at 200 characters, go to
stderr only and never into `--json`. Collecting them (`ps --all` for exit codes,
`logs --no-log-prefix --tail N` per failed service) runs on a fresh context,
even after Ctrl-C, with a 10-second budget in all; past it, `up` says `Service
logs unavailable` and still reports the original failure. Logs can contain
secrets and treerig can't redact them;
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

`logs -f` takes no lock: it is a viewer. Compose's follower can keep waiting
after the containers are gone
([locks.md](locks.md#the-development-session-dev-5)),
so `logs -f` checks every 5 seconds and ends when the project has no containers
left.

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
`-` and stderr says `The router is stopped, so no URL loads. Run 'treerig up' to
start it.` `status --json` is always printed, and carries each service's `url`
and `state`, the router's state (`running`, `stopped` or `unknown`) and, when
Docker didn't answer, `dockerError`; scripts that read `urls` read it instead.
`status` exits 0 whatever the state, since it reports and `doctor` is the check
that fails, and 1 when Docker doesn't answer. In a worktree that isn't
registered it prints `feature-x is not registered. Run 'treerig up' to create
its environment.` on stderr and exits 0.

### `list`

```
$ treerig list
   WORKTREE   STATE             URL                                  DIRECTORY
*  main       running           http://main.myapp.localhost          /home/sam/src/myapp
   feature-x  stopped           -                                    /home/sam/src/myapp.feature-x
   old-spike  directory missing  -                                    /home/sam/src/myapp.old-spike
```

On stderr, only when stderr is a terminal:

```
1 directory is gone: 'treerig remove --gone' lists what it would remove and asks.
Every repository: 'treerig list --all', or http://treerig.localhost
```

- The URL column shows the first routed service, with `+2 more` when there are
  others, and `-` while the worktree or the router is stopped; `list --json`
  carries all of them.
- Paths are absolute. The footer's first line reads `2 directories are gone` for
  more than one.
- Outside a repository, or with `--all`, a REPOSITORY column comes first and
  every registered worktree on the machine is listed. "Outside a repository"
  means outside every Git work tree. Neither needs a `treerig.toml`: they use
  `ContextWithoutConfig`, which still loads settings for the router's port and
  sets a `treerig.toml` that does not parse aside with a warning. Inside a Git
  repository, with a usable `treerig.toml`, a broken one or none, `list` shows
  that repository's worktrees; when treerig knows none, it says so and the
  stderr hint names `list --all`.
- The cost is one `docker ps -a` per label namespace and one `git worktree
  list` per repository, each with a timeout; a repository that doesn't answer
  shows `unknown`.
- The index address in the footer is the real one (with the router's port when
  it isn't 80), and the line is left out when the router isn't running.
- `list` takes no router lock and writes nothing, except `--refresh`, which
  rebuilds the index page under the router lock.
- `list` exits 0, and 1 when Docker didn't answer (the table is still printed,
  with `unknown` rows), as `status` does.

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
    See why: 'treerig logs worker'

  The router is not running, so no worktree URL loads.
    Port 80 on 127.0.0.1 is not available.
    Free it, or set [router] port in /home/sam/.treerig/settings.toml.

Everything else works. Every check: 'treerig doctor -v'
```

With Docker down:

```
$ treerig doctor
1 problem.

  Docker isn't answering, so nothing that needs it was checked.
    Start Docker, then run 'treerig doctor' again.
```

- **New runtime checks**, for this worktree and the shared services it uses:
  each service is running and healthy, or has exited cleanly (code 0), which
  is a one-shot service such as `migrate` that has done its job and passes, as
  in `Classify`; each routed URL of a running service answers through the
  router; the router runs when any worktree on the machine runs. Machine-wide
  checks stay cheap; runtime checks don't reach other worktrees.
- **What fails.** A service that exited with a non-zero code, is unhealthy or
  keeps restarting; a worktree that is `partly running` or `failed`; the
  router stopped while a worktree runs; a URL of a running service that doesn't
  answer. A
  worktree stopped with `down` is fine, and so is a stopped router when no
  worktree runs.
- **Warnings** don't fail: a run with only warnings prints `Everything works,
  with 1 warning.` and lists it.
- **Causes before symptoms.** Checks declare what they depend on. When Docker
  doesn't answer, every check that needs it is `skip` ("not checked: Docker
  isn't answering"), and the first Docker timeout short-circuits the rest. In
  text, a symptom is listed under its cause; in `--json` every check stays,
  with new `causedBy` and `fix` fields. Check IDs don't change, except that
  `checkout.*` become `worktree.*` (see [Words](#words)).
- **Locks.** A runtime check on a worktree whose operation lock is held (an
  `up` in progress) is `skip`.
- **Pin.** A pin that fails or warns (`registry.teardown-pin`) is a problem in
  the default output; a development build with no pin is a warning naming
  `treerig teardown pin --latest`.
- **Time.** The runtime section has one deadline and runs its probes
  concurrently; a slow Docker is a warning, not a hang.
- **Exit.** 0 with no problem. When the `verify` hook failed and is the only
  problem, its own code (124 on timeout); otherwise 1. The hook's output stays
  on stderr. OS-specific hints (`ss`, `lsof`) appear with `-v` only.
- **The index page** is reported stale when it is older than the registry.
- **Backups** under the treerig root are listed with their size by `doctor -v`.

[doctor.md](../doctor.md) is rewritten around this.

### `remove`

```
$ treerig remove feature-x
Removing feature-x (feature-x.myapp.localhost) in myapp
  deletes 2 containers, 1 network, 2 volumes and its data,
  and its directory at /home/sam/src/myapp.feature-x, including 214 ignored
  files (.env, node_modules/, db.sqlite3, ...). The branch feature-x is kept.
  1 backup is kept, under /home/sam/.treerig/backups.
Remove feature-x.myapp.localhost? [y/N] y
Removed feature-x (feature-x.myapp.localhost): 2 containers, 1 network, 2 volumes, data, directory.
```

When the directory is kept:

```
$ treerig remove feature-x
Removing feature-x (feature-x.myapp.localhost) in myapp
  deletes 2 containers, 1 network, 2 volumes and its data.
  Its directory at /home/sam/src/myapp.feature-x is kept: it has uncommitted changes.
  The branch feature-x is kept.
Remove feature-x.myapp.localhost? [y/N] y
Removed feature-x (feature-x.myapp.localhost): 2 containers, 1 network, 2 volumes, data.
Kept /home/sam/src/myapp.feature-x. When you are done with it: 'git worktree remove /home/sam/src/myapp.feature-x'
```

Every line is stderr; `remove` writes nothing to stdout. The reason a directory
is kept is one of:

| Case | Reason | Next step printed |
|---|---|---|
| Modified or untracked files | `it has uncommitted changes` | `git worktree remove <path>`, adding `--force` if the changes can go |
| Detached HEAD with commits no branch holds | `it is on a detached HEAD with commits no branch has` | `git branch <name>` in the directory, then `git worktree remove <path>` |
| Locked with `git worktree lock` | `Git has it locked` | `git worktree unlock <path>`, then `git worktree remove <path>` |
| Submodules | `it has submodules, which Git won't remove` | `git worktree remove --force <path>` once you've checked them |
| The repository's main worktree | `it is the repository's main worktree` | none: `The repository's main worktree is never removed.` |
| The preflight failed or timed out | `Git couldn't check it` | `git worktree remove <path>` |
| `--keep-directory` | `you asked to keep it` | none |

Paths with spaces are quoted in the printed commands.

**Who does what.** The gate stays as [teardown.md](teardown.md) describes it:
the first act of `remove`, before any lock, and it `exec`s, so nothing of the
invoked build runs after it. What changes is what the pinned binary is asked to
do. It receives a stable hidden form, `treerig teardown run [<worktree>] [--yes]
[--keep-directory] [--gone [--all]] [-v]`, with the arguments as the user gave
them, and it does all of the steps below: it resolves the worktree, checks the
directory, prompts, tears down, and removes the directory. So trusted code also
decides what the prompt says and removes the directory. `teardown run` is a
permanent contract, listed beside the pin's others in
[teardown.md](teardown.md). A release build with no pin runs the same code in
its own process; a development build with no pin refuses as today, and with
`--dev-teardown` the invoked build does all of it, relocated (#7) and warning,
as today.

**`teardown run` is never a way around the gate.** Run directly, it enforces
the same gate as `remove` and proceeds only as the pinned binary, the relocated
`--dev-teardown` copy, or a release build with no pin; the same holds for
`--gone`. It applies `remove`'s rules to its own arguments too, so `teardown
run --gone --all` refuses `--yes` however it is invoked. Tests beside
`TestTeardownGate` cover both.

**Older pins.** Before the exec the gate asks the pinned binary which contract
it speaks: it runs the stored binary, through the same verified descriptor as
the exec, with `teardown run --contract`, a short timeout, an allow-listed
environment variables and capped output. Only exit 0 with stdout exactly
`treerig-teardown-run 1` means the stable form; the number versions the
contract. `--help` can't be the probe: cobra answers it before checking
arguments, so an old binary exits 0 for `teardown run --help`. A test with a
fake old pin whose `--help` exits 0 asserts the legacy path. A pin without the
stable form (0.1.x) gets `worktree remove <uuid>`, with `--yes` only if the
user passed it, so the old binary still prompts in its own code. The gate
resolves the UUID and the main worktree read-only to do this, with the same Git
options and allow-listed environment variables as the pinned steps, runs the
old binary
from the target's main worktree with `--config` set to its `treerig.toml`, and
first prints `This pin (0.1.0)
predates directory removal: the directory at /home/sam/src/myapp.feature-x is
kept. Pin this release to remove directories too: 'treerig teardown pin
--latest'`.
The gate changes to that directory before the exec, since `exec` keeps the
working directory. When that `treerig.toml` is missing, it refuses and says to
pin this release. `remove --gone` with an older pin refuses and says to pin
this release, since one exec can't loop over several worktrees.

**The steps, in the pinned binary:**

1. Resolve the worktree. With no lock held, inventory what will be deleted and
   check the directory with `git status --porcelain --ignored` and `git worktree
   list --porcelain`, with a timeout. A preflight that fails or times out keeps
   the directory. Ignored files don't keep it, but the prompt names them, since
   they are deleted with it.
2. Prompt, with no lock held, so the worktree isn't blocked while the user reads.
3. Take the operation and development locks, then inventory again and recheck
   the directory: its path, HEAD, status, and which worktree is the main one. If
   anything differs from what the prompt said, stop with nothing deleted, and
   say so.
4. Write the `removing` marker, then tear down containers, networks, volumes,
   data and registration, proven by ownership labels, as today.
5. Remove the directory, still under the locks: `git -C <main worktree> -c
   core.fsmonitor=false -c core.hooksPath=/dev/null worktree remove <directory>`,
   without `--force`. Git's environment variables come from an allow-list (`PATH`,
   `HOME`, `TMPDIR`, `LANG` and `LC_*`), so no inherited `GIT_*` variable can
   point it elsewhere; the preflight uses the same. The directory is identified
   by identity, not by path: the recorded root (registration or index card) is
   only where treerig starts asking Git, and the entry it removes is the one in
   Git's list whose identity marker carries the target's worktree UUID, as
   `worktree remove` matches today. No match means the directory is treated as
   missing and kept. The path passed to Git is the exact one the prompt showed
   and step 3 confirmed, and the command never runs from inside the directory.
   The pinned binary runs from the pin store, outside every worktree's
   directory.

- **Exit.** 0 when everything in the plan was done, including a directory the
  plan said it would keep. 1 when the teardown fails, when step 3 stops, or when
  Git refuses something the preflight didn't foresee; then `[treerig] ERROR:
  Removed feature-x, but Git kept its directory: <Git's reason>.` and the
  command to finish it.
- **A crash between steps 4 and 5** leaves a directory with no record; `git
  worktree remove` finishes it. The locks are released through the same paths
  they were taken by, though the registration is gone, and `unlock` and
  `doctor` don't treat the leftover lock files as orphans; a test crashes
  between the two steps and checks both.
- **Without a terminal and without `--yes`**: `[treerig] ERROR: Cannot ask for
  confirmation without a terminal. Pass --yes to remove feature-x.` With
  `--yes`, the plan is still printed to stderr, as a record.
- **With a pin**, one line says so: `Removing with the pinned treerig 0.1.0.`
  The digest path and the second pinned-binary line go to `-v`.
- **The worktree you are in**: the last line is `You were in it. Run: cd
  /home/sam/src/myapp`.
- **One worktree per call.** `remove` takes one argument.
- **Keeping the worktree.** `down` stops the environment and keeps its data;
  `remove --keep-directory` deletes the environment, data included, and leaves
  a plain Git worktree, so the next `up` there starts afresh (a new
  registration and empty volumes). That is today's `worktree remove`.

### `remove --gone`

`remove --gone` removes every worktree of this repository whose state is
`directory missing`. It runs through the same gate as `remove` (`teardown run
--gone`), one worktree at a time, holding one worktree's locks at a time, and
replaces two of `remove`'s steps:

- **Instead of step 1's directory check**, which needs a directory to run `git
  status` in, its preflight is the `directory missing` evidence above,
  together with the registration's worktree UUID matching the identity Git
  recorded for that entry. Without both, the worktree is skipped.
- **Step 3** rechecks that evidence under the locks, and step 4 tears down the
  environment as `remove` does.
- **Step 5 doesn't run.** There is no directory left to remove, and `--gone`
  makes no Git change. It lists them with their path,
volumes and data size, then any warnings (a skipped repository, a path that
may have moved), so they sit right above the question, and asks once with the
count and the total data in the question itself (`Remove 3 worktrees and 4.2
GB of data? [y/N]`). It continues past a failure,
and exits 1 if any failed. `--all` does the same for every repository on the
machine and never accepts `--yes`. A repository Git couldn't read, and a
worktree whose state is `unknown`, are listed as skipped.

`remove` on a single worktree, running `git worktree remove`, is the only Git
write in the CLI. Git keeps listing the gone entries as prunable, so
`--gone` ends with the next step: `Git still lists them: run 'git worktree
prune' in /home/sam/src/myapp to clear them.`

### Confirmations

Five commands ask before they act, all the same way: they print the plan, then
a yes-or-no question whose default is No.

| Command | When it asks | Question |
|---|---|---|
| `remove` | always | `Remove feature-x.myapp.localhost? [y/N]` |
| `remove --gone` | always | `Remove 3 worktrees and 4.2 GB of data? [y/N]` |
| `shared down --volumes` | with `--volumes`, which deletes the shared services' data | `Delete the data of myapp's shared services? [y/N]` |
| `teardown pin` | always | `Pin this build for worktree removal? [y/N]` |
| `restore` | always | `Replace feature-x's data with this backup? [y/N]` |

- Only `y` or `yes`, in any case, followed by Enter, goes ahead. Anything
  else, including an empty answer or end of input without a newline, prints
  `Cancelled; nothing changed.` and exits 1.
- Input typed or pasted before the question appears never answers it: unread
  terminal input is discarded (`tcflush`, through the existing TTY code)
  immediately before the question is written. A pty test queues `y` before the
  prompt and expects the command to cancel.
- `remove`'s question names the host, so the last line before the answer
  carries the unambiguous name.
- `--yes` skips the question, and the plan is still printed to stderr as a
  record. `remove --gone --all` never accepts `--yes`.
- Prompts need stdin and stderr to be terminals and `CI` to be unset (see
  [Rendering](#rendering)).
- Nothing asks you to type a name or phrase. The plan is what makes the answer
  informed, so each plan names its target in full: the worktree's slug, host,
  repository and directory for `remove`; every worktree, its path and data size
  for `--gone`; the project and its volumes for `shared down --volumes`; the
  release, source, signer and digest for `teardown pin`, which keeps today's
  `no signature was checked` line for `--file` and its comparison with the pin
  it replaces.
- **`restore`** now asks, because it replaces a worktree's data. Its plan says
  what the backup is and what it replaces:

  ```
  Restoring feature-x (feature-x.myapp.localhost) from
  /home/sam/.treerig/backups/1b0c.../20261021T140312Z
    a backup of feature-x, taken 2026-10-21 14:03
    runs the restore hook (./scripts/restore.sh), which replaces feature-x's
    current data.
  Replace feature-x's data with this backup? [y/N]
  ```

  The backup's origin comes from the manifest `backup` writes beside a default
  bundle; a directory without one says `a backup treerig has no record of`, and
  a backup of another worktree says so (`a backup of login, taken ...`). The
  manifest's text goes through `text.Printable`.

### Everything else

- **`down [worktree]`** with nothing to stop: `feature-x is not running.
  Nothing to stop.` (or `is not registered`), exit 0. It still runs `compose
  down` while any container or network remains, and drops only Compose's
  `No resource found to remove` warning, matched exactly.
- **`rename <new-slug>`** renames under the operation and development locks as
  today. If the services run, it then starts them through an `up` that runs
  under the locks it already holds (an `upLocked` that `up` also uses), so
  Compose recreates the containers whose environment variables changed and no
  other
  command can slip in between. The lock is then held as long as `up --build`
  takes. The Compose project name and volumes don't change. It prints the plan
  first, refuses during an `up -f` session, and `--no-restart` leaves the
  services stopped. If the `up` fails, the rename stands and the message says to
  run `treerig up`. This goes into [locks.md](locks.md), with the nested-lock
  test, before it is built. It prints the new URL only when the worktree runs
  and the router does: `feature-x is now login: http://login.myapp.localhost`.
- **`route add`** waits for the route as `up` does. On timeout it keeps the
  route, warns, and exits 0.
- **`open [service]`** opens the first routed service, or the named one, but
  only a URL treerig built for a route the router serves, with scheme `http`, a
  host ending in `.localhost` or a loopback address, and the router's port. The
  opener (`BROWSER`, `xdg-open` or `open`) gets the URL as an argument, never
  through a shell. With no opener (SSH, CI, an agent) it prints the URL on
  stdout and exits 0. It exits 1 when the service isn't running: `web is not
  running. Run 'treerig up'.` `open` reads `BROWSER` and `DISPLAY` only through
  the entry point.
- **`backup [dir]`** with no directory writes `<treerig root>/backups/<worktree
  UUID>/<timestamp>/`, with the repository and slug in a manifest beside the
  bundle, so a rename keeps the history. It is created with mode 0700, refuses
  an existing path, lives outside every directory, and is kept by `remove`. The
  path is a new storage location, added to `internal/layout` and
  [state.md](state.md). `backup` prints the path on stdout. An explicit
  directory inside the worktree's home still warns that `remove` deletes it.
- **`unlock`** with no argument clears this worktree's lock as `worktree unlock`
  does; `--router`, `--registry` and `--shared` name the machine's locks, and
  `--list` lists the held ones. The checks before clearing are unchanged
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
| `up` | running | did not start | a hook's own code; 130, 143 or 129 ending `up -f` |
| `status`, `list` | any state | Docker didn't answer (output still printed) | |
| `logs` | logs shown | the worktree isn't registered | 130 ending `-f` |
| `open` | opened, or URL printed | service not running | |
| `down` | stopped, or nothing to stop | failed | |
| `remove` | the plan was done | declined, teardown failed, the recheck stopped it, or Git refused unexpectedly | |
| `remove --gone` | every one removed | declined, or any failed | |
| `rename` | renamed (and restarted) | not renamed, or renamed but `up` failed | |
| `unlock` | cleared, or listed | refused by the checks | |
| `doctor` | no problem | a problem | the `verify` hook's code when it is the only problem |
| `config validate` | valid | invalid | |
| `restore` | restored | declined, or the restore hook failed | the hook's own code |
| `route add` | routed, even if not answering yet | not routed | |
| removed command stubs | | always | |

[output-and-exit-codes.md](../output-and-exit-codes.md) gains this table.

## What this design does not change

`treerig.toml` keys, the hook protocol, `run`'s exit codes, the meaning of
`--config`, storage names other than the new backups directory, the ownership
rule, the lock ordering, and `--json` key names except the state values,
the added fields and the renamed `doctor` check IDs. Lock holding changes only
where this note says so: `remove`
(prompt before the locks, recheck after) and `rename` (locks held across the
restart). `doctor` keeps the `verify` hook's exit code only when the hook is the
sole problem. `teardown run` is a new permanent contract. Restarting one
service and shells in containers
stay with Docker Compose.

## What changes for existing users

For the release notes. Before 1.0 nothing is frozen, so these are breaking
changes without a deprecation period beyond the one-release stubs.

- Removed: `dev` (`up -f`), `urls` (`status`), `index` (`list --all`),
  `worktree list|remove|rename|unlock` (`list`, `remove`, `rename`, `unlock`),
  `router up`, `router status`. Each stays for one release as a stub that names
  its replacement and exits 1.
- Renamed: `route ls|rm` to `route list|remove`, with `ls` and `rm` as short
  forms.
- Removed flags: `--domain`, and `--label-namespace` on `remove`.
- New: `-v`, `--progress`, `up -f`, `logs`, `open`, `list --all`, `list
  --refresh`, `remove --gone`, `--keep-directory`, `rename --no-restart`,
  `unlock --list`.
- `remove` also removes the directory, as the plan in its prompt says. A 0.1.x
  pin keeps it, and says so.
- `backup` without a directory writes under the treerig root.
- Confirmations are `[y/N]` questions instead of a typed name or phrase, and
  `restore` now asks, so a script that runs `restore` passes `--yes`.
- Hook output moves from stdout to stderr.
- State words change in text and in JSON.
- The default text of `up`, `status`, `list`, `doctor`, `remove`, `down`,
  `router down` and the errors changes; plain notices lose `[treerig]`.
- Exit codes: `down` with nothing to stop exits 0; `doctor` fails on a crashed
  service, a stopped router with worktrees running, and a URL that doesn't
  answer; `status` and `list` exit 1 when Docker doesn't answer.

## Owner decisions

Recommendations; the owner confirms or changes each.

1. **`remove` removes the directory**, after a preflight shown in the prompt,
   keeping dirty, detached, locked, submodule and main directories and the branch,
   all inside the pinned binary. This widens what the pin is trusted to do: it
   decides the prompt and deletes the directory, not only treerig's own
   resources, and [teardown.md](teardown.md) says so (step 14). The alternative
   is a Git step in the invoking build, which runs unpinned code on the user's
   files. Recommended: yes.
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
9. **Hook output moves to stderr.** Recommended: yes.
10. **Confirmations are `[y/N]`**, default No, for `remove`, `remove --gone`,
    `shared down --volumes`, `teardown pin` and the newly prompting `restore`;
    nothing asks to type a name or phrase. Recommended: yes.

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
6. **State**: `Classify` in `internal/worktree`, gathering in
   `internal/worktreestate`, the `removing` marker, and the new words in
   `status`, `list`, the index page and JSON.
7. **`status`** with URLs and the router's state; `urls` becomes a stub.
8. **`list`** at top level, outside repositories, `--all`, `--refresh`;
   `worktree list` and `index` become stubs; `internal/checkoutindex` becomes
   `internal/overview` and `docs/checkout-index.md` becomes `docs/overview.md`.
9. **Quiet output and `-v`**: the backend's output mode, redaction, hook output
   to stderr, `down`, `shared` and `router down` wording, the `Progress`
   interface with the plain writer.
10. **`up` failure summary**: exit codes from `ps`, `logs --tail`, the build
    ring buffer, the evidence budget, the failure table.
11. **`logs`**, then **`up -f`** with `dev` as a stub, and the locks.md update.
12. **Live progress writer.**
13. **Route wait** for `up` and `route add`.
14. **Teardown gate contract**: [teardown.md](teardown.md) first (`teardown
    run`, the probe for older pins, the steps, the Git step's scope and
    environment variables), then the code and the release-smoke leg.
15. **`remove`** at top level with `--domain` gone, its prompt and preflight,
    then the directory step, then `--gone`.
16. **`rename`** restarting, with locks.md first.
17. **`unlock`** at top level with `--list`, `--domain` gone.
18. **Confirmations**: `[y/N]` in `internal/commands/prompt.go` for every
    prompt, and `restore`'s new prompt and plan.
19. **`doctor`**: `causedBy` and `fix`, then the runtime checks, then the new
    default and the `checkout.*` check IDs renamed to `worktree.*`, then the
    exit rule.
20. **The rest**: `route list|remove`, `init` validation, `open`, `backup`'s
    default, the `$` warning, `--label-namespace` gone from `remove`.
21. **First-screen wording** and one-line descriptions, once the set is final.
22. **"Checkout" removed** from everything still using it, with the
    `internal/style` test that keeps it out (see [Words](#words)), before 1.0.
23. **Stub removal**, a release later.

Docs that quote changed commands or output change in the pull request that
changes them: the README; getting-started, commands, worktrees, doctor,
output-and-exit-codes, json-output, troubleshooting, host-routes,
overview (today's checkout-index, renamed in step 8), teardown-pin, concepts,
glossary, install, hooks, routing,
shared-services, platforms, compose, settings, lock-recovery, trust-model,
configuration and upgrading-from-a-pre-rename-build under `docs/`;
`treerig.example.toml`, CONTRIBUTING.md and AGENTS.md. Design notes change
where this design changes what they decide (locks, teardown, state), and each
note that uses a replaced command name gets a one-line note that the names were
replaced in 1.0. A final grep for every removed name comes back empty outside
design notes and release notes.
