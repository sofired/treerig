# Routing: pinned loopback ports and a loopback proxy

How berth routes `<service>.<slug>.<domain>` to a worktree's services, and
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
- the checkout index, `urls`, `BERTH_URL` and `BERTH_SERVICE_URL_<NAME>`;
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
                     berth-loopback:<port>
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
| 2 | Route files target the alias `berth-loopback`, which the proxy's `extra_hosts` maps to `127.0.0.1` in host mode and to `host-gateway` in bridge mode. | Route files never depend on the mode, so a mode change never rewrites them. |
| 3 | Ports live in an optional `ports` map on the registration record (record version stays 1; a record without it is valid). Allocation runs under the registry lock: start at a hash of (worktree ID, service), probe forward, skip any port in any registration and any port that fails a loopback bind probe. Default range 20000-29999, configurable in `settings.toml`; tests and scratch roots use a separate range. The allocator also skips the index port and the ping port (decisions 7 and 8), which live in router state. One record builder serves every registry writer, so no writer can drop `ports`. A user's own dev server may take an allocated port later; the bind probe at allocation, Docker's clean bind error on `up`, and `--reallocate-port` (decision 4) cover that. | Stable across restarts and reboots; the hash start makes re-allocation after a lost record land on the same port. 20000-29999 sits below Linux's ephemeral range (32768) and macOS's (49152), so the OS does not hand it out. A single builder is the only way to guarantee a writer that does not know about `ports` cannot erase it. |
| 3a | Registry locks are per control home. Two control homes can allocate the same port at once; this race is accepted, and Docker's bind error on the second `up` catches it. It covers the router's index and ping ports too (decisions 7 and 8): the remedy is `--reallocate-port` on the worktree side, or removing `router/state.json` for the router's. | A machine-wide registry lock is not this design (the same trade `docs/design/locks.md` records for the router). The failure is clean and recoverable (decision 4). |
| 4 | Stuck port: `berth up --reallocate-port <service>`. It refuses when the port is held by this worktree's own container, proven by its ownership labels, and says the URL changes. | The escape hatch for a port another program took. Ownership proof stops it from moving a port out from under a running container. |
| 5 | `up` releases the allocation of a service removed from `berth.toml`. Otherwise only `worktree remove` releases allocations, after the worktree's containers are gone (after Reap) and before the record itself is deleted. | A port may not be reused while a container of the old owner could still hold it. Releasing before the record is deleted keeps the allocation reachable if the process dies in between; the step is idempotent. |
| 6 | `urls`, `BERTH_URL` and `BERTH_SERVICE_URL_<NAME>` stay hostname URLs. New: `BERTH_SERVICE_PORT_<NAME>` for hooks and `berth run`, and a `port` key in `urls --json` and `status --json` (additive). | Hostnames are the contract users bookmark; the port is for tools that must bypass the proxy. |
| 7 | The checkout index (busybox) publishes on its own `127.0.0.1` port and is routed like a service. Its port is stored in router state under `BERTH_HOME`, not in any registration, and is allocated once from the same range under the router lock. | One routing path for everything; no shared network for the index either. The index belongs to no worktree, so it has no record to hold a port; router state is where the router's other per-root facts live. |
| 8 | Traefik's ping gets a dedicated entrypoint, never `web`. Host mode: `--entrypoints.traefik.address=127.0.0.1:<ping port>` with `--ping.entryPoint=traefik`, and the healthcheck carries the same address flag. The ping port is allocated like the index port (once, from the same range, under the router lock) and stored in router state. Bridge mode: the ping entrypoint listens on a fixed port inside the container, is never published, and takes no port from the range. The index and ping ports share one router-state file. Never Traefik's default `*:8080`. | Ping on `web` would shadow every routed service's own `/ping` on every hostname. `--ping=true` alone opens `*:8080`, a wildcard listener (spike). The healthcheck runs as a separate process and needs its own copy of the entrypoint flags (verified on Linux). |
| 9 | A route file is judged only when this control home holds a record for that exact repository and worktree. For such a file, a record with no port for its service gets the file removed and a record with another port gets the file rewritten to the recorded one, by `router up`/`Ensure`; `doctor` reports both. A file with no such record is reported by `doctor` (`router.orphaned-routes`) with the count and how to remove it (delete it from the dynamic directory), and never removed, because the router's state is per berth root and it may belong to another control home. It is a warning (instance `unmatched`) when this control home knows the repository but not the worktree, likely stale, and a notice (instance `other-home`, status ok) when it does not know the repository, likely another control home's. `up` commits a port allocation to the registration before it writes the route file. Repair reads registrations without the registry lock (every registry writer goes through `files.AtomicJSON`, a temporary file then a rename) and re-reads a record immediately before acting, which narrows the window against a concurrent `up` or a just-added host route without closing it; the rewrite is eventually consistent with the writer, whose next `up` writes the same port. The router lock and the registry lock are never nested. | Once ports are reused, a stale file would send an old hostname to another worktree's service. |
| 10 | The name `router`, the router-home ownership check (#82) and the refusal of a router started by a newer berth stay. A running router with the per-worktree-network layout is refused with "run `berth router down` first". `Protocol` is bumped only when Compose cannot recreate a router of the older generation into the new one; a change of mode, port or digest never bumps it. `router status` and `doctor` report a running router at another protocol with the same advice. The labels `dev.berth.router-mode` and `dev.berth.router-port` and the file `router/state.json` are storage names, pinned by tests. | Those are safety properties, independent of the transport. No code converts the old layout. |
| 11 | Host routes (#6) are stored in the registration record (`hostRoutes`); route files are derived from it, named `host.<repo>.<wt>.<name>.yaml`; the Traefik router name is `<project>_host_<name>`. A host route may not share a service's name. It takes the same routing fields as a `[service.<name>]` table (`--subdomain`, `--path`) and gets the hostname a service with that name and those fields would get. `route add` and `doctor` warn on a listener that answers only on `::1` in bridge mode (#120 maps `berth-loopback` to both `127.0.0.1` and `::1` in host mode, so there it is reachable; see Open items). | The record is the source of truth and route files are a cache, so decision 9's repair covers host routes too. `host.` is a prefix the service-route sweeps and `ListFragments` never match; `_` is outside the service-name alphabet, so the router name cannot collide with a service's. Repository and worktree IDs are UUIDs and service names are `[a-z0-9-]`, so `host.<repo>.<wt>.<name>.yaml` cannot collide with `<repo>.<wt>.<service>.yaml`; a test uses a repository named `host`. The `::1` warning holds in bridge mode: Docker Desktop cannot reach `::1`. In host mode `berth-loopback` maps to `::1` as well, which reaches a `::1`-only listener (verified). The fix is to bind `127.0.0.1`. |
| 12 | `doctor` warns on a routed service with a fixed published host port. `up` refuses a routed service with `network_mode: host`, `network_mode: service:...`, or replicas above 1. | Berth needs to own the published port. A host-network or shared-namespace service has no port to publish; several replicas cannot share one. |
| 13 | No Traefik API. `doctor` probes each route over HTTP. A 404 is judged by sending the same request (path and Host) straight to the route file's target on `127.0.0.1:<port>`: the same 404 there is the service's own and the route is fine; a 404 the target does not give means the route is not loaded (a failure), retried for 5-10 s. A 502, 503 or 504, an unreachable target or a router that does not answer is retried at most twice, and the total is capped. The comparison has limits: a service that itself 404s at the probed path makes an unloaded route read OK, and a router 404 with an unreachable target reads as target down (a warning), not as an unloaded route. | The file provider picks changes up after about a 2 s throttle. Without the API, the response itself is the evidence. |
| 14 | This note lands in its own docs PR before #119. | So the decisions are reviewable apart from code. |
| 15 | **Model.** A shared service is one instance per repository UUID, defined in a separate Compose file named by `[shared] file` and run as one Compose project per repository, `berth-shared-<24 hex of sha256("shared:" + repositoryID)>`. `[shared.service.<name>]` names a service in that file by its key (an optional routed `port`; optional raw `ports`; `used-by`), keyed like `[service.<name>]` and `[commands.<name>]`. `used-by` lists the worktree services that use it: they make a worktree "need" the service (auto-start) and are the services whose networks it joins (decision 22). A shared name may not equal a service in the worktree's Compose file, a `[service.<name>]` or a host route (the alias would resolve to two things). Host-run shared services (a service running natively outside Docker, used by containers) are out of scope: on Linux with ufw a container cannot reach the host. | A separate file keeps the service out of every worktree's own project (a shared service in the worktree file would start in each of them, and each branch's copy of the file would differ). One project means one ownership label set. Listing consumers puts the opt-in on the shared side, so no `[[service]]` change is needed and non-routed workers can use it. |
| 16 | **Definition drift.** "First up wins" compares a definition hash, label `dev.berth.shared-definition=v1:<sha256>`, on the running container: the canonical JSON of the entries for that service and every service it transitively names in `depends_on`, from `docker compose config --format json` on the shared file, before berth's override (so published ports are not in it), interpolated with the shared environment only (no per-worktree variables). While the container runs, berth never calls `compose up` for it, so Compose cannot recreate it on its own. A difference is a **warning** on `berth up` (it names the worktree in label `dev.berth.shared-defined-by`, or "a removed worktree", and the remedy) and a **refusal** on `berth shared up` without `--recreate`. Stopped or absent: the caller's definition starts it. `berth shared up --recreate <name>` force-recreates that service only, keeps its volumes and reconnects every consuming worktree (decision 22). The hash is not stored in the record. | A branch bumping an image pin must not block an unrelated feature branch's `up`, but an explicit request that cannot be honoured is a failure. The label is the only truth about what runs; a second copy in a file could disagree with it. Compose's own config hash includes berth's override and is Compose-version specific. |
| 17 | **Resources and ownership.** Every resource of the shared stack (containers, volumes, the stack's own default network) carries `dev.berth.managed=true`, `dev.berth.repository-uuid=<repo>` and `dev.berth.shared=true`, and **never** `dev.berth.worktree-uuid`. A worktree's `used-by` containers carry `dev.berth.shared-use.<name>=true` besides their worktree labels. Removal (`shared down`, `--volumes`) lists by the shared labels and re-verifies each resource (managed, repository, `shared=true`, project name equals the derived one, no worktree label) before removing it, never through the caller's Compose file. `shared down` removes containers and the default network and **keeps volumes**; only `--volumes` removes them. `worktree remove` and `down` never remove a shared resource: neither lists one, because it has no worktree label, and a decoy test pins that in both directions, including with the shared container attached to the worktree's network. The label keys and the project name are storage names. | Ownership by label, in a scope no worktree shares. Removal that does not need a Compose file works from any worktree, and after the last one is gone. The `shared-use` label lets a sweep find consumers without loading any worktree's `berth.toml`. |
| 18 | **Locking.** New scope `SharedScope(controlHome, repositoryID)`, target `<control home>/locks/shared/<repo>`, one lock for all of a repository's shared services, timeout 15 min (a `compose up --wait` of 5 min plus a pull and a recreate, as `RouterTimeout` is derived). `lifecycle.Requests` gains `Shared`; order is operation, development, shared, registry, with the router still alone, and **nothing takes the shared lock and then a worktree lock**. (1) `berth up` starts or reuses the instance under the shared lock alone, before `mutate`, so a worktree's 25-minute operation lock is never held waiting on it. (2) Connecting a worktree runs inside that worktree's operation lock and takes the shared lock nested, after it, for the connect only; `lifecycle` needs a way to add a scope to a held set in order. (3) Disconnecting (`down`, `worktree remove`) needs the operation lock only, so removal never waits behind a slow shared start. (4) The reconnect sweep after a shared container is (re)created runs only after the shared lock is released, as one `Hold(Operation, Shared)` per registered worktree with a short timeout (`ReconnectTimeout`, 5 s); a busy worktree is skipped with a notice and left to its own connect step, `berth shared up` or `doctor`. The registry package takes its own short lock inside for a port allocation. The lock is per control home, like the registry's (decision 3a): two control homes on one daemon can both start the stack, and the loser gets "container name ... is already in use" (verified), which berth tolerates by re-inspecting by label; a running instance found without a local record is reported, never re-allocated. Docker calls under it are bounded; if the state of the container cannot be determined, `up` fails rather than assume it is absent. No age-based cleanup: the flock frees on death, and recovery is `berth worktree unlock --shared` through the same `Clear` checks, with the shared project name as the survivor-scan marker. | Two first `up`s serialize and the second reuses the instance. Connect and disconnect for one worktree exclude each other through its operation lock, which is what keeps the shared container from being attached to a network that is being removed. A killed starter frees the lock; the next holder re-inspects by label and `compose up` is idempotent. Failing closed avoids a duplicate or a recreate of a running instance because the daemon was slow. |
| 19 | **State, ports and routes.** State is `<control home>/repositories/<repo>/shared/<name>.json`: `{version: 1, name, domain, ports: {"<container port>": <host port>}}`, written through `files.AtomicJSON` by one record builder. Every declared port (`port` and each of `ports`, at most 16 per service, TCP) gets a pinned host port from the same allocator, keyed by (repository ID, `shared:<name>:<container port>`); the allocator's skip set reads these records. Each is published on `127.0.0.1:<host port>` by the shared project's override. Ports are kept across `down`, released only by `down --volumes` (after the containers and volumes are gone, before the record is deleted, decision 5), and never by `worktree remove`. The web UI (`port`) is also routed at **`<name>.shared.<domain>`** using the domain recorded at allocation, through the route file `shared.<repo>.<name>.yaml` (router name `<shared project>_shared_<name>`; `shared.` is a prefix the service and host sweeps never match). Slug `shared` is reserved by the slug allocator so `<name>.shared.<domain>` cannot be a worktree's host. Decision 9's repair covers shared route files against these records. `berth shared up --reallocate-port <name>:<port>` (decision 4) refuses while the container runs (proven by labels). Two repositories that share a domain and a shared name conflict at allocation; the second is refused, naming the first. | A record is needed because the container is gone after `down` while its ports must stay pinned. Raw ports are for host processes (an IDE-run app, host routes, `berth run`); containers use `<alias>:<port>` on their network. The hostname must not be a worktree slug: any single label under the domain could be a branch name, so `shared` is the one word reserved. |
| 20 | **Discovery.** Container side: the alias is the declared name, so `mailpit:1025`; no variable is injected into Compose (that would tie the worktree's Compose environment to shared state, and a changed variable recreates its containers, #28). Host side (hooks, `berth run`): `BERTH_SHARED_ADDR_<NAME>_<PORT>=127.0.0.1:<host port>` for every declared port (`<PORT>` is the container port; `<NAME>` maps as in `BERTH_SERVICE_URL_<NAME>`, and the trailing number keeps it unambiguous), and `BERTH_SHARED_URL_<NAME>` for a routed `port`. Built next to `ServiceURLVariables`. `status` and `status --json` (an object) gain a `shared` array: `{name, state, url?, addrs: [{port, addr}], definition: "current"\|"differs"\|"unknown", connected: bool, definedBy?}`. `urls` and `urls --json` (a top-level array, a contract) are unchanged. `doctor` lists shared services; the index lists routed UIs. | The name in `berth.toml` is the contract for containers. Hooks and host processes are on the host, where an in-network alias does not resolve. |
| 21 | **CLI.** `berth shared up [name...] [--recreate] [--reallocate-port <name>:<port>]`, `shared down [name...] [--volumes]`, `shared status [--json]`. `shared up` on a running instance with the same definition still runs the reconnect sweep (the repair path). `down --volumes` deletes data and prompts as `worktree remove` does. `worktree remove` of a repository's last registered worktree prints one line, best effort with a bounded Docker probe that never fails the remove, if shared services are still running, naming `berth shared down`. `shared down` works from any checkout of the repository and needs no `berth.toml` (label namespace from `--label-namespace`, as `worktree remove`). | Auto-start, explicit stop: the owner's lifecycle needs a stop command, and a way to reclaim data needs one destructive path. |
| 22 | **Network model: per-worktree connect (Owner ruling).** The shared container joins each consuming worktree's own network with the declared alias, by `docker network connect --alias <name>`. There is no shared network and no external network in any override. All worktrees of a repository share service names, so on one flat network a `used-by` service calling another (`web` to `api`) can resolve to another worktree's container: silent cross-worktree traffic, which the spike confirmed is ambiguous. Every connect and disconnect target is proven by labels first. Connect: after the worktree's `compose up` and before its hooks, for each network a running `used-by` container is on (found by the `shared-use` label) that carries this worktree's ownership labels. Disconnect: `down` before `compose down`, and `worktree remove` before the first Reap, from each network in the verified inventory. Repair: `berth up` and `berth shared up` reconnect idempotently, `doctor` reports a running consumer that is not connected. A network that fails proof is reported and never touched. | This brings back connect and disconnect bookkeeping of the kind the redesign removed from the router. What made the router's version fragile was the host hop and the machine-wide router lock, and neither applies: the traffic is bridge-internal, and every step is under the one worktree's operation lock. Disconnecting before Reap is what stops the attached container from making a worktree's network unremovable. |

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

- The index and ping ports live in `<BERTH_HOME>/router/state.json`
  (`{"version": 1, "indexPort": N, "pingPort": M}`), written by `Ensure` under
  the router lock the first time they are needed and kept. A stored port that
  became the router's own port is replaced, and an unusable stored port is
  dropped from the file too, so it does not stay reserved. `pingPort` appears the first time
  the router runs in host mode.
- Bridge mode's ping entrypoint listens on `127.0.0.1:8082` inside the
  container (`router.BridgePingPort`), reachable only by the health check.
- A running router that carries a lower protocol label, or none, is refused
  with "run `berth router down` first", as one with a higher label is refused
  with the downgrade advice. The protocol number is 6.
- The check that a port is free dials a privileged port and binds an
  unprivileged one (a bind to port 80 fails for a non-root user whether or not
  anything holds it). A port the stack's own running containers already hold
  (published bindings, or the proxy's entrypoint addresses in host mode) is not
  a conflict.
- Route repair judges a file only when this control home holds a record for
  its exact repository and worktree. The router's state is per berth root and
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

The README's Router and Trust model sections are rewritten in the #120 PR.

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
file = "compose.shared.yaml"  # relative to berth.toml; its own Compose project

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

`berth up`, when the worktree has a `used-by` service that exists in its
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
   stamps `dev.berth.shared-use.<name>=true` on each `used-by` container and
   changes nothing else: no network, no `networks` key.
4. Still inside the operation lock, before hooks (`prepare` and `seed` may send
   mail): take the shared lock nested and connect. For each `used-by`
   container that is running, read its networks; for each network carrying
   this worktree's managed, repository and worktree labels (and its project
   label), `docker network connect --alias <name> <network> <shared container
   id>`, skipped when the shared container is already an endpoint. Then hooks.

`berth down` disconnects first, then runs `compose down`: for each
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
sweep finds consumers by label, not by loading any `berth.toml`: running
containers with `managed`, this repository's label and
`shared-use.<name>=true`, grouped by worktree. The sweep never runs while the
shared lock is held: whoever created the container releases it first. For each
worktree, one `Hold(Operation, Shared)` with a 5 s timeout runs the same
connect step as `up`. Busy worktrees are skipped with a notice naming them: a
busy `up` connects itself at its step 4, and any other holder is repaired by
the next `berth up` there or `berth shared up`, which always runs the sweep,
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
| Network | a dedicated network per worktree, created by its Compose project | the worktree's own network, untouched by berth |
| Traffic | across a bridge, and the proxy needed the host on the way to it | container to container on one bridge, no host hop |
| Lock | the machine-wide router lock for reconnect and orphan repair | the worktree's operation lock, plus the repository's shared lock nested for a connect only |
| Removal | `Reap` retried the disconnect, tolerated a kept network, and could fail | disconnect first, from the verified inventory, before any Reap |
| Repair | the router's `Ensure` swept every worktree | `up`, `shared up` and `doctor`, from labels |

### Routing and discovery

The proxy reaches the UI at `127.0.0.1:<host port>` like any routed service;
the shared project publishes it. Raw ports (SMTP) are published the same way,
each on its own pinned loopback port, for host processes: an IDE-run app,
`berth run`, and #6 host routes. Containers use `<alias>:<port>` on their
network and never the pinned port, because a container cannot reach the host's
loopback under ufw. Hooks and `berth run` get
`BERTH_SHARED_ADDR_MAILPIT_1025=127.0.0.1:<host port>` and
`BERTH_SHARED_URL_MAILPIT`.

`status` gains a `shared` group; `shared status` prints, per service: state
(`running`, `stopped`, `absent`, `unhealthy`), URL and addresses, whether the
definition is current or differs from this worktree's, whether this worktree is
connected, and which worktree defined it. The index page lists a repository's
shared UIs in a separate group, read from the records (approved: last PR;
`urls` unchanged).

### Doctor

A Shared section, only when `[shared]` is configured or shared resources
exist for the repository:
- each service's state, and whether this worktree's definition differs from
  the running one (warning);
- a running `used-by` container of this worktree whose network does not have
  the shared container attached: "up but not connected", repaired by `berth
  up` or `berth shared up`;
- the shared container attached to a network of a worktree that is stopped or
  gone, or to a network that does not carry that worktree's labels
  (reported, never touched; `down` and `worktree remove` clean the first kind);
- a record with no route file, a route file with no record, a resource with
  the repository label and no record, and a running instance with no
  registered worktree (information, not an error);
- the shared lock, through `lock.Describe`, in the Locks section;
- the shared routes in the route probe (decision 13).

A lost record is not data loss: down and reap work from labels alone, and
re-allocation lands on the same ports by the allocator's hash start.

### Platforms

Per-worktree connect behaves the same on Docker Desktop and OrbStack as on
Linux (spike below): aliases, SMTP, raw ports, loss of attachments on recreate
and their return on reconnect, survival across `docker restart`, and the
disconnect-before-`compose down` order. One difference is not berth's: OrbStack
does not block traffic between separate Docker networks by IP address, so on
OrbStack any two Compose projects can reach each other by IP whether or not a
shared service exists. Name resolution stays per network on every engine, and
names are what services use, so no worktree reaches another's services by
accident. The macOS smoke test (Testing) still sends mail from a consumer
container to the shared service, loads the routed UI, and recreates the shared
container.

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
  by a newer berth;
- the router lock, and the per-worktree operation lock around a worktree's
  own port allocations and route files.

Added: the shared lock, and connect and disconnect of a shared
container to a worktree's network (decision 22), with the ownership rule that
a shared resource carries no worktree label, so neither removal path can reach
the other's resources.

Route files are berth's own files in its own directory, not Docker resources;
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
| `berth-loopback` alias via `host-gateway` | Works on both |
| WebSocket upgrade | Passes on both |
| Route files added, edited, removed through the bind mount | Picked up in 1-2 s on both |
| Reachable on the LAN address | No, on both (OrbStack listens on `*:80` but refuses LAN connections; Desktop listens on `127.0.0.1:80`) |
| Engine restart | Survives |
| `docker restart` and `compose up --force-recreate` of the proxy | No measurable outage (one 404 poll within 1 s on Desktop) |
| `::1`-only host process | Reachable on OrbStack; **not** on Docker Desktop under any networking mode (IPv4-only and dual-stack, after restart) |
| A `--network host` container reaching the Mac's `127.0.0.1` | Works on both |

Consequence: a dev server must bind `127.0.0.1` (or all IPv4 addresses), not
only `::1`, on Docker Desktop. In Linux host mode `berth-loopback` also maps to
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
| Adding the `dev.berth.shared-use.<name>` label through the override | Recreates only that consumer; re-running is a no-op |
| `compose down` of a worktree with the shared container attached | Exits 0 but leaves the network behind ("Resource is still in use"), on all three: berth disconnects first and never trusts the exit code for a leftover network |
| Disconnect first, then `compose down` | Removes the network cleanly; the shared container keeps running |
| `docker network rm` of an attached network | rc 1, "has active endpoints" |
| Connect twice | rc 1, "endpoint with name ... already exists in network" (treated as connected; an alias change needs a disconnect first) |
| Disconnect when not connected / container gone / network gone | rc 1: "is not connected to network", "No such container", "network ... not found" (all treated as done, matched on the message) |
| Connect or disconnect a stopped container | Succeeds |

Not verified, to check during implementation:

- Whether user-connected networks survive a daemon restart (a plain
  `docker restart` keeps them). The reconnect sweep covers the case either
  way.
- A stale endpoint: a worktree network removed after the shared container was
  killed mid-connect.
- Mailpit checks for its own updates (`/api/v1/info` reports
  `LatestVersion`); whether the shared definition should turn that off is a
  Mailpit setting, not a berth one.

## Open items

Resolved while implementing #120 (Linux, 2026-10-10: Ubuntu 24.04, Docker
29.7.2, Traefik v3.7.9, a native rootful engine, so host mode):

- **The `berth-loopback` alias in host mode.** `extra_hosts` works with
  `network_mode: host`: the container's `/etc/hosts` gets `127.0.0.1
  berth-loopback`.
- **Mapping it to both `127.0.0.1` and `::1`.** Works. With `extra_hosts:
  ["berth-loopback:127.0.0.1", "berth-loopback:::1"]` a host process listening
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
