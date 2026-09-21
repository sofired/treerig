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
> `worktree remove` — identity, locks, the registry, the hook sequencer, and a
> runtime that stamps every resource with ownership labels and refuses to
> touch anything it cannot prove it owns. The shared router and the checkout
> index are the next ports, so the URLs `urls` prints are not yet reachable.
> See [Roadmap](#roadmap).

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
symlinks anywhere in the chain before creating or removing anything.

**Hooks.** `prepare`, `seed`, `verify`, `backup`, and `restore` are
executables you point at from the config file. `seed` never runs unless
`prepare` exited zero. The tool never learns what they do.

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
| `[[service]]` | `name`, optional `subdomain` (empty = host root), optional `path`, optional `index` presence. |
| `[hooks]` | `prepare`, `seed`, `verify`, `backup`, `restore` — commands relative to the config file. |

Unknown keys are rejected, so a typo cannot silently disable a feature.

The control home is `~/.berth`, overridable only through the `BERTH_HOME`
environment variable — deliberately not a config-file setting. The config file
is committed and shared; a path that is only correct on one machine has no
business in it, and one developer pointing it at a synced folder would put the
whole team's state on iCloud Drive.

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
  commands/             the everyday verbs as plain functions over a context
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

Next, in order:

1. **Shared router** — one Traefik instance across every worktree of every
   repository, behind a hardened socket proxy, with the file provider enabled
   from day one so hybrid native-plus-container worktrees stay possible.
2. **Checkout index** — a live page listing every registered worktree on the
   machine with per-service status.
3. **Standalone cleanup binary** — content-addressed, installed outside
   the checkout, so removing a worktree never imports code from the branch
   being deleted.
4. **Binary distribution** — Node SEA or bun-compiled per-platform builds.

## License

Apache-2.0. See [LICENSE](LICENSE).
