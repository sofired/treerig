# Getting started

Write a `treerig.toml`, bring a worktree up, and see what `up` creates.

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

Then:

```bash
treerig config validate     # parse and check the file
treerig config schema       # print its JSON Schema, for editors
treerig up                   # register this worktree, start its services, run prepare (+ seed the first time)
treerig dev                  # the same, then follow its logs until Ctrl-C
treerig status               # per-service state
treerig urls                 # print its routed URLs
treerig route add web --port 5173 --subdomain app   # route a dev server on the host at app.<slug>.<domain>
treerig route ls             # this worktree's host routes
treerig run plan             # run one of the project's own [commands], on the terminal
treerig backup [dir]         # run the backup hook, writing a bundle into dir (default: under the worktree's home)
treerig restore <dir>        # run the restore hook on a bundle directory
treerig down                 # stop it; data is kept
treerig shared up            # start the shared services, or replace one, and reconnect every worktree
treerig shared status        # their state, addresses and whether this worktree is connected
treerig shared down          # stop them; their named volumes are kept
treerig worktree list        # every worktree of this repository, with state
treerig worktree remove      # tear it down, verified by ownership
treerig worktree rename NEW  # change its slug, and with it its hostname
treerig worktree unlock      # clear a dead holder's lock, once it is checked gone
treerig doctor               # check prerequisites, registry, router; run verify
```

`up` runs the branch's own build and hooks. Read [Trust model](trust-model.md)
before bringing up a branch you did not write.

`up` starts the Compose project under a name derived from the worktree's
UUIDs, with every container, network, and image it builds, and every named
volume the Compose file declares, labelled
`dev.treerig.managed`, `dev.treerig.repository-uuid`, and
`dev.treerig.worktree-uuid` (an external volume is left alone, and the anonymous
volume an image declares gets none: see [How treerig works](concepts.md)), and, on everything but the volumes, the
`dev.treerig.format` label that says which format of labels those are (a named
volume carries none: Compose would offer to recreate a volume whose label
changed, losing its data). Your Compose file is never asked to add those
labels; a generated override file does it, so forgetting one — and silently
exempting a resource from cleanup — is not possible. The Compose file sees
the same `TREERIG_*` variables hooks do, so `${TREERIG_WORKTREE_HOME}` works in a
bind mount exactly as it does in a seed script.

A full annotated example is in [`treerig.example.toml`](../treerig.example.toml).
The hook contract is specified in [the hook contract](hooks.md).
