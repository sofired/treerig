# treerig

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
> Docker Compose: `up`, `dev`, `down`, `status`, `urls`, `run`, `backup`,
> `restore`, `worktree list`, `worktree remove`, `worktree rename`,
> `worktree unlock`, and `doctor` — identity, locks, the registry, the hook
> sequencer, a runtime that stamps every resource with ownership labels and
> refuses to touch anything it cannot prove it owns, a shared hardened router
> that makes every worktree reachable at its own hostname, and a machine-wide
> index page listing every checkout. See [Roadmap](#roadmap).

## Documentation

The reference material is in `docs/`:

- [Install](docs/install.md): every install route, verifying a download, shell completion, calling the binary something else
- [Switching from a build before the rename](docs/upgrading-from-a-pre-rename-build.md)
- [Getting started](docs/getting-started.md): the config file, the first `up`, what `up` creates
- [How treerig works](docs/concepts.md): identity, slugs, locks, homes, hooks, backup and restore
- [Configuration](docs/configuration.md): the keys of `treerig.toml`, editor support, TOML syntax
- [Settings](docs/settings.md): the treerig root, the control home, `settings.toml`
- [Trust model](docs/trust-model.md) and [SECURITY.md](SECURITY.md)
- [Routing](docs/routing.md): pinned ports, the shared router, its network modes
- [Worktrees](docs/worktrees.md): development sessions, removal, renaming
- [Status words](docs/commands.md): what `status`, `worktree list` and the index print
- [Checkout index](docs/checkout-index.md): the page at `http://treerig.localhost`
- [Shared services](docs/shared-services.md)
- [Host routes](docs/host-routes.md)
- [Hooks](docs/hooks.md): the hook protocol
- [Teardown pin](docs/teardown-pin.md)
- [Output and exit codes](docs/output-and-exit-codes.md) and [machine-readable output](docs/json-output.md)
- [Doctor](docs/doctor.md): what it checks, and the check ids
- [Platform notes](docs/platforms.md) and [lock recovery](docs/lock-recovery.md)
- [Contributing](CONTRIBUTING.md), including the repository layout, and the [design notes](docs/design/)

## Install

No release exists yet, so build treerig from source (Go, at least the version
[`go.mod`](go.mod) names):

```bash
git clone https://github.com/sofired/treerig && cd treerig
go install ./cmd/treerig   # installs $(go env GOPATH)/bin/treerig (or $GOBIN)
```

Homebrew, the install script, the Linux packages and every other option are in
[docs/install.md](docs/install.md).

### Verifying a download

How to check a release download is in
[docs/install.md](docs/install.md#verifying-a-download).

## Quick start

The tool is tested with Docker Compose 2.21 and newer (`treerig doctor` checks,
as does `docker compose version`). From the root of your repository,
`treerig init` writes a starter `treerig.toml` there (the file is commented, names
your Compose file, and opens with the schema directive described under
[Editor support](docs/configuration.md#editor-support)); it refuses to replace one that exists.
Or write the file yourself and commit it at the root:

```toml
[project]
name   = "acme"
domain = "acme.localhost"          # worktrees route as <slug>.acme.localhost

[compose]
file = "docker/compose.yaml"

[service.api]                      # a service is a table named by its Compose service
                                   # (no subdomain: routes at the worktree host root)
[service.api.index]
label   = "API"
purpose = "REST surface"

[service.mail]
subdomain = "mail"                 # mail.<slug>.acme.localhost

[hooks]
prepare = "./scripts/migrate.sh"   # services healthy, nothing seeded
seed    = "./scripts/seed.py"      # only after prepare exits zero
verify  = "./scripts/check.sh"     # run by doctor; exit 0 means healthy
backup  = "./scripts/backup.sh"    # treerig backup: writes a bundle into the directory it is given
restore = "./scripts/restore.sh"   # treerig restore: loads one back

[hooks.timeout]                    # optional: no entry, no limit
prepare = "30m"                    # a hung migration fails `up` after half an hour
verify  = "2m"                     # and a hung check fails `doctor` after two minutes

[commands]
plan = "./scripts/keycloak.sh plan"   # on demand, with `treerig run plan`
```

Then:

```bash
treerig config validate     # parse and check the file
treerig config schema       # print its JSON Schema, for editors
treerig up                   # register this worktree, start its services, run prepare (+ seed the first time)
treerig dev                  # the same, then follow its logs until Ctrl-C
treerig status               # per-service state
treerig urls                 # print its routed URLs
treerig route add web --port 5173 --subdomain app   # route a dev server on the host at app.<slug>.<domain>
treerig route ls             # this worktree's host routes
treerig run plan             # run one of the project's own [commands], on the terminal
treerig backup [dir]         # run the backup hook, writing a bundle into dir (default: under the worktree's home)
treerig restore <dir>        # run the restore hook on a bundle directory
treerig down                 # stop it; data is kept
treerig shared up            # start the shared services, or replace one, and reconnect every worktree
treerig shared status        # their state, addresses and whether this worktree is connected
treerig shared down          # stop them; their named volumes are kept
treerig worktree list        # every worktree of this repository, with state
treerig worktree remove      # tear it down, verified by ownership
treerig worktree rename NEW  # change its slug, and with it its hostname
treerig worktree unlock      # clear a dead holder's lock, once it is checked gone
treerig doctor               # check prerequisites, registry, router; run verify
```

`up` runs the branch's own build and hooks. Read [Trust model](#trust-model)
before bringing up a branch you did not write.

A full annotated example is in [`treerig.example.toml`](treerig.example.toml).
The hook contract is specified in [`docs/hooks.md`](docs/hooks.md).

## Trust model

To report a vulnerability, see [`SECURITY.md`](SECURITY.md).

Bringing up a worktree runs the branch's code. `up` builds whatever its
`compose.yaml` builds and runs the hooks named in its `treerig.toml`, as you,
with your full environment — the same trust a `make` or an `npm install` in
that checkout asks for. Do not bring up a branch you would not run. `treerig
run` asks the same of a branch's `[commands]`, and gives them your terminal
too.

The rest of the model, including what other local users can reach by design and
what treerig does guarantee, is in [docs/trust-model.md](docs/trust-model.md).

## Roadmap

Shipped:

- **Compose runtime** — `up`/`down` that stamp every resource with
  `<namespace>.managed`, `.repository-uuid` and `.worktree-uuid` (and `.format`
  on containers, networks and images, not on named volumes); inventory and
  teardown that verify the full ownership tuple on every resource before
  touching it.
- **`up` / `down` / `status` / `urls`** — under the worktree lock, with
  interrupts deferred until it can be released cleanly.
- **`worktree list` / `worktree remove`** — ownership-verified teardown in
  crash-safe order, run from a content-addressed copy of the binary
  installed outside any checkout; checkouts Git no longer lists are reported
  missing.
- **Host routes** — `route add|rm|ls` send a hostname of the worktree to a
  process on the host through the same router, recorded in the registration;
  `up` and `down` keep them and `worktree remove` removes them.
- **Shared router** — one Traefik proxy across every worktree of every
  repository, routed entirely through its file provider (no Docker-socket
  access at all) to services on pinned loopback ports, in host network mode on
  a native Linux Engine and bridge mode on Docker Desktop and OrbStack, and
  owned by the treerig root that started it: another root's `up` refuses rather
  than taking it over.
- **Settings file** — `settings.toml` under the treerig root, starting with
  the router's port; `router status` and `doctor` say where the port came
  from.
- **Checkout index** — `treerig.localhost`: every registered worktree on the
  machine, grouped by domain, with per-service links and status.
- **`worktree unlock`** — clears a dead holder's locks (a worktree's own,
  or, with `--registry`/`--router`, the registry's or router's) once it is
  checked gone; `doctor` reports every held lock, and any left unreleased by
  a holder that is gone.
- **`run`** — the project's own named `[commands]`, run on the terminal
  against a registered worktree: they can prompt, answer Ctrl-C, and hold
  the worktree's lock until everything they started has exited.
- **`dev`** — `up`, then the worktree's logs followed until Ctrl-C, holding
  the development-session lock for the whole session so `worktree remove`
  refuses while one is attached.
- **`backup` / `restore`** (#10) — run the hooks of the same name against a
  registered, running worktree with a bundle directory as their first
  argument, under the worktree's lock, exiting with the hook's own status.
- **`worktree rename`** (#11) — change the current worktree's slug and
  hostname; refused while it has containers, and its data, volumes and pinned
  ports stay, keyed by its ID.
- **Shared services** (#123) — `[shared]`: a service such as an email catcher
  that runs once per repository, connected to each worktree that uses it by
  its name, with its definition checked for drift, its ports pinned on
  `127.0.0.1`, a routed web UI at `<name>.shared.<domain>`, and `shared up`,
  `shared down` and `shared status` to run it yourself.
- **`init`** (#9) — writes a commented starter `treerig.toml` at the Git
  checkout's root, with the schema directive and the Compose file it finds
  there, and never replaces an existing one.
- **`teardown pin --release` / `--latest` / `--file`, `teardown status`** (#85) —
  pin a release verified with cosign, or a treerig binary you verified by its
  sha256, as the teardown binary, and check the pin; see
  [Teardown pin](docs/teardown-pin.md). `worktree remove` does not run it yet.
- **`doctor`** — one read-only report: Git, Docker and Compose against the
  versions this tool needs; the treerig root and every registration
  (unreadable records, a hostname claimed twice, the index's own hostname);
  the router's status, mode and loopback-only bind and a probe of each route;
  and, for this
  checkout, whether every configured hook and command resolves to something
  runnable,
  its `verify` hook, and any registration of its repository whose checkout
  Git no longer lists.

Not built yet, in rough order; apart from the pin commands in item 2, none of
these is a command at all.

1. **Migrating stored state between releases** (#29) — every store already
   carries a format version and refuses a newer one
   ([`docs/design/state.md`](docs/design/state.md)); a migration comes with the
   first change to a store's format after a release.
2. **Running `worktree remove` from a pinned, trusted binary** (#85, #146) —
   the stronger guarantee the relocated-copy mechanism ([Worktrees](docs/worktrees.md#removing-a-worktree)) is a step toward.
   The pin, its commands and cosign-verified release pinning are built
   ([Teardown pin](docs/teardown-pin.md)); the gate that makes removal run the pinned
   binary is not. Tagged releases, with the names and
   signing identity in [`docs/design/release.md`](docs/design/release.md), are
   what it will pin to.

## License

Apache-2.0. See [LICENSE](LICENSE).
