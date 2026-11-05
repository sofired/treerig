# Machine-readable output

The `--json` documents: the rules that hold for all of them, then the shape of each.

`worktree list`, `doctor`, `status`, `shared status`, `route ls`, `index`
and `teardown status` take `--json`. With it, stdout carries exactly one JSON
document, indented and ASCII-only (every non-ASCII character is escaped
`\uXXXX`), and the exit code is the text form's. Notices, warnings, the
`doctor` summary and errors stay on stderr in both forms. A command that fails
before it has anything to report (not in a repository, `route ls` in an unregistered
worktree, `index` giving up after waiting up to 60 seconds for the router lock)
prints its usual `[treerig] ERROR:` on stderr, nothing on stdout, and exits 1;
there is no JSON error document. `status`, `worktree list` and `index` are the
exception when Docker does not answer: they print their document, with
`unknown` states, and then exit 1 with the cause on stderr. For a check that
must not wait on a lock, use `status` or `doctor`, which take none.

> **Changed in this release (breaking for scripts).** One set of
> [state words](commands.md) replaces three vocabularies:
>
> - `worktree list`: `state` was `ready`, `pending` or `missing`; it is now
>   `running`, `starting`, `partly_running`, `failed`, `stopped`, `not_started`,
>   `removing`, `directory_missing` or `unknown`. A new `hint` says why a state
>   is `unknown`.
> - `status`: a new top-level `state` (and `hint`). A service's `state` was
>   `running`, `starting`, `stopped`, `absent` or `unknown`; it is now
>   `running`, `starting`, `unhealthy`, `restarting`, `exited`, `stopped`,
>   `not_created` or `unknown`. `absent` is `not_created`; `detail`
>   `unhealthy` on a running service is the state `unhealthy`; a restarting
>   service was `starting` and is `restarting`; an `exited` service has
>   `exitCode`. The same holds for the `shared` entries here and in `shared
>   status`.
> - `index`: `entries[].status` was `running`, `partial`, `stopped`, `missing`
>   or `unknown`; it is now the worktree states above (`partial` is
>   `partly_running`, `missing` is `directory_missing`, and a worktree no `up`
>   has finished is `not_started`), with `hint` for an unknown one.
> - Exit codes: `status` already exited 1 and printed nothing when Docker did
>   not answer; it now prints its document first. `worktree list` and `index`
>   used to exit 0 with `unknown` rows (or, for `list`, without asking Docker);
>   they now exit 1.
> - `urls` is gone, and `urls --json` with it: the command is a hidden stub
>   that prints `'treerig urls' is now 'treerig status'.` on stderr and exits 1,
>   with nothing on stdout, for one release. Read `status --json` instead. Its
>   `services` array has every entry of the old array that was a service or a
>   host route (the same `service` and `port` keys, and `"hostRoute": true`
>   on a host route), each with its `url` while it can load. It has no
>   `label` or `purpose` (they described the overview's links; `index --json`
>   still has them), and, unlike `urls --json`, an unregistered worktree is
>   not an error: it prints `{"registered": false, "services": []}` and exits 0.
>   `status --json` also gains `router`, `dockerError`, `url` and, for a host
>   route, a `state` of `listening` or `not_listening`. It also lists services
>   that are not routed, which have no `url`.

Rules that hold for every document:

- Keys are camelCase. Enum values (`status`, `state`) are lowercase.
- An optional key is omitted when it has no value, never `null`; a list is
  `[]` when empty.
- The name of a worktree's own service is under the key `service`, in `status`
  and `index` (including the `shared` entries of `index`). A shared
  service in `status` and `shared status` is under `name`, and so is a host
  route in `route ls`.
- Free text (`subject`, `detail`, `label`, `purpose`, `branch`, paths) is for
  people. Scripts key on the ids and enum values described below.
- Until 1.0 any shape may change. After 1.0 changes are additive only: a new
  key, or a new member of an enum. Consumers must ignore keys they do not know
  and treat an unknown enum value as unknown. Documents carry no version field.

