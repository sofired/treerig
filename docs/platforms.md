# Platform notes

Notes for Linux, Docker Desktop, OrbStack and other environments.

**Bind mounts on Linux.** Docker Engine enforces host file permissions inside
containers. A worktree's home directory (`${TREERIG_WORKTREE_HOME}`) is 0700, so
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
atomic write a route-file update makes — the container's watcher then keeps
routing (or fails to route) what the file on disk no longer says. If a route
looks stale after an `up`, `down` or `worktree remove` on such a host, run
`X router down` then `X router up` (with `X` the name you invoked this build
as; see [Calling it something else](install.md#calling-it-something-else)) to force Traefik to reread every
route file from a clean start, rather than waiting on the next filesystem
event that touches it.

**Host routes on Docker Desktop and OrbStack.** The proxy runs on a bridge
network there and reaches your host process through the engine's host
gateway, which is IPv4 only: bind the process to `127.0.0.1`, not `::1`. See
[Host routes](host-routes.md).

**Rootless Docker** is refused (#121): its "host" network is RootlessKit's
namespace, not the machine's, so the proxy could not reach your services over
loopback, and a port below 1024 cannot be published by default.

**Remote Docker daemons** (`DOCKER_HOST` or a Docker context pointing at
another machine) are refused: the proxy reaches your services on this
machine's loopback. An `ssh://` or `tcp://` host is remote; a socket forwarded over SSH to a local
unix path looks local and cannot be detected.

**Firewalls.** On Linux the proxy shares the host's network namespace, so a
default-deny host firewall (ufw) never sits between it and a service. On
Docker Desktop and OrbStack the proxy reaches the host's loopback through the
engine's own forwarding, which needs no firewall change on either. A service
that listens only on `::1` is reachable in host mode but not through Docker
Desktop, so bind `127.0.0.1` (or every IPv4 address).

**SELinux.** Hosts with SELinux enforcing need bind mounts relabelled (`:z`),
which the tool does not do for its own router mounts; treat such hosts as
unsupported for now.

## Hostname resolution

Chrome and Firefox map `*.localhost` to loopback
internally on every platform. Other clients — Safari, `curl`, language HTTP
libraries — use the system resolver, and whether it maps `*.localhost` varies:
current macOS and systemd-resolved (Ubuntu, Fedora) do; plain glibc or musl
resolvers (Debian, Alpine, distributions without systemd-resolved) do not.
Check with `curl -sSI http://treerig.localhost` (add the port if you changed
it): any `HTTP/1.1 …` status line means the resolver is fine (the router
answered), `Could not resolve host` means it is not, and a connection error
means resolution worked and the router simply isn't running. Where it fails,
`/etc/hosts` has no wildcards, so add every hostname `treerig urls` prints for
the worktree — `<slug>.<domain>` and any `<subdomain>.<slug>.<domain>` — plus
`treerig.localhost`; or use `curl --resolve` / a `Host:` header.
