# Writing your compose.yaml for treerig

What your Compose file can use, how to expose a port, and what to leave out, so a service comes up at its hostname.

Your Compose file (`compose.yaml`) stays a standard Compose file. treerig adds some variables for it to interpolate and a generated override on top. A variable treerig sets is simply unset under plain `docker compose`; see [Using the variables](#using-the-variables).

Jump to: [Variables](#variables-you-can-use) · [Using the variables](#using-the-variables) · [Ports](#ports) · [Listen on 0.0.0.0](#listen-on-0000) · [What treerig adds](#what-treerig-adds) · [What treerig refuses](#what-treerig-refuses) · [Data and cleanup](#data-and-cleanup) · [Shared services](#shared-services) · [A complete example](#a-complete-example)

## Variables you can use

Compose interpolates `${NAME}` in your Compose file from its environment. `treerig up` and every other Compose call for the worktree add these to your environment:

| Variable | Value |
| --- | --- |
| `TREERIG_WORKTREE_HOME` | The worktree home: the worktree's private directory under the control home |
| `TREERIG_HOST` | `<slug>.<domain>`, for example `feature-login.acme.localhost` |
| `TREERIG_URL` | The worktree's own URL: the service (or [host route](host-routes.md)) routed at the host root. Unset if none is |
| `TREERIG_SERVICE_URL_<NAME>` | The routed URL of each `[service.<name>]` and host route. `<NAME>` is upper-cased, with each `-` turned into `_` |
| `COMPOSE_PROJECT_NAME`, `TREERIG_COMPOSE_PROJECT` | The worktree's Compose project, `treerig-<hash>` |
| `TREERIG_PROJECT` | `project.name` |
| `TREERIG_SLUG` | The worktree's slug |
| `TREERIG_REPOSITORY_ID`, `TREERIG_WORKTREE_ID` | The two UUIDs |
| `TREERIG_CONFIG`, `TREERIG_ROOT` | The absolute path of `treerig.toml`, and the directory that holds it |

Compose does **not** see `TREERIG_STAGE` and `TREERIG_COMMAND` (they exist only inside a hook or command), `TREERIG_SERVICE_PORT_<NAME>`, or `TREERIG_SHARED_ADDR_…` and `TREERIG_SHARED_URL_…` (the shared services' addresses and URLs). [Environment variables](environment.md#variables-treerig-sets) has the full table, including what hooks get. A hook and Compose see the same value for every variable they share. A changed router port or a renamed worktree changes these values, and the next `treerig up` recreates the containers whose configuration uses them, and no others.

`COMPOSE_FILE`, `COMPOSE_PATH_SEPARATOR`, `COMPOSE_ENV_FILES`, `COMPOSE_DISABLE_ENV_FILE` and `COMPOSE_IGNORE_ORPHANS` are dropped from your environment before Compose runs, and treerig names the files itself. Other `COMPOSE_*` variables, and the `.env` file beside your Compose file, work as usual. See [Environment variables](environment.md#variables-you-set).

## Using the variables

### Files and data per worktree

Each worktree has its own worktree home, so a bind mount of a file you supply is per worktree. For example, a `prepare` hook writes a config file there and the Compose file mounts it:

```yaml
services:
  api:
    volumes:
      - ${TREERIG_WORKTREE_HOME}/config:/etc/app:ro
```

Data a container writes belongs on a named volume, not a bind mount: on Linux a container running as another user can leave files in the worktree home that `worktree remove` cannot delete. See [Data and cleanup](#data-and-cleanup) and [Bind mounts on Linux](platforms.md#bind-mounts-on-linux).

### Tell an app its own address

A frontend that must know its public URL, or an API that sets CORS for it, needs a value that differs per worktree:

```yaml
services:
  web:
    environment:
      PUBLIC_URL: "${TREERIG_URL:?no service is routed at the host root}"
      VITE_API_URL: "${TREERIG_SERVICE_URL_API}"
  api:
    environment:
      CORS_ORIGIN: "${TREERIG_URL}"
```

These are the URLs `treerig status` shows, the router's port included. `status` shows one only while its service runs; the variables are set whether it does or not.

These URLs are for browsers, and for configuration that is sent to a browser: public URLs and CORS origins. A container that calls another container uses the service name on the worktree's network, for example `http://api:8080`.

An unset variable interpolates as an empty string, with Compose's `variable is not set` warning (under plain `docker compose` too; give a default with `${TREERIG_URL:-http://localhost:5173}` if you also run the file without treerig). `${TREERIG_URL:?message}` makes Compose stop with `message` instead, which is what you want when the app can't start without it. `TREERIG_URL` is unset when no service is routed at the host root (every `[service.<name>]` has a `subdomain` or `path`), and `doctor`'s read of your Compose file gets no URL variables at all when it can't resolve the router's port.

### Name things per worktree

`${COMPOSE_PROJECT_NAME}` and `${TREERIG_SLUG}` differ for every worktree, so they suit anything that must not collide, such as a cache key prefix:

```yaml
services:
  api:
    environment:
      CACHE_PREFIX: "${COMPOSE_PROJECT_NAME}:"
```

You don't need them for container, network or volume names: Compose already prefixes those with the project name, so leave `container_name` out.

## Ports

treerig routes a hostname to a container port, and publishes that port on a pinned `127.0.0.1` port of its own that the router connects to ([Routing](routing.md#ports)). You only say which container port the app listens on, in one of two ways:

```yaml
services:
  web:
    expose:
      - "5173"        # the container port; nothing is published by this line
```

or in `treerig.toml`:

```toml
[service.web]
port = 5173
```

Don't publish a fixed host port (`ports: ["3000:3000"]`). treerig does the publishing, and a fixed host port is the same number in every worktree, so the second worktree to start fails to bind it. `treerig doctor` warns about a routed service that does this (`checkout.published-port`).

When `port` is not set, treerig reads the Compose service in this order:

1. The first `expose` entry. `"5173/tcp"` reads as 5173.
2. Otherwise the first `ports` entry that has a container port. For `"8080:3000"` that is 3000.

It never reads the image's `EXPOSE` instruction, because Compose doesn't report it. If a service has several ports, or you aren't sure which one comes first, set `port`: it wins over the Compose file.

If neither exists, `up` stops with:

```text
Service web declares no port: set port in its [service.web] table, or expose one in compose.yaml.
```

See [A service declares no port](troubleshooting.md#a-service-declares-no-port).

## Listen on 0.0.0.0

The app must listen on all interfaces inside its container, not on `localhost`. Traffic enters the container through its network address, so a server bound to `127.0.0.1` or `localhost` never receives it, and the hostname answers `502 Bad Gateway` ([Troubleshooting](troubleshooting.md#the-hostname-answers-502-bad-gateway)). Many dev servers default to localhost. Set the host in the command:

| Server | Setting |
| --- | --- |
| Vite | `vite --host 0.0.0.0` |
| Next.js | `next dev -H 0.0.0.0` |
| webpack-dev-server | `webpack serve --host 0.0.0.0` |
| Rails | `bin/rails server -b 0.0.0.0` |
| Django | `python manage.py runserver 0.0.0.0:8000` |
| Flask | `flask run --host 0.0.0.0` |
| Uvicorn | `uvicorn app:app --host 0.0.0.0` |
| Node (`http`, Express) | `server.listen(3000, "0.0.0.0")` |
| Go | `http.ListenAndServe(":8080", handler)` |

This doesn't open the app to your network. The container's network is private to Docker, and treerig publishes the pinned port on `127.0.0.1` only.

A dev server that then answers `403` or `Invalid Host header` is refusing the hostname: see [Allowed hosts](host-routes.md#allowed-hosts).

## What treerig adds

You edit nothing for this. `up` writes an override file that Compose merges over yours and that:

- labels every container, network, image and named volume with the worktree's [ownership labels](concepts.md#ownership-labels), which is how `worktree remove` knows what is the worktree's to delete;
- publishes each routed service's container port on its [pinned `127.0.0.1` port](routing.md#ports).

Services that have no `[service.<name>]` table still run, and still get labels, but are not routed or published.

## What treerig refuses

A routed service needs a network namespace of its own, so treerig can publish one port for it. So `up` stops, naming the service and the fix, for a service that:

- sets `network_mode: host` or `network_mode: service:…`, since it has no network of its own to publish from;
- runs more than one replica (`deploy.replicas` or `scale` above 1), since replicas can't share one port.

Remove the setting, or remove the service's `[service.<name>]` table so it isn't routed.

## Data and cleanup

`treerig worktree remove` deletes what carries the worktree's ownership labels, and then the worktree home:

| Kind of data | On `worktree remove` |
| --- | --- |
| Named volume declared in your `volumes:` | Removed |
| Bind mount under `${TREERIG_WORKTREE_HOME}` | Removed with the worktree home, on Linux only if your user owns the files; otherwise removal stops and prints a `chown` command (see [Bind mounts on Linux](platforms.md#bind-mounts-on-linux)) |
| Bind mount of anywhere else (your checkout, `~/.aws`) | Left alone |
| External volume (`external: true`) | Left alone |
| Anonymous volume an image declares (a `VOLUME` line in its Dockerfile) | Left alone |

Put data a container writes on a named volume, and don't rely on an image's `VOLUME`. `treerig down` keeps all of it. See [Ownership labels](concepts.md#ownership-labels) and [Removing a worktree](worktrees.md#removing-a-worktree).

## Shared services

A service that every worktree can use one copy of, such as a mail catcher, doesn't go in your Compose file. It lives in a Compose file of its own, which runs once per repository. That file gets none of the per-worktree variables above (not the slug, host, worktree home, URLs or worktree ID). It gets only `TREERIG_PROJECT`, `TREERIG_REPOSITORY_ID`, its own `COMPOSE_PROJECT_NAME` (`treerig-shared-…`) and the rest of your environment, apart from other `TREERIG_*` names and the `COMPOSE_*` variables treerig drops; `TREERIG_HOME` and `TREERIG_CONTROL_HOME` pass through. Once it's listed in `used-by`, your services reach it by its name on their network (`mailpit:1025`). See [Shared services](shared-services.md).

## A complete example

`compose.yaml`:

```yaml
services:
  web:
    build: .
    command: npm run dev -- --host 0.0.0.0 --port 5173
    expose:
      - "5173"
    environment:
      PUBLIC_URL: "${TREERIG_URL:?no service is routed at the host root}"
      VITE_API_URL: "${TREERIG_SERVICE_URL_API}"   # read by the browser
    depends_on:
      - api
  api:
    build: ./api
    expose:
      - "8080"
    environment:
      CORS_ORIGIN: "${TREERIG_URL}"
      CACHE_PREFIX: "${COMPOSE_PROJECT_NAME}:"
      DATABASE_URL: postgres://app:app@db:5432/app
    volumes:
      - uploads:/data/uploads
    depends_on:
      - db
  db:
    image: postgres:17-alpine
    environment:
      POSTGRES_USER: app
      POSTGRES_PASSWORD: app
    volumes:
      - pgdata:/var/lib/postgresql/data

volumes:
  pgdata:
  uploads:
```

`treerig.toml`:

```toml
[project]
name   = "acme"
domain = "acme.localhost"

[compose]
file = "compose.yaml"

[service.web]                 # routes at the worktree host root; port 5173 comes from expose

[service.api]
subdomain = "api"             # api.<slug>.acme.localhost; port 8080 comes from expose
```

With this, `treerig up` in a worktree whose slug is `feature-login` serves `http://feature-login.acme.localhost` (web) and `http://api.feature-login.acme.localhost` (api), and `web` gets each of them as its `PUBLIC_URL` and `VITE_API_URL`, for the browser. `db` runs, unrouted, with its data in `pgdata`; `worktree remove` deletes the `pgdata` and `uploads` volumes. If a router port other than 80 is in use, the URLs include it.

Run `treerig config validate` to check the TOML, and `treerig up` to start it.
