# Lock recovery

How `worktree unlock` decides that a dead holder's lock is safe to clear.

The check below runs on its own whenever a command takes a free lock whose
previous holder's record is unreleased, before that command treats the lock as
recovered: an unreleased record alone does not prove the holder is gone.
`worktree unlock` runs the same check on demand, so a lock nobody is waiting on
can be cleared right away.

`worktree unlock [slug|uuid]` clears a worktree's locks (its operation lock,
and its development-session lock if that is held too) only when all of this
holds — checked automatically, without ever waiting to acquire the lock
itself:

- the lock itself is free: `flock(2)` proves this instantly, so a lock
  actually held by a live process is always refused straight away, before
  anything else is checked;
- the record its last holder left names a process this machine can
  identify: by its machine ID, or by its boot ID in a container image that
  has none, in this boot and this PID namespace;
- that holder's PID no longer exists, or now belongs to a later process (told
  apart by its start time), so a reused PID is never mistaken for the same
  one — checked twice, a second
  look about a second later, in case a holder that looked alive was simply
  finishing;
- no command started for the worktree is still running on this machine:
  nothing with its Compose project on the command line, or its
  `TREERIG_WORKTREE_ID` in the environment — checked the same way, twice.

A stopped holder (`Ctrl-Z`) is alive, and is refused: resume it with
`kill -CONT <pid>` and let it finish, or end it. A holder it cannot check —
on another machine or in a container, one that left no record, or one it
cannot look past because other users' processes are hidden from you (`/proc`
mounted with `hidepid`, say) — is refused too, unless you pass
`--holder-unverifiable` once you have made sure yourself; that never
overrides a running holder or a command left running. `--registry` and
`--router` point the same check at the machine-wide registry or router lock
instead of a worktree's, and take no worktree identifier; `--shared` points it
at the shared-services lock of the repository the worktree belongs to, and
looks for a `compose up` the dead holder left running by the shared Compose
project's name. The unlock command
that a lock notice or `doctor` prints names the worktree by its UUID: it
never changes, no other worktree shares it, and it works even for a
worktree whose first `up` died before it registered.

It reads `/proc` on Linux and asks `ps` on macOS. Your own processes are
checked by their command line and their environment; other users' only by
their command line, since their environments are private to them. So it cannot
see a command that left this machine's process table, or one that no longer
carries the worktree's markers where it can look: a command a hook ran inside
a container with `docker exec`, or a daemon handed to a service manager. A
helper a hook ran as another user — through `sudo`, which clears the
environment, or `sudo -E` — is missed too, unless its command line names the
worktree's Compose project. The worktree's own containers are not commands: they are what
`up` built, running or stopped as the holder left them, and the next `up` or
`down` reconciles them. What it did check, it prints, as a `Verified:` line
per lock cleared — or `Not verified, on your word (--holder-unverifiable):`
for whichever it accepted unchecked.
