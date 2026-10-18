# Trust model

What running `treerig up` on a branch trusts, and what treerig guarantees.

To report a vulnerability, see [`SECURITY.md`](../SECURITY.md).

Bringing up a worktree runs the branch's code. `up` builds whatever its
`compose.yaml` builds and runs the hooks named in its `treerig.toml`, as you,
with your full environment — the same trust a `make` or an `npm install` in
that checkout asks for. Do not bring up a branch you would not run. `treerig
run` asks the same of a branch's `[commands]`, and gives them your terminal
too.

Five things follow from that, and this tool does not change them:

- **Hooks and builds see your environment.** Nothing is allow-listed; a hook
  is a program you pointed the config at, and it runs with everything you
  have, `COMPOSE_*` included. Compose's own invocation is the one exception:
  `COMPOSE_FILE`, `COMPOSE_PATH_SEPARATOR`, `COMPOSE_ENV_FILES` and
  `COMPOSE_DISABLE_ENV_FILE` are dropped from it, so a shell setting cannot
  change which files or `.env` it loads, and so is `COMPOSE_IGNORE_ORPHANS`,
  so it cannot turn off the orphan sweep `--remove-orphans` relies on.
  Dropping them from the environment does not reach the Compose file's own
  `.env`, which Compose reads for itself — see
  [Configuration](configuration.md). `COMPOSE_PROGRESS` is always set to
  `plain`, whatever the shell says. Every other `COMPOSE_*` variable, project
  name and profiles included, reaches Compose unchanged: the project name is
  always overridden by the explicit `--project-name` flag Compose gives
  precedence over any inherited value, and a profile a shell setting
  activates still gets every resource it creates labeled, because the same
  environment governs both the config read that writes those labels and the
  `up` that follows it. Naming a service on `up` whose profile is not active
  is refused rather than started unlabelled, since Compose would otherwise
  enable that profile for it.
- **A routed service's loopback port is open to every local user.** It is
  published on `127.0.0.1` only, never on the network, but any local user or
  process can connect to it, as they can to the router's port. It bypasses
  the router, so anything the router would have decided about a request is
  not in the way. On Docker Desktop and OrbStack, containers can also reach
  the host through `host.docker.internal`, so any container can reach a
  routed loopback port directly.
- **In host mode the proxy shares the host's network namespace.** It binds
  only `127.0.0.1:<port>` and its ping port, never a wildcard address, and
  `doctor` checks that no other local address answers on the router's port.
  It also shares the host's abstract unix sockets. It has no Docker socket and
  no path to the Docker API, runs with its root filesystem read-only and every
  capability dropped (bridge mode, and host mode on a privileged port, keep
  `NET_BIND_SERVICE`), and can reach any loopback port on the machine, which is
  its job.
- **No network is shared between worktrees and the router.** A routed
  service stays on its own project's networks; the proxy reaches it through
  its published loopback port, not through a network. Containers you do not
  declare stay on their project's own networks too.
- **A shared service is on every using worktree's network.** Its container is
  connected to each such worktree's own network, so every container on one of
  those networks can reach it, and it can reach all of them. That is the point
  of sharing it, mail from every worktree landing in one inbox, and it is not
  an isolation boundary between the worktrees: they still do not resolve each
  other's services by name, but on OrbStack a container that knows another
  Compose network's container IP can reach it whether or not a service is shared.
  Its declared ports are also published on `127.0.0.1`, open to every local
  user like a routed service's.
- **The index page is machine-wide and unauthenticated.** It lists every
  checkout's branch, path, hostnames and service links. It is served through
  the proxy like any route, and from a loopback port of its own that any
  local user can reach.

What the tool does guarantee is ownership: every resource it creates carries
its labels, and it refuses to touch anything whose full ownership tuple (the
managed, repository and worktree labels, in the format the `dev.treerig.format`
label names) does not match. Named volumes carry no format label, so a volume
is proven by its other ownership labels. That is what keeps `worktree remove` from deleting something it did
not create — not a claim about what your branch's code can do once it runs.
