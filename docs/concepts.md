# How treerig works

The ideas behind treerig: identity, slugs, locks, homes, hooks and backups. Ports and the router are in [routing.md](routing.md); shared services are in [shared-services.md](shared-services.md).

```
treerig.toml ──reads──▶  treerig  ──spawns──▶  your hooks (any language)
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
[`worktree rename`](worktrees.md#renaming-a-worktree), never a side effect.

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
[`hooks.md`](hooks.md)). A waiting command, after a couple of seconds
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
started and left running, before treating the lock as recovered. `treerig
worktree unlock <slug|uuid>` runs that same check on demand, without waiting
to acquire the lock itself, so a lock nobody is actively waiting on can still
be cleared right away; `--registry` and `--router` point it at the registry
and router locks instead of a worktree's. [Lock recovery](lock-recovery.md) says
exactly what it checks and what it cannot see. `doctor` reports every lock
currently held, and every free lock whose record is still unreleased; an
unreleased record alone doesn't prove the holder is gone — the next command to
take that lock checks the recorded holder itself before treating it as
recovered. A treerig command that holds a lock or runs children catches
`SIGINT`, `SIGTERM` and `SIGHUP`, so closing the terminal lets it finish or
stop cleanly, and release its lock, rather than dying at once; a read-only
command just stops.

**Homes.** Each worktree owns a directory under the control home
(`~/.treerig/control-home` by default) created owner-only, with path containment
that refuses symlinks anywhere in the chain before creating or removing
anything. The same goes for the shared router's own state under the treerig
root (`~/.treerig/router`), with one exception: the two directories the router
stack bind-mounts, `router/dynamic` and `router/index`, are 0755, because the
containers that read them run as root without the capability that lets root
bypass file permissions, and on Linux Docker Engine that leaves a 0700
directory unreadable to them. Their parents stay owner-only — a bind mount is
entered at its mount point, so the parents are never traversed. Only the
index page and route files are written there; nothing secret.

**Hooks.** `prepare`, `seed`, `verify`, `backup`, and `restore` are
executables you point at from the config file. `up` invokes `prepare` and
`seed`: `seed` never runs before `prepare`, and not at all if a configured
`prepare` exited non-zero. `doctor` invokes `verify`. `treerig backup [dir]` and
`treerig restore <dir>` invoke `backup` and `restore` with a bundle directory as
their first argument: [`hooks.md`](hooks.md#backup-and-restore) has its
contract. `doctor` also checks that all five hook files can start. The tool
never learns what any of them do. A hook runs without a time limit unless
`[hooks.timeout]` gives it one: past its limit treerig stops the hook and its
whole process group on the host, and reports it as timed out (exit 124), not
failed. (Work it handed to a container, such as a `docker compose exec`, keeps
running there: see [`hooks.md`](hooks.md#timeouts).)

**Backup and restore.** A bundle is a directory the `backup` hook fills and
the `restore` hook reads; treerig creates it (or takes an empty one), marks it
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
`treerig backup ~/backups/before-upgrade` then `treerig restore
~/backups/before-upgrade`. With no directory, `backup` writes under the
worktree's home, which `worktree remove` deletes (its confirmation counts the
bundles); give it a directory to keep one. A restore overwrites the
worktree's live data.
