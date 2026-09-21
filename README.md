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
underneath you. Reclaiming a bare name is an explicit rename, never a side
effect.

**Locks.** Every operation on a worktree holds an advisory lock with an
effectively infinite stale window and a heartbeat, so a twenty-minute build is
never silently stolen. Recovery from a genuinely dead holder is an explicit
command, never automatic.

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
executables you point at from the config file. `seed` never runs unless
`prepare` exited zero. The tool never learns what they do.

**Router.** One Traefik instance per machine, started on first `up`, shared
by every worktree of every repository. It binds a single loopback port and
routes by hostname. It never sees the Docker socket: it reads container
labels through a proxy that exposes only the read-only subset of the API a
router needs and refuses every write. All three containers — the proxy,
Traefik, and the BusyBox `httpd` that serves the index page — run read-only
with every capability dropped (Traefik gets back only the one it needs to bind
port 80), pinned by image digest, and after every start the tool inspects
them and refuses to continue if anything differs from what it asked for.

## Quick start

Commit a `berth.toml` at the root of your repository:

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
verify  = "./scripts/doctor"       # folded into `berth doctor`
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
```

`up` starts the Compose project under a name derived from the worktree's
UUIDs, with every container, volume, and network labelled
`dev.berth.managed`, `dev.berth.repository-uuid`, and
`dev.berth.worktree-uuid`. Your Compose file is never asked to add those
labels; a generated override file does it, so forgetting one — and silently
exempting a container from cleanup — is not possible. The Compose file sees
the same `BERTH_*` variables hooks do, so `${BERTH_WORKTREE_HOME}` works in a
bind mount exactly as it does in a seed script.

Each `[[service]]` becomes a route: the tool generates the Traefik labels and
joins the container to the shared router network through the same override,
so your Compose file never mentions the router. Chrome and Firefox resolve
`*.localhost` to the loopback address on their own; everything else depends
on the system resolver — see [Platform notes](#platform-notes). If port 80 is
taken, set `BERTH_ROUTER_PORT` — the printed URLs follow it.

```bash
berth router status       # is the shared router running, and is it what this version expects?
berth router up           # start it without starting a worktree
berth router down         # stop it; worktree services keep running, unrouted
```

**Index.** `http://berth.localhost` (plus the port, if changed) lists every
checkout on the machine across every project: branch, hostname, path, whether
its services are running, and a link for each service that opted in with
`[service.index]`. It is a static page served from the router stack,
regenerated by `up`, `down`, and `worktree remove` — or by `berth index`,
which also prints it as text — and it reloads itself in the browser.

`worktree remove` is the only destructive verb. It inventories everything
the worktree owns, verifies the full ownership tuple on every resource, and
refuses the whole operation if a single one fails — before it asks you to
type the hostname to confirm (`--yes` skips the prompt). Then, in an order
chosen so that an interrupted removal can simply be run again: Docker
resources, the home directory, the registration, the identity marker.
`berth worktree remove <slug|uuid>` removes another worktree, including one
whose checkout is already gone.

A full annotated example is in [`berth.example.toml`](berth.example.toml).
The hook contract is specified in [`docs/hooks.md`](docs/hooks.md).

## Calling it something else

Install or symlink the binary under whatever name you like. Help text, the
log prefix, and every "run `X urls`" hint follow the invoked name:

```bash
ln -s /usr/local/bin/berth /usr/local/bin/duke
duke --help               # Usage: duke [options] [command]
```

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
| `compose.file` | Compose file, relative to `berth.toml`. |
| `[[service]]` | `name`, optional `subdomain` (empty = host root), optional `path` (route only that prefix, so services can share a host), optional `port` (defaults to the first port the service exposes), optional `index` presence. |
| `[hooks]` | `prepare`, `seed`, `verify`, `backup`, `restore` — commands relative to the config file. |

Unknown keys are rejected, so a typo cannot silently disable a feature.

The control home is `~/.berth`, overridable only through the `BERTH_HOME`
environment variable, and the router's loopback port is 80, overridable only
through `BERTH_ROUTER_PORT` — deliberately not config-file settings. The config file
is committed and shared; a path that is only correct on one machine has no
business in it, and one developer pointing it at a synced folder would put the
whole team's state on iCloud Drive.

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

**Rootless Docker.** Not supported yet: the socket proxy mounts
`/var/run/docker.sock`, which a rootless daemon does not provide.

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

## Development

Node 24 or newer. The project uses pnpm; the `packageManager` field pins the
version for Corepack.

```bash
corepack enable
pnpm install
pnpm check                # lint + typecheck + unit tests
pnpm test:integration     # real Git worktrees and real subprocesses
```

The TypeScript source runs directly on Node 24 — there is no build step yet.
Shipping a self-contained binary is on the roadmap.

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
tests/
  unit/                 hermetic; no Docker, no real Git
  integration/          real Git worktrees, cross-process locks, and a real Docker daemon
```

## Roadmap

Shipped, behind the `Runtime` interface in `src/runtime/runtime.ts`:

- **Compose runtime** — `up`/`down` that stamp every resource with
  `<namespace>.managed`, `.repository-uuid`, `.worktree-uuid`; `inventory`
  as a label-filtered `docker ps` that verifies the full ownership tuple on
  every resource before returning any; `reap` that verifies again before
  removing.
- **`up` / `down` / `status` / `urls`** — with the worktree lock held and
  interrupts deferred until it can be released cleanly.
- **`worktree list` / `worktree remove`** — ownership-verified teardown
  in crash-safe order; orphan detection for checkouts Git no longer lists.
- **Shared router** — one Traefik instance across every worktree of every
  repository, behind a hardened socket proxy, verified after every start;
  routes generated from `[[service]]`. The file provider is enabled from day
  one (watching `<control home>/router/dynamic`) so hybrid
  native-plus-container worktrees can be routed without touching the router.
- **Checkout index** — `berth.localhost`: every registered worktree on the
  machine, grouped by project, with per-service links and status.

Next, in order:

1. **`doctor`** — prerequisites, router verification, registry problems,
   hostname conflicts, and the `verify` hook, in one report.
2. **Native routes** — `berth route` writing file-provider fragments for
   processes running outside Docker.
3. **Standalone cleanup binary** — content-addressed, installed outside
   the checkout, so removing a worktree never imports code from the branch
   being deleted.
4. **Binary distribution** — Node SEA or bun-compiled per-platform builds.

## License

Apache-2.0. See [LICENSE](LICENSE).
