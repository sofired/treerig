<h1>
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/brand/treerig-logo-dark.svg">
    <img alt="treerig" src="docs/brand/treerig-logo.svg" width="262" height="56">
  </picture>
</h1>

[![CI](https://github.com/sofired/treerig/actions/workflows/go.yml/badge.svg?branch=main)](https://github.com/sofired/treerig/actions/workflows/go.yml?query=branch%3Amain)
[![Release](https://img.shields.io/github/v/release/sofired/treerig)](https://github.com/sofired/treerig/releases)
[![Go](https://img.shields.io/github/go-mod/go-version/sofired/treerig)](go.mod)
[![License](https://img.shields.io/github/license/sofired/treerig)](LICENSE)

**A rig for every branch.**

Every worktree gets its own stack, its own data, and its own URL.

Version 0.1.0 is the first stable release: [install](#install) it with the install script or Homebrew.

Git worktrees let you have three branches checked out at once, or three agent
sessions working in parallel. Docker Compose doesn't play along: the second
checkout fights the first for port 5432, a pinned project name or an external
volume makes both share one database, every copy needs its own hand-edited
`.env`, and the containers of the branch you deleted last week are still
running. treerig gives each worktree its
own Compose project, data and hostname. When you remove the worktree, treerig
removes the containers, networks, images and named volumes that worktree
created, and nothing else.

```console
$ treerig up                       # in ~/src/myapp, on main
web  http://main.myapp.localhost

$ git worktree add ../myapp.feature-login -b feature/login
$ cd ../myapp.feature-login && treerig up
web  http://feature-login.myapp.localhost
```

*Illustrative output: `up` also prints Compose's progress before its URL
lines.*

- **A URL per worktree**, from one shared router that listens only on
  loopback. You don't edit any ports.
- **Isolated by default.** Each worktree has its own Compose project,
  networks, volumes and data directory.
- **Safe cleanup.** `treerig worktree remove` deletes only what the worktree
  can prove it created, by its labels, never by its name. External volumes and
  the anonymous volumes an image declares are left alone.
- **One page for everything running** at `http://treerig.localhost`.
- **Your Compose file stays as it is.** A generated override adds the labels
  and ports. Migrations, seeding and health checks are small scripts in
  whatever language you already use.

> **Status: 0.1.0, the first stable release.** Linux and macOS. treerig needs Git,
> Docker and Docker Compose 2.21 or newer; the Docker integration tests run on
> Linux in CI. [Status and roadmap](#status-and-roadmap) lists what is not
> built yet.

## Install

The install script installs the latest release:

```bash
curl -fsSL https://raw.githubusercontent.com/sofired/treerig/main/install.sh | sh
```

or with Homebrew:

```bash
brew install sofired/tap/treerig
```

The script installs into `~/.local/bin` by default (set `TREERIG_INSTALL_DIR` or pass
`--dir` to choose another directory) and says how to add the install directory
to your `PATH` if it isn't there. Then check your machine with `treerig doctor`. The
[release page](https://github.com/sofired/treerig/releases/latest)
also has archives and `.deb`, `.rpm` and `.apk` packages (see
[docs/install.md](docs/install.md)). To build from source instead, you need Go,
at least the version [`go.mod`](go.mod) names:

```bash
git clone https://github.com/sofired/treerig && cd treerig
go install ./cmd/treerig   # installs $(go env GOPATH)/bin/treerig (or $GOBIN)
```

If `treerig` isn't found, add `$(go env GOPATH)/bin` to your `PATH`. See
[docs/install.md](docs/install.md) for every route, checking a download, and
shell completion.

Prefer a shorter name? This gives you `trig` (see
[The `trig` alias](docs/install.md#the-trig-alias)):

```bash
ln -s treerig "$(dirname "$(command -v treerig)")/trig"
```

### Verifying a download

Release downloads come with checksums and a signature. How to check them is in
[docs/install.md](docs/install.md#verifying-a-download).

### After installing

Run `treerig doctor` to check the machine.

## Quick start

Start in the root of a Git repository that has a Compose file.

**1. Write a config.** `treerig init` writes a commented `treerig.toml` that
already names your Compose file, with a project name and domain taken from the
directory's name:

```bash
treerig init
```

Uncomment only the placeholder `[service.<name>]` line, and rename it to the
Compose service you want in a browser. Without the comments, the file is now:

```toml
[project]
name   = "myapp"
domain = "myapp.localhost"   # worktrees route as <slug>.myapp.localhost

[compose]
file = "compose.yaml"

[service.web]                # a service from compose.yaml, routed at the worktree's host
```

A routed service needs a port: one it exposes in the Compose file, or `port`
in its `[service.<name>]` table, and the app must listen on `0.0.0.0`: see
[Writing your compose.yaml](docs/compose.md). Run `treerig config validate` to
check the file.

**2. Commit it**, so every new worktree gets it:

```bash
git add treerig.toml && git commit -m "Add treerig"
```

**3. Bring this worktree up** (`up` runs the branch's code: see
[Trust model](#trust-model)):

```bash
treerig up       # start this worktree's services, then print its URLs
```

**4. Add a second worktree and bring that up too:**

```bash
git worktree add ../myapp.feature-x -b feature/x
cd ../myapp.feature-x && treerig up
```

Open both URLs, or open <http://treerig.localhost> to see every worktree on
the machine (`treerig list --all` prints the same list).

**5. Clean up when you're done:**

```bash
treerig worktree remove   # asks you to type the hostname, then removes what the worktree created
cd - && git worktree remove ../myapp.feature-x
```

Working on treerig itself, or running a build from source? A development build
(`treerig --version` prints `0.0.0-…`) removes only with a pinned teardown binary:
see [Teardown pin](docs/teardown-pin.md#what-counts-as-a-release-build). Don't
`go build` a branch in a shell whose auto-loaded environment (direnv, mise, a
devcontainer) you haven't reviewed; install from a release.

`treerig down` stops a worktree and keeps its data; `treerig dev` is `up`
followed by its logs until Ctrl-C. A hostname that won't resolve, or port 80
already taken? See [Troubleshooting](docs/troubleshooting.md). The full
walkthrough, with hooks, is in [Getting started](docs/getting-started.md).

## How it works

- **Identity.** treerig writes an ID for the repository and for the worktree
  into the Git directory. Paths and branch names can change; the worktree keeps
  its environment.
- **Hostnames.** The branch gives the hostname: `feature/login` becomes
  `feature-login.<domain>`. A clash gets a short suffix, never a surprise.
- **Your Compose file, labelled.** `up` adds a generated override that labels
  every container, network, image and named volume with the worktree's IDs,
  and publishes each routed service on a fixed `127.0.0.1` port.
- **One router.** One shared proxy per Docker daemon routes each hostname to
  its worktree's service. It has no access to the Docker socket.
- **Hooks.** `prepare`, `seed`, `verify`, `backup` and `restore` are scripts
  you name in `treerig.toml`. treerig runs them at the right moment and never
  learns what they do.

More in [How treerig works](docs/concepts.md), [Routing](docs/routing.md) and the
[Glossary](docs/glossary.md).

## Safety

treerig deletes only what it can prove it created: every container, network,
image and named volume it labelled for the worktree. External volumes, and the
anonymous volumes an image declares, carry no labels and are left alone, so
keep data you want removed on a named volume
([Ownership labels](docs/concepts.md#ownership-labels)). `worktree remove` checks the labels on
every resource first and refuses the whole removal if one doesn't match. A
release build removes with itself. A development build, which may carry a
branch's code, removes only with a pinned, verified build and refuses without
one, so branch code does not run while it takes the worktree apart; you can pin
one on any build to keep removal on it ([Teardown pin](docs/teardown-pin.md)).
`worktree remove --dev-teardown` removes with the build you invoked, for work on
removal itself; it says so on every run.

Locks stop two commands from changing a worktree at once. A lock left by a
command that died is cleared only after treerig checks that the holder is
really gone, or you explicitly vouch for it, never because it is old.

### Trust model

`treerig up` runs the branch's own code. It builds what the branch's Compose
file builds and runs the hooks its `treerig.toml` names, as you, with your
environment: the same trust as `make` or `npm install` in that checkout. Don't
bring up a branch you wouldn't run. The full model, including what other local
users can reach, is in [docs/trust-model.md](docs/trust-model.md). To report a
vulnerability, see [SECURITY.md](SECURITY.md).

## Documentation

| Page | What it covers |
| --- | --- |
| [Install](docs/install.md) | Homebrew, the install script, packages and archives, building from source, verifying a download, shell completion, the `trig` alias, renaming the binary |
| [Getting started](docs/getting-started.md) | The config file, the first `up`, and what `up` creates |
| [Troubleshooting](docs/troubleshooting.md) | Common failures, the message you see, and the fix |
| [How treerig works](docs/concepts.md) | Identity, hostnames, locks, ownership labels, hooks, backup and restore |
| [Configuration](docs/configuration.md) | Every key of `treerig.toml`, editor support; the annotated [`treerig.example.toml`](treerig.example.toml) |
| [Writing your compose.yaml](docs/compose.md) | The variables it can use, exposing a port, listening on `0.0.0.0`, what treerig refuses, what `worktree remove` deletes |
| [Environment variables](docs/environment.md) | Every `TREERIG_*` variable you can set, and the ones your hooks and `compose.yaml` can use |
| [Settings](docs/settings.md) | The router's port, the loopback port range, and where treerig keeps its files |
| [Routing](docs/routing.md) | Pinned ports, the shared router and its network modes |
| [Worktrees](docs/worktrees.md) | Development sessions, removing a worktree, renaming one |
| [Shared services](docs/shared-services.md) | One instance of a service, such as a mail catcher, per repository |
| [Host routes](docs/host-routes.md) | Route a dev server running on the host at a worktree's hostname |
| [The overview](docs/overview.md) | The machine-wide page at `http://treerig.localhost` |
| [Hooks](docs/hooks.md) | The hook protocol |
| [State words](docs/commands.md) | What `status`, `list` and the overview print |
| [Doctor](docs/doctor.md) | What `treerig doctor` checks, and its check ids |
| [Output and exit codes](docs/output-and-exit-codes.md) | Which stream each command writes to, and how it exits |
| [Machine-readable output](docs/json-output.md) | The `--json` documents |
| [Platform notes](docs/platforms.md) | Linux, Docker Desktop, OrbStack, name resolution, unsupported setups |
| [Lock recovery](docs/lock-recovery.md) | How `worktree unlock` decides a lock is safe to clear |
| [Trust model](docs/trust-model.md) | What `up` trusts, and what treerig guarantees |
| [Glossary](docs/glossary.md) | Plain definitions of the terms the docs use |
| [Coming from an older build](docs/upgrading-from-a-pre-rename-build.md) | Cleanup for a machine that ran a build from before the rename |

The design notes explain the reasoning behind
[the config file](docs/design/config.md), [locks](docs/design/locks.md),
[releases](docs/design/release.md), [routing](docs/design/routing.md),
[stored state](docs/design/state.md) and [teardown](docs/design/teardown.md).
Every command also explains itself: `treerig <command> --help`. The docs are
also on [GitHub](https://github.com/sofired/treerig/tree/main/docs).

## Status and roadmap

Built today: the whole worktree lifecycle (`init`, `up`, `dev`, `down`,
`status`, `list`, `run`, `backup`, `restore`, and `worktree remove`,
`rename` and `unlock`), the shared router and the overview, host routes
(`route`), shared services and `doctor`; `treerig --help` lists the everyday
commands and `treerig help --all` every command. Releases are listed on the
[releases page](https://github.com/sofired/treerig/releases).

`worktree remove` refuses development builds, which includes a build from
source, that have no pinned, verified teardown binary (#146); pin one with
`treerig teardown pin --latest`. Release builds remove with themselves. See
[Teardown pin](docs/teardown-pin.md).

Not built yet:

1. **Migrating stored state between releases** (#29). Every store treerig
   reads back carries a format version, and a build refuses a newer one; no
   migration code exists yet.

## Contributing

treerig is at 0.1.0, before 1.0, and the most useful thing you can do is try it on your
own stack and open an issue when something surprises you: platform reports,
docs and examples are all welcome. Read [CONTRIBUTING.md](CONTRIBUTING.md)
first. Its safety rules, ownership before removal and no lock cleared by age,
are not negotiable.

## License

Apache-2.0. See [LICENSE](LICENSE).
