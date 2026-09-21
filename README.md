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

> **Status: scaffold.** The engine is ported and tested (identity, locks,
> filesystem safety, process control, config, the hook sequencer). The Docker
> Compose runtime, the router, the checkout index, and cleanup are stubs that
> fail loudly. See [Roadmap](#roadmap).

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
berth dev                 # start this worktree's environment (stub)
berth urls                # print its routed URLs (stub)
berth worktree remove     # tear it down, verified by ownership (stub)
```

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
  runtime/              the Runtime interface, an in-memory backend, the Compose stub
  worktree-registry.ts  identity markers, slugs, the registry
  operation-lock.ts     scoped locks and explicit stale recovery
  layout.ts             where everything lives under the control home
  files.ts              owner-only, symlink-refusing filesystem primitives
tests/
  unit/                 hermetic; no Docker, no real Git
  integration/          real Git worktrees and cross-process locks
```

## Roadmap

In order, each behind the `Runtime` interface in `src/runtime/runtime.ts`:

1. **Compose runtime** — `up`/`down` that stamp every resource with
   `<namespace>.managed`, `.repository-uuid`, `.worktree-uuid`; `inventory`
   as a label-filtered `docker ps`; `reap` that verifies the full ownership
   tuple before removing anything.
2. **Shared router** — one Traefik instance across every worktree of every
   repository, behind a hardened socket proxy, with the file provider enabled
   from day one so hybrid native-plus-container worktrees stay possible.
3. **Checkout index** — a live page listing every registered worktree on the
   machine with per-service status.
4. **Cleanup** — ownership-verified teardown, plus a content-addressed
   standalone cleanup binary so removing a worktree never imports code from
   the branch being deleted.
5. **Binary distribution** — Node SEA or bun-compiled per-platform builds.

## License

Apache-2.0. See [LICENSE](LICENSE).