**`teardown status --json`** is described under [Teardown pin](teardown-pin.md);
every document has `removal` (`"pin"`, `"this-build"` or `"refuse"`), what
`worktree remove` would do, and the exit code is 0 exactly when it is not
`"refuse"`. `ok` says only that the stored binary matches the pin; `removal` is
the answer to rely on. When it prints nothing at all and exits 1, the pin could
not be read (it is of a newer format, or not one treerig wrote): stdout is empty
and stderr says why.

**`worktree list --json`** prints one array, `[]` when there is nothing,
sorted by domain and then slug. Each row has `repositoryId`, `worktreeId`,
`slug`, `domain`, `host`, `state` (`running`, `starting`, `partly_running`, `failed`,
`stopped`, `not_started`, `removing`, `directory_missing` or `unknown`: see
[State words](commands.md)) and `current`, plus `hint` (what made an `unknown` state
unknown), `root`, `branch` and `labelNamespace` when they are known.

**`doctor --json`** is described in [doctor.md](doctor.md#machine-readable-report).

**`route ls --json`** prints one array of `{"name", "url", "port"}`, by route
name, with the route's `subdomain` and `path` when it has them (the fields
`route add` takes again to write a missing route file), `[]` when there are no
host routes (a notice stays on stderr). `port` is the port the process on the
host listens on. An unregistered worktree is an error: exit 1 and no document.

**`status --json`** is always printed: the document is the same whatever the
worktree's state. It has the worktree's `host`, its `state` (one of the worktree
states `worktree list` uses, and `hint` when it is `unknown`), the shared
router's state under `router`, and `services`: one entry per declared service,
then one per host route, in that order.

- A service's `state` is `running`, `starting`, `unhealthy`, `restarting`,
  `exited`, `stopped`, `not_created` or `unknown` (what each means is under
  [State words](commands.md)). `exitCode` appears with `exited`. `detail`
  appears for `unknown`: Docker's own word for the state.
- A host route has `"hostRoute": true`, and its `state` is `listening` or
  `not_listening`: whether a connection to its `port` on `127.0.0.1`
  succeeded. Its `service` is the route's name.
- `url` is the URL the service answers on, and appears only while the service is
  `running` (a host route: `listening`) and the router serves this root's URLs
  (it runs, this root started it, and it listens on the port the URLs name). It
  is never checked over HTTP. When it is absent for a service that has a
  route, look at `state` and at `router`. A service that is not routed has no
  `url`.
- `router` is an object. Its `state` is `"running"`, `"stopped"` (no
  container, or one that is not running) or `"unknown"` (Docker did not answer,
  or the container could not be read). `port` is the port a running router
  serves and `detail` says why it is unknown, or whose it is when this treerig
  root did not start it; both are absent otherwise. `router`
  itself is absent when `status` stopped before it read the services: the
  worktree is not registered, or its directory is missing, undetermined, now in
  another repository, or was never recorded (`state` is then
  `directory_missing`, or `unknown` with a `hint`, and `services` is `[]`).
- `dockerError` appears only when Docker did not answer, and says why. The
  document is still printed, with `"state": "unknown"` and no Compose services
  (host routes are still listed, since their ports need no Docker), and the
  command exits 1. It also exits 1 when Compose could not read the worktree
  (`hint` says so, and there is no `dockerError`), and when the `treerig.toml` of
  a worktree named on the command line would not load, or its Compose file is too
  large or not a regular file; it then prints nothing.
  It exits 0 in every other case, whatever the state.
- `port` is a number (for example `20417`): the routed service's pinned port on
  `127.0.0.1`, or the port a host route's process listens on. It is absent for
  a service that is not routed or has none allocated yet.

A worktree that was never registered prints `{"registered": false, "services":
[]}` (the notice is still on stderr) and exits 0, as the text form does. A
worktree named on the command line (`status feature-x --json`) has the same
document.

```json
{
  "registered": true,
  "host": "feature-login.acme.localhost",
  "state": "partly_running",
  "router": { "state": "running", "port": 80 },
  "services": [
    { "service": "api", "state": "running", "url": "http://feature-login.acme.localhost", "port": 20417 },
    { "service": "db", "state": "unhealthy" },
    { "service": "seed", "state": "exited", "exitCode": 0 },
    { "service": "web", "hostRoute": true, "state": "not_listening", "port": 5173 }
  ]
}
```

