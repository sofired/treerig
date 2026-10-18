# Routing

How a request for a worktree's hostname reaches its service: pinned ports, the shared router and its network modes.

## Ports

**Ports.** Each routed service also gets a port of its own on
`127.0.0.1`, allocated when `up` first needs it, recorded in the worktree's
registration and published by the override `up` generates. It is *pinned*: a
container restart, `down` and `up`, or a reboot leaves it where it was, and
the same worktree and service pick the same port again if the record is ever
lost. A port comes from a range (20000-29999 by default, `[ports] range` in
`settings.toml`), starting at a hash of the worktree and the service and
probing forward, past every port in any registration of this control home and
any port something on the machine is already using. The router still reaches
the service by its hostname exactly as before; the port is for tools that
must bypass it, and reaches them as `TREERIG_SERVICE_PORT_<NAME>` and the
`port` key of `urls --json` and `status --json`. `up` refuses to start when a
pinned port is held by anything but this worktree's own container, proven by
its ownership labels rather than its name, and says who holds it: another
treerig worktree, a container by name, or another process. `treerig up
--reallocate-port <service>` gives that service a new port (and says its
loopback URL changed); it refuses while the worktree's own container holds
the current one. Removing a service from `treerig.toml` releases its port on
the next `up` that starts every service, once Compose has removed the dropped
service's container; an `up` that names some services keeps that port
reserved until the next full `up` or `worktree remove`. `worktree remove`
releases the rest once the worktree's containers are gone. Two worktrees on different control homes can pick the
same port at once (each home has its own registry lock); the second `up`'s
clean bind error catches it, and `up` names the holder. A routed service
with `network_mode: host`, `network_mode: service:…` or more than one replica
is refused, because it has no port treerig can publish; `doctor` warns on a
routed service whose Compose file also publishes a fixed host port of its
own.

