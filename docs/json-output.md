# Machine-readable output

The `--json` documents: the rules that hold for all of them, then the shape of each.

`worktree list`, `doctor`, `status`, `shared status`, `route ls`, `urls`, `index` and `teardown status` take `--json`. With
it, stdout carries exactly one JSON document, indented and ASCII-only (every
non-ASCII character is escaped `\uXXXX`), and the exit code is the text
form's. Notices, warnings, the `doctor` summary and errors stay on stderr in
both forms. A command that fails before it has anything to report (not in a
repository, `urls` in an unregistered worktree, Docker failing under `status`,
`index` giving up after waiting up to 60 seconds for the router lock) prints
its usual `[treerig] ERROR:` on stderr, nothing on stdout, and exits 1; there is
no JSON error document. For a check that must not wait on a lock, use `status`
or `doctor`, which take none.

Rules that hold for every document:

- Keys are camelCase. Enum values (`status`, `state`) are lowercase.
- An optional key is omitted when it has no value, never `null`; a list is
  `[]` when empty.
- The name of a worktree's own service is under the key `service`, in `status`,
  `urls` and `index` (including the `shared` entries of `index`). A shared
  service in `status` and `shared status` is under `name`, and so is a host
  route in `route ls`.
- Free text (`subject`, `detail`, `label`, `purpose`, `branch`, paths) is for
  people. Scripts key on the ids and enum values described below.
- Until 1.0 any shape may change. After 1.0 changes are additive only: a new
  key, or a new member of an enum. Consumers must ignore keys they do not know
  and treat an unknown enum value as unknown. Documents carry no version field.

**`teardown status --json`** is described under [Teardown pin](teardown-pin.md). When
it prints nothing at all and exits 1, the pin could not be read (it is of a
newer format, or not one treerig wrote): stdout is empty and stderr says why.

**`worktree list --json`** prints one array, `[]` when there is nothing,
sorted by domain and then slug. Each row has `repositoryId`, `worktreeId`,
`slug`, `domain`, `host`, `state` (`ready`, `pending` or `missing`) and
`current`, plus `root`, `branch` and `labelNamespace` when they are known.

**`doctor --json`** is described in [doctor.md](doctor.md#machine-readable-report).

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

When `treerig.toml` declares shared services, `status --json` also has a
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
