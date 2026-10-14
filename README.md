# berth

Give every Git worktree a durable identity, an isolated home, and a routed URL.

Check out a branch into a worktree and it gets its own environment: its own
Docker Compose project, its own data directory, its own hostname on a shared
local router, and a lock that stops two commands from mutating it at once.
Remove the worktree and everything it owned is torn down — verified by
ownership label, never by name.

The tool is language- and framework-agnostic. It knows about Git, Docker, and
the filesystem. Everything specific to *your* project — migrations, seeding,
health checks — arrives as a small config file and a handful of executables
in whatever language you already use.

> **Status: pre-release.** The worktree lifecycle works end to end against
> Docker Compose: `up`, `dev`, `down`, `status`, `urls`, `run`, `backup`,
> `restore`, `worktree list`, `worktree remove`, `worktree rename`,
> `worktree unlock`, and `doctor` — identity, locks, the registry, the hook
> sequencer, a runtime that stamps every resource with ownership labels and
> refuses to touch anything it cannot prove it owns, a shared hardened router
> that makes every worktree reachable at its own hostname, and a machine-wide
> index page listing every checkout. See [Roadmap](#roadmap).

## How it works

```
berth.toml ──reads──▶  berth  ──spawns──▶  your hooks (any language)
                         │
            ┌────────────┼────────────┐
            ▼            ▼            ▼
      identity &     control      Docker Compose
      locks          home         (label-owned)
```

**Identity.** Two UUIDs are written into the Git directory: one for the
repository, one for the worktree. Paths and branch names can change; the UUIDs
cannot. A worktree keeps its environment across branch renames, rebases, and
moves. Each marker file starts with a `format` line, then the UUID.

**Slugs.** Each worktree gets a slug derived from its branch — `feature/login`
becomes `feature-login` — and is routed as `feature-login.<your-domain>`. Slugs
are unique per domain, because the hostname is what must not collide: two
projects with different domains can both have a `feature-login`. A collision
within one domain gets a short hash suffix rather than a surprise. The slug
`shared` is reserved for the repository's shared services, so a branch named
`shared` gets the same suffixed slug a collision would (`shared-` and a short hash
of its identity). Slugs are
sticky: once allocated, a worktree keeps its slug — suffix included — across
domain changes whenever it is free, so two clones can never swap hostnames
underneath you. Reclaiming a bare name is an explicit
[`worktree rename`](#renaming-a-worktree), never a side effect.

**Locks.** Every command that changes a worktree, the registry, or the shared
router takes an advisory lock: `flock(2)` on the lock's own file, held by an
open file descriptor. The kernel frees the lock once every copy of that
descriptor is closed: killing the holder — even by `SIGKILL` — closes its
own, but a `docker` or `docker compose` child it started inherits a copy and
keeps the lock until that child exits too; hooks and `run` commands never
inherit it.
Either way, there is no stale window and nothing to go quiet: recovery never
waits on a heartbeat, because there is none. A command that finds the lock
held waits for it — a worktree operation for up to 25 minutes, the router's
own lock for up to 20 — except one run from the worktree's own hooks or
commands, whose caller may already hold the lock: that refuses at once (see
[docs/hooks.md](docs/hooks.md)). A waiting command, after a couple of seconds
and then every 30 seconds, says who holds it (its command, how long) and what
to do: wait for it to finish or stop it (`kill <pid>`), or, if the holder is
merely stopped (`Ctrl-Z`, alive but not working), resume it (`kill -CONT
<pid>`) and let it finish, or end it; on Linux it also lists the processes
still holding the lock. Recovery from a holder that died is automatic and
checked, never assumed: the moment a lock is free, whoever takes it next
verifies that the previous holder's own record names a process that is
genuinely gone — by machine ID (or boot ID, in a container image with none),
PID, and start time, so a reused PID or a different boot is never mistaken for
the same process — then scans, twice a second apart, for anything that holder
started and left running, before treating the lock as recovered. `berth
worktree unlock <slug|uuid>` runs that same check on demand, without waiting
to acquire the lock itself, so a lock nobody is actively waiting on can still
be cleared right away; `--registry` and `--router` point it at the registry
and router locks instead of a worktree's. [Lock recovery](#lock-recovery) says
exactly what it checks and what it cannot see. `doctor` reports every lock
currently held, and every free lock whose record is still unreleased; an
unreleased record alone doesn't prove the holder is gone — the next command to
take that lock checks the recorded holder itself before treating it as
recovered. A berth command that holds a lock or runs children catches
`SIGINT`, `SIGTERM` and `SIGHUP`, so closing the terminal lets it finish or
stop cleanly, and release its lock, rather than dying at once; a read-only
command just stops.

**Homes.** Each worktree owns a directory under the control home
(`~/.berth/control-home` by default) created owner-only, with path containment
that refuses symlinks anywhere in the chain before creating or removing
anything. The same goes for the shared router's own state under the berth
root (`~/.berth/router`), with one exception: the two directories the router
stack bind-mounts, `router/dynamic` and `router/index`, are 0755, because the
containers that read them run as root without the capability that lets root
bypass file permissions, and on Linux Docker Engine that leaves a 0700
directory unreadable to them. Their parents stay owner-only — a bind mount is
entered at its mount point, so the parents are never traversed. Only the
index page and route files are written there; nothing secret.

**Hooks.** `prepare`, `seed`, `verify`, `backup`, and `restore` are
executables you point at from the config file. `up` invokes `prepare` and
`seed`: `seed` never runs before `prepare`, and not at all if a configured
`prepare` exited non-zero. `doctor` invokes `verify`. `berth backup [dir]` and
`berth restore <dir>` invoke `backup` and `restore` with a bundle directory as
their first argument: [docs/hooks.md](docs/hooks.md#backup-and-restore) has its
contract. `doctor` also checks that all five hook files can start. The tool
never learns what any of them do. A hook runs without a time limit unless
`[hooks.timeout]` gives it one: past its limit berth stops the hook and its
whole process group on the host, and reports it as timed out (exit 124), not
failed. (Work it handed to a container, such as a `docker compose exec`, keeps
running there: see [docs/hooks.md](docs/hooks.md#timeouts).)

**Backup and restore.** A bundle is a directory the `backup` hook fills and
the `restore` hook reads; berth creates it (or takes an empty one), marks it
`.incomplete` until the hook exits zero, and refuses to restore one still so
marked. Hooks run with the worktree's own Compose project in their
environment (`COMPOSE_PROJECT_NAME`), so a PostgreSQL round trip is two short
scripts:

```sh
# scripts/backup.sh: dump to a temporary name, then move it into place
set -euo pipefail
bundle=$1
docker compose exec -T db pg_dump -U app --clean --if-exists app > "$bundle/app.sql.tmp"
mv "$bundle/app.sql.tmp" "$bundle/app.sql"
```

```sh
# scripts/restore.sh: stop at the first error; all or nothing
set -euo pipefail
bundle=$1
[ -f "$bundle/app.sql" ] || { echo "restore: $bundle/app.sql is missing" >&2; exit 1; }
docker compose exec -T db psql -U app -d app -v ON_ERROR_STOP=1 --single-transaction < "$bundle/app.sql"
```

Keep the database on a named volume in your Compose file, so `worktree remove`
reaps it; the anonymous volume a Postgres image declares is not reaped.
`berth backup ~/backups/before-upgrade` then `berth restore
~/backups/before-upgrade`. With no directory, `backup` writes under the
worktree's home, which `worktree remove` deletes (its confirmation counts the
bundles); give it a directory to keep one. A restore overwrites the
worktree's live data.

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
must bypass it, and reaches them as `BERTH_SERVICE_PORT_<NAME>` and the
`port` key of `urls --json` and `status --json`. `up` refuses to start when a
pinned port is held by anything but this worktree's own container, proven by
its ownership labels rather than its name, and says who holds it: another
berth worktree, a container by name, or another process. `berth up
--reallocate-port <service>` gives that service a new port (and says its
loopback URL changed); it refuses while the worktree's own container holds
the current one. Removing a service from `berth.toml` releases its port on
the next `up` that starts every service, once Compose has removed the dropped
service's container; an `up` that names some services keeps that port
reserved until the next full `up` or `worktree remove`. `worktree remove`
releases the rest once the worktree's containers are gone. Two worktrees on different control homes can pick the
same port at once (each home has its own registry lock); the second `up`'s
clean bind error catches it, and `up` names the holder. A routed service
with `network_mode: host`, `network_mode: service:…` or more than one replica
is refused, because it has no port berth can publish; `doctor` warns on a
routed service whose Compose file also publishes a fixed host port of its
own.

**Shared services.** Some services are wasteful once per worktree: an email
catcher, an object-store emulator, a broker's web UI. A *shared service* runs
once per repository, in a Compose project of its own, and is connected to
each worktree whose services use it. "Repository" is one repository UUID, so
two clones of the same upstream have two instances. `berth.toml` names the
service, the ports it publishes for the host and the worktree services that
use it (see [Shared services](#shared-services) below); the first `up` of any
worktree that uses it starts it, and every later one finds it running and
leaves it alone. It joins each using worktree's *own* network under its name,
so `mailpit:1025` reaches the same Mailpit from every worktree, and a service
of one worktree still cannot resolve a service of another. Its resources carry
the repository's ownership labels and a `shared` label, and never a worktree's:
`worktree remove` and `down` cannot list one, and only `berth shared down`
removes it. It is the one thing that outlives the worktrees that use it.

**Router.** One Traefik proxy container per machine, started on first `up`,
shared by every worktree of every repository. It serves a single loopback
port and routes by hostname, and every hop to a service is a loopback hop: a
request for a routed service goes to `berth-loopback:<port>`, a name that
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
| Native rootful Linux Engine | `host` | shares the host's network namespace and binds `127.0.0.1:<port>` itself; `berth-loopback` is `127.0.0.1` and `::1` |
| Docker Desktop, OrbStack | `bridge` | listens on `:80` inside its container, published on `127.0.0.1:<port>`; `berth-loopback` is the engine's `host-gateway`, which forwards to the host's loopback (verified on macOS) |

Rootless Docker (its "host" network is RootlessKit's, not the machine's),
a daemon on another machine (`DOCKER_HOST` or a Docker context pointing
away) and anything berth does not recognise are refused with a message
saying why. `[router] mode` in `settings.toml` (`"host"` or `"bridge"`)
overrides the choice, and is the way through the refusal for an engine berth
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
kept in `router/state.json` under the berth root beside the index's port; in
bridge mode it is a fixed port inside the container, never published, and takes
none from the range. There is no
separate verification step: Compose recreates the proxy whenever its
configuration changes, and `router status` reports whether it is running and
healthy, with the container's last log lines when it is not. Before starting,
berth checks that the router, index and (host mode) ping ports are free and
names whatever holds one; `doctor` makes the same check while the router is
absent or unhealthy.

The router belongs to the berth root that started it, which a label on its
containers says: `up` or `router up` from another root refuses to take it over,
and `router down` hands it on (see [Configuration](#configuration)). A
running router with a layout this build does not share is refused until
`berth router down` stops it, and `router status` and `doctor` give the same
advice.

## Install

berth is a single static binary for Linux and macOS, on amd64 and arm64
(Windows is not supported). It needs Git, Docker and Docker Compose at run
time; `berth doctor` checks all three once it is on PATH. Homebrew and the
install script install the latest release, which is never a pre-release (see
[Pre-releases](#pre-releases)); the packages and archives are per release.

### Homebrew

```bash
brew install sofired/tap/berth
```

Homebrew installs a prebuilt bottle where the tap has one for your platform;
otherwise it builds berth from the release's source, which needs Go (Homebrew
installs that for the build). Shell completions install with it.

### Install script

```bash
curl -fsSL https://raw.githubusercontent.com/sofired/berth/main/install.sh | sh
```

[`install.sh`](install.sh) picks the release archive for your platform,
checks its sha256 against the release's `checksums.txt` before extracting
anything, and installs `berth` into `~/.local/bin`. It never uses `sudo`. It
checks the checksum, not the signature on `checksums.txt`: that is a separate
step (see [Verifying a download](#verifying-a-download)). Pass options after
`sh -s --`, and pin the script itself to a tag if you want the exact script a
release shipped with:

```bash
curl -fsSL https://raw.githubusercontent.com/sofired/berth/v1.2.3/install.sh \
  | sh -s -- --version 1.2.3 --dir /usr/local/bin
```

`--dir` (or `BERTH_INSTALL_DIR`) is the directory to install into and
`--version` (or `BERTH_VERSION`) pins a release; without it the latest release
is installed. It uses `curl`, or `wget` when there is no `curl`, and
`sha256sum`, or `shasum -a 256` when there is no `sha256sum`.
If the directory is not on your PATH, it prints the line to add.

### Linux packages

Each release attaches a `.deb`, an `.rpm` and an `.apk` for amd64 and arm64.
They put `berth` in `/usr/bin` and the bash, zsh and fish completions where
those shells look for them:

```bash
VERSION=1.2.3    # a release on the Releases page
curl -fsSLO "https://github.com/sofired/berth/releases/download/v${VERSION}/berth_${VERSION}_amd64.deb"
sudo apt install "./berth_${VERSION}_amd64.deb"                    # Debian, Ubuntu

curl -fsSLO "https://github.com/sofired/berth/releases/download/v${VERSION}/berth-${VERSION}-1.x86_64.rpm"
sudo dnf install "./berth-${VERSION}-1.x86_64.rpm"                 # Fedora, RHEL

curl -fsSLO "https://github.com/sofired/berth/releases/download/v${VERSION}/berth_${VERSION}_x86_64.apk"
sudo apk add --allow-untrusted "./berth_${VERSION}_x86_64.apk"     # Alpine
```

On ARM the architecture is `arm64` in the `.deb` name and `aarch64` in the
`.rpm` and `.apk` names. The packages are not signed with a package-manager
key, so apk needs `--allow-untrusted`; check the download as described under
[Verifying a download](#verifying-a-download) instead. Uninstall them with your
package manager.

### Release archive

Each release also has a `berth_<version>_<os>_<arch>.tar.gz` for `linux` and
`darwin`, with `amd64` and `arm64`. It holds `berth` at its root, with
`LICENSE`, `NOTICE`, `README.md` and the shell completions under
`completions/`; extract it and put `berth` anywhere on PATH.

### Pre-releases

A tag with a suffix, such as `v1.2.3-rc.1`, is published as a GitHub
pre-release. It is never "latest", so the commands above do not install it,
and Homebrew does not see it. Install one with `install.sh --version
1.2.3-rc.1`, or download its files from the
[Releases page](https://github.com/sofired/berth/releases).

### Verifying a download

Every release publishes `checksums.txt`, covering the archives, the source
archive and the packages. Compare the file you downloaded with it (run in the
directory holding both; this works with `sha256sum` or, on macOS,
`shasum -a 256`):

```bash
VERSION=1.2.3
FILE="berth_${VERSION}_linux_amd64.tar.gz"
grep " ${FILE}\$" checksums.txt | sha256sum --check     # macOS: shasum -a 256 --check
```

Optionally, check that `checksums.txt` came from this repository's release
workflow. It is signed with [cosign](https://github.com/sigstore/cosign)
(keyless, with the workflow's identity), and each file has a GitHub build
provenance attestation. Download `checksums.txt.sigstore.json` beside it:

```bash
VERSION=1.2.3
FILE="berth_${VERSION}_linux_amd64.tar.gz"
cosign verify-blob \
  --certificate-identity "https://github.com/sofired/berth/.github/workflows/release.yml@refs/tags/v${VERSION}" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --bundle checksums.txt.sigstore.json checksums.txt
gh attestation verify "$FILE" --repo sofired/berth
```

[`docs/design/release.md`](docs/design/release.md) lists every asset name and
the identity.

### From source

Build it with Go, at least the version [`go.mod`](go.mod) names:

```bash
git clone https://github.com/sofired/berth && cd berth
go install ./cmd/berth   # installs $(go env GOPATH)/bin/berth (or $GOBIN)
```

or build a binary yourself and put it wherever you like:

```bash
mkdir -p ~/.local/bin && go build -o ~/.local/bin/berth ./cmd/berth   # any directory on PATH
```

A plain build prints `0.0.0-dev` for `berth --version`, not a release's
version. To stamp one, build with
`-ldflags "-X github.com/sofired/berth/internal/brand.Version=<version>"`.

### Shell completion

`berth completion <shell>` prints a completion script for bash, zsh, fish or
PowerShell. It completes commands, their flags with descriptions, and
`--config` with `.toml` files. It also completes registered values,
read-only: no lock is taken, Docker is never touched, and nothing is
written:

- `worktree remove` and `worktree unlock` complete a registered worktree's
  slug, described with its domain and its last-known path; `worktree
  unlock` also offers a worktree it can still recover a stale lock for even
  though it was never registered (or is no longer), named by its UUID.
- `worktree rename` completes nothing: the new slug is yours to choose.
- `--domain` on `worktree remove` and `worktree unlock` completes a
  registered domain, and narrows the slugs offered to that domain once one
  is given — hiding any recoverable, unregistered worktree `worktree
  unlock` would otherwise also offer, since one carries no domain to match.
- `up` completes the service names declared in this worktree's `berth.toml`,
  skipping any already named on the command line. Each is described by its
  `[service.<name>.index]` label, else its subdomain or path, else "host root".
- `backup` and `restore` complete directories, for their bundle directory.
- `teardown pin --file` completes file names, for the binary to pin.
- `run` completes the command names under `[commands]`, in the order the file
  lists them, each described by its command line. Only the name is completed,
  never what follows it.

A registry that is missing, empty or unreadable, or a `berth.toml` that
cannot be found or parsed, completes nothing rather than failing.
`berth completion <shell> --help` says how to load the script for good; to
try it in the current shell:

```bash
source <(berth completion bash)   # or zsh
berth completion fish | source    # fish
```

A renamed binary or symlink (see [Calling it something else](#calling-it-something-else))
prints a script for the name it was invoked as.

## Quick start

The tool is tested with Docker Compose 2.21 and newer (`berth doctor` checks,
as does `docker compose version`). From the root of your repository,
`berth init` writes a starter `berth.toml` there (the file is commented, names
your Compose file, and opens with the schema directive described under
[Editor support](#editor-support)); it refuses to replace one that exists.
Or write the file yourself and commit it at the root:

```toml
[project]
name   = "acme"
domain = "acme.localhost"          # worktrees route as <slug>.acme.localhost

[compose]
file = "docker/compose.yaml"

[service.api]                      # a service is a table named by its Compose service
                                   # (no subdomain: routes at the worktree host root)
[service.api.index]
label   = "API"
purpose = "REST surface"

[service.mail]
subdomain = "mail"                 # mail.<slug>.acme.localhost

[hooks]
prepare = "./scripts/migrate.sh"   # services healthy, nothing seeded
seed    = "./scripts/seed.py"      # only after prepare exits zero
verify  = "./scripts/check.sh"     # run by doctor; exit 0 means healthy
backup  = "./scripts/backup.sh"    # berth backup: writes a bundle into the directory it is given
restore = "./scripts/restore.sh"   # berth restore: loads one back

[hooks.timeout]                    # optional: no entry, no limit
prepare = "30m"                    # a hung migration fails `up` after half an hour
verify  = "2m"                     # and a hung check fails `doctor` after two minutes

[commands]
plan = "./scripts/keycloak.sh plan"   # on demand, with `berth run plan`
```

Then:

```bash
berth config validate     # parse and check the file
berth config schema       # print its JSON Schema, for editors
berth up                   # register this worktree, start its services, run prepare (+ seed the first time)
berth dev                  # the same, then follow its logs until Ctrl-C
berth status               # per-service state
berth urls                 # print its routed URLs
berth route add web --port 5173 --subdomain app   # route a dev server on the host at app.<slug>.<domain>
berth route ls             # this worktree's host routes
berth run plan             # run one of the project's own [commands], on the terminal
berth backup [dir]         # run the backup hook, writing a bundle into dir (default: under the worktree's home)
berth restore <dir>        # run the restore hook on a bundle directory
berth down                 # stop it; data is kept
berth shared up            # start the shared services, or replace one, and reconnect every worktree
berth shared status        # their state, addresses and whether this worktree is connected
berth shared down          # stop them; their named volumes are kept
berth worktree list        # every worktree of this repository, with state
berth worktree remove      # tear it down, verified by ownership
berth worktree rename NEW  # change its slug, and with it its hostname
berth worktree unlock      # clear a dead holder's lock, once it is checked gone
berth doctor               # check prerequisites, registry, router; run verify
```

`up` runs the branch's own build and hooks. Read [Trust model](#trust-model)
before bringing up a branch you did not write.

`up` starts the Compose project under a name derived from the worktree's
UUIDs, with every container, volume, network, and image it builds labelled
`dev.berth.managed`, `dev.berth.repository-uuid`, and
`dev.berth.worktree-uuid`, and, on everything but the volumes, the
`dev.berth.format` label that says which format of labels those are (a named
volume carries none: Compose would offer to recreate a volume whose label
changed, losing its data). Your Compose file is never asked to add those
labels; a generated override file does it, so forgetting one — and silently
exempting a resource from cleanup — is not possible. The Compose file sees
the same `BERTH_*` variables hooks do, so `${BERTH_WORKTREE_HOME}` works in a
bind mount exactly as it does in a seed script.

`dev` is `up` followed by `docker compose logs --follow` on the worktree's
project: the last 100 lines of each container, then new ones. It is `up` all
the way through (the same checks, router, shared services, hooks and URL
table), and if `up` fails, `dev` fails the same way, with the same exit code,
and follows nothing. What it adds is a session: while it is attached,
`worktree remove` and a second `dev` refuse, naming the session, and telling
you to press Ctrl-C in the terminal running it. `up`, `down`, `run` and the
other verbs still work meanwhile.

The session lasts until you end it. What `berth down` from another terminal
does to it is up to Compose's follower, which varies: it may end once the
containers are gone, and then so does the session, with a message; or it may
keep waiting for new containers (Compose 5.5.0 has done so after a
`restart: always` service restarted), and then `dev` says once that no
container is left and idles, and a later `berth up` may show the new
containers' logs. An idle session still refuses `worktree remove` until
Ctrl-C. The services keep running when it ends; `berth down` stops them.

Ctrl-C, SIGTERM and SIGHUP stop the log follower's whole process group and
release the lock, print `The development session of <host> ended (<signal>).
Its services are still running; "berth down" stops them.` as a notice rather
than an error (`<signal>` names the one that ended it, SIGINT for Ctrl-C; the
second sentence is left out when no container is left), and exit with the
status a shell reports for that signal: 130 for Ctrl-C, 143 for SIGTERM, 129
for SIGHUP (see
[Output and exit codes](#output-and-exit-codes)). A follower that ends on its
own, because Docker went away or every container is gone, ends the session
with a message on stderr and its own exit code (1 when it exited 0). A
`kill -9` of berth itself frees the lock with the process but leaves the log
follower running on its own; end it with `kill`.

`status` reports each declared service as `running`, `starting` (Compose says
`restarting`, or `running` with its health check still pending), `stopped`
(Compose says `created`, `exited`, `dead`, `paused` or `removing` — the
container exists but nothing is serving), `absent` (no container at all, the
state after `down`), or `unknown` for any state Compose adds that this list
does not name. `unknown` carries Compose's own word in parentheses, and a
running service that is failing its health check carries `(unhealthy)`.

Those five words are about one service. Two other vocabularies are about a
whole checkout. `worktree list` reports each registration as `ready`,
`pending` (registered, but its first `up` never finished), or `missing`
(registered, but the checkout is gone). Only this repository's checkouts can
be called missing — that needs Git, and `--all` lists records from other
repositories as the registry knows them. The index page answers a different
question — whether a checkout's services are up — with `Running`, `Partial`,
`Stopped`, `Unknown`, and the same `Missing`, which means the same thing in
both places. None of the three vocabularies is frozen before 1.0. Their
machine-readable forms are in [Machine-readable output](#machine-readable-output).

Each `[service.<name>]` table becomes a route: the tool publishes the service's
pinned loopback port through the generated override and writes the
file-provider route file that sends the hostname to it, so your Compose file
never mentions the router. Chrome and Firefox resolve `*.localhost` to the loopback address on
their own; everything else depends on the system resolver — see
[Platform notes](#platform-notes). If port 80 is taken, set `[router] port`
in `settings.toml` (see [Configuration](#configuration)) or
`BERTH_ROUTER_PORT` — the printed URLs follow it.

```bash
berth router status       # is the shared router running and healthy, and in which mode?
berth router up           # start it without starting a worktree
berth router down         # stop it; worktree services keep running, unrouted
```

**Index.** `http://berth.localhost` (plus the port, if changed) lists every
checkout on the machine across every project: branch, hostname, path, whether
its services are running, and a link for each service that opted in with
`[service.<name>.index]`. It is a static page served from the router stack,
regenerated by `up`, `dev`, `down`, `worktree rename` and `worktree remove`,
and by `shared up` and `shared down` when a routed shared UI changed — or by
`berth index`, which also prints it as text. Beneath the checkouts it lists the
routed web UIs of the repositories' [shared services](#shared-services) in a
group of their own. An open tab fetches the page every 15 seconds and swaps the
table in when the rows have actually changed, advancing the
timestamp quietly otherwise, so it follows along without losing your place. It
holds an update back while you are reading inside a table with the keyboard,
says so in the header when it has stopped being able to read the page, and
pauses on request. Without scripting it falls back to reloading every 60
seconds.

`index` needs a `berth.toml` — the nearest one above the working directory,
or the one `--config <path>` names — even though what it prints spans the
machine. The same is true of `worktree list --all`, and of every verb that
touches a project; only `router` works without one.

`worktree remove` is the only destructive verb. It inventories everything
the worktree owns, verifies the full ownership tuple on every resource, and
refuses the whole operation if a single one fails — before it asks you to
type the hostname to confirm (`--yes` skips the prompt). Then, in an order
chosen so that an interrupted removal can simply be run again: Docker
resources, the home directory, the registration, the identity marker.
`berth worktree remove <slug|uuid>` removes another worktree, including one
whose checkout is already gone; `--label-namespace` supplies the Docker label
namespace of its resources when nothing on disk records it.

A SIGINT, SIGTERM or SIGHUP that arrives once the removal has started
destroying things leaves the worktree `pending`, and berth says so on stderr
before it dies by the signal: `Interrupted (<signal>) while removing <host>.
Removed so far (at least): <what>. It is still registered, now pending:
'berth worktree remove' finishes the removal, 'berth up' starts it again and
runs the seed hook again.` The counts are a lower bound (the resource being
removed when the signal came is not counted), and "nothing confirmed yet"
stands when none is known to be gone. The command names the target as you
typed it (`'berth worktree remove <slug|uuid> --domain <domain>'`) and leaves out
`up` when it is another worktree's removal that was interrupted. A signal
during the earlier `compose down` destroys nothing and prints no such notice,
and neither does one that also left a lock unreleased: that is reported as
an error, and [Lock recovery](#lock-recovery) applies. `doctor` deliberately
does not flag a `pending` worktree: a new worktree is `pending` until its
first `up` finishes, and so is one whose first `up` failed, and nothing in
the registration tells either from an interrupted removal, so a check could
only warn about the ordinary case. `worktree list` shows it.

`worktree remove` does not run the removal from wherever the invoked binary
happens to live: before doing anything else, it installs a content-addressed
copy of itself under the control home — outside any checkout — and hands the
removal to that copy, so deleting a checkout cannot delete the program still
removing it. That holds whenever the copy can be installed and run; when it
cannot, removal continues with the invoked binary, as described below. The
copy is exactly
the binary that was invoked, though, nothing more: a build made from a
branch still runs that branch's code once relocated, just from somewhere the
checkout it is removing cannot take down with it. Running only a trusted,
pinned build regardless of what invoked `worktree remove` is a separate,
later guarantee tied to how releases are distributed (#85, see
[`docs/design/release.md`](docs/design/release.md)). Copies are
content-addressed, so a rebuild that changes nothing installs nothing new;
one that has not been installed or reused in the last 30 days, or that is
not among the 5 most recently used besides the current one, whichever rule
catches it first, is swept away automatically — disk hygiene for a workflow
that rebuilds often, not something to manage by hand. That count is a
steady-state target, not a hard ceiling: the sweep only runs as part of a
later `worktree remove`, so a burst of removals from distinct builds in
quick succession can briefly leave more than 5 until the next one catches
up. A copy installed or reused in the last 10 minutes is never swept away by
either rule, however old or however far past the count it would otherwise
be: that window exists for another `worktree remove`, running at the same
time, that might be mid-way through installing or about to run the very
copy this one's own sweep would otherwise be free to take. This is invisible
in normal use; a copy that cannot be installed or run — including on a
control home whose filesystem cannot execute programs at all — is reported
on stderr as it happens, and removal continues with whichever binary was
invoked.

A full annotated example is in [`berth.example.toml`](berth.example.toml).
The hook contract is specified in [`docs/hooks.md`](docs/hooks.md).

### Renaming a worktree

`berth worktree rename <new-slug>` renames the worktree you run it from: it
changes the slug it is registered under, so it routes as `<new-slug>.<domain>`
from then on. The slug must be a lowercase hostname label (letters, digits and
inner hyphens, at most 63 characters), must not be `shared`, and must not be
held by another worktree in the same domain, whichever repository it belongs
to. It is also how to reclaim a bare name that an earlier collision suffixed.

Everything keyed by the worktree's IDs stays exactly as it is: the Compose
project name, and with it every container, network and volume name, the home
directory, the locks and the pinned loopback ports. berth renames and deletes
no volume. What carries the slug moves to the new one: the registration and
the worktree's index entry, and the checkout index is refreshed. Route files
are removed, not moved: a worktree with no containers has none that are live,
and `up` writes them at the new hostname. A Compose file that builds
`${BERTH_SLUG}` or `${BERTH_HOST}` into a volume name would name a new volume
on the next `up`; berth cannot see inside the file, so the old volume is left
in place, and `docker volume ls --filter label=<namespace>.worktree-uuid=<id>`
(the namespace defaults to `dev.berth`) lists every volume the worktree owns.
`worktree remove` removes the old one along with the rest.

A worktree with containers, running or stopped, is refused: they were created
with the old `BERTH_SLUG` and `BERTH_HOST`. Run `berth down` first, which keeps
the data, then rename, then `berth up` at the new hostname. The rename takes
the worktree's operation and development-session locks, so it cannot overlap
an `up`, a `down` or a `dev` session, and it cannot run from the worktree's
own hooks. If it is interrupted, the old slug stays registered until the
record is replaced, last; run the command again, with the same slug or a
different one. Hooks are not told of a rename; the next `up` runs them with
the new `BERTH_SLUG` and `BERTH_HOST`.

## Calling it something else

Name the binary or symlink whatever you like. Help text, the log prefix, and
every "run `X urls`" hint follow the name it was invoked as, which is read
from `argv[0]` and from nowhere else:

```bash
ln -s "$(go env GOPATH)/bin/berth" ~/.local/bin/duke
duke --help               # Usage:
                          #   duke [flags]
                          #   duke [command]
```

A binary built or invoked with no meaningful name to read — `main`, a bare
`.`, or an empty `argv[0]` — says `berth` instead.

What does **not** follow the name is anything written to disk: the config
and settings filenames (`berth.toml`, `settings.toml`), the berth root and
control-home directories, the
environment variables (`BERTH_HOME`, `BERTH_CONTROL_HOME`, …), the variables
handed to hooks (`BERTH_STAGE`, …), and the Docker label namespace. Those are
storage contracts. If they followed the binary name, renaming a symlink
would orphan state.

## Configuration

| Key | Meaning |
| --- | ------- |
| `project.name` | Lower-case slug. |
| `project.domain` | Worktrees are routed as `<slug>.<domain>`. |
| `project.label-namespace` | Optional. Only to adopt resources created by a predecessor tool. |
| `compose.file` | Compose file, relative to `berth.toml`. Paths inside it (build contexts, bind mounts, `env_file`) and its `.env` resolve against the Compose file's directory, as with `docker compose -f`; `${BERTH_ROOT}` is still `berth.toml`'s directory. |
| `[service.<name>]` | One table per service, named by its Compose service (lower-case letters, digits, hyphens; at most 64 tables, each name at most 63 characters, as for `[commands]`). Optional `subdomain` (empty = host root), optional `path` (route only that prefix, so services can share a host), optional `port` (defaults to the first port the service exposes), optional `index` presence. |
| `[shared]` | Optional. `file`: the shared services' Compose file, relative to `berth.toml`; required when any `[shared.service.<name>]` table exists. |
| `[shared.service.<name>]` | One table per shared service, named by its service in the shared file (same naming rules as `[service.<name>]`). Optional `port` (the web UI's container port), optional `ports` (more container ports), both pinned on `127.0.0.1` for the host; at most 16 in all. `used-by`: the worktree's own Compose services that use it. See [Shared services](#shared-services). |
| `[hooks]` | `prepare`, `seed`, `verify`, `backup`, `restore` — resolved as in [docs/hooks.md](docs/hooks.md): relative to the config file when the command starts with `.` or contains `/`, otherwise from `PATH`. `up` runs `prepare` and `seed`, `doctor` runs `verify`, `berth backup` and `berth restore` run `backup` and `restore`. |
| `[hooks.timeout]` | Optional. A time limit per hook, keyed by the hook's name (`prepare`, `seed`, `verify`, `backup`, `restore`): a duration such as `30s`, `10m` or `1h30m`, greater than zero and at most `24h`. A hook with no entry has no limit. When the limit passes berth stops the hook and its whole process group on the host (not work it handed to a container, such as a `docker compose exec`), fails the command with `The <hook> hook timed out after <limit> and was terminated.` and exits 124. See [docs/hooks.md](docs/hooks.md#timeouts). |
| `[commands]` | Optional. Named commands for `berth run <name>`, resolved like hooks. Each name (lower-case letters, digits, hyphens) maps to a command line, or to a table: `command`, `lock` (default `true`: hold the worktree's lock while it runs) and `kill-after` (default `10s`, at most `1h`: a duration such as `30s` or `5m`, how long its process group has to exit once stopped, before it is killed). See [docs/hooks.md](docs/hooks.md#commands). |

Unknown keys are rejected, so a typo cannot silently disable a feature.
Services are tables keyed by name, the way `[commands]` is, and they appear in
the order the file writes them, in `config show` and everywhere else services
are listed:

```toml invalid
# rejected: services are not an array
[[service]]

# the form: one table per service, named by the key
[service.api]
```

Every invocation passes the project name and the file list as flags and sets
`COMPOSE_PROGRESS`, so `COMPOSE_PROJECT_NAME`, `COMPOSE_FILE`,
`COMPOSE_PATH_SEPARATOR` and `COMPOSE_PROGRESS` in the Compose file's `.env`
have no effect. Other `COMPOSE_*` keys there still reach Compose — in
particular `COMPOSE_IGNORE_ORPHANS`, which defeats the `--remove-orphans` that
`up` and `down` rely on. Do not set it.

`BERTH_HOME` is the berth root — `~/.berth` by default — holding
`settings.toml`, the shared router's own state and, unless
`BERTH_CONTROL_HOME` overrides it, the control home, nested under it as
`control-home/`. `BERTH_CONTROL_HOME` moves only the control home, leaving
everything else where `BERTH_HOME` puts it. None of these are config-file
settings: the config file is committed and shared, and a path that is only
correct on one machine has no business in it — one developer pointing it at
a synced folder would put the whole team's state on iCloud Drive.

`settings.toml` is the machine's own file, never committed, for what is true
of this machine rather than of a project. Today that is the router's
loopback port and network mode and the range routed services' loopback ports
are allocated from:

```toml
[router]
port = 8080
mode = "host"           # optional: "host" or "bridge"; omit it to detect from docker info

[ports]
range = "20000-29999"   # the default; <low>-<high>, 1024 to 65535
```

`[ports] range` is where each routed service's pinned `127.0.0.1` port comes
from (see *Ports* above). The default, 20000-29999, sits below Linux's
ephemeral range (from 32768) and macOS's (from 49152), so the operating
system does not hand those ports out on its own. A range of your own should
stay clear of them too, and on Linux of anything in `ip_local_port_range`
and `ip_local_reserved_ports` (`sysctl net.ipv4.ip_local_port_range
net.ipv4.ip_local_reserved_ports`). Widen it when a range fills up; narrowing
it never moves a port already allocated, only where new ones come from. A
scratch berth root (`BERTH_HOME`) set up for testing should give itself a
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

Unknown keys are rejected, as in `berth.toml`; a missing file means the
defaults. The router's port is `BERTH_ROUTER_PORT` when that is set, else the file's,
else 80. `router status` and `doctor` say which one applied and name the
file; `doctor` warns when the running router serves a different port; and
an `up` that moves the router to a new port says that URLs already open — the
index page included — still use the old one.

The berth root also holds the shared router's own bookkeeping: `router/`,
with the dynamic-configuration and index directories the file provider
watches, and the router's own lock. The router *container*, though, is one
per Docker daemon regardless of berth root — its Compose project name never
changes — and its containers carry a label naming the berth root that
started them. `up` or `router up` from a different `BERTH_HOME` refuses while
that router is running, naming the root it belongs to, rather than recreating
it against its own bookkeeping; a router carrying no such label is refused
the same way. Taking a router over is an explicit step: `router down` stops
it from any root, and the next root's `up` starts its own. `doctor` reports
the owner. Every control home that shares one berth root shares that
bookkeeping too, rather than taking turns recreating it.

Because `worktree remove` deletes inside it, the control home must be empty
or one the tool created earlier — it leaves a `.berth-control-home` marker to
recognise its own, and refuses a directory whose marker holds anything but
its own format, or is empty beside existing content. The marker says which
format of control home it is (`format 2`). Your home directory, anything
above it, and the filesystem root are always refused, whether reached through
`BERTH_HOME` or `BERTH_CONTROL_HOME`.

**Stored-state format.** Everything berth writes and reads back says which
format wrote it: the control home's marker, the identity markers in the Git
directory, registrations and shared-service records, the router's `state.json`,
a lock's holder record, a worktree's index card, and a `dev.berth.format` label
on every container, network and image berth creates for a worktree or a shared
stack (named volumes carry none: Compose would offer to recreate a volume whose
label changed, losing its data). A build that finds a newer format than it
reads refuses, names what it found and what it reads, and says to upgrade
berth, for example `Refusing to use the router state … : it is format 2,
written by a newer berth, and this build reads format 1. Upgrade berth, then
run this again.`; it does not read, repair, rewrite or delete that state. A
container, network or image of a newer format, or without the label, is not
proven to be yours, so `worktree remove`, `shared down` and the network
connects and disconnects refuse to touch it, and the message says what to
remove by hand. State with no recognisable format is treated as not berth's.
`doctor` reports it for the control home, registrations, shared-service
records, the router state, lock records and a checkout's identity markers; it
does not look at Docker resources. [`docs/design/state.md`](docs/design/state.md)
has the full list.

### Editor support

`berth config schema` prints a [JSON Schema](https://json-schema.org)
(draft-07) of `berth.toml`, generated from the same definitions that validate
it. Every key has a description. Any editor that accepts a JSON Schema for
TOML can use it; [Even Better TOML](https://taplo.tamasfe.dev) (Taplo)
completes keys, shows their descriptions on hover and flags unknown ones. Point
it at the schema with a directive on the file's first line (`berth init`
writes it for you):

```toml
#:schema https://raw.githubusercontent.com/sofired/berth/main/schema/berth.schema.json
```

That copy, [`schema/berth.schema.json`](schema/berth.schema.json), tracks
`main`. To match the version you have installed, pin it with
`berth config schema > berth.schema.json` and point `#:schema` at that file
(`#:schema ./berth.schema.json`).

The schema states what a valid file looks like, not everything berth checks:
that two services do not claim the same route, for one, only `berth config
validate` can tell you.

### TOML syntax

The examples here and [`berth.example.toml`](berth.example.toml) stick to
TOML 1.0, with no multi-line inline tables or other 1.1 additions; keep yours
to it if other tools should read it. berth itself accepts more, but Taplo and
Python's `tomllib` do not.

## Shared services

A shared service is one instance per repository, defined in a Compose file of
its own and started by whichever worktree needs it first:

```toml
[shared]
file = "docker/compose.shared.yaml"   # relative to berth.toml; its own Compose project

[shared.service.mailpit]              # a service in that file; also the name containers reach it by
port    = 8025                        # optional: the web UI's container port, routed at mailpit.shared.<domain>
ports   = [1025]                      # optional: more container ports, for processes on the host
used-by = ["api", "worker"]           # the worktree's own Compose services that use it
```

```yaml
# docker/compose.shared.yaml
services:
  mailpit:
    image: axllent/mailpit:v1.31.3
    volumes: ["mailbox:/data"]
volumes:
  mailbox: {}
```

**What the file may say.** A shared name is a table key, like a service's:
lower-case letters, digits and hyphens, at most 63. `used-by` entries are
unique and may not name a shared service. `port` and `ports` are TCP ports,
1 to 65535, unique within the service, at most 16 in all. `[shared]` needs
`file` as soon as one shared service is declared, and the file may not be the
worktree's own Compose file. A `[[shared.service]]` array is refused the way
`[[service]]` is. `up` also checks the worktree's Compose file: every
`used-by` service must exist in it, a shared name must not (the name would
mean two things on the worktree's network), and a `used-by` service may not
have `network_mode: host`, `none`, `service:…` or `container:…`, because it
has no network a shared container could join.

A shared definition must not depend on the checkout that started it. `build:`
is refused, and so is a bind mount (or a config or secret `file:`) whose
source is inside any worktree of the repository: the project directory and
every relative path resolve to whichever worktree starts the service first,
which `worktree remove` can delete. Images, named volumes and absolute paths
outside every worktree are fine. Whatever the service `depends_on` in the
shared file starts with it, and is labelled and handled the same way. The
file is read with only variables that mean the same in every worktree:
your environment, `BERTH_PROJECT`, `BERTH_REPOSITORY_ID` and the Compose
project name — never `BERTH_ROOT`, `BERTH_WORKTREE_ID` and the rest of a
worktree's own, and not the `BERTH_*` a hook passes down.

**Reading the same from every checkout.** Two checkouts of a repository must
compute the same hash for the file, or `up` would warn that the definition
differs when nothing changed. A relative path resolves against each checkout's
own directory, so `../shared-data` names a different directory in each, and
`${PWD}` is the directory the command ran in; `PWD` and the rest of your shell's
environment are inherited. berth reads the file a second time from a scratch
directory and refuses it, naming the first value that changed, such as
`services.db.volumes[0].source resolves to "/work/shared-data" here, from a
relative path` (or, when it cannot tell, `a path or variable that depends on the
checkout`); `berth up` fails when a shared service it uses is refused, and so do
`berth shared up` and `doctor`. Use
an absolute path outside every worktree, or a named volume. This checks the
whole file, not only the services you use, and costs one extra `compose config`
inside the shared lock. The worktree roots it checks against are the config's
directory and the registered worktrees; a file kept outside every worktree is not
checked, and neither is a read made with no roots (`berth shared status`).

The check is best effort. If it cannot read the file the second time, which
happens when an `env_file`, `include` or `extends` climbs out of the file's
directory (`../../outside.env`), berth accepts the file and warns `Could not
check whether … reads the same from every checkout`, on every command that reads
it. Absolute paths there, or keeping the files it reads under the shared file's own
directory, silence the warning. A scratch directory
(`shared-probe-*`) is made under the control home for each read and removed
after it; a killed `berth` can leave one, which is harmless.

Variables safe in the shared file are the ones with the same value for every
checkout: `BERTH_PROJECT`, `BERTH_REPOSITORY_ID`, the Compose project name and
any variable you set identically everywhere. Do not use one that differs between
users, shells or machines (`${USER}`, `${HOME}`, a machine-specific setting):
berth reads the file in one environment and cannot see that, so the hash would
differ across them and `berth up` would warn.

**Starting it.** A worktree whose Compose file has a `used-by` service
*needs* the shared service. `up` starts it before it touches the worktree, and
a failure to start it fails the `up`. Two worktrees' first `up`s serialise on
a per-repository shared lock (up to 15 minutes): the second finds the
instance running and reuses it. A running instance is never touched by
`compose up`, so Compose cannot recreate it behind your back. A stopped one is
replaced by the caller's definition, keeping its volumes, anonymous ones
included: a stop followed by `up` keeps the data (Compose recreates the
container in place with `--force-recreate --no-deps`). A `shared down` removes
the container, and an anonymous volume (a `/data` mount with no name) belongs
to that container: the next start gets a fresh one. Use a named volume in the
shared file for data that must survive a down. Give a
service that should come back after a reboot `restart: unless-stopped` in the
shared file; berth does not add one, and `doctor` reports a stopped container.

**Connecting it.** After the worktree's own `compose up`, and before its
hooks (which may send mail), `up` connects the shared container, under the
shared name as an alias, to each of the worktree's own networks that a running
`used-by` container is on, proven by the worktree's ownership labels. It does
not connect it to anything else. `down` disconnects first and then stops the
worktree; `worktree remove` disconnects before it reaps anything. Compose
leaves a network that something is still attached to, so this order is what
lets a worktree's network be removed while the shared service keeps running.
The worktrees never see one another's services: `web` calling `api` resolves
inside its own worktree's network only.

**Replacing it.** Each shared container carries a hash of its definition (the
service and everything it `depends_on`, before berth's own override) and the
worktree that started it. A worktree whose file hashes differently gets a
warning from `up`, naming who started the running one, and nothing is
restarted: a branch that bumps an image tag must not break an unrelated
branch's `up`. `berth shared up` refuses a differing definition;
`berth shared up --recreate mailpit` replaces that named service (at least one
name is required; the refusal lists the running ones), whether or not its
definition differs (so it is also how to restart one), keeps its
volumes, and reconnects every worktree that uses it. Attachments belong to a
container, so a new container has none: whoever creates one reconnects the
other worktrees after releasing the shared lock, one worktree at a time, and a
worktree that is busy is skipped with a warning and repaired by its own next
`up` or by `berth shared up`. `shared up` ends with `N worktree(s) were skipped
and are not connected yet; run 'berth shared up' again when they are idle.` and
still exits 0; a worktree it could not read at all is an error. `berth shared up` always does this sweep, also
when nothing needed replacing, so it is the repair for a worktree left
unconnected, and it repairs before it reports a differing definition (which
still exits non-zero); `doctor` finds one.

The hash is computed from pinned inputs: reading the shared file always means
`--profile '*'` and an empty `--env-file`, so a `.env` beside it is ignored and
every profile counts, and it covers
the top-level volumes, networks, configs and secrets the service closure
references. It is `compose config`'s output, so a Compose upgrade can change it
and show `differs` once for a service that did not change; run `berth shared up
--recreate <name>` to restamp it.

**Reaching it.** From a container, `mailpit:1025` on the worktree's network.
From the host, each declared port is published on a pinned `127.0.0.1` port
from the same allocator as a routed service's (see *Ports* above), kept across
`shared down` and released only by `shared down --volumes`; hooks and `berth
run` get it as `BERTH_SHARED_ADDR_<NAME>_<PORT>=127.0.0.1:<host port>`, with
`<NAME>` mapped as in `BERTH_SERVICE_URL_<NAME>` and `<PORT>` the container
port (`BERTH_SHARED_ADDR_MAILPIT_1025`). Nothing is injected into Compose:
that would tie a worktree's Compose environment to shared state and recreate
its containers when the state changed. A host process uses the pinned port; a
container never does, because on a host firewalled by default a container
cannot reach the host's loopback.

**Its web UI.** A `port` is also routed, through the same proxy as your
services, at `<name>.shared.<domain>` (`mailpit.shared.acme.localhost`), under
the domain the service was first pinned with. `shared` is therefore reserved:
a branch that slugs to `shared` gets a suffixed slug. The route file is written when the service is up
and removed by `shared down`; `berth shared up` starts the router first when
it has a web UI to route, its own or a dependency's. `BERTH_SHARED_URL_<NAME>` gives hooks and `berth run`
the URL (router port included), `status` and `shared status` print it (and
`--json` carries it as `url`), and the checkout index lists it in a group of
its own beneath the checkouts. Two repositories on one domain cannot both
route a shared service of the same name: the second is refused when it would
become routed, naming the first; rename one of the services, or run `shared
down --volumes` where the first lives, which deletes its record. A container
stopped any other way than `shared down` (a reboot, `docker stop`) keeps its
route file and the hostname answers 502 until `shared up` or `up` starts it
again; `doctor` reports that case, and `shared down` clears the file. `doctor`
also probes the route like any other and warns when a running service's route
file is missing; `router up` and `up` repair a route file that points at a
stale port from the record. Dropping `port` from `berth.toml` stops routing a
running service at the next `up` or `shared up` without restarting it; a port
newly declared as the web UI that the container was not started with needs
`shared up --recreate <name>`.

```bash
berth shared up [name...]                    # start (or reuse) them, then reconnect every worktree
berth shared up --recreate mailpit           # replace a running one with this worktree's definition; keeps its data (a name is required)
berth shared up --reallocate-port mailpit:1025   # give a container port a new pinned port; refused while it runs
berth shared down [name...]                  # stop and remove the containers; named volumes and pinned ports are kept
berth shared down --volumes [name...]        # also delete the data and release the ports; asks first
berth shared status [--json]                 # state, web UI URL, addresses, definition, connection, who started it
berth worktree unlock --shared               # clear a dead holder's shared lock, once it is checked gone
```

`shared down` finds everything by its labels, from any checkout of the
repository and without a `berth.toml` (give `--label-namespace` when a project
sets its own), re-verifies each resource before it removes it, and never goes
through any worktree's Compose file. It does not ask whether worktrees are
using the service: it is the explicit stop, and says how many were left
without one. It is the only way a shared service's data is deleted; `--volumes`
asks for a typed confirmation (the phrase it prints, as `worktree remove` does)
that lists the volumes by name, and `--yes` skips it. Without names it removes
every volume of the shared stack, mounted or not. With names it removes only
the volumes a selected container mounts, so a stopped sibling's data is kept;
anonymous volumes (Compose's `/data` without a name) go too, but only those of
containers `shared down` has proven are the stack's, and the prompt marks them
`(anonymous)`. A plain `shared down` (without `--volumes`) leaves named volumes
alone, but warns when a container it removes has anonymous ones: it names them
and says their data will not be reattached on the next start. If the plan grows while you decide, the command refuses and
says to run it again. A name that is not a declared, running or recorded
shared service is refused, listing the ones that are. A repository that was
never brought up has nothing to stop: `shared down` says so and exits 0, and
`shared status` lists the declared services as `absent`.
Removing a repository's last registered worktree prints one line pointing at
`shared down` when the shared services are still running.

A shared service may not publish ports in the shared file: `ports:` there, and
any non-loopback publish, is refused with the instruction to declare the port
in `[shared.service.<name>]`, where berth publishes it on `127.0.0.1`. A shared service that runs natively on the host and
is used by containers is not supported: on Linux with a default-deny firewall
a container cannot reach the host.

## Host routes

A process running on the host (a dev server, an API you start from the IDE) can
be routed at the worktree's hostname through the same router as its services:

```bash
berth route add web --port 5173 --subdomain app   # http://app.<slug>.<domain> -> 127.0.0.1:5173
berth route add api --port 8080 --path /api       # http://<slug>.<domain>/api -> 127.0.0.1:8080
berth route ls [--json]                           # name, URL and port of each
berth route rm web
```

`route add` takes the routing fields of a `[service.<name>]` table: with no
`--subdomain` or `--path` it claims the worktree host itself, and it gets the
hostname a service with that name and those fields would get. It refuses a
name that is a service in `berth.toml`, a shared service's, or another host
route's, a route that answers where a service or another host route already
does, the router's own port, and a port berth pinned for a service (a host
route names a port a process on the host listens on), except that adding a route identical to a recorded one is accepted and rewrites its route file. Adding and removing take
the worktree's operation lock and need a registered worktree (`berth up`
first).

A host route is recorded in the worktree's registration (`hostRoutes`), and its
route file, `host.<repository>.<worktree>.<name>.yaml` under the router's
`dynamic/` directory, is derived from the record. `berth up` and `berth router
up` repair a route file that disagrees with its record, the same way they do a
service's; `berth doctor` only reports it. A recorded route whose file is
missing is reported by `doctor` and not recreated by the repair: run `berth
route add` again with the same name, port, subdomain and path (`berth route ls`
shows the URL) and it writes the file and changes nothing else. `up` and `down`
leave host routes alone, so a route survives `down` (the process on the host is
not berth's to stop) and the hostname answers `502 Bad Gateway` until something
listens on the port. `worktree remove` removes them before the registration.
`berth urls` (`urls --json` marks them `"hostRoute": true`), the checkout index,
and `BERTH_SERVICE_URL_<NAME>` and `BERTH_SERVICE_PORT_<NAME>` in hooks and
`berth run` include them; the index lists them from the registration and does
not know whether the process is up. `doctor` lists each one with whether its
port answers (a stopped dev server is `ok`, with the 502 note; only a port that
answers on `::1` alone is a warning), and probes each through the router.

**Bind `127.0.0.1`.** The proxy reaches a host process on `127.0.0.1`. A server
that listens only on `::1` (some tools resolve `localhost` to IPv6 first; Vite's
default host can be `::1` on a Mac) answers through the router on a native Linux
Engine, but not on Docker Desktop or OrbStack, where the host is reached over
IPv4 alone; `route add` and `doctor` warn when a port answers only on `::1`.
Bind the server to `127.0.0.1` explicitly (Vite: `server.host: '127.0.0.1'`, or
`--host 127.0.0.1`). A server bound to `0.0.0.0` also works, but is reachable
from the network and may trigger the macOS firewall prompt; prefer `127.0.0.1`.
Forcing `[router] mode = "bridge"` on a Linux engine does not work for host
routes: the bridge container cannot reach a `127.0.0.1` listener there.

**Allowed hosts.** The router passes the worktree hostname through as the
request's `Host` header, and many dev servers refuse hosts they do not know.
Allow it in the dev server's own settings. Vite answers a blocked host with a
403 "Blocked request" page: set `server.allowedHosts` (for example
`['.acme.test']`, or the hostname in the URL `berth route ls` prints; Vite
already allows `localhost` and every `*.localhost` host, so a `project.domain`
under `.localhost` needs nothing). webpack-dev-server answers "Invalid Host
header" (`allowedHosts`), and Django a 400 (`ALLOWED_HOSTS`). Next.js's
`allowedDevOrigins` is a different thing: it lets another origin load the dev
server's resources, and matters when pages are opened from a berth hostname
while the server is addressed as `localhost`.

A host route needs the router to be running, as a service route does:
`berth up` or `berth router up` starts it. Host processes that must be reached
by containers are out of scope: on Linux with a default-deny firewall a
container cannot reach the host.

## Trust model

Bringing up a worktree runs the branch's code. `up` builds whatever its
`compose.yaml` builds and runs the hooks named in its `berth.toml`, as you,
with your full environment — the same trust a `make` or an `npm install` in
that checkout asks for. Do not bring up a branch you would not run. `berth
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
  [Configuration](#configuration). `COMPOSE_PROGRESS` is always set to
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
managed, repository and worktree labels, in the format the `dev.berth.format`
label names) does not match. Named volumes carry no format label, so a volume
is proven by its other ownership labels. That is what keeps `worktree remove` from deleting something it did
not create — not a claim about what your branch's code can do once it runs.

## Teardown pin

`worktree remove` is meant to run a pinned, trusted build of berth, not the
code of whichever build you invoked, so that code from a branch does not run
while it takes a worktree apart ([`docs/design/teardown.md`](docs/design/teardown.md),
#85). **Only the pin exists so far.** `worktree remove` does not run the pinned
binary yet and does not refuse without one (#146); nothing here changes what it
does. The two commands below install a pin and check it.

The pin lives under the control home, in `teardown/`: `pin.json` and the pinned
binaries, each named by its sha256. The control home is `control-home/` under
the berth root (`~/.berth` by default) unless `BERTH_CONTROL_HOME` moves it, and
`berth doctor` prints it. There is one pin per control home, shared by every
berth root that uses it. Nothing pins, updates or unpins on its own, and
nothing downloads.

### `teardown pin --file`

Pin a berth binary you have verified yourself, such as one you built. For a
download, first verify the archive by the steps under
[Verifying a download](#verifying-a-download), then extract `berth` from it and
take that binary's own digest:

```bash
sha256sum berth     # macOS: shasum -a 256 berth
berth teardown pin --file ./berth --sha256 <that digest> --release my-build
```

The digest you give approves exactly the bytes you hold: berth hashes the file,
requires the same digest, and stores those bytes. It also checks that the file is
an executable for this machine (an ELF file on Linux, a Mach-O file on macOS, for
this architecture) and refuses an empty file or a binary for another platform;
beyond that, the digest is the approval. It is a meaningful check only
when the digest comes from a second trusted source, such as your own build; a
digest taken from the file you are about to pin proves nothing more than that
you meant to pin it. `--release` is a label for the pin: letters, digits, `.`,
`_`, `+` and `-`, starting with a letter or digit, at most 64 characters. The
command shows what it will pin and asks you to type `pin teardown binary`;
`--yes` skips the phrase, and without a terminal it refuses to ask. A file pin
records only this machine's platform, so another platform pins its own binary.
Pinning again replaces the pin, a downgrade included. There is no unpin: delete
`<control home>/teardown/`.

No signature is checked and no program is run. Pinning a downloaded release,
verified with cosign, is not built yet.

### `teardown status`

Reports whether the pin is intact on this machine, and nothing about removal:
whether it can be read, and whether the stored binary for this platform exists
and has the sha256 the pin records. It prints the pin on stdout and exits 0 when
all is well. When there is no pin, the pin is of a newer format or is not one
berth wrote, or the binary is missing or different, it says so on stderr and
exits 1; the first two print nothing on stdout, and the others print the report
first. With `--json` it prints one document instead:
`pinned`, `ok`, `release`, `source`, `signer` (absent for a file pin),
`pinnedAt`, `platform`, `binary` (`path` and `sha256`, absent when the state is
`none`, and `state`: `ok`, `missing`, `mismatch` or `none` for a platform the
pin has no binary for) and `running` (`version`, `isPin`: this build is the
pinned binary, and `newerThanPin`). `mismatch` also covers a stored binary that
cannot be read or is not a regular file; the text form says which. With no pin
it prints `{"pinned": false}` and exits 1. When the running build is newer than
the pinned release, the text form adds a line on stderr saying to pin again.

### What it does not protect against

The pin is meant to keep unreviewed branch code from running as berth during a
removal. It does not protect against a hostile process running as you with write
access to the control home, which can rewrite the pin as easily as anything else
there, and a file pin is exactly as trustworthy as the digest you checked.

## Output and exit codes

The commands that produce data — `index`, `status`, `urls`, `router status`,
`shared status`, `route ls`, `config show`, `config validate`, `worktree list`, `doctor`, `teardown status` — write that data to stdout
and nothing else, so `berth index > page.txt` and
`berth worktree list --json | jq` get what they asked for. Progress, waiting
notices, confirmation prompts, warnings and failures go to stderr. Eight of
these (`index`, `status`, `urls`, `shared status`, `route ls`, `worktree list`, `doctor` and `teardown status`) take `--json`:
see [Machine-readable output](#machine-readable-output).

`worktree rename` writes only its notice, to stderr, and nothing to stdout.

`teardown pin` writes only its notices to stderr, and nothing to stdout.
`teardown status` writes its report to stdout, and takes `--json` (see
[Teardown pin](#teardown-pin)); a problem, no pin included, is an error on
stderr that exits 1.

`up`, `dev`, `down`, `backup`, `restore`, `router down` and `worktree remove`
are the exception: they pass your hooks' and Compose's own stdout through as
those programs wrote it, and `up` adds its URL table there. `dev` is `up` and
then the followed logs, which keep their streams: a container's stdout on
berth's stdout, its stderr on berth's stderr, and one notice on stderr when
following starts. `backup` and `restore` write their own closing line, and any
warning, to stderr, so a script that needs a bundle's path passes it a
directory. `shared up` passes Compose's stdout through the same way; `shared
down` writes only notices and warnings, to stderr. `run` is the same, more
strictly: the command it runs writes straight to berth's stdout and stderr, and
berth adds nothing while it runs, so `berth run plan > out` holds only what
`plan` wrote.

`router status` has a fourth vocabulary, about the shared router rather than
any checkout: `running on <address>, <mode> mode`, `not running (would use
<address>)`, `degraded on <address>, <mode> mode` when a container is down or
unhealthy, and nothing at all on stdout when Docker could not be asked —
that case is a `Router state unknown:` line on stderr. Each reported problem is
its own `Router problem:` line on stderr, followed by the container's last
log lines for one that is not running or not healthy. A running router whose
container carries no mode label reads `running on <address>, mode unknown`.
Both `running` and `not running` exit 0, because "is it there?" is the question and both are answers; `degraded` and
an unreachable Docker exit 1. Whenever the status line is printed, a second
stdout line follows it saying which port was resolved and from where —
`port <n> from <settings file>`, `port <n> from BERTH_ROUTER_PORT (overriding
<settings file>)` or `port <n> by default (<settings file> sets none)`; it too
is absent when Docker could not be asked.

`doctor` writes its report to stdout: one section per area (`Prerequisites`,
`Registry`, `Locks`, `Router`, `This checkout`, and `Shared services` when
`berth.toml` declares some or Docker still holds some of the repository's),
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
also reports one line per configured hook — `prepare`, `seed`, `verify`,
`backup` and `restore` alike, whether or not this worktree is registered —
for whether the file it names would even start: as `<stage> hook file`,
distinct from `verify hook`'s own run line so the two are never mistaken for
one repeated check, and one `<name> command file` line for each `[commands]`
entry after them. A hook file that would not start names the exit code a
real run would give in its own line, 127 or 126
([docs/hooks.md](docs/hooks.md)), but that is a prediction, not an
observation — it never supplies `doctor`'s own exit code, which stays the
verify hook's own (only when verify itself actually ran and failed) or 1.
It takes no lock, writes nothing, and repairs nothing; it reports. When the
router itself could be resolved, its
`Router` section also names the router's owner (a `fail` for a router
without berth's owner label or one another berth root started), the mode it
runs in against the mode this build would use (and fails for a daemon berth
refuses), that nothing but `127.0.0.1` answers on its port, and — while it
runs — probes every route through it, with retries, telling the proxy's own
404 (the route is not loaded) from a 502, 503 or 504 (the route is loaded and
its target is down or unreachable). It ends with a `router port` line saying
the same as `router status`'s second line, a `warn` when the running router
serves a different port than the one resolved; when port or settings
resolution fails instead, the section holds that one router failure line
alone.

`init` writes nothing to stdout: that it wrote the file, and a warning when no
Compose file was found, are notices on stderr. It exits 1, writing nothing,
outside a Git checkout or when `berth.toml` is already at the root, and 127
when Git is not on `PATH`, as for any command that cannot start. A kill in the
middle of the write can leave a truncated `berth.toml`; `init` then refuses, and
you delete the file and run it again.

`worktree unlock` writes nothing to stdout: what it verified and what it
cleared are notices on stderr. A refusal is an error that exits 1 and clears
nothing: a holder that may be alive, a process it left running, or a lock that
changed while it looked.

Every failure the tool itself reports, a bad command line included, ends in a
non-zero exit code and a `[berth] ERROR:` line on stderr saying why. Set
`BERTH_DEBUG=1` to see the stack trace behind a failure. A failing hook's own
exit code becomes berth's, and so does a `run` command's, a `backup` or `restore`
hook's — see [docs/hooks.md](docs/hooks.md) — except that a hook which outlives
its `[hooks.timeout]` limit is stopped and berth exits 124, not the hook's own
code ([docs/hooks.md](docs/hooks.md#timeouts)). A `dev` session that you end
ends with the status of the signal that ended it: 130 for Ctrl-C, 143 for
SIGTERM, 129 for SIGHUP.

### Machine-readable output

`worktree list`, `doctor`, `status`, `shared status`, `route ls`, `urls`, `index` and `teardown status` take `--json`. With
it, stdout carries exactly one JSON document, indented and ASCII-only (every
non-ASCII character is escaped `\uXXXX`), and the exit code is the text
form's. Notices, warnings, the `doctor` summary and errors stay on stderr in
both forms. A command that fails before it has anything to report (not in a
repository, `urls` in an unregistered worktree, Docker failing under `status`,
`index` giving up after waiting up to 60 seconds for the router lock) prints
its usual `[berth] ERROR:` on stderr, nothing on stdout, and exits 1; there is
no JSON error document. For a check that must not wait on a lock, use `status`
or `doctor`, which take none.

Rules that hold for every document:

- Keys are camelCase. Enum values (`status`, `state`) are lowercase.
- An optional key is omitted when it has no value, never `null`; a list is
  `[]` when empty.
- The name of a service is always under the key `service`.
- Free text (`subject`, `detail`, `label`, `purpose`, `branch`, paths) is for
  people. Scripts key on the ids and enum values described below.
- Until 1.0 any shape may change. After 1.0 changes are additive only: a new
  key, or a new member of an enum. Consumers must ignore keys they do not know
  and treat an unknown enum value as unknown. Documents carry no version field.

**`teardown status --json`** is described under [Teardown pin](#teardown-pin). When
it prints nothing at all and exits 1, the pin could not be read (it is of a
newer format, or not one berth wrote): stdout is empty and stderr says why.

**`worktree list --json`** prints one array, `[]` when there is nothing,
sorted by domain and then slug. Each row has `repositoryId`, `worktreeId`,
`slug`, `domain`, `host`, `state` (`ready`, `pending` or `missing`) and
`current`, plus `root`, `branch` and `labelNamespace` when they are known.

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
| `registry.berth-root` | `registry` | | the berth root in use |
| `registry.control-home` | `registry` | | the control home is usable |
| `registry.router-state` | `registry` | | the router's state directory |
| `registry.registrations` | `registry` | | how many registrations read cleanly |
| `registry.unreadable-record` | `registry` | `<repository>/<file>` | a registry record that could not be read |
| `registry.host-conflict` | `registry` | the host | a hostname claimed by more than one worktree |
| `registry.index-host` | `registry` | worktree ID | a worktree registered on the index's own hostname |
| `registry.port-conflict` | `registry` | the port | a loopback port recorded for more than one service, in one worktree or several |
| `locks.none` | `locks` | | no lock is held |
| `locks.scan` | `locks` | | the locks could not be read |
| `locks.registry` | `locks` | | the registry lock is held or was left unreleased, or could not be checked |
| `locks.router` | `locks` | | the router lock is held or was left unreleased, or could not be checked |
| `locks.shared` | `locks` | repository ID | a repository's shared-services lock is held or was left unreleased, or could not be checked |
| `locks.worktree.operation` | `locks` | worktree ID | a worktree's operation lock |
| `locks.worktree.development` | `locks` | worktree ID | a worktree's development-session lock |
| `router.container` | `router` | the problem's code, when the router is degraded or unreachable (see below) | the router's own status: `ok` when up and healthy or not running, `skip` when Docker or Compose is unavailable, `fail` when it could not be resolved, or one `fail` per problem, as `router status` prints them |
| `router.owner` | `router` | | which berth root started the router |
| `router.mode` | `router` | | the network mode the proxy runs in (`host` or `bridge`, read from the running container), against the mode this build would start it in; says `mode set by settings.toml` when `[router] mode` is in effect and `warn`s when detection would pick another; `fail` when Docker is one berth refuses (remote, rootless, unrecognized), `warn` when the running mode differs or the running router's layout is not this version's |
| `router.ports` | `router` | | the ports the router needs (its own, the index's and, in host mode, the ping's) are free while the router is not running (`fail`) or is running but unhealthy (`warn`), naming what holds one; not checked while it is healthy |
| `router.bind` | `router` | | only while the router runs: that its port answers on `127.0.0.1` alone, by asking this machine's other addresses for it (`fail` naming any that answer) |
| `router.routes` | `router` | | the route files, none claiming a shared rule |
| `router.route-conflict` | `router` | `<host>` or `<host><path>` | a route two files claim |
| `router.orphaned-routes` | `router` | none, `unmatched` or `other-home` | route files that disagree with their registration, or a shared service's route file with its record: no port recorded for the service (or no host route of that name), or a different port than the one recorded (or, for a host route, a different port or hostname) (the next `up` or `router up` repairs them); with instance `unmatched` (a warning), files whose repository this control home knows but whose worktree it holds no record of, and with instance `other-home` (ok), files of a repository it does not know; both are counted and never removed, since they may belong to another control home (delete them from the router's `dynamic/` directory if their worktrees are gone). A shared service's route file is judged against its shared-service record in the same way, and named as such in these lines |
| `router.route-probe` | `router` | `<host>` or `<host><path>` | only while the router runs, one per route including the index, each host route and the shared services' web UIs: an HTTP request through the router. A 404 is judged by sending the same request straight to the route file's target on `127.0.0.1`: the target giving the same 404 is the service's own (`ok`); a 404 it does not give means the route is not loaded (`fail`), retried for several seconds because Traefik's file provider takes a moment to load a change. A 502, 503 or 504, or a target that does not answer, is `warn` (the target is down or unreachable); a router that does not answer is `fail`; these are retried at most twice; a host route whose process is stopped is `ok` ("nothing is listening"), as in `checkout.host-routes` |
| `router.port` | `router` | | the resolved router port and where it came from |
| `checkout.config` | `checkout` | | this checkout's `berth.toml` |
| `checkout.worktree` | `checkout` | | whether this worktree is registered |
| `checkout.missing` | `checkout` | worktree ID | a registration whose checkout Git no longer lists |
| `checkout.missing-scan` | `checkout` | | the scan for missing checkouts failed |
| `checkout.ports` | `checkout` | the routed service's name | whether its allocated loopback port is free or held by this worktree's own container (`fail` when anything else holds it, `skip` before one is allocated) |
| `checkout.published-port` | `checkout` | the routed service's name | a routed service whose own Compose file also publishes a fixed host port |
| `checkout.published-port-scan` | `checkout` | | reading the Compose file's published ports failed |
| `checkout.host-routes` | `checkout` | the host route's name | whether the process behind a [host route](#host-routes) answers on `127.0.0.1` (`ok`, also when nothing listens, with the 502 note; `warn` when only `::1` answers and the router may not reach it: bind `127.0.0.1`) |
| `checkout.host-route-files` | `checkout` | the host route's name | `warn` when a recorded host route has no route file; `berth route add` with the same fields (see `berth route ls`) writes it again |
| `checkout.hooks.resolve` | `checkout` | the stage: `prepare`, `seed`, `verify`, `backup` or `restore` (the set may gain values) | whether that configured hook's file would start |
| `checkout.commands.resolve` | `checkout` | the command's name from `[commands]` | whether that command's file would start |
| `checkout.hooks.verify` | `checkout` | | the `verify` hook's own run |
| `shared.scan` | `shared` | | the Shared services section could not be built |
| `shared.services` | `shared` | | the shared services could not be listed, the repository has never been brought up, or Docker is not answering (`skip`, `fail`), or there is nothing to report |
| `shared.definition` | `shared` | | the shared Compose file cannot be used (`fail`): it does not parse, builds an image, publishes `ports:`, or bind-mounts a path inside a worktree |
| `shared.service` | `shared` | the shared service's name | its state: `ok` running, `warn` stopped, unhealthy or running with a different definition than this worktree's, `skip` absent |
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
To require that the router was actually checked and healthy, use
`jq -e 'any(.sections[].checks[]; .id=="router.container" and .status=="ok")'`.
For a per-item kind, filter by id and read `instance`:
`jq -r '.sections[].checks[] | select(.id=="locks.worktree.operation") | .instance'`.

```json
{
  "ok": false,
  "exitCode": 1,
  "counts": { "ok": 6, "warn": 0, "fail": 2, "skip": 2 },
  "sections": [
    { "id": "prerequisites", "title": "Prerequisites", "checks": [
      { "id": "prerequisites.git", "status": "ok", "subject": "Git", "detail": "2.45.0" },
      { "id": "prerequisites.docker.engine", "status": "fail", "subject": "Docker Engine", "detail": "…" },
      { "id": "prerequisites.docker.compose", "status": "fail", "subject": "Docker Compose", "detail": "…" } ] },
    { "id": "registry", "title": "Registry", "checks": [
      { "id": "registry.berth-root", "status": "ok", "subject": "berth root", "detail": "/home/you/.berth" },
      { "id": "registry.control-home", "status": "ok", "subject": "control home", "detail": "…" },
      { "id": "registry.router-state", "status": "ok", "subject": "router state", "detail": "…" },
      { "id": "registry.registrations", "status": "ok", "subject": "registrations", "detail": "1 readable" } ] },
    { "id": "locks", "title": "Locks", "checks": [
      { "id": "locks.none", "status": "ok", "subject": "locks", "detail": "none held" } ] },
    { "id": "router", "title": "Router", "checks": [
      { "id": "router.container", "status": "skip", "subject": "router", "detail": "needs Docker" } ] },
    { "id": "checkout", "title": "This checkout", "checks": [
      { "id": "checkout.config", "status": "skip", "subject": "berth.toml", "detail": "…" } ] }
  ]
}
```

(Shown compacted, and with `…` standing in for text you would see in full;
the real output is indented, one key per line.)

**`route ls --json`** prints one array of `{"name", "url", "port"}`, by route
name, with the route's `subdomain` and `path` when it has them (the fields `route add`
takes again to write a missing route file), `[]` when there are no host routes (a notice stays on stderr). `port` is
the port the process on the host listens on. An unregistered worktree is an
error: exit 1 and no document.

**`status --json`** prints the worktree's `host` and one entry per declared
service. `state` is `running`, `starting`, `stopped`, `absent` or `unknown`.
`detail` appears when there is something to add: for `unknown` it is Docker's
own word for the state, and for a `running` service failing its health check
it is `unhealthy`. `port` is a number (for example `20417`): the routed
service's pinned port on `127.0.0.1`, absent for a service that is not routed
or has none allocated yet. A worktree that was never registered prints
`{"registered": false, "services": []}` (the notice is still on stderr) and
exits 0, as the text form does.

```json
{
  "registered": true,
  "host": "feature-login.acme.localhost",
  "services": [
    { "service": "api", "state": "running", "port": 20417 },
    { "service": "db", "state": "running", "detail": "unhealthy" }
  ]
}
```

When `berth.toml` declares shared services, `status --json` also has a
`shared` array, one entry per declared service, in file order (the key is
absent otherwise). `state` uses the words of `services`: `running`, `starting` (also a restarting
container or a pending health check), `stopped`, `absent` or `unknown`, with
`detail` `unhealthy` for a running one failing its check. One inconsistent
container is an `unknown` entry with a `detail`, not a failure of the whole
command; a record that cannot be read leaves `addrs` out with a warning and
makes the definition `unknown`. If Docker cannot be asked about the shared
services at all, `status` warns on stderr, leaves the `shared` array out (and
the `shared services` block in the text form) and still reports the worktree's
own services, exiting 0.
`addrs` lists the declared container ports that have a pinned loopback port,
`{"port": <container port>, "addr": "127.0.0.1:<host port>"}`, running or not.
`definition` is `current` when the running instance was started from the
definition this worktree's shared file hashes to, `differs` when from another,
and `unknown` when nothing is running or the file cannot be read.
`connected` is true when the shared container is attached to every network of
this worktree's own that one of its running `used-by` services is on, and at
least one; it is false for a worktree with no running consumer, which the text
form calls `not used here` (`not connected` is reserved for a running consumer
whose network the container is missing). `url` is the routed web UI
(`http://<name>.shared.<domain>`, router port included) and is absent for a
service with no routed `port`. `definedBy` is the hostname of the worktree that started the
running instance, or `a removed worktree`, and is absent when nothing runs.

```json
{
  "registered": true,
  "host": "feature-login.acme.localhost",
  "services": [
    { "service": "api", "state": "running", "port": 20417 }
  ],
  "shared": [
    {
      "name": "mailpit",
      "state": "running",
      "url": "http://mailpit.shared.acme.localhost",
      "addrs": [
        { "port": 8025, "addr": "127.0.0.1:20450" },
        { "port": 1025, "addr": "127.0.0.1:20451" }
      ],
      "definition": "current",
      "connected": true,
      "definedBy": "feature-login.acme.localhost"
    }
  ]
}
```

**`shared status --json`** prints the same entries as an object,
`{"shared": [ … ]}`, and `{"shared": []}` when nothing is declared. Unlike
`status` it works from a worktree that is not yet registered, as long as the
repository has been brought up once.

**`urls --json`** prints one array, in config order, then the worktree's host
routes (a host route has `"hostRoute": true`, a `port`, the one its process
listens on, and no `label` or `purpose`; the key is absent for a service), `[]` when nothing is routed (the warning stays on stderr). `label` and `purpose` are absent for a
service without `[service.<name>.index]`, and both present, possibly `""`, for one
with it. `port` is a number (for example `20417`): the service's pinned port
on `127.0.0.1`, absent until `up` has allocated one; `url` is still the
hostname URL through the router. Unlike `status`, an unregistered worktree is
an error: exit 1 and no
document.

```json
[
  {
    "service": "api",
    "url": "http://feature-login.acme.localhost",
    "label": "API",
    "purpose": "REST surface",
    "port": 20417
  }
]
```

**`index --json`** prints what the index page is built from, in the page's
order. Like the text form it refreshes the page, which takes the shared
router's lock and waits up to a minute for it; use `status` or `doctor` for a
check that takes no lock. Only services that declare `[service.<name>.index]` are
listed, unlike `urls`, and each has a `label`; `purpose` is absent when there
is none.

- `branch` is `""` for a detached HEAD and absent when nothing knows it;
  `root` is absent when unknown. `project` is the project name `up` last
  recorded, and the domain when nothing was recorded.
- `available` on a service is set only when it was checked, which needs Docker
  to have answered and the checkout to exist. Absent means unknown; there is no
  `false` for "could not check".
- `shared` lists the routed web UIs of the repositories' shared services
  (`repositoryId`, `service`, `project`, `domain`, `host`, `url`, and `available`
  under the same rule as a service's); it is absent when there are none.
- When Docker did not answer, `dockerReachable` is false and `dockerError` says
  why.
- `identityError` (Git could not say which worktrees exist) appears only when
  that happened, and `cardReadFailures` counts index cards that could not be
  read.
- `entries[].status` is `running`, `partial`, `stopped`, `missing` or `unknown`.

```json
{
  "url": "http://berth.localhost",
  "generatedAt": "2026-09-28T10:00:00.000Z",
  "dockerReachable": true,
  "cardReadFailures": 0,
  "entries": [
    {
      "repositoryId": "…",
      "worktreeId": "…",
      "project": "acme",
      "domain": "acme.localhost",
      "slug": "feature-login",
      "host": "feature-login.acme.localhost",
      "branch": "feature/login",
      "root": "/src/acme.feature-login",
      "status": "running",
      "services": [
        {
          "service": "api",
          "label": "API",
          "purpose": "REST surface",
          "url": "http://feature-login.acme.localhost",
          "available": true
        }
      ]
    }
  ],
  "shared": [
    {
      "repositoryId": "…",
      "service": "mailpit",
      "project": "acme",
      "domain": "acme.localhost",
      "host": "mailpit.shared.acme.localhost",
      "url": "http://mailpit.shared.acme.localhost",
      "available": true
    }
  ]
}
```

## Platform notes

**Bind mounts on Linux.** Docker Engine enforces host file permissions inside
containers. A worktree's home directory (`${BERTH_WORKTREE_HOME}`) is 0700, so
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
as; see "Calling it something else") to force Traefik to reread every
route file from a clean start, rather than waiting on the next filesystem
event that touches it.

**Host routes on Docker Desktop and OrbStack.** The proxy runs on a bridge
network there and reaches your host process through the engine's host
gateway, which is IPv4 only: bind the process to `127.0.0.1`, not `::1`. See
[Host routes](#host-routes).

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

**Hostname resolution.** Chrome and Firefox map `*.localhost` to loopback
internally on every platform. Other clients — Safari, `curl`, language HTTP
libraries — use the system resolver, and whether it maps `*.localhost` varies:
current macOS and systemd-resolved (Ubuntu, Fedora) do; plain glibc or musl
resolvers (Debian, Alpine, distributions without systemd-resolved) do not.
Check with `curl -sSI http://berth.localhost` (add the port if you changed
it): any `HTTP/1.1 …` status line means the resolver is fine (the router
answered), `Could not resolve host` means it is not, and a connection error
means resolution worked and the router simply isn't running. Where it fails,
`/etc/hosts` has no wildcards, so add every hostname `berth urls` prints for
the worktree — `<slug>.<domain>` and any `<subdomain>.<slug>.<domain>` — plus
`berth.localhost`; or use `curl --resolve` / a `Host:` header.

### Lock recovery

`worktree unlock [slug|uuid]` clears a worktree's locks (its operation lock,
and its development-session lock if that is held too) only when all of this
holds — checked automatically, without ever waiting to acquire the lock
itself:

- the lock itself is free: `flock(2)` proves this instantly, so a lock
  actually held by a live process is always refused straight away, before
  anything else is checked;
- the record its last holder left names a process this machine can
  identify: by its machine ID, or by its boot ID in a container image that
  has none, in this boot and this PID namespace;
- that holder's PID no longer exists, or now belongs to a later process, so
  a reused PID is never mistaken for the same one — checked twice, a second
  look about a second later, in case a holder that looked alive was simply
  finishing;
- no command started for the worktree is still running on this machine:
  nothing with its Compose project on the command line, or its
  `BERTH_WORKTREE_ID` in the environment — checked the same way, twice.

A stopped holder (`Ctrl-Z`) is alive, and is refused: resume it with
`kill -CONT <pid>` and let it finish, or end it. A holder it cannot check —
on another machine or in a container, one that left no record, or one it
cannot look past because other users' processes are hidden from you (`/proc`
mounted with `hidepid`, say) — is refused too, unless you pass
`--holder-unverifiable` once you have made sure yourself; that never
overrides a running holder or a command left running. `--registry` and
`--router` point the same check at the machine-wide registry or router lock
instead of a worktree's, and take no worktree identifier; `--shared` points it
at the shared-services lock of the repository the worktree belongs to, and
looks for a `compose up` the dead holder left running by the shared Compose
project's name. The unlock command
that a lock notice or `doctor` prints names the worktree by its UUID: it
never changes, no other worktree shares it, and it works even for a
worktree whose first `up` died before it registered.

It reads `/proc` on Linux and asks `ps` on macOS. Your own processes are
checked by their command line and their environment; other users' only by
their command line, since their environments are private to them. So it cannot
see a command that left this machine's process table, or one that no longer
carries the worktree's markers where it can look: a command a hook ran inside
a container with `docker exec`, or a daemon handed to a service manager. A
helper a hook ran as another user — through `sudo`, which clears the
environment, or `sudo -E` — is missed too, unless its command line names the
worktree's Compose project. The worktree's own containers are not commands: they are what
`up` built, running or stopped as the holder left them, and the next `up` or
`down` reconciles them. What it did check, it prints, as a `Verified:` line
per lock cleared — or `Not verified, on your word (--holder-unverifiable):`
for whichever it accepted unchecked.

## Development

Go, at least the version [`go.mod`](go.mod) names.

```bash
gofmt -l cmd internal                                             # must print nothing
go vet ./...
go vet -tags integration ./...
go run honnef.co/go/tools/cmd/staticcheck@2025.1.1 ./...
go run honnef.co/go/tools/cmd/staticcheck@2025.1.1 -tags integration ./...
go test -race ./...                                               # unit
go test -race -p 1 -timeout 20m -count=1 -tags integration ./...  # real git, subprocesses, signals, Docker
CGO_ENABLED=0 go test ./... && CGO_ENABLED=0 go test -p 1 -timeout 20m -count=1 -tags integration ./...
```

`-p 1`, `-timeout 20m` and `-count=1` are explained in [`CONTRIBUTING.md`](CONTRIBUTING.md).

`-tags integration` marks tests that touch something real — a process, a
signal, or a Docker daemon — and are excluded from the default `go test
./...`. Without a daemon the Docker suites skip; CI's `docker` job sets
`BERTH_TEST_REQUIRE_DOCKER=1`, which turns that skip into a failure, so a green
run on a machine without Docker is not the run CI makes. `BERTH_TEST_SKIP_DOCKER=1`
skips them even when a daemon is up. The Docker suites
sweep up whatever a killed run left behind, without regard to age, so run one
of them at a time on a machine.
The `CGO_ENABLED=0` run matters on its own: cgo changes how the Go runtime
behaves (an extra thread, notably), and releases will build with it
disabled. CI also runs the streams contract — every command's
stdout/stderr split — as part of the `-race` integration run above,
against a binary it builds itself (`internal/cli/streams_integration_test.go`).
The help and output goldens under `internal/cli/testdata/` are
hand-maintained fixtures; [`CONTRIBUTING.md`](CONTRIBUTING.md) says how.

### Layout

```
cmd/berth/               entry point: argv and the environment come in here and
                          are handed down as parameters; library code never looks
                          a variable up by name itself
internal/
  cli/                    command tree, dispatch, error reporting
  commands/               the everyday verbs as plain functions over a context
  config/                 berth.toml schema, discovery and the JSON Schema it publishes
  shape/                  the small schema library config files are validated
                          against, strict about unknown keys
  hooks/                  hook protocol and sequencer
  progress/               phase logging with an elapsed-time heartbeat
  ports/                  the lock-free loopback port allocator and its free-port probe
  checkoutindex/          per-worktree cards, the snapshot, and the rendered page
  router/                 the shared router's Compose stack per network mode, mode
                          detection, its file-provider route files and their
                          repair, and the ports it keeps
  backend/                the Compose backend: labels, overrides, inventory, reap
  worktree/               what the control plane and backend both say about a worktree
  registry/               identity markers, slugs, the registry
  lock/                   flock-based locks and their checked recovery
  binstore/               the content-addressed store `worktree remove` runs
                          itself from, outside any checkout
  settings/               the machine's own settings.toml, read as strictly as berth.toml
  layout/                 where everything lives under the berth root and control home
  files/                  owner-only, symlink-refusing filesystem primitives
  text/                   the filter every printed line goes through
  brand/                  the product name and the presentation/storage split
  stateformat/            the format version of each stored state, and the
                          refusal of one a newer berth wrote
  clierr/                 the error type a command reports and exits with
  logger/                 the stdout/stderr split every command writes through
  run/, run/osrun,
  run/fakerun/            the subprocess seam, and its real and fake implementations
  docker/                 runs the `docker` CLI and tells it apart from Docker being unavailable
  procid/                 process identity and process-table lookups, for lock recovery
  lifecycle/              signal handling and the lock-ordered mutation wrapper
  style/                  the code-style rules gofmt, vet and staticcheck don't
                          enforce, written as tests over the module's own source
```

Tests live beside the code they cover, as `_test.go` files; anything that
needs a real process, a signal, or a Docker daemon is behind
`//go:build integration` and runs separately from the default `go test ./...`.

## Roadmap

Shipped:

- **Compose runtime** — `up`/`down` that stamp every resource with
  `<namespace>.managed`, `.repository-uuid` and `.worktree-uuid` (and `.format`
  on containers, networks and images, not on named volumes); inventory and
  teardown that verify the full ownership tuple on every resource before
  touching it.
- **`up` / `down` / `status` / `urls`** — under the worktree lock, with
  interrupts deferred until it can be released cleanly.
- **`worktree list` / `worktree remove`** — ownership-verified teardown in
  crash-safe order, run from a content-addressed copy of the binary
  installed outside any checkout; checkouts Git no longer lists are reported
  missing.
- **Host routes** — `route add|rm|ls` send a hostname of the worktree to a
  process on the host through the same router, recorded in the registration;
  `up` and `down` keep them and `worktree remove` removes them.
- **Shared router** — one Traefik proxy across every worktree of every
  repository, routed entirely through its file provider (no Docker-socket
  access at all) to services on pinned loopback ports, in host network mode on
  a native Linux Engine and bridge mode on Docker Desktop and OrbStack, and
  owned by the berth root that started it: another root's `up` refuses rather
  than taking it over.
- **Settings file** — `settings.toml` under the berth root, starting with
  the router's port; `router status` and `doctor` say where the port came
  from.
- **Checkout index** — `berth.localhost`: every registered worktree on the
  machine, grouped by domain, with per-service links and status.
- **`worktree unlock`** — clears a dead holder's locks (a worktree's own,
  or, with `--registry`/`--router`, the registry's or router's) once it is
  checked gone; `doctor` reports every held lock, and any left unreleased by
  a holder that is gone.
- **`run`** — the project's own named `[commands]`, run on the terminal
  against a registered worktree: they can prompt, answer Ctrl-C, and hold
  the worktree's lock until everything they started has exited.
- **`dev`** — `up`, then the worktree's logs followed until Ctrl-C, holding
  the development-session lock for the whole session so `worktree remove`
  refuses while one is attached.
- **`backup` / `restore`** (#10) — run the hooks of the same name against a
  registered, running worktree with a bundle directory as their first
  argument, under the worktree's lock, exiting with the hook's own status.
- **`worktree rename`** (#11) — change the current worktree's slug and
  hostname; refused while it has containers, and its data, volumes and pinned
  ports stay, keyed by its ID.
- **Shared services** (#123) — `[shared]`: a service such as an email catcher
  that runs once per repository, connected to each worktree that uses it by
  its name, with its definition checked for drift, its ports pinned on
  `127.0.0.1`, a routed web UI at `<name>.shared.<domain>`, and `shared up`,
  `shared down` and `shared status` to run it yourself.
- **`init`** (#9) — writes a commented starter `berth.toml` at the Git
  checkout's root, with the schema directive and the Compose file it finds
  there, and never replaces an existing one.
- **`teardown pin --file` / `teardown status`** (#85) — pin a berth binary you
  verified by its sha256 as the teardown binary, and check the pin; see
  [Teardown pin](#teardown-pin). `worktree remove` does not run it yet.
- **`doctor`** — one read-only report: Git, Docker and Compose against the
  versions this tool needs; the berth root and every registration
  (unreadable records, a hostname claimed twice, the index's own hostname);
  the router's status, mode and loopback-only bind and a probe of each route;
  and, for this
  checkout, whether every configured hook and command resolves to something
  runnable,
  its `verify` hook, and any registration of its repository whose checkout
  Git no longer lists.

Not built yet, in rough order; apart from the pin commands in item 2, none of
these is a command at all.

1. **Migrating stored state between releases** (#29) — every store already
   carries a format version and refuses a newer one
   ([`docs/design/state.md`](docs/design/state.md)); a migration comes with the
   first change to a store's format after a release.
2. **Running `worktree remove` from a pinned, trusted binary** (#85, #146) —
   the stronger guarantee the relocated-copy mechanism above is a step toward.
   The pin and its two commands are built ([Teardown pin](#teardown-pin));
   pinning a downloaded release, verified with cosign, and the gate that makes
   removal run the pinned binary are not. Tagged releases, with the names and
   signing identity in [`docs/design/release.md`](docs/design/release.md), are
   what it will pin to.

## License

Apache-2.0. See [LICENSE](LICENSE).
