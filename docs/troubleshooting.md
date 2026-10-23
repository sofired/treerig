# Troubleshooting

The failures people hit most: the message you see, how to fix it, and why it happens.

Start with `treerig doctor`: it checks prerequisites, the registry, locks, the router and this checkout, and it names what is wrong. The entries below quote what treerig prints; a name, path, port or number in a quote stands for your own. Run as `trig`, the `[treerig]` prefix on errors and warnings reads `[trig]`, and some hints still say `treerig`; either name works.

Jump to:

- [A hostname does not resolve](#a-hostname-does-not-resolve)
- [A dev server answers 403 or "Invalid Host header"](#a-dev-server-answers-403-or-invalid-host-header)
- [Port 80 is taken](#port-80-is-taken)
- [A service declares no port](#a-service-declares-no-port)
- [The hostname answers 502 Bad Gateway](#the-hostname-answers-502-bad-gateway)
- [A route is stale](#a-route-is-stale)
- [A lock is held](#a-lock-is-held)
- [Refusing to use state from a newer treerig](#refusing-to-use-state-from-a-newer-treerig)
- [`worktree remove` refuses to run](#worktree-remove-refuses-to-run)
- [`worktree remove` cannot delete a directory](#worktree-remove-cannot-delete-a-directory)
- [`doctor` reports a failure](#doctor-reports-a-failure)
- [Rootless or remote Docker is refused](#rootless-or-remote-docker-is-refused)
- [Docker Compose is too old](#docker-compose-is-too-old)
- [A pinned port is held](#a-pinned-port-is-held)

## A hostname does not resolve

Chrome opens the URL, but `curl`, Safari or a language HTTP library cannot find the host. `curl` says the following; that wording is `curl`'s, not treerig's.

```text
curl: (6) Could not resolve host: feature-login.acme.localhost
```

**Fix:** run `curl -sSI http://treerig.localhost` and read the answer. If the router is not on port 80, add its port, as in `curl -sSI http://treerig.localhost:8080`. `treerig router status` prints the port and says whether `TREERIG_ROUTER_PORT`, `[router] port` in `settings.toml` or the default of 80 chose it; `treerig doctor` says the same.

- An `HTTP/1.1` status line: resolution works, and the router answered.
- `Could not resolve host`: add every hostname `treerig urls` prints, plus `treerig.localhost`, to `/etc/hosts` (it has no wildcards), or use `curl --resolve`.
- A connection error: resolution works and the router is not running. Run `treerig router up`.

**Why:** Chrome and Firefox map `*.localhost` to your own machine themselves. Other clients use the system resolver, and on plain glibc or musl systems (Debian, Alpine) it does not. See [Hostname resolution](platforms.md#hostname-resolution).

## A dev server answers 403 or "Invalid Host header"

A [host route](host-routes.md) to a dev server shows a 403 "Blocked request" page (Vite) or "Invalid Host header" (webpack-dev-server). Those are the dev server's words, not treerig's.

**Fix:** allow the worktree's hostname in the dev server's own settings, for Vite `server.allowedHosts`. [Allowed hosts](host-routes.md#allowed-hosts) has the setting for Vite, webpack-dev-server and Django.

**Why:** the router passes the worktree hostname through as the request's `Host` header, and many dev servers refuse hosts they do not know.

## Port 80 is taken

`up` or `router up` stops with:

```text
Cannot start the shared router: a port it needs is in use.
  127.0.0.1:80 (the router) is held by …; stop it, or choose another port with TREERIG_ROUTER_PORT or [router] port in …/settings.toml
```

When the container itself fails to start, the message is `The shared router failed to start. Make sure 127.0.0.1:80 is free, or choose another port with TREERIG_ROUTER_PORT or [router] port in …`.

**Fix:** stop the holder, or move the router. Put `[router]` / `port = 8080` in `~/.treerig/settings.toml`, or set `TREERIG_ROUTER_PORT=8080`. The URLs treerig prints follow the new port. See [Settings](settings.md) and [Environment variables](environment.md).

**Why:** something else listens on the router's port. The message names the holder: a container, or "another process" with the command that finds it.

## A service declares no port

`up` stops with:

```text
Service web declares no port: set port in its [service.web] table, or expose one in compose.yaml.
```

**Fix:** add `port = 8080` to the `[service.web]` table, using the port the container listens on, or add `expose: ["8080"]` to the service in the Compose file. See [Writing your compose.yaml](compose.md#ports) and [Configuration](configuration.md).

**Why:** a routed service needs a port, and treerig finds none: the table sets no `port`, and the Compose service has no `expose` or `ports` entry. treerig does not read the `EXPOSE` line of the image's Dockerfile.

## The hostname answers 502 Bad Gateway

For a routed Compose service: the browser or `curl` gets `502 Bad Gateway` from its hostname, and `up` printed no error.

**Fix:** make the app listen on `0.0.0.0`, not `localhost`: [Listen on 0.0.0.0](compose.md#listen-on-0000) has the setting for common dev servers. Then check that `port` or `expose` is the port it listens on.

**Still 502?** Find which side is broken. Get the service's pinned port from `treerig urls --json` (its `port` key; if there is none, run `treerig up` first), then ask it directly:

```bash
curl -i http://127.0.0.1:<pinned port>/
```

- Any response, whatever the status: the container answers, and the problem is on the router side. Run `treerig doctor` and see [A route is stale](#a-route-is-stale).
- `Failed to connect … Couldn't connect to server` (curl exit 7; older curls say `Connection refused`): the container is not running, or has exited. `treerig status` shows its state; `docker compose logs` in the worktree shows why.
- Usually `Empty reply from server` or `Recv failure: Connection reset by peer`: the port is published but the app inside does not answer on it. The app is probably bound to localhost, or `port` is not the port it listens on.

**Why:** the router reaches a service at its pinned `127.0.0.1` port and answers 502 when nothing there accepts the request. The usual cause is a server bound to localhost inside its container, which Docker's published port cannot reach. See [Writing your compose.yaml](compose.md#ports).

For a host route or a shared service's web UI, see [Host routes](host-routes.md) and [Shared services](shared-services.md).

## A route is stale

A hostname answers with the router's own 404, or still reaches an old service, after `up`, `down` or `worktree remove`. `treerig doctor` reports:

```text
web.feature-login.acme.localhost: the router answers 404 for it after 6 s but its target does not, so the route is not loaded; its route file may be missing or not valid
```

**Fix:** run `treerig up` or `treerig router up`, which repair a route file that disagrees with its worktree's registration. If that does not help, make the router reread every route file. This stops routing for every worktree on this Docker daemon, whichever treerig root started the router, until it restarts (see [Router ownership](routing.md#router-ownership)): `treerig router down`, then `treerig router up`.

**Why:** a route file can disagree with its registration, for instance after a port was reused. On Docker Desktop for macOS, the file-sharing layer can also miss the change that updates a route file, so the router keeps routing what the file no longer says. See [Docker Desktop file sharing](platforms.md#docker-desktop-file-sharing-and-the-file-provider).

## A lock is held

A command prints, and keeps waiting:

```text
Waiting for the worktree feature-login lock. …
```

Or it gives up with `Another process holds the worktree feature-login lock. …`.

**Fix:** wait for the holder to finish, or stop it as the message says (`kill <pid>`). If it is stopped with Ctrl-Z, resume it with `kill -CONT <pid>`. If the holder died, the next command to take the lock checks that and recovers it by itself. To clear a lock on demand, run `treerig worktree unlock <slug>`; it refuses unless the holder is provably gone. See [Lock recovery](lock-recovery.md).

**Why:** another treerig command is working on the same worktree, registry or router, or a command that was killed left a `docker` child holding the lock. The message says who holds it, for how long, and what to do. See [Locks](concepts.md#locks).

## Refusing to use state from a newer treerig

A command stops with a line like:

```text
Refusing to use the control home …: it is format 3, written by a newer treerig, and this build reads format 2. Upgrade treerig, then run this again.
```

The same sentence follows `Refusing to touch the container …` and similar openings.

**Fix:** upgrade the treerig you are running, or run the newer one. Do not delete the state to get past the message. See [Stored-state format](settings.md#stored-state-format).

**Why:** something on this machine, a stored file or a Docker resource, was written by a newer treerig than the one you ran, perhaps from another checkout or a different install. This build does not read, repair, rewrite or delete it.

## `worktree remove` refuses to run

`worktree remove` stops before it touches anything. A release build never does this
for a missing pin: it removes with itself. A development build, one built from
source or a snapshot, refuses without a pinned teardown binary:

```text
[treerig] ERROR: Refusing to remove this worktree: this is a development build (0.0.0-dev), which may carry a branch's code, and no teardown binary is pinned. A development build removes only with a pinned one (a release build needs none).
  Pin the latest release: `treerig teardown pin --latest`
  or a treerig you trust: `treerig teardown pin --file <a treerig you trust> --sha256 <its sha256> --release <a label>`
  (`treerig teardown pin --help` explains both). To remove with this build's own code while you work on removal itself, add `--dev-teardown`. Nothing was removed.
```

**Fix:** use a release build, or pin a treerig build once and run the removal again.

1. With cosign 3.0.0 or newer on your `PATH`: `treerig teardown pin --latest`, which pins the newest release that is not a pre-release.
2. Without cosign: verify a download by hand ([Verifying a download](install.md#verifying-a-download)), then `treerig teardown pin --file ./treerig --sha256 "$(sha256sum treerig | cut -d' ' -f1)" --release my-build` (on macOS, `shasum -a 256 treerig | cut -d' ' -f1` for the digest).

`treerig teardown status` says whether the pin is intact and whether removal will run it; `treerig doctor` has a line for it. See [Teardown pin](teardown-pin.md).

A pin that exists is used by every build, release builds included, so the same opening, with another reason, covers:

- a stored binary that is missing or has other bytes: pin it again with the command the message gives;
- a pin of a newer format, or one treerig did not write: delete `<control home>/teardown/` and pin again;
- a pin with no binary for this platform: pin one for this machine;
- a control home inside a git work tree, or a pin store you do not own or that others can write: the message names the path; move `TREERIG_HOME` (or `TREERIG_CONTROL_HOME`, if you set it) to a directory that is not inside a git work tree, and fix the owner and mode, delete `<control home>/teardown/` and pin again, or move the control home to a local filesystem (`sudo treerig worktree remove` also refuses once a pin exists: run it as yourself);
- `could not run the pinned teardown binary … (permission denied)`: the control home is probably on a `noexec` filesystem; remount it with `exec`, or move it with `TREERIG_CONTROL_HOME`;
- `… (no such file or directory)` on Linux, with a note that `/proc` is not available: mount `/proc`, or run treerig where `/proc` is available.

**Why:** a development build may carry code from a branch, so removal runs only a build you verified once. A pin you made is honoured by every build, so that you can keep removal on a build you checked. `--dev-teardown` removes with the build you invoked, for work on removal itself, and warns on every run.

## `worktree remove` cannot delete a directory

`worktree remove` stops with:

```text
Could not delete … (first refusal: …). Some of its files are most likely owned by another user — written by a container running as a different uid. Take ownership back, for example with

  docker run --rm --network none -v …:/d <image> sh -c "chown -R 1000:1000 /d && chmod -R u+rwX /d"

then run 'treerig worktree remove' again to finish.
```

**Fix:** run the `docker run` command it prints, then run `treerig worktree remove` again. It finishes the removal.

**Why:** a container that runs as another uid, such as `postgres`, took ownership of files in a bind-mounted directory, and you cannot delete them. Use a named volume for data such a service writes. See [Bind mounts on Linux](platforms.md#bind-mounts-on-linux).

## `doctor` reports a failure

`treerig doctor` ends with this on stderr, and exits 1 (or with the `verify` hook's own code when that hook failed):

```text
[treerig] ERROR: 2 check(s) failed.
```

Every check is one line marked `ok`, `warn`, `fail` or `skip`. Without Docker on `PATH`, for example:

```text
  fail  Docker Engine   the `docker` command is not on PATH; is Docker installed?
  skip  Docker Compose  needs the `docker` command
```

**Fix:** fix what the `fail` line names, then run `treerig doctor` again. To read the result from a script, use `treerig doctor --json` and select by check id. See [Doctor](doctor.md).

**Why:** the `fail` lines say what is wrong. A `skip` means the check needed something that failed.

## Rootless or remote Docker is refused

`up` or `router up` stops with:

```text
Rootless Docker is not supported yet (#121): its host network is RootlessKit's, not this machine's, so the proxy could not reach your services. Use a rootful Docker Engine, Docker Desktop or OrbStack.
```

A remote daemon is refused too, with `treerig routes through a proxy that reaches your services on this machine's loopback, so it needs a Docker daemon on this machine, but Docker is at …`.

**Fix:** use a rootful Docker Engine on this machine, Docker Desktop or OrbStack. For a remote daemon, unset `DOCKER_HOST` or switch `docker context` to a local socket. See [Platform notes](platforms.md#rootless-docker).

**Why:** the router reaches your services over this machine's loopback, which a rootless or remote daemon does not share with you.

## Docker Compose is too old

`treerig doctor` shows a failing line for Docker Compose:

```text
  fail  Docker Compose  2.20.3; 2.21 or newer is needed
```

**Fix:** update Docker Compose; `docker compose version` shows what you have. If the line says `could not read a version from …` instead, Compose answered with output treerig could not parse; run `docker compose version --short` to see it. The 2.21 floor is stated in [Getting started](getting-started.md).

**Why:** Compose is older than 2.21, the oldest version treerig is tested with.

## A pinned port is held

`up` stops with:

```text
Cannot start: a routed service's pinned loopback port is in use.
  api: 127.0.0.1:20417 is held by another process (…)
Stop what holds it, or run 'treerig up --reallocate-port <service>' to give the service another port.
```

**Fix:** stop the holder, or run `treerig up --reallocate-port api` to give that service a new port. See [Ports](routing.md#ports).

**Why:** something else took the port treerig pinned for the service: another treerig worktree, a container (named in the message), or another process. `up` proves a container is the worktree's own by its labels, never by its name.