When `treerig.toml` declares shared services, `status --json` also has a
`shared` array, one entry per declared service, in file order (the key is absent
otherwise). `state` uses the words of `services` (`not_created` for one with no
container), with `exitCode` beside `exited`. One inconsistent container is an
`unknown` entry with a `detail`, not a failure of the whole command; a record
that cannot be read leaves `addrs` out with a warning and makes the definition
`unknown`. If Docker cannot be asked about the shared services at all, `status`
warns on stderr, leaves the `shared` array out (and the `shared services` block
in the text form) and still reports the worktree's own services, exiting 0.
`addrs` lists the declared container ports that have a pinned loopback port,
`{"port": <container port>, "addr": "127.0.0.1:<host port>"}`, running or not.
`definition` is `current` when the running instance was started from the
definition this worktree's shared file hashes to, `differs` when from another,
and `unknown` when nothing is running or the file cannot be read. `connected` is
true when the shared container is attached to every network of this worktree's
own that one of its running `used-by` services is on, and at least one; it is
false for a worktree with no running consumer, which the text form calls `not
used here` (`not connected` is reserved for a running consumer whose network the
container is missing). `url` is the routed web UI
(`http://<name>.shared.<domain>`, router port included) and is absent for a
service with no routed `port`; in `status --json` it is also absent unless the
service is `running` and the router serves this root's URLs, as for
`services` (`shared status --json` always has it). It is a separate entry type
for that reason: the two commands share every other field. `definedBy` is the hostname of the worktree that
started the running instance, or `a removed worktree`, and is absent when
nothing runs.

```json
{
  "registered": true,
  "host": "feature-login.acme.localhost",
  "state": "running",
  "router": { "state": "running", "port": 80 },
  "services": [
    { "service": "api", "state": "running", "url": "http://feature-login.acme.localhost", "port": 20417 }
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
`{"shared": [ … ]}`, or `{"shared": []}` when nothing is declared. Its `url`
is the routed address whether the service runs or not, where in `status --json`
it is present only while the service runs and the router serves this root's
URLs. Unlike
`status` it works from a worktree that is not yet registered, as long as the
repository has been brought up once.

**`index --json`** prints what the index page is built from, in the page's
order. Like the text form it refreshes the page, which takes the shared
router's lock and waits up to a minute for it; use `status` or `doctor` for a
check that takes no lock. Only services that declare `[service.<name>.index]` are
listed, and each has a `label`; `purpose` is absent when there
is none.

- `branch` is `""` for a detached HEAD and absent when nothing knows it;
  `root` is absent when unknown. `project` is the project name `up` last
  recorded, and the domain when nothing was recorded.
- `available` on a service is set only when it was checked, which needs Docker
  to have answered and the worktree's directory not to be missing. Absent means
  unknown; there is no `false` for "could not check".
- `shared` lists the routed web UIs of the repositories' shared services
  (`repositoryId`, `service`, `project`, `domain`, `host`, `url`, and `available`
  under the same rule as a service's); it is absent when there are none.
- When Docker did not answer, `dockerReachable` is false and `dockerError` says
  why.
- `identityError` (Git could not say which worktrees exist) appears only when
  that happened, and `cardReadFailures` counts index cards that could not be
  read.
- `entries[].status` is a worktree state: `running`, `starting`, `partly_running`,
  `failed`, `stopped`, `not_started`, `removing`, `directory_missing` or `unknown`
  (see [State words](commands.md)), with `hint` naming the cause of an unknown one.

```json
{
  "url": "http://treerig.localhost",
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
    },
    {
      "repositoryId": "…",
      "worktreeId": "…",
      "project": "acme",
      "domain": "acme.localhost",
      "slug": "old-spike",
      "host": "old-spike.acme.localhost",
      "branch": "spike",
      "root": "/mnt/work/acme.old-spike",
      "status": "unknown",
      "hint": "Its parent directory is missing or empty: is the volume mounted?",
      "services": []
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
