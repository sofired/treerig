# Environment variables

The environment variables you can set to change how treerig runs, and the ones treerig sets for your hooks.

treerig reads few environment variables. The `TREERIG_*` names do not follow the binary's name: run as `trig`, or as a name of your own, they stay the same.

## Variables you set

| Variable | Read by | What it does |
| --- | --- | --- |
| `TREERIG_HOME` | every command | The treerig root: the directory that holds `settings.toml`, the router's state and, unless `TREERIG_CONTROL_HOME` moves it, the control home. Default `~/.treerig`. A variable that is set but empty is not the same as unset: it resolves to the current directory. The filesystem root, your home directory and anything that contains it are refused. See [Settings](settings.md). |
| `TREERIG_CONTROL_HOME` | every command | The control home, where each worktree's private directory lives. Default `control-home/` under the treerig root. It moves only the control home. Same empty-value rule and refusals as `TREERIG_HOME`. See [Settings](settings.md). |
| `TREERIG_ROUTER_PORT` | every command that starts or reports the router | The loopback port the shared router serves: a whole number from 1 to 65535 (plain decimal recommended; JavaScript-style forms such as `8e1` or `0x50` are also accepted, and so is surrounding whitespace). It wins over `[router] port` in `settings.toml`; with neither, the port is 80. An empty value counts as unset. Anything else fails with `TREERIG_ROUTER_PORT must be a port number between 1 and 65535, not "<value>".` See [Settings](settings.md) and [Troubleshooting](troubleshooting.md#port-80-is-taken). |
| `TREERIG_DEBUG` | every command | `1`, `true`, `yes` or `on` (any case) prints the chain of causes behind a failure, after its message. Without it, an unexpected failure ends with the hint `(set TREERIG_DEBUG=1 to see the stack trace)`. See [Output and exit codes](output-and-exit-codes.md). |
| `HOME` | every command | Where the default treerig root, `~/.treerig`, lives. If `HOME` is unset or empty, treerig asks the system's password database for your home directory instead. That directory is also what a treerig root or control home may not be, nor contain. |
| `DOCKER_HOST` | every command that starts the router | Not a `TREERIG_` variable, but it matters: a Docker host that is not a local socket is refused. See [Platform notes](platforms.md#remote-docker-daemons). |
| `FORCE_COLOR`, `NO_COLOR`, `NODE_DISABLE_COLORS`, `TERM` | confirmation prompts | Colour in the prompt that asks you to type a phrase. `FORCE_COLOR` (empty, `1`, `2`, `3` or `true`) turns it on and (`0` or `false`) off; `NO_COLOR` or `NODE_DISABLE_COLORS`, set to anything, or `TERM=dumb`, turn it off; otherwise it is on when stderr is a terminal. |

`PATH` matters too: a hook or `[commands]` entry whose command names no path is looked up on it (see [Resolution](hooks.md#resolution)).

The `COMPOSE_*` variables are handled for you. `COMPOSE_FILE`, `COMPOSE_PATH_SEPARATOR`, `COMPOSE_ENV_FILES`, `COMPOSE_DISABLE_ENV_FILE` and `COMPOSE_IGNORE_ORPHANS` are dropped from the environment of every Compose call, and `COMPOSE_PROGRESS` is always set to `plain`. Every other `COMPOSE_*` variable reaches Compose unchanged. See [Configuration](configuration.md) and the [Trust model](trust-model.md).

### The install script

`install.sh` reads these. An option on its command line wins over its variable. See [Install](install.md).

| Variable | What it does |
| --- | --- |
| `TREERIG_INSTALL_DIR` | The directory to install into, like `--dir`. Default `$HOME/.local/bin`. |
| `TREERIG_VERSION` | The version to install, like `--version`. Default: the latest release. |
| `TREERIG_NO_ALIAS` | Any non-empty value except `0` skips the `trig` alias. With `curl \| sh`, set it on the `sh` side of the pipe. |
| `TREERIG_BASE_URL` | The release-download base URL, laid out as `<base>/v<version>/<file>`. For mirrors and testing. |
| `TREERIG_LATEST_URL` | A `checksums.txt` that stands for the latest release. For testing. |

### Tests and CI only

These are for people working on treerig itself, and they change nothing about how treerig runs. They are the ones you set; test helpers also set private `TREERIG_*_HELPER_*` and similar variables for child processes. See [CONTRIBUTING.md](../CONTRIBUTING.md).

| Variable | What it does |
| --- | --- |
| `TREERIG_TEST_REQUIRE_DOCKER` | `1` makes a Docker test suite fail, instead of skip, when no daemon answers. |
| `TREERIG_TEST_SKIP_DOCKER` | `1` skips the Docker suites even when a daemon answers. |
| `TREERIG_REQUIRE_COSIGN` | `1` makes the teardown-pin tests fail, instead of skip, when `cosign` or the snapshot is missing. |
| `TREERIG_SNAPSHOT_DIST`, `TREERIG_SNAPSHOT_VERSION` | The `dist/` directory and version of a GoReleaser snapshot, for those tests. |

## Variables treerig sets

Every hook and every [command](hooks.md#commands) inherits your environment plus these. The Compose file sees only the ones marked in the last column, so `${TREERIG_WORKTREE_HOME}` works in a bind mount. [The hook contract](hooks.md#environment) is the full reference, including when each is unset. `<NAME>` is a service's, host route's or shared service's name, upper-cased, with each `-` turned into `_`.

| Variable | Set for | Compose file sees it | Value |
| --- | --- | --- | --- |
| `TREERIG_STAGE` | hooks | no | `prepare`, `seed`, `verify`, `backup` or `restore` |
| `TREERIG_COMMAND` | commands | no | The name of the `[commands]` entry being run |
| `TREERIG_PROJECT` | hooks and commands | yes | `project.name` from the config |
| `TREERIG_REPOSITORY_ID` | hooks and commands | yes | UUID of the repository |
| `TREERIG_WORKTREE_ID` | hooks and commands | yes | UUID of this worktree |
| `TREERIG_SLUG` | hooks and commands | yes | The worktree's slug |
| `TREERIG_HOST` | hooks and commands | yes | `<slug>.<domain>` |
| `TREERIG_WORKTREE_HOME` | hooks and commands | yes | The worktree home: the worktree's private directory under the control home |
| `TREERIG_CONFIG` | hooks and commands | yes | Absolute path of the loaded `treerig.toml` |
| `TREERIG_ROOT` | hooks and commands | yes | The directory that contains `treerig.toml`; also the working directory |
| `TREERIG_COMPOSE_PROJECT` | hooks and commands | yes | The worktree's Compose project name, `treerig-<hash>` |
| `COMPOSE_PROJECT_NAME` | hooks and commands | yes | The same name, for Compose; replaces an inherited value |
| `COMPOSE_FILE` | hooks and commands | no | Absolute path of the Compose file `[compose] file` names; replaces an inherited value |
| `TREERIG_URL` | hooks, commands and Compose | yes | The worktree's own URL: the service or host route at the host root. Unset if none |
| `TREERIG_SERVICE_URL_<NAME>` | hooks, commands and Compose | yes | One per service and host route: its routed URL |
| `TREERIG_SERVICE_PORT_<NAME>` | hooks and commands | no | One per service with a pinned port, and per host route: the port on `127.0.0.1` |
| `TREERIG_SHARED_ADDR_<NAME>_<PORT>` | hooks and commands | no | One per pinned port of a [shared service](shared-services.md): `127.0.0.1:<host port>` |
| `TREERIG_SHARED_URL_<NAME>` | hooks and commands | no | One per shared service with a routed `port`: its web UI's URL |

`TREERIG_HOME` and `TREERIG_CONTROL_HOME` are not in this list on purpose: they pass through to hooks unchanged, so a hook that runs treerig resolves the same treerig root and control home you did.

The Compose file sees the variables marked in the table's last column, and [Writing your compose.yaml for treerig](compose.md#variables-you-can-use) is the one list of them, with examples. The shared services' own Compose file gets none of the per-worktree ones.
