# Switching from a build before the rename

Cleanup steps for a machine that ran a build from before the rename.

treerig reads none of what a build from before the rename left behind, and will
not touch it, because it cannot prove that it owns it. That covers the
containers, networks and volumes of every worktree the old build started (they
carry its own label namespace, not `dev.treerig`), its shared services, its
router, its root directory (the registry, the router state and the control
home) and the identity markers it wrote into each repository's `.git`
directory. The old config file and environment variables are not read either.
If you have already installed treerig, keep the old binary until the cleanup is
done. Run every command below with the old build:

1. **Worktrees.** In each worktree, run `worktree remove`; `worktree remove
   <slug|uuid>` also removes one whose checkout is already gone. It runs the
   compose down itself and deletes the worktree's containers, data directory
   and markers, so first run the old build's `backup <dir>` for anything you
   need, to a directory outside the worktree and the root directory (with no
   directory, `backup` writes where `worktree remove` deletes). A `worktree
   remove` that refuses must be resolved, not skipped: whatever it leaves
   behind is orphaned once the root directory is gone.
2. **Shared services.** In each repository that uses them, run `shared down`.
   Plain `shared down` keeps the volumes, and they are orphaned once the root
   directory is gone; for a full cleanup, run `shared down --volumes`, which
   asks first, then deletes their data.
3. **Router.** Run `router down` once. It stops the single router for the
   whole Docker daemon, so worktrees from any other root keep running but are
   unrouted until their next `up`. Do this before the first treerig `up`,
   because the old router can still hold the ports.
4. **Root directory.** Once every removal above has succeeded, delete the
   hidden directory the old build created in your home directory, or the one
   its home variable pointed to. Check that it holds only old-build state
   first. Deleting it earlier orphans whatever was left, and nothing could then
   prove it owned it. Step 1 normally removes the identity markers; if any
   are left, they are a directory named after the old build inside a
   repository's Git directory and its common Git directory, and can go too.
5. **Config and scripts.** Rename each checkout's config file to
   `treerig.toml`, and change any scripts or hooks that read the old
   environment variables to read the `TREERIG_*` ones.

If the old binary is already gone, its resources still carry the old build's
label namespace (`dev.` followed by the old name), so you can list them with
`docker ps -a`, `docker network ls` and `docker volume ls`, each with
`--filter label=<that namespace>.managed`. They cannot be proven owned without
the old build's registry, so check each one before you remove it: inspect its
labels (the managed label and the repository and worktree UUIDs) and make sure
nothing still uses it.

Run this once per machine. Nothing is migrated, because nothing had shipped
when the name changed; from the first tag (v0.1.0-rc.1), a change to a storage
name comes with a migration (see [`CONTRIBUTING.md`](../CONTRIBUTING.md)).
