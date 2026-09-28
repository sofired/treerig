# berth

Give every Git worktree a durable identity, an isolated home, and a routed URL.

Check out a branch into a worktree and it gets its own environment: its own
Docker Compose project, its own data directory, its own hostname on a shared
local router, and a lock that stops two commands from mutating it at once.
Remove the worktree and everything it owned is torn down — verified by
ownership label, never by name.

The tool is language- and framework-agnostic. It knows about Git, Docker, and
the filesystem. Everything specific to *your* project — migrations, seeding,
health checks — arrives as a small config file and a handful of executables
in whatever language you already use.

> **Status: pre-release.** The worktree lifecycle works end to end against
> Docker Compose: `up`, `down`, `status`, `urls`, `worktree list`,
> `worktree remove`, `worktree unlock`, and `doctor` — identity, locks, the
> registry, the hook sequencer, a runtime that stamps every resource with
> ownership labels and refuses to touch anything it cannot prove it owns, a
> shared hardened router that makes every worktree reachable at its own
> hostname, and a machine-wide index page listing every checkout. See
> [Roadmap](#roadmap).

## How it works

```
berth.toml ──reads──▶  berth  ──spawns──▶  your hooks (any language)
                         │
            ┌────────────┼────────────┐
            ▼            ▼            ▼
      identity &     control      Docker Compose
      locks          home         (label-owned)
```

**Identity.** Two UUIDs are written into the Git directory: one for the
repository, one for the worktree. Paths and branch names can change; the UUIDs
cannot. A worktree keeps its environment across renames, rebases, and moves.

**Slugs.** Each worktree gets a slug derived from its branch — `feature/login`
becomes `feature-login` — and is routed as `feature-login.<your-domain>`. Slugs
are unique per domain, because the hostname is what must not collide: two
projects with different domains can both have a `feature-login`. A collision
within one domain gets a short hash suffix rather than a surprise. Slugs are
sticky: once allocated, a worktree keeps its slug — suffix included — across
domain changes whenever it is free, so two clones can never swap hostnames
underneath you. Reclaiming a bare name would be an explicit rename — a verb
the tool does not have yet — never a side effect.

**Locks.** Every command that changes a worktree, the registry, or the shared
router takes an advisory lock: `flock(2)` on the lock's own file, held by an
open file descriptor. The kernel frees the lock once every copy of that
descriptor is closed: killing the holder — even by `SIGKILL` — closes its
own, but a `docker` or `docker compose` child it started inherits a copy and
keeps the lock until that child exits too; hooks never inherit it.
Either way, there is no stale window and nothing to go quiet: recovery never
waits on a heartbeat, because there is none. A command that finds the lock
held waits for it — a worktree operation for up to 25 minutes, the router's
own lock for up to 20 — and after a couple of seconds, then every 30 seconds,
says who holds it (its command, how long) and what to do: wait for it to
finish or stop it (`kill <pid>`), or, if the holder is merely stopped
(`Ctrl-Z`, alive but not working), resume it (`kill -CONT <pid>`) and let it
finish, or end it; on Linux it also lists the processes still holding the
lock. Recovery from a holder that died is automatic and checked, never
assumed: the moment a lock is free, whoever takes it next verifies that the
previous holder's own record names a process that is genuinely gone — by
machine ID (or boot ID, in a container image with none), PID, and start time,
so a reused PID or a different boot is never mistaken for the same process —
then scans, twice a second apart, for anything that holder started and left
running, before treating the lock as recovered. `berth worktree unlock
<slug|uuid>` runs that same check on demand, without waiting to acquire the
lock itself, so a lock nobody is actively waiting on can still be cleared
right away; `--registry` and `--router` point it at the registry and router
locks instead of a worktree's. [Lock recovery](#lock-recovery) says exactly
what it checks and what it cannot see. `doctor` reports every lock currently
held, and every free lock whose record is still unreleased; an unreleased
record alone doesn't prove the holder is gone — the next command to take that
lock checks the recorded holder itself before treating it as recovered.

**Homes.** Each worktree owns a directory under the control home
(`~/.berth/control-home` by default) created owner-only, with path containment
that refuses symlinks anywhere in the chain before creating or removing
anything. The same goes for the shared router's own state under the berth
root (`~/.berth/router`), with one exception: the two directories the router
stack bind-mounts, `router/dynamic` and `router/index`, are 0755, because the
containers that read them run as root without the capability that lets root
bypass file permissions, and on Linux Docker Engine that leaves a 0700
directory unreadable to them. Their parents stay owner-only — a bind mount is
entered at its mount point, so the parents are never traversed. Only the
index page and route fragments are written there; nothing secret.

**Hooks.** `prepare`, `seed`, `verify`, `backup`, and `restore` are
executables you point at from the config file. `up` invokes `prepare` and
`seed`: `seed` never runs before `prepare`, and not at all if a configured
`prepare` exited non-zero. `doctor` invokes `verify`. `backup` and `restore`
are accepted and validated so a repository can commit them once, and future
commands of the same names will run them. The tool never learns what they do.

**Router.** One Traefik instance per machine, started on first `up`, shared
by every worktree of every repository. It binds a single loopback port and
routes by hostname, reading routes only from Traefik's file provider: `up`
writes one dynamic-configuration fragment per routed service into a
directory the router's own state mounts read-only into the container, and
`down` and `worktree remove` remove it again. It belongs to the berth root
that started it — a label on its containers says which — and `up` or
`router up` from another root refuses to take it over; `router down` hands
it on (see [Configuration](#configuration)). The router has no path to the
Docker socket at all, direct or proxied — Traefik's Docker-provider discovery
is disabled entirely, so nothing about routing depends on it reading a
container label. Each worktree gets its own dedicated network rather than
sharing one with every other worktree, so a routed service resolving another
routed service's name by its Compose alias reaches only its own checkout's
container, never another's; the router sits on every worktree's network too
(each carries a label naming the router, which is how `router up` finds and
reconnects every one after a restart), so through it — by hostname, over
HTTP, the way the host itself reaches them — a routed service can still reach
any checkout's routed services and the
index page. The router and index containers both run read-only with every
capability dropped (the router gets back only the one it needs to bind port
80) and pinned by image digest; each is inspected after every start,
recreated once if it has drifted, and refused if the drift survives that. The
verifier checks that ownership and protocol labels, running and healthy
state, the read-only root filesystem, `no-new-privileges`, dropped
capabilities, published ports, image digest and mounts are all as expected,
and — read straight off the running containers, never assumed — that neither
container is privileged, shares a host PID, IPC, network or UTS namespace, or
has a device attached, that neither seccomp nor AppArmor is left unconfined,
and that each container's entrypoint still matches the one its pinned image
bakes in.

## Install

There is no published binary yet — releases aren't set up (#8). Build it
from source with Go, at least the version [`go.mod`](go.mod) names:

```bash
git clone https://github.com/sofired/berth && cd berth
go install ./cmd/berth   # installs $(go env GOPATH)/bin/berth (or $GOBIN)
```

or build a binary yourself and put it wherever you like:

```bash
mkdir -p ~/.local/bin && go build -o ~/.local/bin/berth ./cmd/berth   # any directory on PATH
```

`berth doctor` checks Git, Docker and Docker Compose once it is on PATH.

### Shell completion

`berth completion <shell>` prints a completion script for bash, zsh, fish or
PowerShell. It completes commands, their flags with descriptions, and
`--config` with `.toml` files. `berth completion <shell> --help` says how to
load the script for good; to try it in the current shell:

```bash
source <(berth completion bash)   # or zsh
berth completion fish | source    # fish
```

A renamed binary or symlink (see [Calling it something else](#calling-it-something-else))
prints a script for the name it was invoked as.

## Quick start

The tool is tested with Docker Compose 2.21 and newer (`berth doctor` checks,
as does `docker compose version`). Commit a `berth.toml` at the root of your
repository:

```toml
[project]
name   = "acme"
domain = "acme.localhost"          # worktrees route as <slug>.acme.localhost

[compose]
file = "docker/compose.yaml"

[[service]]
name = "api"                       # routes at the worktree host root
[service.index]
label   = "API"
purpose = "REST surface"

[[service]]
name      = "mail"
subdomain = "mail"                 # mail.<slug>.acme.localhost

[hooks]
prepare = "./scripts/migrate.sh"   # services healthy, nothing seeded
seed    = "./scripts/seed.py"      # only after prepare exits zero
verify  = "./scripts/check.sh"     # run by doctor; exit 0 means healthy
```

Then:

```bash
berth config validate     # parse and check the file
berth up                   # register this worktree, start its services, run prepare (+ seed the first time)
berth status               # per-service state
berth urls                 # print its routed URLs
berth down                 # stop it; data is kept
berth worktree list        # every worktree of this repository, with state
berth worktree remove      # tear it down, verified by ownership
berth worktree unlock      # clear a dead holder's lock, once it is checked gone
berth doctor               # check prerequisites, registry, router; run verify
```

`up` runs the branch's own build and hooks. Read [Trust model](#trust-model)
before bringing up a branch you did not write.

`up` starts the Compose project under a name derived from the worktree's
UUIDs, with every container, volume, network, and image it builds labelled
`dev.berth.managed`, `dev.berth.repository-uuid`, and
`dev.berth.worktree-uuid`. Your Compose file is never asked to add those
labels; a generated override file does it, so forgetting one — and silently
exempting a resource from cleanup — is not possible. The Compose file sees
the same `BERTH_*` variables hooks do, so `${BERTH_WORKTREE_HOME}` works in a
bind mount exactly as it does in a seed script.

`status` reports each declared service as `running`, `starting` (Compose says
`restarting`, or `running` with its health check still pending), `stopped`
(Compose says `created`, `exited`, `dead`, `paused` or `removing` — the
container exists but nothing is serving), `absent` (no container at all, the
state after `down`), or `unknown` for any state Compose adds that this list
does not name. `unknown` carries Compose's own word in parentheses, and a
running service that is failing its health check carries `(unhealthy)`.

Those five words are about one service. Two other vocabularies are about a
whole checkout. `worktree list` reports each registration as `ready`,
`pending` (registered, but its first `up` never finished), or `missing`
(registered, but the checkout is gone). Only this repository's checkouts can
be called missing — that needs Git, and `--all` lists records from other
repositories as the registry knows them. The index page answers a different
question — whether a checkout's services are up — with `Running`, `Partial`,
`Stopped`, `Unknown`, and the same `Missing`, which means the same thing in
both places. All three are pre-1.0. `worktree list --json` is the only
machine-readable form; `status` and `index` print text. It prints one array,
`[]` when there is nothing, sorted by domain and then slug, each row carrying
`repositoryId`, `worktreeId`, `slug`, `domain`, `host`, `state`, `current`,
and — when they are known — `root`, `branch` and `labelNamespace`. Every
non-ASCII character is escaped, so the text is ASCII and parses back to what
it was.

Each `[[service]]` becomes a route: the tool generates the file-provider
fragment and joins the container to the worktree's own dedicated router
network through the same override, so your Compose file never mentions the
router. Chrome and Firefox resolve `*.localhost` to the loopback address on
their own; everything else depends on the system resolver — see
[Platform notes](#platform-notes). If port 80 is taken, set `[router] port`
in `settings.toml` (see [Configuration](#configuration)) or
`BERTH_ROUTER_PORT` — the printed URLs follow it.

```bash
berth router status       # is the shared router running, and is it what this version expects?
berth router up           # start it without starting a worktree
berth router down         # stop it; worktree services keep running, unrouted
```

Every routed worktree owns one more Docker bridge network, so `doctor`
warns once the machine's networks start getting close to Docker's own
default address-pool limit.

**Index.** `http://berth.localhost` (plus the port, if changed) lists every
checkout on the machine across every project: branch, hostname, path, whether
its services are running, and a link for each service that opted in with
`[service.index]`. It is a static page served from the router stack,
regenerated by `up`, `down`, and `worktree remove` — or by `berth index`,
which also prints it as text. An open tab fetches the page every 15 seconds
and swaps the table in when the rows have actually changed, advancing the
timestamp quietly otherwise, so it follows along without losing your place. It
holds an update back while you are reading inside a table with the keyboard,
says so in the header when it has stopped being able to read the page, and
pauses on request. Without scripting it falls back to reloading every 60
seconds.

`index` needs a `berth.toml` — the nearest one above the working directory,
or the one `--config <path>` names — even though what it prints spans the
machine. The same is true of `worktree list --all`, and of every verb that
touches a project; only `router` works without one.

`worktree remove` is the only destructive verb. It inventories everything
the worktree owns, verifies the full ownership tuple on every resource, and
refuses the whole operation if a single one fails — before it asks you to
type the hostname to confirm (`--yes` skips the prompt). Then, in an order
chosen so that an interrupted removal can simply be run again: Docker
resources, the home directory, the registration, the identity marker.
`berth worktree remove <slug|uuid>` removes another worktree, including one
whose checkout is already gone; `--label-namespace` supplies the Docker label
namespace of its resources when nothing on disk records it.

`worktree remove` does not run the removal from wherever the invoked binary
happens to live: before doing anything else, it installs a content-addressed
copy of itself under the control home — outside any checkout — and hands the
removal to that copy, so deleting a checkout cannot delete the program still
removing it. That holds whenever the copy can be installed and run; when it
cannot, removal continues with the invoked binary, as described below. The
copy is exactly
the binary that was invoked, though, nothing more: a build made from a
branch still runs that branch's code once relocated, just from somewhere the
checkout it is removing cannot take down with it. Running only a trusted,
pinned build regardless of what invoked `worktree remove` is a separate,
later guarantee tied to how releases are distributed (#8, #85). Copies are
content-addressed, so a rebuild that changes nothing installs nothing new;
one that has not been installed or reused in the last 30 days, or that is
not among the 5 most recently used besides the current one, whichever rule
catches it first, is swept away automatically — disk hygiene for a workflow
that rebuilds often, not something to manage by hand. That count is a
steady-state target, not a hard ceiling: the sweep only runs as part of a
later `worktree remove`, so a burst of removals from distinct builds in
quick succession can briefly leave more than 5 until the next one catches
up. A copy installed or reused in the last 10 minutes is never swept away by
either rule, however old or however far past the count it would otherwise
be: that window exists for another `worktree remove`, running at the same
time, that might be mid-way through installing or about to run the very
copy this one's own sweep would otherwise be free to take. This is invisible
in normal use; a copy that cannot be installed or run — including on a
control home whose filesystem cannot execute programs at all — is reported
on stderr as it happens, and removal continues with whichever binary was
invoked.

A full annotated example is in [`berth.example.toml`](berth.example.toml).
The hook contract is specified in [`docs/hooks.md`](docs/hooks.md).

## Calling it something else

Name the binary or symlink whatever you like. Help text, the log prefix, and
every "run `X urls`" hint follow the name it was invoked as, which is read
from `argv[0]` and from nowhere else:

```bash
ln -s "$(go env GOPATH)/bin/berth" ~/.local/bin/duke
duke --help               # Usage:
                          #   duke [flags]
                          #   duke [command]
```

A binary built or invoked with no meaningful name to read — `main`, a bare
`.`, or an empty `argv[0]` — says `berth` instead.

What does **not** follow the name is anything written to disk: the config
and settings filenames (`berth.toml`, `settings.toml`), the berth root and
control-home directories, the
environment variables (`BERTH_HOME`, `BERTH_CONTROL_HOME`, …), the variables
handed to hooks (`BERTH_STAGE`, …), and the Docker label namespace. Those are
storage contracts. If they followed the binary name, renaming a symlink
would orphan state.

## Configuration

| Key | Meaning |
| --- | ------- |
| `project.name` | Lower-case slug. |
| `project.domain` | Worktrees are routed as `<slug>.<domain>`. |
| `project.label-namespace` | Optional. Only to adopt resources created by a predecessor tool. |
| `compose.file` | Compose file, relative to `berth.toml`. Paths inside it (build contexts, bind mounts, `env_file`) and its `.env` resolve against the Compose file's directory, as with `docker compose -f`; `${BERTH_ROOT}` is still `berth.toml`'s directory. |
| `[[service]]` | `name`, optional `subdomain` (empty = host root), optional `path` (route only that prefix, so services can share a host), optional `port` (defaults to the first port the service exposes), optional `index` presence. |
| `[hooks]` | `prepare`, `seed`, `verify`, `backup`, `restore` — resolved as in [docs/hooks.md](docs/hooks.md): relative to the config file when the command starts with `.` or contains `/`, otherwise from `PATH`. `up` runs `prepare` and `seed`, `doctor` runs `verify`; `backup` and `restore` are accepted but nothing runs them yet. |

Unknown keys are rejected, so a typo cannot silently disable a feature.

Every invocation passes the project name and the file list as flags and sets
`COMPOSE_PROGRESS`, so `COMPOSE_PROJECT_NAME`, `COMPOSE_FILE`,
`COMPOSE_PATH_SEPARATOR` and `COMPOSE_PROGRESS` in the Compose file's `.env`
have no effect. Other `COMPOSE_*` keys there still reach Compose — in
particular `COMPOSE_IGNORE_ORPHANS`, which defeats the `--remove-orphans` that
`up` and `down` rely on. Do not set it.

`BERTH_HOME` is the berth root — `~/.berth` by default — holding
`settings.toml`, the shared router's own state and, unless
`BERTH_CONTROL_HOME` overrides it, the control home, nested under it as
`control-home/`. `BERTH_CONTROL_HOME` moves only the control home, leaving
everything else where `BERTH_HOME` puts it. None of these are config-file
settings: the config file is committed and shared, and a path that is only
correct on one machine has no business in it — one developer pointing it at
a synced folder would put the whole team's state on iCloud Drive.

`settings.toml` is the machine's own file, never committed, for what is true
of this machine rather than of a project. Today that is the router's
loopback port:

```toml
[router]
port = 8080
```

Unknown keys are rejected, as in `berth.toml`; a missing file means the
defaults. The port is `BERTH_ROUTER_PORT` when that is set, else the file's,
else 80. `router status` and `doctor` say which one applied and name the
file; `doctor` warns when the running router publishes a different port; and
an `up` that moves the router to a new port says that URLs already open — the
index page included — still use the old one.

The berth root also holds the shared router's own bookkeeping: `router/`,
with the dynamic-configuration and index directories the file provider
watches, and the router's own lock. The router *container*, though, is one
per Docker daemon regardless of berth root — its Compose project name never
changes — and its containers carry a label naming the berth root that
started them. `up` or `router up` from a different `BERTH_HOME` refuses while
that router is running, naming the root it belongs to, rather than recreating
it against its own bookkeeping; a router carrying no such label is refused
the same way. Taking a router over is an explicit step: `router down` stops
it from any root, and the next root's `up` starts its own. `doctor` reports
the owner. Every control home that shares one berth root shares that
bookkeeping too, rather than taking turns recreating it.

Because `worktree remove` deletes inside it, the control home must be empty
or one the tool created earlier — it leaves a `.berth-control-home` marker to
recognise its own, and refuses a directory whose marker holds anything but
its own format, or is empty beside existing content. Your home directory, anything above it, and the
filesystem root are always refused, whether reached through `BERTH_HOME` or
`BERTH_CONTROL_HOME`.

## Trust model

Bringing up a worktree runs the branch's code. `up` builds whatever its
`compose.yaml` builds and runs the hooks named in its `berth.toml`, as you,
with your full environment — the same trust a `make` or an `npm install` in
that checkout asks for. Do not bring up a branch you would not run.

Three things follow from that, and this tool does not change them:

- **Hooks and builds see your environment.** Nothing is allow-listed; a hook
  is a program you pointed the config at, and it runs with everything you
  have, `COMPOSE_*` included. Compose's own invocation is the one exception:
  `COMPOSE_FILE`, `COMPOSE_PATH_SEPARATOR`, `COMPOSE_ENV_FILES` and
  `COMPOSE_DISABLE_ENV_FILE` are dropped from it, so a shell setting cannot
  change which files or `.env` it loads, and so is `COMPOSE_IGNORE_ORPHANS`,
  so it cannot turn off the orphan sweep `--remove-orphans` relies on.
  Dropping them from the environment does not reach the Compose file's own
  `.env`, which Compose reads for itself — see
  [Configuration](#configuration). `COMPOSE_PROGRESS` is always set to
  `plain`, whatever the shell says. Every other `COMPOSE_*` variable, project
  name and profiles included, reaches Compose unchanged: the project name is
  always overridden by the explicit `--project-name` flag Compose gives
  precedence over any inherited value, and a profile a shell setting
  activates still gets every resource it creates labeled, because the same
  environment governs both the config read that writes those labels and the
  `up` that follows it. Naming a service on `up` whose profile is not active
  is refused rather than started unlabelled, since Compose would otherwise
  enable that profile for it.
- **Routed services share their own worktree's network.** Every container
  you declare as a `[[service]]` joins this worktree's own dedicated router
  network, alongside the shared router itself. A routed service resolving
  another routed service's name by its Compose alias reaches only its own
  checkout's container — never another checkout's same-named one — but
  through the router, by hostname over HTTP, the way the host itself reaches
  them, it can still reach any checkout's routed services and the index
  page. Containers you do not declare stay on their project's own networks.
- **The index page is machine-wide and unauthenticated.** It lists every
  checkout's branch, path, hostnames and service links. Its only exposure to
  the host is the router's loopback port; a routed container is not on the
  shared network directly — it joins only its own worktree's network, above
  — so it reaches the index only indirectly, through the router, the same
  way it reaches another checkout's own routed services. Only the router and
  the index container itself sit on the shared network directly.

What the tool does guarantee is ownership: every resource it creates carries
its labels, and it refuses to touch anything whose full ownership tuple does
not match. That is what keeps `worktree remove` from deleting something it did
not create — not a claim about what your branch's code can do once it runs.

## Output and exit codes

The commands that produce data — `index`, `status`, `urls`, `router status`,
`config show`, `config validate`, `worktree list`, `doctor` — write that data to stdout
and nothing else, so `berth index > page.txt` and
`berth worktree list --json | jq` get what they asked for. Progress, waiting
notices, confirmation prompts, warnings and failures go to stderr.

`up`, `down`, `router down` and `worktree remove` are the exception: they pass
your hooks' and Compose's own stdout through as those programs wrote it, and
`up` adds its URL table there.

`router status` has a fourth vocabulary, about the shared router rather than
any checkout: `running on <address>`, `not running (would use <address>)`,
`degraded on <address>` when it is up but something about it is not what this
version expects, and nothing at all on stdout when Docker could not be asked —
that case is a `Router state unknown:` line on stderr. Each reported problem is
its own `Router problem:` line on stderr. Both `running` and `not running` exit
0, because "is it there?" is the question and both are answers; `degraded` and
an unreachable Docker exit 1. Whenever the status line is printed, a second
stdout line follows it saying which port was resolved and from where —
`port <n> from <settings file>`, `port <n> from BERTH_ROUTER_PORT (overriding
<settings file>)` or `port <n> by default (<settings file> sets none)`; it too
is absent when Docker could not be asked.

`doctor` writes its report to stdout: one section per area (`Prerequisites`,
`Registry`, `Locks`, `Router`, `This checkout`), one line per check, each marked `ok`,
`warn`, `fail` or `skip`. How it went is one line on stderr, and so is the
output of the `verify` hook it runs, so the report is alone on stdout. It exits
0 when no check failed — warnings and skips do not count — with the `verify`
hook's own exit code when that hook failed, and 1 when anything else failed.
The exit code names one failure; the report names them all. A `verify` hook
that exits 124 makes `doctor` exit 124 too — the code `timeout(1)` uses for a
command it had to kill — so a script that wraps `doctor` in `timeout` cannot
tell the two apart by exit code alone. It takes no lock, writes nothing, and
repairs nothing; it reports. When the router itself could be resolved, its
`Router` section also names the router's owner (a `fail` for a router
without berth's owner label or one another berth root started) and ends with a
`router port` line saying the same as `router status`'s second line, a
`warn` when the running router publishes a different port than the one
resolved; when port or settings resolution fails instead, the section holds
that one router failure line alone.

`worktree unlock` writes nothing to stdout: what it verified and what it
cleared are notices on stderr. A refusal is an error that exits 1 and clears
nothing: a holder that may be alive, a process it left running, or a lock that
changed while it looked.

Every failure the tool itself reports, a bad command line included, ends in a
non-zero exit code and a `[berth] ERROR:` line on stderr saying why. Set
`BERTH_DEBUG=1` to see the stack trace behind a failure. A failing hook's own
exit code becomes berth's — see [docs/hooks.md](docs/hooks.md) — and a verb
that is registered but not implemented exits 2.

## Platform notes

**Bind mounts on Linux.** Docker Engine enforces host file permissions inside
containers. A worktree's home directory (`${BERTH_WORKTREE_HOME}`) is 0700, so
a service whose process runs as a uid other than yours — `postgres`, `redis`,
and `mysql` drop to uid 999 in their default images (`postgres:*-alpine` uses
70) — cannot read a bind mount of it. For content such a service only *reads*
(seeds, config), bind-mount a subdirectory of the home rather than the home
itself: a bind mount is entered at the mount point, so the container never
traverses the 0700 parents, and every `up` resets only the home itself to
0700, so a subdirectory you loosened (`chmod -R o+rX` it from your `prepare`
hook if your umask made it private) stays loosened. For anything such a
service *writes*, use a named volume: `postgres`, `mysql`, and `redis` `chown`
their data directory to their own uid as root before dropping privileges, and
`worktree remove` cannot delete a directory another uid has taken ownership
of. When that happens it stops and prints a one-off `docker run … chown` that
hands them back to you; run it, then run the removal again and it finishes.

**Docker Desktop on macOS.** File sharing performs I/O as your macOS user, so
mode bits are not enforced against the container's uid: a bind mount that
works on macOS can fail on a Linux teammate's Docker Engine for the reason
above. Projects that must run on both should use named volumes. Docker
Desktop on Windows behaves like Linux when the tool runs inside the WSL 2
distribution with the control home and checkout on its own filesystem; paths
under `/mnt/c` present as 0777 and behave like the macOS case.

**Docker Desktop file sharing and the file provider.** Routes come from files
`up`, `down` and `worktree remove` write and remove under the router's own
state directory, watched by Traefik's file provider inside the container. On
macOS Docker Desktop, both file-sharing backends (virtiofs, the current
default, and the older grpcfuse) bridge the host's filesystem events into
the container, and that bridge can occasionally miss the rename half of the
atomic write a fragment update makes — the container's watcher then keeps
routing (or fails to route) what the file on disk no longer says. If a route
looks stale after an `up`, `down` or `worktree remove` on such a host, run
`X router down` then `X router up` (with `X` the name you invoked this build
as; see "Calling it something else") to force Traefik to reread every
fragment from a clean start, rather than waiting on the next filesystem
event that touches it.

**Rootless Docker.** The router no longer needs the Docker socket at all
(see [Router](#how-it-works) above), which removes what used to be the main
blocker for a rootless daemon; whether the rest of the tool — the Compose
backend, bind mounts and the rest — works unmodified against one is untested
and not claimed here.

**SELinux.** Hosts with SELinux enforcing need bind mounts relabelled (`:z`),
which the tool does not do for its own router mounts; treat such hosts as
unsupported for now.

**Hostname resolution.** Chrome and Firefox map `*.localhost` to loopback
internally on every platform. Other clients — Safari, `curl`, language HTTP
libraries — use the system resolver, and whether it maps `*.localhost` varies:
current macOS and systemd-resolved (Ubuntu, Fedora) do; plain glibc or musl
resolvers (Debian, Alpine, distributions without systemd-resolved) do not.
Check with `curl -sSI http://berth.localhost` (add the port if you changed
it): any `HTTP/1.1 …` status line means the resolver is fine (the router
answered), `Could not resolve host` means it is not, and a connection error
means resolution worked and the router simply isn't running. Where it fails,
`/etc/hosts` has no wildcards, so add every hostname `berth urls` prints for
the worktree — `<slug>.<domain>` and any `<subdomain>.<slug>.<domain>` — plus
`berth.localhost`; or use `curl --resolve` / a `Host:` header.

### Lock recovery

`worktree unlock [slug|uuid]` clears a worktree's locks (its operation lock,
and its development-session lock if that is held too) only when all of this
holds — checked automatically, without ever waiting to acquire the lock
itself:

- the lock itself is free: `flock(2)` proves this instantly, so a lock
  actually held by a live process is always refused straight away, before
  anything else is checked;
- the record its last holder left names a process this machine can
  identify: by its machine ID, or by its boot ID in a container image that
  has none, in this boot and this PID namespace;
- that holder's PID no longer exists, or now belongs to a later process, so
  a reused PID is never mistaken for the same one — checked twice, a second
  look about a second later, in case a holder that looked alive was simply
  finishing;
- no command started for the worktree is still running on this machine:
  nothing with its Compose project on the command line, or its
  `BERTH_WORKTREE_ID` in the environment — checked the same way, twice.

A stopped holder (`Ctrl-Z`) is alive, and is refused: resume it with
`kill -CONT <pid>` and let it finish, or end it. A holder it cannot check —
on another machine or in a container, one that left no record, or one it
cannot look past because other users' processes are hidden from you (`/proc`
mounted with `hidepid`, say) — is refused too, unless you pass
`--holder-unverifiable` once you have made sure yourself; that never
overrides a running holder or a command left running. `--registry` and
`--router` point the same check at the machine-wide registry or router lock
instead of a worktree's, and take no worktree identifier. The unlock command
that a lock notice or `doctor` prints names the worktree by its UUID: it
never changes, no other worktree shares it, and it works even for a
worktree whose first `up` died before it registered.

It reads `/proc` on Linux and asks `ps` on macOS. Your own processes are
checked by their command line and their environment; other users' only by
their command line, since their environments are private to them. So it cannot
see a command that left this machine's process table, or one that no longer
carries the worktree's markers where it can look: a command a hook ran inside
a container with `docker exec`, or a daemon handed to a service manager. A
helper a hook ran as another user — through `sudo`, which clears the
environment, or `sudo -E` — is missed too, unless its command line names the
worktree's Compose project. The worktree's own containers are not commands: they are what
`up` built, running or stopped as the holder left them, and the next `up` or
`down` reconciles them. What it did check, it prints, as a `Verified:` line
per lock cleared — or `Not verified, on your word (--holder-unverifiable):`
for whichever it accepted unchecked.

## Development

Go, at least the version [`go.mod`](go.mod) names.

```bash
gofmt -l cmd internal                                   # must print nothing
go vet ./...
go vet -tags integration ./...
go run honnef.co/go/tools/cmd/staticcheck@2025.1.1 ./...
go run honnef.co/go/tools/cmd/staticcheck@2025.1.1 -tags integration ./...
go test -race ./...                                     # unit
go test -race -tags integration ./...                   # real git, subprocesses, signals, Docker
CGO_ENABLED=0 go test ./... && CGO_ENABLED=0 go test -tags integration ./...
```

`-tags integration` marks tests that touch something real — a process, a
signal, or a Docker daemon — and are excluded from the default `go test
./...`. Without a daemon the Docker suites skip; CI's Linux leg sets
`BERTH_TEST_REQUIRE_DOCKER=1`, which turns that skip into a failure, so a green
run on a machine without Docker is not the run CI makes. The Docker suites
sweep up whatever a killed run left behind, without regard to age, so run one
of them at a time on a machine.
The `CGO_ENABLED=0` run matters on its own: cgo changes how the Go runtime
behaves (an extra thread, notably), and releases will build with it
disabled. CI also runs the streams contract — every command's
stdout/stderr split — as part of `go test -race -tags integration ./...`,
against a binary it builds itself (`internal/cli/streams_integration_test.go`).
The help and output goldens under `internal/cli/testdata/` are
hand-maintained fixtures; [`CONTRIBUTING.md`](CONTRIBUTING.md) says how.

### Layout

```
cmd/berth/               entry point: argv and the environment come in here and
                          are handed down as parameters; library code never looks
                          a variable up by name itself
internal/
  cli/                    command tree, dispatch, error reporting
  commands/               the everyday verbs as plain functions over a context
  config/                 berth.toml schema and discovery
  shape/                  the small schema library config files are validated
                          against, strict about unknown keys
  hooks/                  hook protocol and sequencer
  progress/               phase logging with an elapsed-time heartbeat
  checkoutindex/          per-worktree cards, the snapshot, and the rendered page
  router/                 the shared router's Compose stack, its file-provider
                          fragments, and the verifier
  backend/                the Compose backend: labels, overrides, inventory, reap
  worktree/               what the control plane and backend both say about a worktree
  registry/               identity markers, slugs, the registry
  lock/                   flock-based locks and their checked recovery
  binstore/               the content-addressed store `worktree remove` runs
                          itself from, outside any checkout
  settings/               the machine's own settings.toml, read as strictly as berth.toml
  layout/                 where everything lives under the berth root and control home
  files/                  owner-only, symlink-refusing filesystem primitives
  text/                   the filter every printed line goes through
  brand/                  the product name and the presentation/storage split
  clierr/                 the error type a command reports and exits with
  logger/                 the stdout/stderr split every command writes through
  run/, run/osrun,
  run/fakerun/            the subprocess seam, and its real and fake implementations
  docker/                 runs the `docker` CLI and tells it apart from Docker being unavailable
  procid/                 process identity and process-table lookups, for lock recovery
  lifecycle/              signal handling and the lock-ordered mutation wrapper
  style/                  the code-style rules gofmt, vet and staticcheck don't
                          enforce, written as tests over the module's own source
```

Tests live beside the code they cover, as `_test.go` files; anything that
needs a real process, a signal, or a Docker daemon is behind
`//go:build integration` and runs separately from the default `go test ./...`.

## Roadmap

Shipped:

- **Compose runtime** — `up`/`down` that stamp every resource with
  `<namespace>.managed`, `.repository-uuid`, `.worktree-uuid`; inventory and
  teardown that verify the full ownership tuple on every resource before
  touching it.
- **`up` / `down` / `status` / `urls`** — under the worktree lock, with
  interrupts deferred until it can be released cleanly.
- **`worktree list` / `worktree remove`** — ownership-verified teardown in
  crash-safe order, run from a content-addressed copy of the binary
  installed outside any checkout; checkouts Git no longer lists are reported
  missing.
- **Shared router** — one Traefik instance across every worktree of every
  repository, routed entirely through its file provider (no Docker-socket
  access at all), each worktree on its own dedicated network, verified after
  every start, and owned by the berth root that started it: another root's
  `up` refuses rather than taking it over.
- **Settings file** — `settings.toml` under the berth root, starting with
  the router's port; `router status` and `doctor` say where the port came
  from.
- **Checkout index** — `berth.localhost`: every registered worktree on the
  machine, grouped by domain, with per-service links and status.
- **`worktree unlock`** — clears a dead holder's locks (a worktree's own,
  or, with `--registry`/`--router`, the registry's or router's) once it is
  checked gone; `doctor` reports every held lock, and any left unreleased by
  a holder that is gone.
- **`doctor`** — one read-only report: Git, Docker and Compose against the
  versions this tool needs; the berth root and every registration
  (unreadable records, a hostname claimed twice, the index's own hostname);
  the router's verification and its address-pool headroom; and, for this
  checkout, its `verify` hook and any registration of its repository whose
  checkout Git no longer lists.

Not built yet, in rough order. `dev` is registered and refuses by name
rather than pretending; the rest are not commands at all. The config file
already accepts the `backup` and `restore` hooks, so a repository can commit
them once.

1. **`dev`** (#5) — start the worktree and follow its services' logs,
   holding the development-session lock so `worktree remove` refuses while a
   session is attached.
2. **Native routes** (#6) — `berth route` writing file-provider fragments
   for processes running outside Docker.
3. **Binary distribution** (#8) — per-platform release builds, so installing
   no longer means building from source.
4. **`init`** (#9) — write a starter `berth.toml`. Until then, copy
   `berth.example.toml` to the repository root.
5. **`backup` / `restore`** (#10) — invoke the hooks of the same name with a
   bundle directory.
6. **`rename`** (#11) — change a worktree's slug, or the product's own
   storage names, by migrating the state that carries them.
7. **Config: services keyed by name** (#3), with a published JSON Schema.
8. **Hook timeouts** (#26) — so a hung `prepare` or `verify` cannot hang
   the command that runs it forever.
9. **`--json`** (#27) for `doctor`, `status`, `urls` and `index`, to match
   `worktree list`'s own machine-readable form.
10. **`BERTH_URL`** (#28) — a hook's own worktree URL, alongside the
    variables [docs/hooks.md](docs/hooks.md) already lists.
11. **Migrating stored state between releases** (#29), once there are
    releases to migrate between.
12. **Running `worktree remove` from a pinned, trusted binary** (#85) — the
    stronger guarantee the relocated-copy mechanism above is a step toward,
    tied to binary distribution (#8).

## License

Apache-2.0. See [LICENSE](LICENSE).
