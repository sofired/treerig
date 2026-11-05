# Routing: pinned loopback ports and a loopback proxy

> In 1.0 the command `urls` is `status` ([the command line](cli-1.0.md#removed-and-where-each-job-goes)). This note keeps the names it was written with.

How treerig routes `<service>.<slug>.<domain>` to a worktree's services, and
why. This note is where the spike evidence and the approved decisions live;
#119 (pinned ports), #120 (the proxy) and #6 (host routes) are implemented
from it, and #121 (rootless Docker) is a follow-up. Shared services (one
instance per repository, below) is designed here and tracked in #123.
Where an issue body disagrees with this note, this note wins.

## Goals

Local-to-local development on the developer's own machine, with services under
their control:

- a stable hostname per worktree and service, `<service>.<slug>.<domain>`,
  and the same for a process running on the host (#6);
- several worktrees of the same stack running side by side;
- the checkout index, `urls`, `TREERIG_URL` and `TREERIG_SERVICE_URL_<NAME>`;
- a service run once per repository and used by every worktree of it, such as
  an email catcher (shared services, below);
- working under a default-deny host firewall (ufw) and on macOS.

Not goals: isolation between mutually distrusting users, TLS, remote Docker
daemons.

## Why the design replaces per-worktree router networks

A router that reaches services over Docker bridge networks needs the router
attached to every worktree's network, and the host firewall in the path
whenever traffic crosses from a bridge to the host. On Linux with ufw's
default-deny INPUT policy, a container reaching the host through
`host-gateway` is dropped even when the target port is published on
`127.0.0.1` (spike below). Any design that has a container call back to the
host fails on such a machine, and a firewall drop hangs the request rather
than failing it.

The design instead makes every hop a loopback hop:

```
browser ──▶ 127.0.0.1:80  proxy container (Traefik)
                              │  file provider: <router state>/dynamic/*.yaml
                              ▼
                     treerig-loopback:<port>
                              │
             ┌────────────────┴───────────────┐
             ▼                                ▼
   127.0.0.1:<port> published by         127.0.0.1:<port> listened on
   a Compose service (#119)              by a host process (#6)
```

- **#119.** Each routed Compose service gets a pinned port, published on
  `127.0.0.1:<port>` through the generated override. Pinned, because an
  ephemeral published port changes on every container restart.
- **#120.** One proxy container (Traefik, pinned by digest) routes hostnames
  to loopback targets. In host mode (native Linux) every hop is loopback, so
  the host firewall is never involved. In bridge mode (Docker Desktop,
  OrbStack) the proxy reaches the host's loopback ports through the engine's
  `host-gateway` forwarding, which the spike verified needs no firewall change
  on either. No network is shared between worktrees. (Shared
  services attach one shared container to a consuming worktree's own network,
  bridge-internal; the proxy is never on it.)
- **#6.** Host routes send hostnames for processes outside Docker through the
  same proxy and the same route files.
- **#121.** Rootless Docker is refused in v1: its "host" network is
  RootlessKit's namespace, not the host's.

## Decisions

All rows were approved by the owner: 1-14 for routing, 15-22 for shared
services (the owner ruled on the network model, 22, and on raw ports).
"Why" is the reason to keep it when reimplementing.

| # | Decision | Why |
|---|----------|-----|
| 1 | The proxy's mode comes from `docker info`, not the host OS: **host** network on a native rootful Linux Engine; **bridge** network on Docker Desktop and OrbStack. Rootless, remote daemons and anything unrecognized are refused with a clear message (rootless points at #121). `[router] mode` in `settings.toml` overrides detection: the override replaces only the "unrecognized" refusal and never lifts the remote, rootless or non-Linux-containers refusals. A mode change recreates the proxy through Compose, because its configuration changes. `doctor` and `router status` show the running container's mode, read from a label on it, not the configured one. | The daemon decides what "localhost" means to a container, not the OS the CLI runs on (Docker Desktop for Linux is a VM). Refusing the unknown beats guessing a mode that half-works. |
| 2 | Route files target the alias `treerig-loopback`, which the proxy's `extra_hosts` maps to `127.0.0.1` in host mode and to `host-gateway` in bridge mode. | Route files never depend on the mode, so a mode change never rewrites them. |
| 3 | Ports live in an optional `ports` map on the registration record (record version stays 1; a record without it is valid). Allocation runs under the registry lock: start at a hash of (worktree ID, service), probe forward, skip any port in any registration and any port that fails a loopback bind probe. Default range 20000-29999, configurable in `settings.toml`; tests and scratch roots use a separate range. Both allocators, a worktree's and a shared service's, also skip the index port and the ping port (decisions 7 and 8), which live in router state. One record builder serves every registry writer, so no writer can drop `ports`. A user's own dev server may take an allocated port later; the bind probe at allocation, Docker's clean bind error on `up`, and `--reallocate-port` (decision 4) cover that. | Stable across restarts and reboots; the hash start makes re-allocation after a lost record land on the same port. 20000-29999 sits below Linux's ephemeral range (32768) and macOS's (49152), so the OS does not hand it out. A single builder is the only way to guarantee a writer that does not know about `ports` cannot erase it. |
| 3a | Registry locks are per control home. Two control homes can allocate the same port at once; this race is accepted, and Docker's bind error on the second `up` catches it. It covers the router's index and ping ports too (decisions 7 and 8): the remedy is `--reallocate-port` on the worktree side, or removing `router/state.json` for the router's. | A machine-wide registry lock is not this design (the same trade `docs/design/locks.md` records for the router). The failure is clean and recoverable (decision 4). |
| 4 | Stuck port: `treerig up --reallocate-port <service>`. It refuses when the port is held by this worktree's own container, proven by its ownership labels, and says the URL changes. | The escape hatch for a port another program took. Ownership proof stops it from moving a port out from under a running container. |
| 5 | `up` releases the allocation of a service removed from `treerig.toml`. Otherwise only `worktree remove` releases allocations, after the worktree's containers are gone (after Reap) and before the record itself is deleted. | A port may not be reused while a container of the old owner could still hold it. Releasing before the record is deleted keeps the allocation reachable if the process dies in between; the step is idempotent. |
| 6 | `urls`, `TREERIG_URL` and `TREERIG_SERVICE_URL_<NAME>` stay hostname URLs (a host route's entry in `urls --json` carries `"hostRoute": true`, and its `port` is the one its process listens on). New: `TREERIG_SERVICE_PORT_<NAME>` for hooks and `treerig run`, and a `port` key in `urls --json` and `status --json` (additive). | Hostnames are the contract users bookmark; the port is for tools that must bypass the proxy. |
| 7 | The checkout index (busybox) publishes on its own `127.0.0.1` port and is routed like a service. Its port is stored in router state under `TREERIG_HOME`, not in any registration, and is allocated once from the same range under the router lock. | One routing path for everything; no shared network for the index either. The index belongs to no worktree, so it has no record to hold a port; router state is where the router's other per-root facts live. |
| 8 | Traefik's ping gets a dedicated entrypoint, never `web`. Host mode: `--entrypoints.traefik.address=127.0.0.1:<ping port>` with `--ping.entryPoint=traefik`, and the healthcheck carries the same address flag. The ping port is allocated like the index port (once, from the same range, under the router lock) and stored in router state. Bridge mode: the ping entrypoint listens on a fixed port inside the container, is never published, and takes no port from the range. The index and ping ports share one router-state file. Never Traefik's default `*:8080`. | Ping on `web` would shadow every routed service's own `/ping` on every hostname. `--ping=true` alone opens `*:8080`, a wildcard listener (spike). The healthcheck runs as a separate process and needs its own copy of the entrypoint flags (verified on Linux). |
| 9 | A route file is judged only when this control home holds a record for that exact repository and worktree. For such a file, a record with no port for its service gets the file removed and a record with another port gets the file rewritten to the recorded one, by `router up`/`Ensure`; `doctor` reports both. A host route file is judged against `hostRoutes` the same way: a file whose record has no route of that name is removed, and a file that is not exactly what the record produces (another port, or another rule after the worktree's hostname changed) is rewritten whole. A recorded host route whose file is missing is reported by `doctor` (`checkout.host-route-files`) and never recreated by repair, which only acts on a file that exists, so it does not bring back a file a `worktree remove` has taken; `treerig route add` with the route's own name, port, subdomain and path rewrites the file and changes no record, which is the heal. A repair rewrite that races a `worktree remove` can still put one file back, a residual window service files share; `doctor` reports it as unmatched. A file with no such record is reported by `doctor` (`router.orphaned-routes`) with the count and how to remove it (delete it from the dynamic directory), and never removed, because the router's state is per treerig root and it may belong to another control home. It is a warning (instance `unmatched`) when this control home knows the repository but not the worktree, likely stale, and a notice (instance `other-home`, status ok) when it does not know the repository, likely another control home's. `up` commits a port allocation to the registration before it writes the route file. Repair reads registrations without the registry lock (every registry writer goes through `files.AtomicJSON`, a temporary file then a rename) and re-reads a record immediately before acting, which narrows the window against a concurrent `up` or a just-added host route without closing it; the rewrite is eventually consistent with the writer, whose next `up` writes the same port. The router lock and the registry lock are never nested. | Once ports are reused, a stale file would send an old hostname to another worktree's service. |
| 10 | The name `router`, the router-home ownership check (#82) and the refusal of a router started by a newer treerig stay. A running router with the per-worktree-network layout is refused with "run `treerig router down` first". `Protocol` is bumped only when Compose cannot recreate a router of the older generation into the new one; a change of mode, port or digest never bumps it. `router status` and `doctor` report a running router at another protocol with the same advice. The labels `dev.treerig.router-mode` and `dev.treerig.router-port` and the file `router/state.json` are storage names, pinned by tests. | Those are safety properties, independent of the transport. No code converts the old layout. |
| 11 | Host routes (#6) are stored in the registration record (`hostRoutes`, a list sorted by name of `{name, port, subdomain?, path?}`); route files are derived from it, named `host.<repo>.<wt>.<name>.yaml`; the Traefik router name is `<project>_host_<name>`. A host route may not share a service's name, and two host routes may share a port (different places, one dev server). It takes the same routing fields as a `[service.<name>]` table (`--subdomain` and `--path`, either or both) and gets the hostname a service with that name and those fields would get. `route add` and `doctor` warn on a listener that answers only on `::1` in bridge mode (#120 maps `treerig-loopback` to both `127.0.0.1` and `::1` in host mode, so there it is reachable; see Open items); they warn too when the mode cannot be read, since then it may be bridge. `route add`, `route rm` and `worktree remove` change host routes; `up` and `down` leave them alone, and neither writes nor removes a host route file. `route add` refuses a name that is a `[service.<name>]`, a declared `[shared.service.<name>]` or another host route's; a place (subdomain and path) a service or another host route already answers at; the router's own port; and a port treerig pinned for a service or a shared service. The allocator treats a host route's port as taken. A shared service, or a `[service.<name>]`, named like an existing host route, or a `[service.<name>]` answering at its place, is refused by `up` and `shared up` before anything starts and again under the operation lock, because host routes live in the record and config validation cannot see them. `router.route-probe` probes host route files too, the only check of the proxy-to-host hop; a host route whose process is stopped (a 502, 503 or 504) is `ok` there, in the words of `checkout.host-routes`, since that is its normal state. The checkout index lists host routes from the record, never from the card, which only counts containers. | The record is the source of truth and route files are a cache, so decision 9's repair covers host routes too. `host.` is a prefix the service-route sweeps and `ListFragments` never match; `_` is outside the service-name alphabet, so the router name cannot collide with a service's. Repository and worktree IDs are UUIDs and service names are `[a-z0-9-]`, so `host.<repo>.<wt>.<name>.yaml` cannot collide with `<repo>.<wt>.<service>.yaml`; a test uses a repository named `host`. The `::1` warning holds in bridge mode: Docker Desktop cannot reach `::1`. In host mode `treerig-loopback` maps to `::1` as well, which reaches a `::1`-only listener (verified). The fix is to bind `127.0.0.1`. |
| 12 | `doctor` warns on a routed service with a fixed published host port. `up` refuses a routed service with `network_mode: host`, `network_mode: service:...`, or replicas above 1. | treerig needs to own the published port. A host-network or shared-namespace service has no port to publish; several replicas cannot share one. |
| 13 | No Traefik API. `doctor` probes each route over HTTP. A 404 is judged by sending the same request (path and Host) straight to the route file's target on `127.0.0.1:<port>`: the same 404 there is the service's own and the route is fine; a 404 the target does not give means the route is not loaded (a failure), retried for 5-10 s. A 502, 503 or 504, an unreachable target or a router that does not answer is retried at most twice, and the total is capped. The comparison has limits: a service that itself 404s at the probed path makes an unloaded route read OK, and a router 404 with an unreachable target reads as target down (a warning), not as an unloaded route. | The file provider picks changes up after about a 2 s throttle. Without the API, the response itself is the evidence. |
| 14 | This note lands in its own docs PR before #119. | So the decisions are reviewable apart from code. |
| 15 | **Model.** A shared service is one instance per repository UUID, defined in a separate Compose file named by `[shared] file` and run as one Compose project per repository, `treerig-shared-<24 hex of sha256("shared:" + repositoryID)>`. `[shared.service.<name>]` names a service in that file by its key (an optional routed `port`; optional raw `ports`; `used-by`), keyed like `[service.<name>]` and `[commands.<name>]`. `used-by` lists the worktree services that use it: they make a worktree "need" the service (auto-start) and are the services whose networks it joins (decision 22). A shared name may not equal a service in the worktree's Compose file, a `[service.<name>]` or a host route (the alias would resolve to two things). Host-run shared services (a service running natively outside Docker, used by containers) are out of scope: on Linux with ufw a container cannot reach the host. | A separate file keeps the service out of every worktree's own project (a shared service in the worktree file would start in each of them, and each branch's copy of the file would differ). One project means one ownership label set. Listing consumers puts the opt-in on the shared side, so no `[[service]]` change is needed and non-routed workers can use it. |
| 16 | **Definition drift.** "First up wins" compares a definition hash, label `dev.treerig.shared-definition=v1:<sha256>`, on the running container: the canonical JSON of the entries for that service and every service it transitively names in `depends_on`, from `docker compose config --format json` on the shared file, before treerig's override (so published ports are not in it), interpolated with the shared environment only (no per-worktree variables). The shared file must resolve the same from every checkout, so every checkout computes the same hash: treerig refuses a file whose resolved values differ with the directory it is read from (a relative path that climbs out of the checkout, `${PWD}`), best effort, and warns when it cannot check (mechanism and limits in the Configuration section below). While the container runs, treerig never calls `compose up` for it, so Compose cannot recreate it on its own. A difference is a **warning** on `treerig up` (it names the worktree in label `dev.treerig.shared-defined-by`, or "a removed worktree", and the remedy) and a **refusal** on `treerig shared up` without `--recreate`. Stopped or absent: the caller's definition starts it. `treerig shared up --recreate <name>` force-recreates that service only, keeps its volumes and reconnects every consuming worktree (decision 22). The hash is not stored in the record. | A branch bumping an image pin must not block an unrelated feature branch's `up`, but an explicit request that cannot be honoured is a failure. The label is the only truth about what runs; a second copy in a file could disagree with it. Compose's own config hash includes treerig's override and is Compose-version specific. |
| 17 | **Resources and ownership.** Every resource of the shared stack (containers, volumes, the stack's own default network) carries `dev.treerig.managed=true`, `dev.treerig.repository-uuid=<repo>` and `dev.treerig.shared=true`, and **never** `dev.treerig.worktree-uuid`. A worktree's `used-by` containers carry `dev.treerig.shared-use.<name>=true` besides their worktree labels. Removal (`shared down`, `--volumes`) lists by the shared labels and re-verifies each resource (managed, repository, `shared=true`, project name equals the derived one, no worktree label) before removing it, never through the caller's Compose file. `shared down` removes containers and the default network and **keeps volumes**; only `--volumes` removes them. `worktree remove` and `down` never remove a shared resource: neither lists one, because it has no worktree label, and a decoy test pins that in both directions, including with the shared container attached to the worktree's network. The label keys and the project name are storage names. | Ownership by label, in a scope no worktree shares. Removal that does not need a Compose file works from any worktree, and after the last one is gone. The `shared-use` label lets a sweep find consumers without loading any worktree's `treerig.toml`. |
| 18 | **Locking.** New scope `SharedScope(controlHome, repositoryID)`, target `<control home>/locks/shared/<repo>`, one lock for all of a repository's shared services, timeout 15 min (a `compose up --wait` of 5 min plus a pull and a recreate, as `RouterTimeout` is derived). `lifecycle.Requests` gains `Shared`; order is operation, development, shared, registry, with the router still alone, and **nothing takes the shared lock and then a worktree lock**. (1) `treerig up` starts or reuses the instance under the shared lock alone, before `mutate`, so a worktree's 25-minute operation lock is never held waiting on it. (2) Connecting a worktree runs inside that worktree's operation lock and takes the shared lock nested, after it, for the connect only; `lifecycle` needs a way to add a scope to a held set in order. (3) Disconnecting (`down`, `worktree remove`) needs the operation lock only, so removal never waits behind a slow shared start. (4) The reconnect sweep after a shared container is (re)created runs only after the shared lock is released, as one `Hold(Operation, Shared)` per registered worktree with a short timeout (`ReconnectTimeout`, 5 s); a busy worktree is skipped with a notice and left to its own connect step, `treerig shared up` or `doctor`. The registry package takes its own short lock inside for a port allocation. The lock is per control home, like the registry's (decision 3a): two control homes on one daemon can both start the stack, and the loser gets "container name ... is already in use" (verified), which treerig tolerates by re-inspecting by label; a running instance found without a local record is reported, never re-allocated. Docker calls under it are bounded; if the state of the container cannot be determined, `up` fails rather than assume it is absent. No age-based cleanup: the flock frees on death, and recovery is `treerig worktree unlock --shared` through the same `Clear` checks, with the shared project name as the survivor-scan marker. | Two first `up`s serialize and the second reuses the instance. Connect and disconnect for one worktree exclude each other through its operation lock, which is what keeps the shared container from being attached to a network that is being removed. A killed starter frees the lock; the next holder re-inspects by label and `compose up` is idempotent. Failing closed avoids a duplicate or a recreate of a running instance because the daemon was slow. |
| 19 | **State, ports and routes.** State is `<control home>/repositories/<repo>/shared/<name>.json`: `{version: 1, name, domain, ports: {"<container port>": <host port>}, routed?: <container port>}`, written through `files.AtomicJSON` by one record builder. Every declared port (`port` and each of `ports`, at most 16 per service, TCP) gets a pinned host port from the same allocator, keyed by (repository ID, `shared:<name>:<container port>`); the allocator's skip set reads these records. Each is published on `127.0.0.1:<host port>` by the shared project's override. Ports are kept across `down`, released only by `down --volumes` (after the containers and volumes are gone, before the record is deleted, decision 5), and never by `worktree remove`. The web UI (`port`) is also routed at **`<name>.shared.<domain>`** using the domain recorded at allocation, through the route file `shared.<repo>.<name>.yaml` (router name `<shared project>_shared_<name>`; `shared.` is a prefix the service and host sweeps never match). The record names the routed container port (`routed`, one of `ports`' keys), so repair, `doctor` and the index judge a route file from the record alone; the file is written from the record under the shared lock once the service is up (a running instance too, which repairs a deleted or stale file), and `shared down` removes it before it stops anything. Slug `shared` is reserved by the slug allocator so `<name>.shared.<domain>` cannot be a worktree's host. Decision 9's repair covers shared route files against these records: judged only when this control home holds the record, rewritten when the port differs, removed when the record routes none, and otherwise reported by `doctor` (`router.orphaned-routes`) and never removed. `treerig shared up --reallocate-port <name>:<port>` (decision 4) refuses while the container runs (proven by labels). Two repositories that share a domain and a shared name conflict when the second is made routed (`SyncSharedPorts`, under the registry lock, before any port is allocated or anything starts); it is refused, naming the first by the hostname of one of its registered worktrees, or by its repository ID when this control home holds none. Only a record that routes counts, and a record that cannot be read refuses too, since it may be the first. The first's record keeps its domain, so the way out is renaming one service, or `shared down --volumes` in the first repository, which deletes its record and so releases the name. | A record is needed because the container is gone after `down` while its ports must stay pinned. Raw ports are for host processes (an IDE-run app, host routes, `treerig run`); containers use `<alias>:<port>` on their network. The hostname must not be a worktree slug: any single label under the domain could be a branch name, so `shared` is the one word reserved. |
| 20 | **Discovery.** Container side: the alias is the declared name, so `mailpit:1025`; no variable is injected into Compose (that would tie the worktree's Compose environment to shared state, and a changed variable recreates its containers, #28). Host side (hooks, `treerig run`): `TREERIG_SHARED_ADDR_<NAME>_<PORT>=127.0.0.1:<host port>` for every declared port (`<PORT>` is the container port; `<NAME>` maps as in `TREERIG_SERVICE_URL_<NAME>`, and the trailing number keeps it unambiguous), and `TREERIG_SHARED_URL_<NAME>` for a routed `port`. Built next to `ServiceURLVariables`. `status` and `status --json` (an object) gain a `shared` array: `{name, state, url?, addrs: [{port, addr}], definition: "current"\|"differs"\|"unknown", connected: bool, definedBy?}`. `urls` and `urls --json` (a top-level array, a contract) are unchanged. `doctor` lists shared services; the index lists routed UIs. | The name in `treerig.toml` is the contract for containers. Hooks and host processes are on the host, where an in-network alias does not resolve. |
| 21 | **CLI.** `treerig shared up [name...] [--recreate] [--reallocate-port <name>:<port>]`, `shared down [name...] [--volumes]`, `shared status [--json]`. `shared up` on a running instance with the same definition still runs the reconnect sweep (the repair path). `down --volumes` deletes data and prompts as `worktree remove` does. `worktree remove` of a repository's last registered worktree prints one line, best effort with a bounded Docker probe that never fails the remove, if shared services are still running, naming `treerig shared down`. `shared down` works from any checkout of the repository and needs no `treerig.toml` (label namespace from `--label-namespace`, as `worktree remove`). | Auto-start, explicit stop: the owner's lifecycle needs a stop command, and a way to reclaim data needs one destructive path. |
| 22 | **Network model: per-worktree connect (Owner ruling).** The shared container joins each consuming worktree's own network with the declared alias, by `docker network connect --alias <name>`. There is no shared network and no external network in any override. All worktrees of a repository share service names, so on one flat network a `used-by` service calling another (`web` to `api`) can resolve to another worktree's container: silent cross-worktree traffic, which the spike confirmed is ambiguous. Every connect and disconnect target is proven by labels first. Connect: after the worktree's `compose up` and before its hooks, for each network a running `used-by` container is on (found by the `shared-use` label) that carries this worktree's ownership labels. Disconnect: `down` before `compose down`, and `worktree remove` before the first Reap, from each network in the verified inventory. Removal reads the stack under the running command's label namespace (its `treerig.toml`'s, or the default when it has none) for a worktree of the command's own repository, and under the namespace the target recorded for a worktree of another repository, whose stack this checkout's `treerig.toml` says nothing about; either way it proves each network under the removed worktree's own namespace. A stack it cannot find stays attached, so the Reap keeps that network and a later `worktree remove` retries. If the Docker listing or a disconnect itself fails, `down` and `worktree remove` stop with that error before anything is stopped or reaped, rather than leave a network that cannot be removed. Repair: `treerig up` and `treerig shared up` reconnect idempotently, `doctor` reports a running consumer that is not connected. A network that fails proof is reported and never touched. | This brings back connect and disconnect bookkeeping of the kind the redesign removed from the router. What made the router's version fragile was the host hop and the machine-wide router lock, and neither applies: the traffic is bridge-internal, and every step is under the one worktree's operation lock. Disconnecting before Reap is what stops the attached container from making a worktree's network unremovable. |
| 23 | **Routed URLs reach Compose (#177).** The worktree's own Compose invocations get `TREERIG_URL` and `TREERIG_SERVICE_URL_<NAME>`, built by `worktree.URLVariables` from the same routes `treerig urls` prints; these names are now a contract for users' Compose files. The router's port is therefore a Compose input, but only for a service whose file interpolates a URL, so a changed router port recreates just those containers. #28 kept it out of Compose entirely so that a change to the port alone could not recreate every worktree's containers; that still holds for a container that does not use the variables. The shared stack never gets them (decision 20: its definition must not vary by worktree). | A user's Compose file needs its own public URL for a public-URL setting or a CORS origin, and only the router knows the port. Interpolation makes the dependency visible per container instead of global. |

### Proxy container

In host mode the proxy runs as root inside the container only when the router
port is below 1024, because binding `127.0.0.1:80` as uid 65534 is refused
(spike). With a high router port (#79) it runs as a non-root user. It is
otherwise hardened: read-only root filesystem, `cap_drop: ALL` plus
`NET_BIND_SERVICE` (only when the port is privileged), `no-new-privileges`, no
Docker socket. The mode and the router port are both inputs to the proxy's
Compose configuration, so changing either recreates it.

In bridge mode the `web` entrypoint binds `:80` inside the container and the
`127.0.0.1` restriction comes from the publish; the `127.0.0.1` bind is host
mode only.

Mode detection (decision 1) is a pure function over the parsed `docker info`,
the endpoint the client talks to (`DOCKER_HOST`, else the current context's
docker endpoint) and the client's operating system. The rules, in order, as
implemented in `internal/router/mode.go` and pinned by its fixture table:

| # | Condition | Result |
|---|-----------|--------|
| 1 | Endpoint is not a unix socket, a named pipe or TCP on a loopback address (ssh, TCP to another host) | refused: remote |
| 2 | `OSType` is not `linux` | refused |
| 3 | `SecurityOptions` contains `name=rootless` | refused, points at #121 |
| 4 | `[router] mode` is set | that mode (replaces only rule 7's refusal) |
| 5 | `OperatingSystem` or `Name` is `Docker Desktop` / `docker-desktop` or `OrbStack` / `orbstack` (case-insensitive) | bridge |
| 6 | `Name` is (lower-cased) `colima`, `lima` or `rancher-desktop`, or begins with one followed by a dash (`lima-default`, `lima-rancher-desktop`); a host merely named like `limassol` is not matched | refused as unrecognized, on any client; the message names `[router] mode` |
| 7 | Client is Linux | host |
| 8 | Anything else (a Linux engine reached from macOS) | refused as unrecognized; the message names `[router] mode` and leads with `bridge` |

A Linux client reaching a VM engine over a forwarded socket or an SSH tunnel
cannot be told apart from a native engine and gets host mode.

Real values: a native Ubuntu 24.04 engine reports `OperatingSystem` "Ubuntu
24.04.4 LTS", `Name` "cc-worker" (the machine's host name), `OSType` "linux"
and `SecurityOptions` `["name=apparmor", "name=seccomp,profile=builtin",
"name=cgroupns"]`; Docker Desktop reports "Docker Desktop" / "docker-desktop"
and OrbStack "OrbStack" / "orbstack", on macOS and (Desktop) on Linux alike.
The override sits after the refusals that no mode can fix (1 to 3), so it never
turns a rootless or remote daemon into a working one.

Before starting, `Ensure` checks that the proxy, index and (in host mode) ping
ports are free and names the holder. When a start comes up unhealthy it shows
the container's last log lines, because a taken `:80` otherwise crash-loops
with Compose reporting only "unhealthy".

Details #120 settled that the decisions above leave open:

- The index and ping ports live in `<TREERIG_HOME>/router/state.json`
  (`{"version": 1, "indexPort": N, "pingPort": M}`), written by `Ensure` under
  the router lock the first time they are needed and kept. A stored port that
  became the router's own port is replaced, and an unusable stored port is
  dropped from the file too, so it does not stay reserved. `pingPort` appears the first time
  the router runs in host mode.
- Bridge mode's ping entrypoint listens on `127.0.0.1:8082` inside the
  container (`router.BridgePingPort`), reachable only by the health check.
- A running router that carries a lower protocol label, or none, is refused
  with "run `treerig router down` first", as one with a higher label is refused
  with the downgrade advice. The protocol number is 6.
- The check that a port is free dials a privileged port and binds an
  unprivileged one (a bind to port 80 fails for a non-root user whether or not
  anything holds it). A port the stack's own running containers already hold
  (published bindings, or the proxy's entrypoint addresses in host mode) is not
  a conflict.
- Route repair judges a file only when this control home holds a record for
  its exact repository and worktree. The router's state is per treerig root and
  registries are per control home, so a file without such a record may belong
  to another control home (the same repository used under two): `doctor`
  reports it and nothing removes it. A file pointing at a stale port is
  rewritten to the recorded one; a file whose record holds no port for its
  service is removed.
- When a port the router needs is held, the message for the index or ping
  port suggests removing `router/state.json` so the next start picks another,
  and for a container holder shows `docker ps --filter publish=<port>`.
- `doctor` checks the router, index and (host mode) ping ports are free while
  the router is absent (a failure) or present but unhealthy (a warning), and
  the container's last log lines accompany an unhealthy problem in `router
  status` and `doctor`.
- The proxy's route probe and loopback-only proof (`router.route-probe`,
  `router.bind`) run only while the router is running. `router.bind` dials
  every global-unicast address of the machine on the router's port and counts
  an address as answering only when it returns an HTTP response.

### Trust model

- A published loopback port is reachable by any local user and process, the
  same as the router port today.
- In host mode the proxy shares the host's network namespace, and with it
  the host's abstract unix sockets.
- On Docker Desktop and OrbStack, containers can reach the host through
  `host.docker.internal`, so any container can reach a routed loopback port
  directly.
- The proxy has no Docker socket and no path to the Docker API.

With shared services, a shared container is attached to each
consuming worktree's network (decision 22). Worktrees do not resolve each
other's services by name, but this is not a network isolation boundary: on
OrbStack a container that knows another worktree container's IP can reach it
across Compose networks (see Platforms). The shared container can see all of
them, and any container on a connected network can reach it. Its raw ports
are also published on pinned loopback ports, reachable by any local user,
like the routed ports.

## Shared services

Some services are wasteful once per worktree: an email catcher, an object
store emulator, a broker's web UI. A shared service runs once per repository
and every worktree of it uses the same instance. "Repository" is one
repository UUID, so two clones of the same upstream are two repositories with
two instances. Decisions 15-22 are the contract; this is how they fit
together. Items marked **Owner** still need a decision; the owner has ruled on
the network model (decision 22) and on raw ports.

### Configuration

```toml
[shared]
file = "compose.shared.yaml"  # relative to treerig.toml; its own Compose project

[shared.service.mailpit]      # a service in that file; also the DNS alias
port = 8025                   # optional: the web UI's container port, routed
ports = [1025]                # optional: container ports for host processes
used-by = ["web", "worker"]   # worktree Compose services that use it
```

Names are the table keys and use the service-name pattern (lower-case
letters, digits, hyphens, at most 63); `used-by` entries are unique and may
not name a shared service. `port` and `ports` are 1-65535, unique within the
service, at most 16 in all. `up` checks that each `used-by` service exists in
the worktree's Compose file and that a shared name does not (decision 15),
and refuses a `used-by` service with a host or shared network namespace,
which cannot join a network. `[shared]` needs `file` when any
`[shared.service.<name>]` table exists, and a `[[shared.service]]` array is
rejected with the same hint as `[[service]]`.

Every service the started ones `depends_on` in the shared file starts with
them, carries the same labels and hash, and is not discoverable.

A shared definition must not depend on the checkout that started it: `build:`
and a bind mount whose source is inside a worktree are refused (approved),
because the project directory and every relative path resolve to the first
worktree, which `worktree remove` can delete, and the resolved absolute paths
would make every other worktree's hash differ. Images, named volumes and
absolute paths outside worktrees are fine.

**Reading the same from every checkout.** The hash is only worth comparing if
every checkout computes the same one. A relative path is resolved against the
project directory, which is the checkout's own, so `../shared-data` names a
different directory in each, and `${PWD}` is whatever directory the command ran
in; `PWD` and the rest of the shell environment are inherited. `ReadDefinition`
therefore reads the file a second time with `compose config`, from a scratch
project directory with `PWD` set to it, and refuses the file when any resolved
value differs, naming the first in sorted key order and its likely cause (a
relative path, `${PWD}`, or a neutral "path or variable that depends on the checkout" when substituting the scratch
directory for the real one does not reproduce the second read). The refusal reaches `treerig up`
(which fails when a shared service it uses is refused), `treerig shared up` and `doctor`. The check
checks against the worktree roots, which are the config's directory and the registered worktrees of the
repository; a read made with no roots (`treerig shared status`) is not checked, and neither is a file whose
directory is outside every root. The whole file is
checked, not only the closure that is hashed. It costs one extra `compose config`,
and `ensureSharedLocked` runs it inside the shared lock.

The check is best effort, and says when it could not be made. Any failure other
than a difference (no scratch directory, a failed second read, undecodable
output) skips it with a `treerig` warning on stderr naming Compose's first error
line, repeated by each command that reads the file; only a cancelled context
is returned. The second read fails for an `env_file`, `include` or `extends`
that climbs out of the file's directory (`../../outside.env`), because the
scratch directory mirrors the project directory's entries and nothing above it:
such a file with a `../shared-data` bind is accepted with the warning. The warning ends
with its remedy: absolute paths there, or keeping the files it reads under the file's own
directory, silence it. A shared file whose own
directory is outside every worktree reads the same for every checkout and is not
checked. A variable that differs between users, shells or machines rather than
checkouts (`${USER}`, `${HOME}`) is invisible to the check, since both reads run
in one environment; keeping such a variable out of the file is the user's job.

The scratch directory is `shared-probe-*` under the control home, created per
read and removed after it. It is there because library code may not read
`TMPDIR` (the environment rule rules out `os.TempDir`). A `SIGKILL` leaves it
behind; it holds only symbolic links, is harmless, and is not swept.

A shared file that publishes a host port of its own (a fixed `ports:` entry)
is the user's choice; `doctor` warns, as decision 12 does for routed services.
Host-run shared services are not supported: a container cannot reach the host
on Linux with ufw.

### Why per-worktree connect

Owner ruling. All worktrees of a repository share service names. On one
network shared by their `used-by` services, `web` calling `api` could resolve
to another worktree's `api`, and the spike found exactly that ambiguous (below).
A wrong-worktree answer is silent, so the flat network is not built. The shared
container instead joins each consuming worktree's own network, where only that
worktree's services live; the worktrees never see each other, and the shared
container sees all of them, which is the point of sharing it (mail from every
worktree lands in one inbox).

This is the connect and disconnect bookkeeping the routing redesign took out
of the router, so it is designed explicitly below. The reasons for removing the
router's version do not apply: the traffic is container-to-container on a
bridge, with no host hop and no firewall in the path, and every step runs under
one worktree's own operation lock instead of a machine-wide lock.

### Lifecycle

`treerig up`, when the worktree has a `used-by` service that exists in its
Compose file:

1. Registration and `routerSvc.Ensure`, unchanged.
2. Under the shared lock, holding no worktree lock (decision 18): commit the
   pinned ports for each declared port of each needed service; inspect each by
   label. Running with the same hash: reuse it, and call nothing. Running with
   another hash: warn (decision 16) and reuse it. Not running: `compose up
   --wait` with the caller's definition, then write the route file. Release
   the shared lock, then run the reconnect sweep for the *other* registered
   worktrees (below), because a new container has none of their
   attachments. If starting fails, `up` fails before touching the worktree
   (approved: an unreachable dependency otherwise shows up later as an
   unrelated connection error).
3. `mutate` (the operation lock) and the worktree's `compose up`. The override
   stamps `dev.treerig.shared-use.<name>=true` on each `used-by` container and
   changes nothing else: no network, no `networks` key.
4. Still inside the operation lock, before hooks (`prepare` and `seed` may send
   mail): take the shared lock nested and connect. For each `used-by`
   container that is running, read its networks; for each network carrying
   this worktree's managed, repository and worktree labels (and its project
   label), `docker network connect --alias <name> <network> <shared container
   id>`, skipped when the shared container is already an endpoint. Then hooks.

`treerig down` disconnects first, then runs `compose down`: for each
worktree-owned network the shared container is on, `docker network disconnect`.
`worktree remove` does the same from the verified inventory, before the first
Reap, so removing the worktree's network can never fail on the attached
container. Neither takes the shared lock (decision 18) and both tolerate "no
such container" or "not connected": if the shared container went away, the
attachment did too. A network that is not in the inventory, or does not carry
the worktree's labels, is never disconnected.

When a shared container is created (a first start, `--recreate`, or a start
after `shared down`), every consuming worktree must be reconnected, because
attachments belong to the container and a new one has none. The reconnect
sweep finds consumers by label, not by loading any `treerig.toml`: running
containers with `managed`, this repository's label and
`shared-use.<name>=true`, grouped by worktree. The sweep never runs while the
shared lock is held: whoever created the container releases it first. For each
worktree, one `Hold(Operation, Shared)` with a 5 s timeout runs the same
connect step as `up`. Busy worktrees are skipped with a notice naming them: a
busy `up` connects itself at its step 4, and any other holder is repaired by
the next `treerig up` there or `treerig shared up`, which always runs the sweep,
even when the instance is already running with the same definition. A daemon
restart is handled the same way: Docker is expected to keep a container's
user-connected networks across a restart, but this is unverified, and `doctor`
detects the case either way.

Crash recovery needs no new mechanism. A kill between steps 3 and 4 leaves a
worktree that is up and not connected; a kill between `down`'s disconnect and
`compose down` leaves a worktree that is disconnected and up, or a network that
Compose keeps ("A network ... is kept") until the next `down` or `remove`
disconnects again. Every step is idempotent, label-proven and repeated by the
next `up`, `down`, `remove` or `shared up`.

`shared down` never asks whether worktrees are using the instance: it is the
explicit stop. The removed container's attachments go with it, and it says how
many consuming worktrees were left without a server.

### How this differs from the removed router networks

| | Per-worktree router network (removed) | Shared container connected to a worktree's network |
|---|---|---|
| What joins | the shared router, to reach every routed service | one shared service container, by one alias |
| Network | a dedicated network per worktree, created by its Compose project | the worktree's own network, untouched by treerig |
| Traffic | across a bridge, and the proxy needed the host on the way to it | container to container on one bridge, no host hop |
| Lock | the machine-wide router lock for reconnect and orphan repair | the worktree's operation lock, plus the repository's shared lock nested for a connect only |
| Removal | `Reap` retried the disconnect, tolerated a kept network, and could fail | disconnect first, from the verified inventory, before any Reap |
| Repair | the router's `Ensure` swept every worktree | `up`, `shared up` and `doctor`, from labels |

### Routing and discovery

The proxy reaches the UI at `127.0.0.1:<host port>` like any routed service;
the shared project publishes it. Raw ports (SMTP) are published the same way,
each on its own pinned loopback port, for host processes: an IDE-run app,
`treerig run`, and #6 host routes. Containers use `<alias>:<port>` on their
network and never the pinned port, because a container cannot reach the host's
loopback under ufw. Hooks and `treerig run` get
`TREERIG_SHARED_ADDR_MAILPIT_1025=127.0.0.1:<host port>` and
`TREERIG_SHARED_URL_MAILPIT`.

`status` gains a `shared` group; `shared status` prints, per service: state
(`running`, `stopped`, `absent`, `unhealthy`), URL and addresses, whether the
definition is current or differs from this worktree's, whether this worktree is
connected, and which worktree defined it. The index page lists a repository's
shared UIs in a separate group beneath the checkouts, read from the records
(approved; `urls` unchanged). Their state comes from the same `docker ps` as
the checkouts', which now reads the shared label as a sixth field, because a
shared container carries no worktree label and used to make the whole
inventory unreadable. `index --json` gains an additive `shared` array.

### Doctor

A Shared section, only when `[shared]` is configured or shared resources
exist for the repository:
- each service's state, and whether this worktree's definition differs from
  the running one (warning);
- a running `used-by` container of this worktree whose network does not have
  the shared container attached: "up but not connected", repaired by `treerig
  up` or `treerig shared up`;
- the shared container attached to a network of a worktree that is stopped or
  gone, or to a network that does not carry that worktree's labels
  (reported, never touched; `down` and `worktree remove` clean the first kind);
- a running service whose record routes a web UI but has no route file
  (`shared.route`, a warning; a service that is down has none by design), a
  route file whose record routes another port or none (repaired by `up`, `shared up` and `router up`, and reported by the Router
  section as `router.orphaned-routes`), a route file this control home holds
  no record for (reported there, never removed), a resource with the
  repository label and no record, and a running instance with no registered
  worktree (information, not an error);
- the shared lock, through `lock.Describe`, in the Locks section;
- the shared routes in the route probe (decision 13).

A lost record is not data loss: down and reap work from labels alone, and
re-allocation lands on the same ports by the allocator's hash start.

### Implementation details (#123)

Where the decisions above leave a choice, this is what the implementation does.

- `used-by` may be empty or absent. A shared service nothing uses is never
  started by `up`, only by `treerig shared up`; it still gets pinned ports.
- `up` considers every `used-by` service of the Compose file, whether or not
  `up` names it: starting a shared service nothing needs is harmless, and
  failing to start one something needs is not. A `used-by` service that only
  an inactive profile defines passes the existence check (the file is read
  again with every profile on) and is stamped when its profile is active.
- A `used-by` service with `network_mode: none` or `container:…` is refused
  with the host and `service:…` ones: none of them has a network to join.
- Stopped and absent are both "started from the caller's definition": a
  stopped container is replaced with `compose up --force-recreate --no-deps
  <service>` (the caller's definition), which carries named and anonymous
  volumes over; `rm` followed by `--no-recreate` would orphan the anonymous
  ones. A running instance is only ever touched by `--no-recreate`, which
  guarantees that a dependency already running, or a sibling whose definition
  differs, is not recreated by a start.
- `shared up --recreate` replaces the running services it names whether or
  not the definition differs; it is how to restart one. It needs at least one
  name: the no-name form is refused, listing the running services (decision
  16). Approved by the owner. A differing definition without `--recreate` refuses before
  anything is changed.
- A shared file may not declare `ports:` (nor any non-loopback publish); the
  refusal points at `[shared.service.<name>]`, where treerig publishes on
  127.0.0.1. The worktree slug `shared` is reserved by the slug allocator.
  Approved.
- The drift hash uses pinned inputs: the file is read with an explicit empty
  `--env-file` and `--profile '*'`, and the hash covers the top-level volumes,
  networks, configs and secrets the closure references (prefix `v1:` kept). A
  Compose upgrade can change `compose config` output and show `differs` once;
  `shared up --recreate <name>` restamps. Approved.
- Named `shared down` takes along helper containers started only to serve the
  named services, once no declared service still uses them. Approved.
- `shared down --volumes` with names removes only the volumes a selected
  container mounts; without names it removes every volume of the stack. It
  also removes anonymous volumes (label `com.docker.volume.anonymous`) of
  containers it has proven are its own, and the prompt lists volume names.
  The plan is made again under the lock and the command refuses if it grew
  while the user decided. Names are validated against the slug pattern before
  they touch a record path, and a name with no record, declared service or
  container is refused up front. Approved.
- `shared up --recreate` deliberately makes two Compose calls: the services
  being started (dependencies included) with `--no-recreate`, then the target
  with `--force-recreate --no-deps`. A kill between them is harmless: the next
  `up` or `shared up` finds the dependencies running and replaces the target.
  A plain `shared down` strands the anonymous volumes of the containers it
  removes (the next container gets a fresh one, and `--volumes` can no longer
  find the old one), so it warns, naming them; named volumes keep their data.
- When `compose up` of a worktree fails with "has active endpoints" (Compose
  wanted to recreate a network a shared container is attached to) and shared
  services are in use, `up` disconnects them (`disconnectShared`, which needs
  only the operation lock it holds) and fails at once, telling the user to run
  `treerig down` and then `treerig up`, with Compose's error as the cause. It does
  not run `compose up` a second time: on the real daemon (Docker 29.7.2,
  Compose 5.5.0) the first attempt has already stopped the worktree's
  containers and half-removed the network, so a second attempt fails with "is
  not connected to the network". The real-daemon test pins this, then follows
  the message (`down`, `up`) and checks the shared service never stopped.
- A shared definition is hashed from the file read with the inherited
  environment minus every `TREERIG_*` variable except `TREERIG_HOME` and
  `TREERIG_CONTROL_HOME`, plus `TREERIG_PROJECT`, `TREERIG_REPOSITORY_ID` and the
  Compose project name, so a process started from a worktree's hook reads the
  same file as one started from a shell. Each service of the file carries the
  hash of its own closure, not only the declared ones.
- "Inside a worktree" for the bind-mount refusal is the config's directory and
  every live worktree of the repository Git lists, compared as written and
  with symbolic links resolved. A config or secret `file:` is refused the same
  way.
- `shared down` with names removes those services' containers, and the
  containers that were started only to serve them once no declared service's
  container is left; the stack's network goes when no container does. With
  `--volumes`, a volume goes when no container that stays mounts it. Only
  `--volumes` asks for confirmation, since only it deletes data.
- A record exists only for a service that declares a port. Starting a
  container prunes the pinned ports of container ports no longer declared
  (nothing runs that could still publish them); a running container's are
  left alone until it is recreated.
- `shared status --json` is `{"shared": [...]}`, the same entries as
  `status --json`'s `shared` array, which is omitted when nothing is declared.
  `definedBy` is the starting worktree's hostname or "a removed worktree".

- The routed UI (`port`) is recorded as `routed` in the shared record, set by
  the same `SyncSharedPorts` call that pins the ports whenever a container is
  created, so it always describes the container that runs. A running instance
  whose record does not route the declared `port` adopts it, with no restart,
  when that port is already pinned (it was a raw port when the container was
  created); a port that is not pinned waits for `shared up --recreate`, and
  `up` says so in one line. The converse holds too: a reused container whose
  `treerig.toml` no longer declares `port` stops being routed (the record's
  `routed` is cleared and the file removed) while its port stays pinned and
  published until the next recreate; the routed port follows the declaration
  of whichever worktree last ran `up` or `shared up`.
- A container stopped or removed any way but `shared down` leaves its route
  file, and the hostname answers 502. `doctor` reports it (`shared.route`),
  and `shared down` removes the file of a declared or named service that has no
  running container, even when its container is already gone. That decision is
  made again under the shared lock: if a service started since the plan was
  made, `shared down` refuses and removes nothing. A start that fails with a service up routes
  that service anyway; `up` does not refresh the index on that failure path,
  so the index catches up at the next refresh.
- `up` and `shared up` write the route file for every service they were asked
  about once it is up, and for a declared service that came up only as a
  dependency. `shared up` ensures the router first, but only when a selected
  service, or one it depends on, declares a `port`, and refreshes the index after it releases the
  shared lock (the router lock is never nested). `startSharedForUp` needs no
  router handle: `up` has already ensured the router, and the dynamic
  directory is `CommandContext.RouterState`.
- `TREERIG_SHARED_URL_<NAME>` is built next to `TREERIG_SHARED_ADDR_<NAME>_<PORT>`,
  from the records, with the router's port folded in as `urls` folds it in.
  `status` and `shared status` carry it as `url` (omitted for a service with no
  routed port). The verify hook of `doctor` gets it too, when the router's
  port resolved.
- `doctor` probes a shared route like any other (decision 13), counts and
  conflict-checks it with the other route files, and reports a running routed
  service whose file is missing as `shared.route`. A stopped service has no
  file by design.

### Platforms

Per-worktree connect behaves the same on Docker Desktop and OrbStack as on
Linux (spike below): aliases, SMTP, raw ports, loss of attachments on recreate
and their return on reconnect, survival across `docker restart`, and the
disconnect-before-`compose down` order. One difference is not treerig's: OrbStack
does not block traffic between separate Docker networks by IP address, so on
OrbStack any two Compose projects can reach each other by IP whether or not a
shared service exists. Name resolution stays per network on every engine, and
names are what services use, so no worktree reaches another's services by
accident. The macOS smoke test (Testing) still sends mail from a consumer
container to the shared service, loads the routed UI, and recreates the shared
container.

## Renaming a worktree (#11)

A worktree's slug names its hostname, `<slug>.<domain>`, and nothing else
that Docker or the disk holds: the Compose project name, the home, the locks
and the pinned ports derive from the repository and worktree IDs. So a rename
is a change of hostname and not of resources, and treerig renames and deletes no
volume. What carries the hostname is the registry record, the `Host` matchers
of the worktree's route files, the index card's slug, host and service URLs,
and the generated index page. The environment Compose gave its containers
also carries it (`TREERIG_SLUG`, `TREERIG_HOST`), which is why the verb refuses
while the worktree has any container, found by ownership labels, and asks for
`down`, which also removes the route files. A Compose file that builds the
slug into a volume name is beyond what treerig can see; `docs/worktrees.md` says what
that leaves behind.

The route files of Compose services can therefore only be crash leftovers
when a rename runs, dead routes to a stopped worktree. The rename removes
them (`router.RemoveWorktreeFragments`, as `down` does) and `up` writes them
afresh at the new hostname. It does not rewrite the old hostname to the new
one in place: a rename interrupted after that step and retried with another
slug would leave the file at the first one, which no record owns.

`moveRoutes` in `internal/commands/rename.go` is the one place a rename deals
with every route file of a record, and the seam for any other kind of file
that carries the hostname. Host routes (#6) are handled there: each recorded
route's file is rewritten from the renamed record (the new slug and domain and
the route's own definition, `hostRouteDocument` and `WriteHostFragment`), never
by replacing the old hostname in the file, and before the record changes. It is
idempotent: a rename that crashed after this step and is retried with another
slug overwrites each file from the record, so none is left at a hostname no
record owns, and a route whose file was missing gets one. A file the record has
no route for is left to the router's repair. The repair is also the net for a
file that disagrees after a crash between this step and the record commit
under the old slug: it rewrites the file from the record on the next `up` or
`router up`.

## What the implementation removes and keeps

Removed:
- per-worktree router networks and the router's connect, disconnect and
  reconnect of them;
- network-based orphan repair (the sweep of fragments whose network is gone;
  decision 9 replaces it with registration-based repair);
- the address-pool check;
- the hardening verifier and the protocol drift checks; Compose recreates
  the proxy when its configuration changes.

Kept:
- ownership proof by labels before anything is removed, never by name;
- no age-based lock cleanup; recovery stays explicit and checked;
- the router-home ownership check (#82) and the refusal of a router started
  by a newer treerig;
- the router lock, and the per-worktree operation lock around a worktree's
  own port allocations and route files.

Added: the shared lock, and connect and disconnect of a shared
container to a worktree's network (decision 22), with the ownership rule that
a shared resource carries no worktree label, so neither removal path can reach
the other's resources.

Route files are treerig's own files in its own directory, not Docker resources;
repairing them by registration match (decision 9) is not the removal the
ownership invariant governs.

## Spike evidence

Linux, 2026-09-29: Ubuntu 24.04, Docker 29.7.2, Compose 5.5.0, ufw with
default-deny INPUT, Traefik v3.7.9 (digest `652929a1...`).

| Question | Result |
|----------|--------|
| Bridge container to host through `host-gateway`, target published on `127.0.0.1` | Dropped by ufw |
| Host-network proxy to a `127.0.0.1`-published container port | Reaches it |
| Host-network proxy to a `127.0.0.1`-only host process | Reaches it |
| Host-network proxy to a `::1`-only host process | Reaches it via `localhost` or `[::1]`; `127.0.0.1` gives 502 |
| Hardened host-network Traefik binding `127.0.0.1:80` | Works as root; refused as uid 65534 |
| Exposure with `--entrypoints.web.address=127.0.0.1:80` | Loopback only; connections to the machine's other addresses refused |
| `--ping=true` with defaults | Opens `*:8080` |
| Healthcheck | Needs its own entrypoint flags |
| File-provider add, edit, delete | Picked up in 2-4 s (throttle about 2 s); a batch can land partly |
| WebSocket upgrade | Passes |
| Unchanged `compose up -d` / changed command | Keeps the container / recreates it |
| `docker restart` of the proxy | Routes kept |
| Restarted host process | Answers at once |
| Ephemeral published ports across a container restart | Change every time (hence pinned ports) |
| Pinned port already in use | `up` fails cleanly |
| `127.0.0.1:80` already taken | Proxy crash-loops; Compose shows only "unhealthy" |
| Target dropped by the firewall | Request hangs, no 502 |

macOS, 2026-09-29: macOS 26.5.1 arm64; Docker Desktop 29.7.2 with Compose
5.4.0; OrbStack 29.4.0 with Compose 5.1.2.

| Question | Result |
|----------|--------|
| Bridge proxy publishing `127.0.0.1:80` to a `127.0.0.1`-published container port and to a `127.0.0.1` host process, via `host.docker.internal` | Reaches both, on Desktop and OrbStack |
| `treerig-loopback` alias via `host-gateway` | Works on both |
| WebSocket upgrade | Passes on both |
| Route files added, edited, removed through the bind mount | Picked up in 1-2 s on both |
| Reachable on the LAN address | No, on both (OrbStack listens on `*:80` but refuses LAN connections; Desktop listens on `127.0.0.1:80`) |
| Engine restart | Survives |
| `docker restart` and `compose up --force-recreate` of the proxy | No measurable outage (one 404 poll within 1 s on Desktop) |
| `::1`-only host process | Reachable on OrbStack; **not** on Docker Desktop under any networking mode (IPv4-only and dual-stack, after restart) |
| A `--network host` container reaching the Mac's `127.0.0.1` | Works on both |

Consequence: a dev server must bind `127.0.0.1` (or all IPv4 addresses), not
only `::1`, on Docker Desktop. In Linux host mode `treerig-loopback` also maps to
`::1`, so a `::1`-only listener works there (see Open items).

Two early stalls (about 45 s on Docker Desktop's first run, and after a proxy
restart on a freshly installed OrbStack) did not reproduce in a targeted
follow-up. They are recorded as first-use warm-up and are covered by
`doctor`'s retries (decision 13).

## Spike evidence: shared services

Linux, 2026-09-29: Docker 29.7.2, Compose 5.5.0, ufw default-deny, a busybox
stand-in (not Mailpit). The spike tried a flat per-repository network, which
the owner rejected (decision 22); its collision result is the reason.

| Question | Result |
|----------|--------|
| Containers of two Compose projects on one labelled user-defined network reach a shared service by service name and by a network alias, SMTP 1025 and HTTP 8025 | Works under ufw |
| Two worktrees' consumers on that flat network | Each sees the other's services by name; same-named services collide (from the shared container `app` resolves to both; a consumer's own lookup hit its own `app` 20 of 20, not guaranteed). This is why the flat network is not built |
| Services kept off that network | Isolated |
| Parallel `compose up` of the shared project | One wins; the rest rc 1 "Conflict. The container name ... is already in use" |
| Publishing the shared UI on `127.0.0.1:<pinned>` while on a network | No conflict |
| `network_mode` service given `networks` | Compose refuses (hence the refusal of such a `used-by` service) |
| Changing a service's network attachment through the override | Recreates the container |

Per-worktree connect, 2026-09-29. Linux: Docker 29.7.2, Compose 5.5.0, ufw
default-deny. macOS 26.5.1 arm64: Docker Desktop 29.7.2 with Compose 5.4.0,
OrbStack 29.4.0 with Compose 5.1.2. Real Mailpit `axllent/mailpit:v1.31.3`,
busybox consumers.

| Question | Result |
|----------|--------|
| One labelled container connected with `--alias mailpit` to two worktree projects' default networks | Both worktrees reach it by the alias on 1025 (SMTP) and 8025 (UI), on all three engines |
| Worktree A resolving worktree B's services by name | Fails, as intended, on all three; `app` in A resolves to A's own `app` |
| Worktree A reaching B's containers by IP | Blocked on Linux and Docker Desktop; reachable on OrbStack, which does not isolate Docker networks by IP (see Platforms) |
| Mail from both worktrees' consumers, and from a host process through the published `127.0.0.1` raw port | All delivered and visible in Mailpit's API, on all three |
| `compose up --force-recreate` of the shared project | Attachments are lost and the alias stops resolving; reconnecting restores both, on all three |
| `docker restart`, stop/start of the shared container | Attachments and aliases survive |
| Consumer recreated (spec change, or whole-project force-recreate) | The shared container stays attached; the alias still resolves |
| Adding the `dev.treerig.shared-use.<name>` label through the override | Recreates only that consumer; re-running is a no-op |
| `compose down` of a worktree with the shared container attached | Exits 0 but leaves the network behind ("Resource is still in use"), on all three: treerig disconnects first and never trusts the exit code for a leftover network |
| Disconnect first, then `compose down` | Removes the network cleanly; the shared container keeps running |
| `docker network rm` of an attached network | rc 1, "has active endpoints" |
| Connect twice | rc 1, "endpoint with name ... already exists in network" (treated as connected; an alias change needs a disconnect first) |
| Disconnect when not connected / container gone / network gone | rc 1: "is not connected to network", "No such container", "network ... not found" (all treated as done, matched on the message) |
| Connect or disconnect a stopped container | Succeeds |

Verified while implementing #123, Linux, Docker
29.7.2, Compose 5.5.0, busybox stand-ins:

| Question | Result |
|----------|--------|
| `docker compose config --format json` of a service with `depends_on: [db]`, `env_file` and a relative bind mount | `depends_on` is printed as a map (`db: {condition, required}`), `env_file` is inlined into `environment`, a bind `source` is the absolute path, and a named volume's `name` carries the project name. treerig reads both `depends_on` forms |
| `compose up --no-recreate <svc>` with the running container's definition changed | Leaves the container alone. Without the flag Compose recreates it |
| `compose up --no-recreate <svc>` with the container stopped | Starts the stopped container with the definition it was created from, not the file's current one, so treerig replaces a stopped shared container with `compose up --force-recreate --no-deps <svc>`, which keeps its named and anonymous volumes, to start it from the caller's definition |
| Two processes' first `up`s of two worktrees at the same moment | The shared lock serialises them: one runs `compose up`, the other finds the container running; both end connected |
| `docker network disconnect` before `compose down` of a worktree whose network the shared container is on | The worktree's network is removed cleanly and the shared container keeps running |
| `worktree remove` of a worktree the shared container is attached to | Succeeds, with the shared container, its volume and its pinned ports untouched |

| `up` of an already-up worktree whose network Compose wants to recreate (a changed network label), the shared container attached | Fails: the network removal says "has active endpoints" and the worktree's containers were already stopped. `up` recognises that message when a shared service is in use, disconnects the shared services and stops with the message saying to run `treerig down` and then `treerig up`; it does not retry, because a second `compose up` cannot succeed here |
| `compose up --force-recreate --no-deps` on a stopped shared container with `volumes: ["/data"]` | The new container mounts the same anonymous volume; a file written before the stop is still there. `shared down --volumes` removes it |

Not verified, to check during implementation:

- Whether user-connected networks survive a daemon restart (a plain
  `docker restart` keeps them). The reconnect sweep covers the case either
  way. Still open: the daemon was not restarted, because it belongs to every
  other project on the machine.
- A stale endpoint: a worktree network removed after the shared container was
  killed mid-connect.
- Mailpit checks for its own updates (`/api/v1/info` reports
  `LatestVersion`); whether the shared definition should turn that off is a
  Mailpit setting, not a treerig one.

## Open items

Resolved while implementing #120 (Linux, 2026-10-10: Ubuntu 24.04, Docker
29.7.2, Traefik v3.7.9, a native rootful engine, so host mode):

- **The `treerig-loopback` alias in host mode.** `extra_hosts` works with
  `network_mode: host`: the container's `/etc/hosts` gets `127.0.0.1
  treerig-loopback`.
- **Mapping it to both `127.0.0.1` and `::1`.** Works. With `extra_hosts:
  ["treerig-loopback:127.0.0.1", "treerig-loopback:::1"]` a host process listening
  only on `[::1]` answers through the proxy (200), as does one on `127.0.0.1`
  only, and a target nothing listens on answers 502. The stack maps both in
  host mode, so a `::1`-only listener is reachable on Linux host mode and the
  `::1` warning of decision 11 (#6) can be limited to bridge mode, where it
  still holds on Docker Desktop (`host-gateway` is IPv4 only).
- **No wildcard listener.** `ss -ltn` with the proxy running in host mode,
  router on `127.0.0.1:30700` and ping on `127.0.0.1:30701`, lists those two
  and no `0.0.0.0` or `[::]` listener for the proxy; a request to the
  machine's other addresses on the router port is refused. As a non-root user
  `ss -ltnp` shows no process names for them (the proxy is root's, or
  uid 65534's); the integration test therefore asserts on `ss -ltn`. `sudo ss -ltnp` during an integration run showed traefik on `127.0.0.1:<router port>` and `127.0.0.1:<ping port>` only, and docker-proxy on `127.0.0.1:<index port>`.
- **Healthcheck flags.** `traefik healthcheck --ping` alone fails ("dial tcp
  :8080"): the healthcheck process needs the ping entrypoint's address, and
  `--ping.entryPoint` defaults to `traefik`. The stack passes both flags.
- **Ping on its own entrypoint.** `/ping` on the `web` entrypoint for an
  unrouted host answers the proxy's own 404, and the ping entrypoint answers
  `OK` on its loopback port.
- **The proxy's own 404 and 502 bodies.** An unmatched host gets `404 page not
  found` as `text/plain`; a loaded route whose target refuses gets `502 Bad
  Gateway`. `doctor` tells the two apart by that body (decision 13).
- **The `docker info` rules.** The table under "Proxy container" above, pinned
  in `internal/router/mode_test.go`.

Still open:

- Compose long-syntax `ports` merging with a user's own `ports` on Compose
  5.x.
- Bridge mode on Docker Desktop and OrbStack against the implemented stack
  (the spike verified the transport on macOS; the macOS smoke test below
  covers the stack), and on Docker Desktop for Linux and for Windows, which
  no spike covered. On a Linux engine bridge mode can be started with `[router] mode =
  "bridge"` and is tested for its wiring only: a bridge container on this
  machine's engine reaching `host-gateway` for a listener on `127.0.0.1` times
  out, so routing to targets does not work there.

## Testing

CI's macOS leg has no Docker. On macOS, `doctor`'s route probe (decision 13)
is the smoke test: run it against Docker Desktop and OrbStack by hand before
a release and record the result on the PR. The Linux Docker integration suite
covers host mode; bridge mode is exercised there by the `[router] mode`
override, which proves the wiring but not Desktop's networking. Shared
services add a decoy test that a worktree's inventory never lists a shared
resource and the shared inventory never lists a worktree's, including with the
shared container attached to the worktree's network; `worktree remove` and
`down` while it is attached; a two-process test of concurrent first `up`s; and
a recreate followed by the sweep, with a busy worktree skipped.

Host routes (#6) are proved on Linux by the real-daemon suite
(`host_routes_docker_integration_test.go`): a listener on the host, outside
Docker, answers at its worktree hostname through the proxy, `router.route-probe`
probes it, `down` keeps the route, `worktree remove` removes it, and a
`::1`-only listener answers in host mode. Docker Desktop and OrbStack cannot be
tested on Linux.

**Manual Mac check for host routes (#6), Docker Desktop and then OrbStack.**

Run these steps from a checkout of the branch with Docker Desktop running. Use a scratch home and router port 30080, not 80. If a router that another treerig root started is already running on this daemon, `up` refuses; stop that router yourself first.


**Setup**

1. Build treerig and create the scratch home:

   ```sh
   go build -o /tmp/treerig-mac ./cmd/treerig
   export TREERIG_HOME=$(mktemp -d)
   printf '[ports]\nrange = "30000-30999"\n[router]\nport = 30080\n' > $TREERIG_HOME/settings.toml
   (docker ps -a --format '{{.Names}}'; docker network ls --format '{{.Name}}'; docker volume ls -q) | sort > /tmp/mac-before.txt
   ```

2. Make a small git repo with a treerig.toml and a one-service compose file. Copying treerig.example.toml works. Then:
   - `cd` into the repo and run `/tmp/treerig-mac up`, then `/tmp/treerig-mac router status`.
   - Expect: mode `bridge`, port 30080.


**Routing to an IPv4 server**

3. In another terminal, start the server: `python3 -m http.server 5173 --bind 127.0.0.1`
4. Add the route: `/tmp/treerig-mac route add web --port 5173 --subdomain app`
   - Expect: "Routed http://app.<slug>.<domain>:30080/ to 127.0.0.1:5173", with no warning.
5. Request it: `curl -sS -m 10 -w '\n%{http_code} %{time_total}s\n' <URL>`
   - Expect: 200 and a directory listing. Record the time.
6. Run `/tmp/treerig-mac doctor`.
   - Expect: "127.0.0.1:5173 answers" on the web host route, plus a router.route-probe OK line for it.
7. Stop the server with Ctrl-C, then immediately run `curl -sS -m 30 -w '\n%{http_code} %{time_total}s\n' <URL>`.
   - PASS: a 502 within a few seconds.
   - FAIL, report it: a hang until the 30s limit, or a 504.
   - Run `/tmp/treerig-mac doctor` again. Expect a note that nothing is listening, and a route-probe warning.
8. Restart the server on 127.0.0.1 and curl again.
   - Expect: 200 immediately, with no treerig action.


**Other binds**

9. Restart the server on IPv6 only: `python3 -m http.server 5173 --bind ::1`
   - Run `/tmp/treerig-mac route add web2 --port 5173 --subdomain two`.
     - Expect: a warning that it "answers on ::1 but not on 127.0.0.1".
   - Curl two.<slug>.<domain>:30080.
     - Expect: failure (502). On OrbStack it may succeed; record what you see.
   - Run `/tmp/treerig-mac doctor`.
     - Expect: a warning on web2.
   - Clean up: `/tmp/treerig-mac route rm web2`
10. Bind all interfaces: `python3 -m http.server 5173 --bind 0.0.0.0`
    - Curl the web URL. Expect 200, and note any macOS firewall prompt.
    - Stop the server.
11. Optional, with Vite: `npx vite --host 127.0.0.1 --port 5173`
    - Open the page in a browser and check that HMR works.
    - If your domain isn't under .localhost, confirm you get a 403 until `server.allowedHosts` is set.


**Lifecycle**

12. Run `/tmp/treerig-mac route ls` and expect web.
    - Run `/tmp/treerig-mac down`.
    - `route ls` still shows web.
    - The web URL returns 502, and the service URL returns 404.
    - The file `$TREERIG_HOME/router/dynamic/host.*.web.yaml` remains.
13. Run `/tmp/treerig-mac worktree remove`, adding `-y` if it asks.
    - The host.*.yaml file is gone.
    - The web URL returns 404.


**Cleanup and reporting**

14. Stop the router and compare the daemon state with the snapshot from step 1:
    - `/tmp/treerig-mac router down`
    - Run the snapshot command from step 1 again, into /tmp/mac-after.txt.
    - `diff /tmp/mac-before.txt /tmp/mac-after.txt` should show nothing.
    - `lsof -nP -iTCP:30080 -sTCP:LISTEN` should be empty.
15. Post the output of steps 5, 7 and 9 on the PR, with your Docker Desktop and OrbStack versions.

## Sequence

1. This note, in its own docs PR.
2. #119, pinned ports and the record builder.
3. #120, the proxy, on the pinned ports.
4. #6, host routes, on the proxy.
5. Shared services (#123), in two pull requests. The first (connect and
   disconnect bookkeeping, reconnect sweep, drift check, lock, CLI, raw ports)
   reads #119's allocator and record builder, so it follows #119 and can run
   in parallel with #120 and #6. The
   second (the routed UI: route file, repair, `shared.` prefix, reserved slug,
   index) follows #120.
6. #121, rootless Docker, later.
