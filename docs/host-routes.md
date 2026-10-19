# Host routes

Route a process running on the host, such as a dev server, at a worktree's hostname.

A process running on the host (a dev server, an API you start from the IDE) can
be routed at the worktree's hostname through the same router as its services:

```bash
treerig route add web --port 5173 --subdomain app   # http://app.<slug>.<domain> -> 127.0.0.1:5173
treerig route add api --port 8080 --path /api       # http://<slug>.<domain>/api -> 127.0.0.1:8080
treerig route ls [--json]                           # name, URL and port of each
treerig route rm web
```

`route add` takes the routing fields of a `[service.<name>]` table: with no
`--subdomain` or `--path` it claims the worktree host itself, and it gets the
hostname a service with that name and those fields would get. It refuses a
name that is a service in `treerig.toml`, a shared service's, or another host
route's, a route that answers where a service or another host route already
does, the router's own port, and a port treerig pinned for a service (a host
route names a port a process on the host listens on), except that adding a route identical to a recorded one is accepted and rewrites its route file. Adding and removing take
the worktree's operation lock and need a registered worktree (`treerig up`
first).

A host route is recorded in the worktree's registration (`hostRoutes`), and its
route file, `host.<repository>.<worktree>.<name>.yaml` under the router's
`dynamic/` directory, is derived from the record. `treerig up` and `treerig router
up` repair a route file that disagrees with its record, the same way they do a
service's; `treerig doctor` only reports it. A recorded route whose file is
missing is reported by `doctor` and not recreated by the repair: run `treerig
route add` again with the same name, port, subdomain and path (`treerig route ls`
shows the URL) and it writes the file and changes nothing else. `up` and `down`
leave host routes alone, so a route survives `down` (the process on the host is
not treerig's to stop) and the hostname answers `502 Bad Gateway` until something
listens on the port. `worktree remove` removes them before the registration.
`treerig urls` (`urls --json` marks them `"hostRoute": true`), the checkout index,
and `TREERIG_SERVICE_URL_<NAME>` and `TREERIG_SERVICE_PORT_<NAME>` in hooks and
`treerig run` include them; the index lists them from the registration and does
not know whether the process is up. `doctor` lists each one with whether its
port answers (a stopped dev server is `ok`, with the 502 note; only a port that
answers on `::1` alone is a warning), and probes each through the router.

## Bind `127.0.0.1`

The proxy reaches a host process on `127.0.0.1`. A server
that listens only on `::1` (some tools resolve `localhost` to IPv6 first; Vite's
default host can be `::1` on a Mac) answers through the router on a native Linux
Engine, but not on Docker Desktop or OrbStack, where the host is reached over
IPv4 alone; `route add` and `doctor` warn when a port answers only on `::1`.
Bind the server to `127.0.0.1` explicitly (Vite: `server.host: '127.0.0.1'`, or
`--host 127.0.0.1`). A server bound to `0.0.0.0` also works, but is reachable
from the network and may trigger the macOS firewall prompt; prefer `127.0.0.1`.
Forcing `[router] mode = "bridge"` on a Linux engine does not work for host
routes: the bridge container cannot reach a `127.0.0.1` listener there.

## Allowed hosts

The router passes the worktree hostname through as the
request's `Host` header, and many dev servers refuse hosts they do not know.
Allow it in the dev server's own settings. Vite answers a blocked host with a
403 "Blocked request" page: set `server.allowedHosts` (for example
`['.acme.test']`, or the hostname in the URL `treerig route ls` prints; Vite
already allows `localhost` and every `*.localhost` host, so a `project.domain`
under `.localhost` needs nothing). webpack-dev-server answers "Invalid Host
header" (`allowedHosts`), and Django a 400 (`ALLOWED_HOSTS`). Next.js's
`allowedDevOrigins` is a different thing: it lets another origin load the dev
server's resources, and matters when pages are opened from a treerig hostname
while the server is addressed as `localhost`.

A host route needs the router to be running, as a service route does:
`treerig up` or `treerig router up` starts it. Host processes that must be reached
by containers are out of scope: on Linux with a default-deny firewall a
container cannot reach the host.
