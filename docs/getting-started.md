# Getting started

Write a `treerig.toml`, bring a worktree up, and see what `up` creates. The
[Glossary](glossary.md) defines the terms used here and on the other pages.
Writing the Compose file first? [Writing your compose.yaml for treerig](compose.md)
says how to expose a port and which variables it can use.

Not installed yet? `brew install sofired/tap/treerig`, or see [Install](install.md).

## Write a `treerig.toml`

The tool is tested with Docker Compose 2.21 and newer (`treerig doctor` checks,
as does `docker compose version`). From the root of your repository,
`treerig init` writes a starter `treerig.toml` there (the file is commented, names
your Compose file, and opens with the schema directive described under
[Editor support](configuration.md#editor-support)); it refuses to replace one that exists.
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
backup  = "./scripts/backup.sh"    # treerig backup: writes a bundle into the directory it is given
restore = "./scripts/restore.sh"   # treerig restore: loads one back

[hooks.timeout]                    # optional: no entry, no limit
prepare = "30m"                    # a hung migration fails `up` after half an hour
verify  = "2m"                     # and a hung check fails `doctor` after two minutes

[commands]
plan = "./scripts/keycloak.sh plan"   # on demand, with `treerig run plan`
```

## Bring a worktree up

With the file in place, these are the commands you will use:

```bash
treerig config validate     # parse and check the file
treerig config schema       # print its JSON Schema, for editors
treerig up                   # register this worktree, start its services, run prepare (+ seed the first time)
treerig dev                  # the same, then follow its logs until Ctrl-C
treerig status               # this worktree's state, each service's, and its URLs while its services run
treerig status feature-x     # another worktree's, by slug, host, UUID or path
treerig route add web --port 5173 --subdomain app   # route a dev server on the host at app.<slug>.<domain>
treerig route ls             # this worktree's host routes
treerig run plan             # run one of the project's own [commands], on the terminal
treerig backup [dir]         # run the backup hook, writing a bundle into dir (default: under the worktree's home)
treerig restore <dir>        # run the restore hook on a bundle directory
treerig down                 # stop it; data is kept
treerig shared up            # start the shared services, or replace one, and reconnect every worktree
treerig shared status        # their state, addresses and whether this worktree is connected
treerig shared down          # stop them; their named volumes are kept
treerig worktree list        # this repository's worktrees, with state
treerig worktree remove      # tear it down, verified by ownership
treerig worktree rename NEW  # change its slug, and with it its hostname
treerig worktree unlock      # clear a dead holder's lock, once it is checked gone
treerig doctor               # check prerequisites, registry, router; run verify
```

A release build (Homebrew, the install script, a package or an archive) removes
with itself, so there is nothing to set up. A development build (one built from
source, which may carry a branch's code) removes only with a pinned teardown
binary: `treerig teardown pin --latest` pins the newest release. See
[Teardown pin](teardown-pin.md).

`up` runs the branch's own build and hooks. Read [Trust model](trust-model.md)
before bringing up a branch you did not write.

## What `up` creates

`up` starts the Compose project under a name derived from the worktree's UUIDs
and labels the containers, networks, images and named volumes it creates for
the worktree with its [ownership labels](concepts.md#ownership-labels). The
anonymous volumes an image declares get none, so `worktree remove` leaves them.
Your Compose file is never asked to add them: a generated override file does
it, and also publishes each routed service's pinned port. The Compose file sees
the variables listed in
[Writing your compose.yaml](compose.md#variables-you-can-use), so
`${TREERIG_WORKTREE_HOME}` works in a bind mount exactly as it does in a seed
script. See [Environment variables](environment.md).

## Next steps

- [`treerig.example.toml`](../treerig.example.toml) is a full annotated example,
  and [Configuration](configuration.md) lists every key.
- [Writing your compose.yaml for treerig](compose.md) covers the variables your
  Compose file can use, ports, and what treerig refuses.
- [The hook contract](hooks.md) specifies `prepare`, `seed`, `verify`, `backup`
  and `restore`.
- [Routing](routing.md) explains pinned ports and the shared router;
  [Shared services](shared-services.md) and [Host routes](host-routes.md) cover
  a mail catcher for the whole repository and a dev server on the host.
- [Worktrees](worktrees.md) covers development sessions, removing a worktree
  and renaming one.
- Something not working? See [Troubleshooting](troubleshooting.md).
