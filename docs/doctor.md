# Doctor

What `treerig doctor` checks, how it exits, and the ids of its checks.

## The text report

`doctor` writes its report to stdout: one section per area (`Prerequisites`,
`Registry`, `Locks`, `Router`, `This checkout`, and `Shared services` when
`treerig.toml` declares some or Docker still holds some of the repository's),
one line per check, each marked `ok`,
`warn`, `fail` or `skip`. How it went is one line on stderr, and so is the
output of the `verify` hook it runs, so the report is alone on stdout. It exits
0 when no check failed — warnings and skips do not count — with the `verify`
hook's own exit code when that hook ran and failed, and 1 for any other
failure. The exit code names one failure; the report names them all. A
`verify` hook that exits 124 by itself makes `doctor` exit 124. So does one
that outlives its `[hooks.timeout]` limit and is stopped for it (`The verify
hook timed out after 2m and was terminated.`); the report's `verify hook` line
tells the two apart, the exit code does not. If `timeout(1)` kills `doctor`
itself, which also exits 124, there is no report at all. `doctor`
also reports one line per configured hook, and one per `[commands]` entry,
for whether the file it names would even start (`<stage> hook file` and
`<name> command file`, distinct from `verify hook`'s own run line). How that
prediction works is in [`hooks.md`](hooks.md#doctors-own-check). It is a
prediction, not an observation: it never supplies `doctor`'s own exit code,
which stays the verify hook's own (only when verify itself actually ran and
failed) or 1.
It takes no lock, writes nothing, and repairs nothing; it reports. When the
router itself could be resolved, its
`Router` section also names the router's owner (a `fail` for a router
without treerig's owner label or one another treerig root started), the mode it
runs in against the mode this build would use (and fails for a daemon treerig
refuses), that nothing but `127.0.0.1` answers on its port, and — while it
runs — probes every route through it, with retries, telling the proxy's own
404 (the route is not loaded) from a 502, 503 or 504 (the route is loaded and
its target is down or unreachable). It ends with a `router port` line saying
the same as `router status`'s second line, a `warn` when the running router
serves a different port than the one resolved; when port or settings
resolution fails instead, the section holds that one router failure line
alone.

## Machine-readable report

**`doctor --json`** prints the whole report and still exits non-zero when a
check fails. `ok` is true when no check failed, and `exitCode` is what the
command exits with (0 exactly when `ok`). `counts` always has all four
statuses. Sections and checks are in report order, and `status` is `ok`,
`warn`, `fail` or `skip`. `subject` and `detail` are always present (`detail`
may be `""`).

Scripts key on ids, not on text. A section's `id` is one word (`registry`); a
check's `id` is `<section>.<kind>` (`registry.host-conflict`), and it names
what was checked. `title`, `subject` and `detail` are display text and may be
reworded. A per-item kind appears once per item and repeats its `id`; then
`instance` is the item's key, and `(id, instance)` is a key for every kind in a
document: no pair appears twice. A kind that is not per item has no
`instance`. `instance` is opaque: compare it for equality, and do not parse
it. Where one question can be answered in different ways, the answers have
different ids: `locks.none` (nothing is held) versus `locks.registry`,
`locks.router` and `locks.worktree.*` (something is held or was left
unreleased, both `ok`), and `locks.scan` when the locks could not be read at
all; `router.routes` (the route-file count) versus `router.route-conflict` and
`router.orphaned-routes`; `checkout.missing` versus `checkout.missing-scan`
when the scan itself failed. So a "no lock is held" gate selects on the id
(`locks.none`), not on `status`. `hookExitCode` appears only on a failed
`checkout.hooks.verify`, and is the code `doctor` then exits with. Note that
`checkout.hooks.verify` is the run of the `verify` hook, while
`checkout.hooks.resolve` with instance `verify` says only that its file would
start.

| id | section | instance | meaning |
|---|---|---|---|
| `prerequisites.git` | `prerequisites` | | Git is on PATH and new enough |
| `prerequisites.docker.engine` | `prerequisites` | | the Docker daemon answers; also the id when Docker could not be probed at all |
| `prerequisites.docker.compose` | `prerequisites` | | Compose is present and new enough |
| `registry.treerig-root` | `registry` | | the treerig root in use |
| `registry.control-home` | `registry` | | the control home is usable |
| `registry.router-state` | `registry` | | the router's state directory |
| `registry.registrations` | `registry` | | how many registrations read cleanly |
| `registry.unreadable-record` | `registry` | `<repository>/<file>` | a registry record that could not be read |
| `registry.host-conflict` | `registry` | the host | a hostname claimed by more than one worktree |
| `registry.index-host` | `registry` | worktree ID | a worktree registered on the index's own hostname |
| `registry.port-conflict` | `registry` | the port | a loopback port recorded for more than one service, in one worktree or several |
| `registry.teardown-pin` | `registry` | | the [teardown pin](teardown-pin.md) `worktree remove` runs: ok when it and its stored binary are intact, and also when none is pinned and the running build is a release (removal needs none); `warn` when none is pinned and the running build is a development build (removal will refuse), it has no binary for this platform, or the running build is newer than it; `fail` when it cannot be read, is of a newer format or not one treerig wrote, or its binary is missing or different. Nothing is executed |
| `registry.teardown-store` | `registry` | `location`, `ownership`, `noexec` | why removal will refuse a pin that is otherwise intact: the control home is inside a git work tree or the pin store is not private to you (`fail`), or the control home's filesystem is mounted noexec (`warn`). Only with a pin |
| `locks.none` | `locks` | | no lock is held |
| `locks.scan` | `locks` | | the locks could not be read |
| `locks.registry` | `locks` | | the registry lock is held or was left unreleased, or could not be checked |
| `locks.router` | `locks` | | the router lock is held or was left unreleased, or could not be checked |
| `locks.shared` | `locks` | repository ID | a repository's shared-services lock is held or was left unreleased, or could not be checked |
| `locks.worktree.operation` | `locks` | worktree ID | a worktree's operation lock |
| `locks.worktree.development` | `locks` | worktree ID | a worktree's development-session lock |
| `router.container` | `router` | the problem's code, when the router is degraded or unreachable (see below) | the router's own status: `ok` when up and healthy or not running, `skip` when Docker or Compose is unavailable, `fail` when it could not be resolved, or one `fail` per problem, as `router status` prints them |
| `router.owner` | `router` | | which treerig root started the router |
| `router.mode` | `router` | | the network mode the proxy runs in (`host` or `bridge`, read from the running container), against the mode this build would start it in; says `mode set by settings.toml` when `[router] mode` is in effect and `warn`s when detection would pick another; `fail` when Docker is one treerig refuses (remote, rootless, unrecognized), `warn` when the running mode differs or the running router's layout is not this version's |
| `router.ports` | `router` | | the ports the router needs (its own, the index's and, in host mode, the ping's) are free while the router is not running (`fail`) or is running but unhealthy (`warn`), naming what holds one; not checked while it is healthy |
| `router.bind` | `router` | | only while the router runs: that its port answers on `127.0.0.1` alone, by asking this machine's other addresses for it (`fail` naming any that answer) |
| `router.routes` | `router` | | the route files, none claiming a shared rule |
| `router.route-conflict` | `router` | `<host>` or `<host><path>` | a route two files claim |
| `router.orphaned-routes` | `router` | none, `unmatched` or `other-home` | route files that disagree with their registration, or a shared service's route file with its record: no port recorded for the service (or no host route of that name), or a different port than the one recorded (or, for a host route, a different port or hostname) (the next `up` or `router up` repairs them); with instance `unmatched` (a warning), files whose repository this control home knows but whose worktree it holds no record of, and with instance `other-home` (ok), files of a repository it does not know; both are counted and never removed, since they may belong to another control home (delete them from the router's `dynamic/` directory if their worktrees are gone). A shared service's route file is judged against its shared-service record in the same way, and named as such in these lines |
| `router.route-probe` | `router` | `<host>` or `<host><path>` | only while the router runs, one per route including the index, each host route and the shared services' web UIs: an HTTP request through the router. A 404 is judged by sending the same request straight to the route file's target on `127.0.0.1`: the target giving the same 404 is the service's own (`ok`); a 404 it does not give means the route is not loaded (`fail`), retried for several seconds because Traefik's file provider takes a moment to load a change. A 502, 503 or 504, or a target that does not answer, is `warn` (the target is down or unreachable); a router that does not answer is `fail`; these are retried at most twice; a host route whose process is stopped is `ok` ("nothing is listening"), as in `checkout.host-routes` |
| `router.port` | `router` | | the resolved router port and where it came from |
| `checkout.config` | `checkout` | | this checkout's `treerig.toml` |
| `checkout.worktree` | `checkout` | | whether this worktree is registered |
| `checkout.missing` | `checkout` | worktree ID | a registration of this repository that Git no longer lists: `warn` that its directory is gone (`directory missing`, judged on positive evidence), or that treerig cannot tell whether it is, with the cause (see [State words](commands.md#directory-missing)) |
| `checkout.missing-scan` | `checkout` | | the scan for missing worktrees failed |
| `checkout.ports` | `checkout` | the routed service's name | whether its allocated loopback port is free or held by this worktree's own container (`fail` when anything else holds it, `skip` before one is allocated) |
| `checkout.published-port` | `checkout` | the routed service's name | a routed service whose own Compose file also publishes a fixed host port |
| `checkout.published-port-scan` | `checkout` | | reading the Compose file's published ports failed |
| `checkout.host-routes` | `checkout` | the host route's name | whether the process behind a [host route](host-routes.md) answers on `127.0.0.1` (`ok`, also when nothing listens, with the 502 note; `warn` when only `::1` answers and the router may not reach it: bind `127.0.0.1`) |
| `checkout.host-route-files` | `checkout` | the host route's name | `warn` when a recorded host route has no route file; `treerig route add` with the same fields (see `treerig route ls`) writes it again |
| `checkout.hooks.resolve` | `checkout` | the stage: `prepare`, `seed`, `verify`, `backup` or `restore` (the set may gain values) | whether that configured hook's file would start |
| `checkout.commands.resolve` | `checkout` | the command's name from `[commands]` | whether that command's file would start |
| `checkout.hooks.verify` | `checkout` | | the `verify` hook's own run |
| `shared.scan` | `shared` | | the Shared services section could not be built |
| `shared.services` | `shared` | | the shared services could not be listed, the repository has never been brought up, or Docker is not answering (`skip`, `fail`), or there is nothing to report |
| `shared.definition` | `shared` | | the shared Compose file cannot be used (`fail`): it does not parse, builds an image, publishes `ports:`, or bind-mounts a path inside a worktree |
| `shared.service` | `shared` | the shared service's name | its state ([state words](commands.md)): `ok` when it runs (`running`, or `starting` while its health check passes); `warn` when it is `stopped`, `exited`, `unhealthy` or `restarting`, or runs with a different definition than this worktree's; `skip` when it is `not created` |
| `shared.port` | `shared` | `<name>:<container port>` | whether the pinned loopback port is published by the running container, or free while it is down (`warn` otherwise) |
| `shared.route` | `shared` | the shared service's name | a running service whose web UI is routed: `ok` when its route file exists, `warn` when it does not (`shared up` writes it); also `warn` for a route file whose service is not running (the hostname answers 502) |
| `shared.record` | `shared` | the shared service's name | a running service whose pinned ports this control home has no record of |
| `shared.unreadable-record` | `shared` | `<repository>/<file>` | a shared-service record that could not be read |
| `shared.connection` | `shared` | the shared service's name | a running service of this worktree that uses it, on a network the shared container is not attached to: up but not connected (`warn`) |
| `shared.attachment` | `shared` | `<service>:<network>` | the shared container attached to a network that carries no worktree's labels, or belongs to a stopped or removed worktree; reported, never touched |
| `shared.orphan` | `shared` | the Compose service | a shared container no declared service accounts for (information), or one that claims to be the stack's but cannot be proven so (`warn`; never touched), or a listing that failed (`fail`) |
| `shared.unused` | `shared` | | shared services running with no registered worktree of the repository (information) |

These codes appear only as the `instance` of `router.container`; they are not
check ids like `router.port`. A code is `<service>.<what>`, where the service is
`router`, `index` or `stack` (`stack` covers failures that concern both
containers):

- `router.not-running`, `router.not-healthy`, `index.not-running`,
  `index.not-healthy` (each carries the container's last log lines)
- `router.protocol`: the running router's layout is not this version's; the
  line gives the advice `up` refuses with (`router down` first, or not to
  downgrade a newer router)
- a container that is gone or an unreadable stack: `router.missing`,
  `index.missing`, `router.absent` (neither container exists; `doctor` reports this as `ok`,
  not running, with no instance), `stack.inspect`, and `stack.unreachable` (Docker could not be
  asked; the line has no problem list)

A code appears at most once in a report. The set of codes may gain values.

In CI, select by id and status, for example
`jq -e '[.sections[].checks[] | select(.id=="router.container" and .status=="fail")] | length == 0'`.
That passes when the router check is `skip`, which it is while Docker is down.
`router.container` is also `ok` when the router is not running, so to require
that the router is running and healthy, require both that check and an `ok`
`router.bind`, which is reported only while the router runs:
`jq -e 'any(.sections[].checks[]; .id=="router.container" and .status=="ok") and any(.sections[].checks[]; .id=="router.bind" and .status=="ok")'`.
For a per-item kind, filter by id and read `instance`:
`jq -r '.sections[].checks[] | select(.id=="locks.worktree.operation") | .instance'`.

```json
{
  "ok": false,
  "exitCode": 1,
  "counts": { "ok": 6, "warn": 1, "fail": 2, "skip": 2 },
  "sections": [
    { "id": "prerequisites", "title": "Prerequisites", "checks": [
      { "id": "prerequisites.git", "status": "ok", "subject": "Git", "detail": "2.45.0" },
      { "id": "prerequisites.docker.engine", "status": "fail", "subject": "Docker Engine", "detail": "…" },
      { "id": "prerequisites.docker.compose", "status": "fail", "subject": "Docker Compose", "detail": "…" } ] },
    { "id": "registry", "title": "Registry", "checks": [
      { "id": "registry.treerig-root", "status": "ok", "subject": "treerig root", "detail": "/home/you/.treerig" },
      { "id": "registry.control-home", "status": "ok", "subject": "control home", "detail": "…" },
      { "id": "registry.router-state", "status": "ok", "subject": "router state", "detail": "…" },
      { "id": "registry.registrations", "status": "ok", "subject": "registrations", "detail": "1 readable" },
      { "id": "registry.teardown-pin", "status": "warn", "subject": "teardown pin", "detail": "none pinned; `treerig worktree remove` will refuse until you pin one with `treerig teardown pin --latest` (or `--file`; see `treerig teardown pin --help`)" } ] },
    { "id": "locks", "title": "Locks", "checks": [
      { "id": "locks.none", "status": "ok", "subject": "locks", "detail": "none held" } ] },
    { "id": "router", "title": "Router", "checks": [
      { "id": "router.container", "status": "skip", "subject": "router", "detail": "needs Docker" } ] },
    { "id": "checkout", "title": "This checkout", "checks": [
      { "id": "checkout.config", "status": "skip", "subject": "treerig.toml", "detail": "…" } ] }
  ]
}
```

(Shown compacted, and with `…` standing in for text you would see in full;
the real output is indented, one key per line. The example is a development
build with no teardown pin, so `registry.teardown-pin` is the one warning: a
release build would show it as `ok` and `"warn": 0`.)
