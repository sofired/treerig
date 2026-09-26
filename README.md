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
> Docker Compose: `up`, `down`, `status`, `urls`, `worktree list`, and
> `worktree remove` — identity, locks, the registry, the hook sequencer, a
> runtime that stamps every resource with ownership labels and refuses to
> touch anything it cannot prove it owns, a shared hardened router that
> makes every worktree reachable at its own hostname, and a machine-wide
> index page listing every checkout. See [Roadmap](#roadmap).

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

**Locks.** Every command that changes a worktree holds an advisory lock with
an effectively infinite stale window and a heartbeat, so a twenty-minute build
is never silently stolen. A command that finds the lock held waits for it — a
worktree operation for up to 25 minutes — and after a couple of seconds, then
every 30 seconds, says which process holds it, for how long, and how fresh its
heartbeat is. Recovery from a genuinely dead holder is never automatic. Once
the heartbeat has stayed silent across two looks, the waiting command names
`berth worktree unlock <uuid>` (one that has to give up sooner names it too).
That clears a worktree's locks only after checking, not assuming, that the
holder and every command it started on this machine are gone, and refuses
whenever it cannot tell; [Lock recovery](#lock-recovery) says what it checks
and what it cannot see. The registry and router locks guard only moments of
work and keep the older advice: `rmdir` the directory once the holder, and
anything it started, is gone. `doctor` reports every held and stale lock.

**Homes.** Each worktree owns a directory under the control home
(`~/.berth` by default) created owner-only, with path containment that refuses
symlinks anywhere in the chain before creating or removing anything. The two
control-home directories the router stack bind-mounts (`router/index`,
`router/dynamic`) are the one exception: they are 0755, because the containers
that read them run as root without the capability that lets root bypass file
permissions, and on Linux Docker Engine that leaves a 0700 directory
unreadable to them. Their parents stay owner-only — a bind mount is entered
at its mount point, so the parents are never traversed. Only the index page
and route fragments are written there; nothing secret.

**Hooks.** `prepare`, `seed`, `verify`, `backup`, and `restore` are
executables you point at from the config file. `up` invokes `prepare` and
`seed`: `seed` never runs before `prepare`, and not at all if a configured
`prepare` exited non-zero. `doctor` invokes `verify`. `backup` and `restore`
are accepted and validated so a repository can commit them once, and future
commands of the same names will run them. The tool never learns what they do.

**Router.** One Traefik instance per machine, started on first `up`, shared
by every worktree of every repository. It binds a single loopback port and
routes by hostname. It never sees the Docker socket: it reads container
labels through a proxy that denies every write; Traefik itself never holds the
socket. What that proxy still allows is the whole container section, plus
networks, events, ping and version — and the container section is as narrow
as the API gets: enough to read every container's labels, and with them its
environment, its logs, and its filesystem through the archive and export
endpoints. As the stack is written, only the router is on the network the
proxy is on, and that network is internal. All three containers — the proxy,
Traefik, and the BusyBox `httpd` that serves the index page — run read-only
with every capability dropped (Traefik gets back only the one it needs to bind
port 80) and pinned by image digest; each is inspected after every start,
recreated once if it has drifted, and refused if the drift survives that.

**Go build only.** As of the Go build's router protocol 5, the socket proxy
and Traefik's Docker-provider discovery are gone entirely: the router
container has no path to the Docker socket at all, direct or proxied. Routes
come only from Traefik's file provider, watching a read-only directory the
router state mounts; berth itself writes and removes one fragment there per
routed service on `up`, `down` and `worktree remove`, and each worktree gets
its own dedicated router network instead of sharing one — see
[`docs/port/divergences.md`](docs/port/divergences.md) rows 29 and 30. The
verifier's own inspection is also deeper here: on both the router and the
index container, it checks that neither is privileged, shares a host PID,
IPC, network or UTS namespace, or has a device attached, that neither
seccomp nor AppArmor is left unconfined, and that each container's
entrypoint still matches the one its pinned image bakes in — every hardening
claim this section makes, read back off the running containers rather than
assumed (row 32).

## Install

There is no published package and no binary yet — see [Roadmap](#roadmap).
Today berth runs from a clone, on Node 24, with Git 2.36 or newer, Docker, and
Docker Compose available; `berth doctor` checks each of them. A Node too old to
run TypeScript at all stops before any of berth's code — `doctor` included —
can say so, with Node's own error (`ERR_UNKNOWN_FILE_EXTENSION`, or a
`SyntaxError` about an `import` statement); `node --version` is the check then.
To install:

```bash
git clone <this repository> berth && cd berth
corepack enable && pnpm install
ln -s "$PWD/src/cli/main.ts" ~/.local/bin/berth   # any directory on PATH
```

The entry point carries a `#!/usr/bin/env node` line and is executable, so the
symlink is all there is to it; Node strips the types as it loads. Nothing is
compiled and nothing is copied, so the symlink follows the clone — `git pull`
is the upgrade.

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
berth up                  # register this worktree, start its services, run prepare (+ seed the first time)
berth status              # per-service state
berth urls                # print its routed URLs
berth down                # stop it; data is kept
berth worktree list       # every worktree of this repository, with state
berth worktree remove     # tear it down, verified by ownership
berth worktree unlock     # clear a dead holder's lock, once it is checked gone
berth doctor              # check prerequisites, registry, router; run verify
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

Each `[[service]]` becomes a route: the tool generates the Traefik labels and
joins the container to the shared router network through the same override,
so your Compose file never mentions the router. Chrome and Firefox resolve
`*.localhost` to the loopback address on their own; everything else depends
on the system resolver — see [Platform notes](#platform-notes). If port 80 is
taken, set `BERTH_ROUTER_PORT` — the printed URLs follow it.

**Go build only.** The override adds no Traefik or discovery labels,
`dev.berth.router` included, and joins no container to the shared network
(row 29). `up` writes one file-provider fragment per routed service instead,
and joins the service to this worktree's own dedicated network under an
alias (row 30). An upgrade from any router below protocol 5 leaves
already-running worktrees unrouted until their next `up`, and the run says
so.

```bash
berth router status       # is the shared router running, and is it what this version expects?
berth router up           # start it without starting a worktree
berth router down         # stop it; worktree services keep running, unrouted
```

Only containers whose `dev.berth.router` label holds the shared router
network's name are routed. The override adds that label under the tool's own
namespace, whatever `project.label-namespace` says, and Traefik is told to
discover nothing else, so two router stacks on one machine (a real
installation's and the test suite's, say) never route each other's
containers. A new version that changes the router upgrades it on the next
`up` and says so. When that upgrade is the one that introduced this label,
worktrees that were already running stay unrouted until their own next `up`,
and the run says that too.

**Go build only.** No container carries `dev.berth.router`; Traefik's
Docker-provider discovery is gone, so nothing reads a container label at
all. `RoutedByLabel` (the same string) survives only as a label on a
worktree's own dedicated network (row 30), which is what lets `router up`
find and reconnect every worktree's network on every start.

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

A full annotated example is in [`berth.example.toml`](berth.example.toml).
The hook contract is specified in [`docs/hooks.md`](docs/hooks.md).

## Calling it something else

Name the symlink whatever you like. Help text, the log prefix, and every
"run `X urls`" hint follow the name it was invoked as, which is read from
`argv[1]` and from nowhere else:

```bash
ln -s "$PWD/src/cli/main.ts" ~/.local/bin/duke
duke --help               # Usage: duke [options] [command]
```

Running the source by path (`node src/cli/main.ts`) has no name to read, so it
says `berth`.

What does **not** follow the name is anything written to disk: the config
filename (`berth.toml`), the control-home directory, the environment variable
(`BERTH_HOME`), the variables handed to hooks (`BERTH_STAGE`, …), and the
Docker label namespace. Those are storage contracts. If they followed the
binary name, renaming a symlink would orphan state.

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

The control home holds one more file for the shared router: `router/protocol`,
the protocol of the last router known to have run for it, as best the tool can
tell. `router down` deliberately leaves it, so a later `up` can still say what
changed; deleting it hands the router to whichever version runs next, unless a
newer one's containers are still there.

The router itself belongs to whichever control home last started it: it serves
that home's index page and watches that home's dynamic-configuration
directory. A second control home on the same machine quietly takes it over on
its next `up`, recreating the stack against its own directories. Point
`BERTH_HOME` somewhere else only when you mean to take the router with you.

The control home is `~/.berth`, overridable only through the `BERTH_HOME`
environment variable, and the router's loopback port is 80, overridable only
through `BERTH_ROUTER_PORT` — deliberately not config-file settings. The
config file is committed and shared; a path that is only correct on one
machine has no business in it, and one developer pointing it at a synced
folder would put the whole team's state on iCloud Drive.

Because `worktree remove` deletes inside it, the control home must be empty
or one the tool created earlier — it leaves a `.berth-control-home` marker to
recognise its own. Your home directory, anything above it, and the filesystem
root are always refused. A control home the Go build of berth manages — its
marker is non-empty — is read-only to this build: every mutating command
refuses rather than touch it.

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
- **Routed services share one network.** Every container you declare as a
  `[[service]]` joins the router's network, so a routed service in one
  checkout can reach a routed service in another. Containers you do not
  declare stay on their project's own networks. **Go build only:** each
  worktree gets its own dedicated router network instead (row 30 in
  [`docs/port/divergences.md`](docs/port/divergences.md)), so a routed
  service resolving another routed service's name reaches only its own
  checkout's container, never another's. The router is on every
  worktree's network, though, so through it — by hostname, over HTTP, the
  way the host itself reaches them — a routed service can still reach any
  checkout's routed services and the index page.
- **The index page is machine-wide and unauthenticated.** It lists every
  checkout's branch, path, hostnames and service links. Its only exposure to
  the host is the router's loopback port, and everything on the router's
  shared network — every routed container of every checkout — reaches it
  too. **Go build only:** a routed container is not on the shared network at
  all — it joins only its own worktree's network (previous bullet) — so it
  reaches the index only indirectly, through the router, the same way it
  reaches another checkout's own routed services; only the router and the
  index container itself sit on the shared network directly.

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
an unreachable Docker exit 1.

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
repairs nothing; it reports.

`worktree unlock` writes nothing to stdout: what it verified and what it
cleared are notices on stderr. A refusal is an error that exits 1 and clears
nothing: a holder that may be alive, a process it left running, or a lock that
changed while it looked.

Every failure the tool itself reports ends in a non-zero exit code and a
`[berth] ERROR:` line on stderr saying why; a bad command line is answered by
the argument parser in its own format. Set `BERTH_DEBUG=1` to see the stack
trace behind a failure. A failing hook's own exit code becomes berth's — see
[docs/hooks.md](docs/hooks.md) — and a verb that is registered but not
implemented exits 2.

The stream split and the state vocabulary above are both recent, and pre-1.0
means neither is a promise yet. What changed: the notices moved off stdout;
`config validate` lost its `[berth]` prefix there, counts only the hooks that
run, and adds a line naming the ones nothing invokes; `router status` reports
an unreachable Docker on stderr rather than as an `unknown:` line on stdout,
and says `degraded` for a router that is running but not as expected — a word
it did not have, and the one case where it exits non-zero while a router is up;
`config show` escapes non-ASCII (the JSON still parses back identically); and
`worktree list` renamed its `orphaned` state to `missing`. Anything scripted
against the old shapes needs a look.

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

**Docker Desktop file sharing and the file provider (Go build only).** Routes
come from files `up`, `down` and `worktree remove` write and remove under the
router's own state directory, watched by Traefik's file provider inside the
container. On macOS Docker Desktop, both file-sharing backends (virtiofs, the
current default, and the older grpcfuse) bridge the host's filesystem
events into the container, and that bridge can occasionally miss the rename
half of the atomic write a fragment update makes — the container's watcher
then keeps routing (or fails to route) what the file on disk no longer says.
If a route looks stale after an `up`, `down` or `worktree remove` on such a
host, run `X router down` then `X router up` (with `X` the name you invoked
this build as; see "Calling it something else") to force Traefik to reread
every fragment from a clean start, rather than waiting on the next
filesystem event that touches it.

**Rootless Docker.** Not supported yet: the socket proxy mounts
`/var/run/docker.sock`, which a rootless daemon does not provide. **Go build
only:** the router itself no longer needs the socket at all (see
[Router](#how-it-works) above), which removes this specific blocker for the
router stack; whether the rest of the tool works unmodified against a
rootless daemon is untested and not claimed here.

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

`worktree unlock [slug|uuid]` clears a worktree's stale locks only when all of
this holds:

- the lock's record belongs to the lock directory beside it;
- its holder ran on this machine (known by its machine ID, or by its boot ID
  in a container image that has none), in this boot and this PID namespace;
- the holder's PID no longer exists, or now belongs to a later process;
- the lock stays silent through a second look of its own, about 12 seconds;
- no command started for the worktree is still running on this machine:
  nothing with its Compose project on the command line, or its
  `BERTH_WORKTREE_ID` in the environment.

A stopped holder (`Ctrl-Z`) is alive, and is refused. A holder it cannot
check — on another machine or in a container, one that left no record, or one
it cannot look past because other users' processes are hidden from you
(`/proc` mounted with `hidepid`, say) — is refused too, unless you pass
`--holder-unverifiable` once you have made sure yourself; that never overrides
a running holder or a command left running. The unlock command that lock
notices and `doctor` print names the worktree by its UUID: it never changes, no
other worktree shares it, and it works even for a worktree whose first `up`
died before it registered.

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
`down` reconciles them. What it did check, it prints.

## Development

Node 24 — `engines` is `>=24.14.1 <25`, because the tool runs TypeScript
directly on Node's own type stripping. The project uses pnpm; the
`packageManager` field pins the version for Corepack.

```bash
corepack enable
pnpm install
pnpm check                # lint + typecheck + unit and CLI tests
pnpm test:integration     # real Git worktrees and real subprocesses
```

The TypeScript source runs directly on Node 24 — there is no build step yet.
Shipping a self-contained binary is on the roadmap.

A Go build is also under development, in `cmd/` and `internal/`; it is not
yet the one to install. Don't use it on a machine where you use the
TypeScript build: their locks do not exclude each other. See
[`CONTRIBUTING.md`](CONTRIBUTING.md) for how to build and test it.

### Layout

```
src/
  brand.ts              the product name and the presentation/storage split
  cli/main.ts           entry point; the only place argv is read for a name
  config/               berth.toml schema and discovery
  hooks/                hook protocol and sequencer
  checkout-index/       per-worktree cards, the snapshot, and the rendered page
  commands/             the everyday verbs as plain functions over a context
  router/               the shared router: its Compose stack as constants, and the verifier
  runtime/              the Runtime interface, an in-memory backend, the Compose backend
  worktree-registry.ts  identity markers, slugs, the registry
  operation-lock.ts     scoped locks and explicit stale recovery
  layout.ts             where everything lives under the control home
  files.ts              owner-only, symlink-refusing filesystem primitives
  text.ts               the filter every printed line goes through
tests/
  unit/                 hermetic; no Docker, no real Git
  cli/                  the CLI as a real process: which stream carries what
  integration/          real Git worktrees, cross-process locks, and a real Docker daemon
```

## Roadmap

Shipped, behind the `Runtime` interface in `src/runtime/runtime.ts`:

- **Compose runtime** — `up`/`down` that stamp every resource with
  `<namespace>.managed`, `.repository-uuid`, `.worktree-uuid`; `inventory`
  as a label-filtered `docker ps` that verifies the full ownership tuple on
  every resource before returning any; `reap` that verifies again before
  removing.
- **`up` / `down` / `status` / `urls`** — `up` and `down` under the worktree
  lock, `up` with interrupts deferred until it can be released cleanly.
- **`worktree list` / `worktree remove`** — ownership-verified teardown
  in crash-safe order; checkouts Git no longer lists are reported missing.
- **Shared router** — one Traefik instance across every worktree of every
  repository, behind a hardened socket proxy, verified after every start;
  routes generated from `[[service]]`. The file provider is enabled from day
  one (watching `<control home>/router/dynamic`) so hybrid
  native-plus-container worktrees can be routed without touching the router.
- **Checkout index** — `berth.localhost`: every registered worktree on the
  machine, grouped by domain, with the projects on it named in the heading, with per-service links and status.
- **`worktree unlock`** — clears a dead holder's worktree locks once the
  holder, and every command it started on this machine, is checked gone (see
  [Lock recovery](#lock-recovery)); `doctor`
  reports every held and stale lock.
- **`doctor`** — one read-only report: Node, Git, Docker and Compose against
  the versions this tool needs; the control home and every registration
  (unreadable records, a hostname claimed twice, the index's own hostname);
  the router's verification, and any rule two routed containers share, read
  from Docker so it spans control homes; and, for this checkout, its `verify`
  hook and any registration of its repository whose checkout Git no longer lists.

Not built yet, in rough order. `dev` is registered and refuses by name rather
than pretending; the rest are not commands at all. The config file already
accepts the `backup` and `restore` hooks, so a repository can commit them once.

1. **`dev`** — start the worktree and follow its services' logs, holding the
   development-session lock so `worktree remove` refuses while a session is
   attached.
2. **Native routes** — `berth route` writing file-provider fragments for
   processes running outside Docker.
3. **Standalone cleanup binary** — content-addressed, installed outside
   the checkout, so removing a worktree never imports code from the branch
   being deleted.
4. **Binary distribution** — Node SEA or bun-compiled per-platform builds.
5. **`init`** — write a starter `berth.toml`. Until then, copy
   `berth.example.toml` to the repository root.
6. **`backup` / `restore`** — invoke the hooks of the same name with a bundle
   directory.
7. **`rename`** — change a worktree's slug, or the product's own storage
   names, by migrating the state that carries them. Both are migrations: a
   control home, identity markers and Docker labels all embed the old name.

## License

Apache-2.0. See [LICENSE](LICENSE).
