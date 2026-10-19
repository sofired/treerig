# Shared services

Run one instance of a service, such as an email catcher, for a whole repository instead of once per worktree.

## How shared services work

Some services are wasteful once per worktree: an email
catcher, an object-store emulator, a broker's web UI. A *shared service* runs
once per repository, in a Compose project of its own, and is connected to
each worktree whose services use it. "Repository" is one repository UUID, so
two clones of the same upstream have two instances. `treerig.toml` names the
service, the ports it publishes for the host and the worktree services that
use it (see [Declaring shared services](#declaring-shared-services) below); the first `up` of any
worktree that uses it starts it, and every later one finds it running and
leaves it alone. It joins each using worktree's *own* network under its name,
so `mailpit:1025` reaches the same Mailpit from every worktree, and a service
of one worktree still cannot resolve a service of another. Its resources carry
the repository's ownership labels and a `shared` label, and never a worktree's:
`worktree remove` and `down` cannot list one, and only `treerig shared down`
removes it. It is the one thing that outlives the worktrees that use it.

## Declaring shared services

A shared service is one instance per repository, defined in a Compose file of
its own and started by whichever worktree needs it first:

```toml
[shared]
file = "docker/compose.shared.yaml"   # relative to treerig.toml; its own Compose project

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

### What the file may say

A shared name is a table key, like a service's:
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
your environment, `TREERIG_PROJECT`, `TREERIG_REPOSITORY_ID` and the Compose
project name — never `TREERIG_ROOT`, `TREERIG_WORKTREE_ID` and the rest of a
worktree's own, and not the `TREERIG_*` a hook passes down.

### Reading the same from every checkout

Two checkouts of a repository must
compute the same hash for the file, or `up` would warn that the definition
differs when nothing changed. A relative path resolves against each checkout's
own directory, so `../shared-data` names a different directory in each, and
`${PWD}` is the directory the command ran in; `PWD` and the rest of your shell's
environment are inherited. treerig reads the file a second time from a scratch
directory and refuses it, naming the first value that changed, such as
`services.db.volumes[0].source resolves to "/work/shared-data" here, from a
relative path` (or, when it cannot tell, `a path or variable that depends on the
checkout`); `treerig up` fails when a shared service it uses is refused, and so do
`treerig shared up` and `doctor`. Use
an absolute path outside every worktree, or a named volume. This checks the
whole file, not only the services you use, and costs one extra `compose config`
inside the shared lock. The worktree roots it checks against are the config's
directory and the registered worktrees; a file kept outside every worktree is not
checked, and neither is a read made with no roots (`treerig shared status`).

The check is best effort. If it cannot read the file the second time, which
happens when an `env_file`, `include` or `extends` climbs out of the file's
directory (`../../outside.env`), treerig accepts the file and warns `Could not
check whether … reads the same from every checkout`, on every command that reads
it. Absolute paths there, or keeping the files it reads under the shared file's own
directory, silence the warning. A scratch directory
(`shared-probe-*`) is made under the control home for each read and removed
after it; a killed `treerig` can leave one, which is harmless.

Variables safe in the shared file are the ones with the same value for every
checkout: `TREERIG_PROJECT`, `TREERIG_REPOSITORY_ID`, the Compose project name and
any variable you set identically everywhere. Do not use one that differs between
users, shells or machines (`${USER}`, `${HOME}`, a machine-specific setting):
treerig reads the file in one environment and cannot see that, so the hash would
differ across them and `treerig up` would warn.

### Starting it

A worktree whose Compose file has a `used-by` service
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
shared file; treerig does not add one, and `doctor` reports a stopped container.

### Connecting it

After the worktree's own `compose up`, and before its
hooks (which may send mail), `up` connects the shared container, under the
shared name as an alias, to each of the worktree's own networks that a running
`used-by` container is on, proven by the worktree's ownership labels. It does
not connect it to anything else. `down` disconnects first and then stops the
worktree; `worktree remove` disconnects before it reaps anything. Compose
leaves a network that something is still attached to, so this order is what
lets a worktree's network be removed while the shared service keeps running.
The worktrees never see one another's services: `web` calling `api` resolves
inside its own worktree's network only.

### Replacing it

Each shared container carries a hash of its definition (the
service and everything it `depends_on`, before treerig's own override) and the
worktree that started it. A worktree whose file hashes differently gets a
warning from `up`, naming who started the running one, and nothing is
restarted: a branch that bumps an image tag must not break an unrelated
branch's `up`. `treerig shared up` refuses a differing definition;
`treerig shared up --recreate mailpit` replaces that named service (at least one
name is required; the refusal lists the running ones), whether or not its
definition differs (so it is also how to restart one), keeps its
volumes, and reconnects every worktree that uses it. Attachments belong to a
container, so a new container has none: whoever creates one reconnects the
other worktrees after releasing the shared lock, one worktree at a time, and a
worktree that is busy is skipped with a warning and repaired by its own next
`up` or by `treerig shared up`. `shared up` ends with `N worktree(s) were skipped
and are not connected yet; run 'treerig shared up' again when they are idle.` and
still exits 0; a worktree it could not read at all is an error. `treerig shared up` always does this sweep, also
when nothing needed replacing, so it is the repair for a worktree left
unconnected, and it repairs before it reports a differing definition (which
still exits non-zero); `doctor` finds one.

The hash is computed from pinned inputs: reading the shared file always means
`--profile '*'` and an empty `--env-file`, so a `.env` beside it is ignored and
every profile counts, and it covers
the top-level volumes, networks, configs and secrets the service closure
references. It is `compose config`'s output, so a Compose upgrade can change it
and show `differs` once for a service that did not change; run `treerig shared up
--recreate <name>` to restamp it.

### Reaching it

From a container, `mailpit:1025` on the worktree's network.
From the host, each declared port is published on a pinned `127.0.0.1` port
from the same allocator as a routed service's (see [Ports](routing.md#ports)), kept across
`shared down` and released only by `shared down --volumes`; hooks and `treerig
run` get it as `TREERIG_SHARED_ADDR_<NAME>_<PORT>=127.0.0.1:<host port>`, with
`<NAME>` mapped as in `TREERIG_SERVICE_URL_<NAME>` and `<PORT>` the container
port (`TREERIG_SHARED_ADDR_MAILPIT_1025`). Nothing is injected into Compose:
that would tie a worktree's Compose environment to shared state and recreate
its containers when the state changed. A host process uses the pinned port; a
container never does, because on a host firewalled by default a container
cannot reach the host's loopback.

### Its web UI

A `port` is also routed, through the same proxy as your
services, at `<name>.shared.<domain>` (`mailpit.shared.acme.localhost`), under
the domain the service was first pinned with. `shared` is therefore reserved:
a branch that slugs to `shared` gets a suffixed slug. The route file is written when the service is up
and removed by `shared down`; `treerig shared up` starts the router first when
it has a web UI to route, its own or a dependency's. `TREERIG_SHARED_URL_<NAME>` gives hooks and `treerig run`
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
stale port from the record. Dropping `port` from `treerig.toml` stops routing a
running service at the next `up` or `shared up` without restarting it; a port
newly declared as the web UI that the container was not started with needs
`shared up --recreate <name>`.

### Commands

```bash
treerig shared up [name...]                    # start (or reuse) them, then reconnect every worktree
treerig shared up --recreate mailpit           # replace a running one with this worktree's definition; keeps its data (a name is required)
treerig shared up --reallocate-port mailpit:1025   # give a container port a new pinned port; refused while it runs
treerig shared down [name...]                  # stop and remove the containers; named volumes and pinned ports are kept
treerig shared down --volumes [name...]        # also delete the data and release the ports; asks first
treerig shared status [--json]                 # state, web UI URL, addresses, definition, connection, who started it
treerig worktree unlock --shared               # clear a dead holder's shared lock, once it is checked gone
```

`shared down` finds everything by its labels, from any checkout of the
repository and without a `treerig.toml` (give `--label-namespace` when a project
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
in `[shared.service.<name>]`, where treerig publishes it on `127.0.0.1`. A shared service that runs natively on the host and
is used by containers is not supported: on Linux with a default-deny firewall
a container cannot reach the host.