`[ports] range` is where each routed service's pinned `127.0.0.1` port comes
from (see [Ports](#ports) above). The default, 20000-29999, sits below Linux's
ephemeral range (from 32768) and macOS's (from 49152), so the operating
system does not hand those ports out on its own. A range of your own should
stay clear of them too, and on Linux of anything in `ip_local_port_range`
and `ip_local_reserved_ports` (`sysctl net.ipv4.ip_local_port_range
net.ipv4.ip_local_reserved_ports`). Widen it when a range fills up; narrowing
it never moves a port already allocated, only where new ones come from. A
scratch treerig root (`TREERIG_HOME`) set up for testing should give itself a
range of its own, so it never takes a port a real worktree of yours holds.

Publishing the pinned port is part of a routed service's Compose
configuration, so the first `up` that gives a service one changes that
configuration and Compose recreates the service's running containers once,
keeping their volumes. Every later `up` writes the same configuration and
recreates nothing.

When a port is held by something that is not a container, `up` and `doctor`
say "another process" and how to find it: `ss -ltnp 'sport = :<port>'` on
Linux, `lsof -nP -iTCP:<port> -sTCP:LISTEN` on macOS. On Docker Desktop and
OrbStack the daemon runs in a VM, so those tools name its backend process
rather than a container; `docker ps --filter publish=<port>` finds the
container.

## Router

**Router.** One Traefik proxy container per machine, started on first `up`,
shared by every worktree of every repository. It serves a single loopback
port and routes by hostname, and every hop to a service is a loopback hop: a
request for a routed service goes to `treerig-loopback:<port>`, a name that
resolves to this machine's loopback, where the service's pinned port (see
*Ports*) is published on `127.0.0.1`. No Docker network is shared between
worktrees and the router, and no host firewall sits between them, so routing
works under ufw's default-deny policy. Routes are read only from Traefik's
file provider: `up` writes one dynamic-configuration file per routed service
into a directory mounted read-only into the container, and `down` and
`worktree remove` remove it again. The checkout index is routed the same
way, on a loopback port of its own. A route file is judged only when this
control home holds a record for its repository and worktree. If that record
holds no port for the service the file is removed, and if it holds another one
the file is rewritten to it, by the next `up` or `router up`, and `doctor`
reports both, because a port can be reused and a stale file would otherwise
send an old hostname to another worktree's service. A file this control home
holds no record of may belong to another control home (the same repository
used under two): `doctor` counts it and says to delete it from the router's
`dynamic/` directory, and nothing removes it.

The proxy's network mode comes from `docker info`, not from the operating
system the CLI runs on, because the daemon decides what "localhost" means to
a container:

| Daemon | Mode | How the proxy reaches a service |
|---|---|---|
| Native rootful Linux Engine | `host` | shares the host's network namespace and binds `127.0.0.1:<port>` itself; `treerig-loopback` is `127.0.0.1` and `::1` |
| Docker Desktop, OrbStack | `bridge` | listens on `:80` inside its container, published on `127.0.0.1:<port>`; `treerig-loopback` is the engine's `host-gateway`, which forwards to the host's loopback (verified on macOS) |

Rootless Docker (its "host" network is RootlessKit's, not the machine's),
a daemon on another machine (`DOCKER_HOST` or a Docker context pointing
away) and anything treerig does not recognise are refused with a message
saying why. `[router] mode` in `settings.toml` (`"host"` or `"bridge"`)
overrides the choice, and is the way through the refusal for an engine treerig
does not recognise; it does not lift a refusal for a rootless, remote or
Windows-containers daemon. Colima, Lima and Rancher Desktop are refused by
their engine name on every client. For them on macOS use `mode = "bridge"`
(the engine forwards `host.docker.internal` to this machine; untested), and use
`"host"` only for a Linux engine on this machine's own kernel. A Linux client
reaching a VM engine over a forwarded socket or an SSH tunnel cannot be told
apart from a native engine and gets host mode. `router status` and `doctor`
report the mode the running container was started in, read from a label on it
(`doctor` says `mode set by settings.toml` when the override is in effect, and
warns when detection would pick another). A change of mode or of the router
port recreates the proxy.

In host mode the proxy runs as root inside its container only when the router
port is below 1024, because a non-root user cannot bind it; with a higher port
it runs as `nobody` with no capability at all. Either way its root filesystem
is read-only, every capability is dropped (it keeps only `NET_BIND_SERVICE`: in
host mode only when the port needs it, in bridge mode always), `no-new-privileges`
is set and the image is pinned by digest. It has no Docker socket and no path to the Docker API, so
nothing about routing depends on a container label. Traefik's ping, the
proxy's own health check, has an entrypoint of its own, never the router's own,
so a routed service's `/ping` is never shadowed. In host mode it is on a
loopback port, allocated once from the same range as routed services' ports and
kept in `router/state.json` under the treerig root beside the index's port; in
bridge mode it is a fixed port inside the container, never published, and takes
none from the range. There is no
separate verification step: Compose recreates the proxy whenever its
configuration changes, and `router status` reports whether it is running and
healthy, with the container's last log lines when it is not. Before starting,
treerig checks that the router, index and (host mode) ping ports are free and
names whatever holds one; `doctor` makes the same check while the router is
absent or unhealthy.

The router belongs to the treerig root that started it, which a label on its
containers says: `up` or `router up` from another root refuses to take it over,
and `router down` hands it on (see [Router ownership](#router-ownership)). A
running router with a layout this build does not share is refused until
`treerig router down` stops it, and `router status` and `doctor` give the same
advice.

## Router ownership

The treerig root also holds the shared router's own bookkeeping: `router/`,
with the dynamic-configuration and index directories the file provider
watches, and the router's own lock. The router *container*, though, is one
per Docker daemon regardless of treerig root — its Compose project name never
changes — and its containers carry a label naming the treerig root that
started them. `up` or `router up` from a different `TREERIG_HOME` refuses while
that router is running, naming the root it belongs to, rather than recreating
it against its own bookkeeping; a router carrying no such label is refused
the same way. Taking a router over is an explicit step: `router down` stops
it from any root, and the next root's `up` starts its own. `doctor` reports
the owner. Every control home that shares one treerig root shares that
bookkeeping too, rather than taking turns recreating it.

## How a service gets a route

Each `[service.<name>]` table becomes a route: the tool publishes the service's
pinned loopback port through the generated override and writes the
file-provider route file that sends the hostname to it, so your Compose file
never mentions the router. Chrome and Firefox resolve `*.localhost` to the loopback address on
their own; everything else depends on the system resolver — see
[Platform notes](platforms.md). If port 80 is taken, set `[router] port`
in `settings.toml` (see [Settings](settings.md)) or
`TREERIG_ROUTER_PORT` — the printed URLs follow it.

```bash
treerig router status       # is the shared router running and healthy, and in which mode?
treerig router up           # start it without starting a worktree
treerig router down         # stop it; worktree services keep running, unrouted
```
