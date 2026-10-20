# Configuration

The keys of `treerig.toml`. The machine's own `settings.toml` is in [settings.md](settings.md).

| Key | Meaning |
| --- | ------- |
| `project.name` | Lower-case slug. |
| `project.domain` | Worktrees are routed as `<slug>.<domain>`. |
| `project.label-namespace` | Optional. Only to adopt resources created by a predecessor tool. |
| `compose.file` | Compose file, relative to `treerig.toml`. How to write it for treerig: [Writing your compose.yaml](compose.md). Paths inside it (build contexts, bind mounts, `env_file`) and its `.env` resolve against the Compose file's directory, as with `docker compose -f`; `${TREERIG_ROOT}` is still `treerig.toml`'s directory. |
| `[service.<name>]` | One table per service, named by its Compose service (lower-case letters, digits, hyphens; at most 64 tables, each name at most 63 characters, as for `[commands]`). Optional `subdomain` (empty = host root), optional `path` (route only that prefix, so services can share a host), optional `port` (the container port to route to; defaults to the first `expose` entry, then the first `ports` target, see [Ports](compose.md#ports)), optional `index` presence. |
| `[shared]` | Optional. `file`: the shared services' Compose file, relative to `treerig.toml`; required when any `[shared.service.<name>]` table exists. |
| `[shared.service.<name>]` | One table per shared service, named by its service in the shared file (same naming rules as `[service.<name>]`). Optional `port` (the web UI's container port), optional `ports` (more container ports), both pinned on `127.0.0.1` for the host; at most 16 in all. `used-by`: the worktree's own Compose services that use it. See [Shared services](shared-services.md). |
| `[hooks]` | `prepare`, `seed`, `verify`, `backup`, `restore` — resolved as in [`hooks.md`](hooks.md): relative to the config file when the command starts with `.` or contains `/`, otherwise from `PATH`. `up` runs `prepare` and `seed`, `doctor` runs `verify`, `treerig backup` and `treerig restore` run `backup` and `restore`. |
| `[hooks.timeout]` | Optional. A time limit per hook, keyed by the hook's name (`prepare`, `seed`, `verify`, `backup`, `restore`): a duration such as `30s`, `10m` or `1h30m`, greater than zero and at most `24h`. A hook with no entry has no limit. When the limit passes treerig stops the hook and its whole process group on the host (not work it handed to a container, such as a `docker compose exec`), fails the command with `The <hook> hook timed out after <limit> and was terminated.` and exits 124. See [`hooks.md`](hooks.md#timeouts). |
| `[commands]` | Optional. Named commands for `treerig run <name>`, resolved like hooks. Each name (lower-case letters, digits, hyphens) maps to a command line, or to a table: `command`, `lock` (default `true`: hold the worktree's lock while it runs) and `kill-after` (default `10s`, at most `1h`: a duration such as `30s` or `5m`, how long its process group has to exit once stopped, before it is killed). See [`hooks.md`](hooks.md#commands). |

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

## Editor support

`treerig config schema` prints a [JSON Schema](https://json-schema.org)
(draft-07) of `treerig.toml`, generated from the same definitions that validate
it. Every key has a description. Any editor that accepts a JSON Schema for
TOML can use it; [Even Better TOML](https://taplo.tamasfe.dev) (Taplo)
completes keys, shows their descriptions on hover and flags unknown ones. Point
it at the schema with a directive on the file's first line (`treerig init`
writes it for you):

```toml
#:schema https://raw.githubusercontent.com/sofired/treerig/main/schema/treerig.schema.json
```

That copy, [`schema/treerig.schema.json`](../schema/treerig.schema.json), tracks
`main`. To match the version you have installed, pin it with
`treerig config schema > treerig.schema.json` and point `#:schema` at that file
(`#:schema ./treerig.schema.json`).

The schema states what a valid file looks like, not everything treerig checks:
that two services do not claim the same route, for one, only `treerig config
validate` can tell you.

## TOML syntax

The examples here and [`treerig.example.toml`](../treerig.example.toml) stick to
TOML 1.0, with no multi-line inline tables or other 1.1 additions; keep yours
to it if other tools should read it. treerig itself accepts more, but Taplo and
Python's `tomllib` do not.
