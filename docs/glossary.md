# Glossary

What the terms in these docs mean, with a link to the page that explains each one.

## Bundle

A directory that the `backup` hook fills and the `restore` hook reads. treerig marks it `.incomplete` until the hook exits zero. See [The bundle directory](hooks.md#the-bundle-directory).

## Checkout

The directory where one worktree's files are checked out: the files you edit. See [Worktrees](worktrees.md).

## Control home

The directory where treerig keeps what each worktree owns: its private directory, its locks and its backup bundles. It is `control-home/` under the [treerig root](#treerig-root) unless `TREERIG_CONTROL_HOME` moves it. See [Settings](settings.md).

## Development session

What `treerig dev` holds while it follows a worktree's logs. While it is attached, `worktree remove` and a second `dev` refuse. See [Development sessions](worktrees.md#development-sessions).

## Domain

The `project.domain` of `treerig.toml`. Each worktree is routed as `<slug>.<domain>`, and a slug is unique within its domain. See [Configuration](configuration.md).

## Hook

An executable you name in the `[hooks]` table of `treerig.toml`. treerig runs it at a fixed moment (`prepare`, `seed`, `verify`, `backup` or `restore`) and learns only its exit code. See [The hook contract](hooks.md).

## Host route

A route from a worktree's hostname to a process on your machine, such as a dev server, rather than to a Compose service. You add one with `treerig route add`. See [Host routes](host-routes.md).

## Index

The page at `http://treerig.localhost` that lists every checkout on the machine, with its status and links. `treerig index` prints the same thing as text. See [Checkout index](checkout-index.md).

## Label namespace

The prefix of the Docker labels treerig puts on what it creates: `dev.treerig` by default, so `dev.treerig.managed` and so on. The `project.label-namespace` key exists only to adopt resources that a predecessor tool created. See [Ownership labels](concepts.md#ownership-labels).

## Ownership labels

The labels that make a Docker resource a worktree's: managed, repository and worktree, plus a format label on everything except named volumes. treerig removes only what carries the full set. See [Ownership labels](concepts.md#ownership-labels).

## Pinned port

The port on `127.0.0.1` that a routed service is published on. It is allocated once and stays the same across restarts and reboots, so a tool that cannot use a hostname can reach the service. It reaches your hooks as `TREERIG_SERVICE_PORT_<NAME>`. See [Routing](routing.md#ports).

## Registration

treerig's record of a worktree: its slug, domain, pinned ports and host routes. `treerig worktree list` shows each one. See [Worktrees](worktrees.md).

## Router

The one proxy container per Docker daemon, shared by every worktree of every repository. It listens on one loopback port and sends each hostname to its worktree's service. See [Routing](routing.md#router).

## Shared service

A service that runs once per repository, in a Compose project of its own, rather than once per worktree: a mail catcher, say. It is connected to each worktree that uses it. See [Shared services](shared-services.md).

## Slug

The hostname part that identifies a worktree, derived from its branch: `feature/login` becomes `feature-login`, routed as `feature-login.<domain>`. It is unique per domain, and `treerig worktree rename` changes it. See [Slugs](concepts.md#slugs).

## Teardown pin

A treerig build that you install and verify once with `treerig teardown pin`, which `worktree remove` is meant to run instead of whichever binary you invoked. The pin exists; the removal that uses it does not yet. See [Teardown pin](teardown-pin.md).

## treerig root

The directory where treerig keeps what belongs to the machine rather than to a project: `~/.treerig` by default, or whatever `TREERIG_HOME` names. It holds `settings.toml`, the router's state and, by default, the [control home](#control-home). See [Settings](settings.md).

## Worktree

A Git worktree that treerig manages: a checkout of a branch with its own identity, Compose project, data directory and hostname. See [Identity](concepts.md#identity) and [Worktrees](worktrees.md).
