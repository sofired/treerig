# State words

What `status`, `worktree list`, `index`, the overview page and their `--json`
documents say about a worktree and its services. One set of words is used
everywhere: it is computed in one place, from the same inputs, so no command
can disagree with another.

## A worktree

A worktree is in exactly one of these states. The first that applies wins, in
the order of the table.

| Text | JSON | Means |
|---|---|---|
| `removing` | `removing` | `worktree remove` started and did not finish; run it again to finish. |
| `directory missing` | `directory_missing` | Git, asked successfully, no longer has the worktree's directory ([below](#directory-missing)). |
| `unknown` | `unknown` | Docker or Git didn't answer, a record could not be read, or a service is in a state treerig has no word for (or exited without a code), so the state cannot be told. A hint names the cause. |
| `starting` | `starting` | A service is running and its health check has not passed yet. |
| `failed` | `failed` | No service runs, and at least one exited with an error, is restarting or is unhealthy. |
| `partly running` | `partly_running` | Some service runs and at least one other does not: it exited with an error, is restarting, is unhealthy, is stopped, or was never created. After `up --services …` the services left out are `not created`, so the worktree reads `partly running` until a plain `up`. |
| `running` | `running` | At least one service runs, and every other one is running too or has finished. |
| `stopped` | `stopped` | An `up` has finished before (the registration is complete), and no service runs now: after `down`, after `docker compose stop` or a reboot, or when every service exited cleanly. Data is kept. |
| `not started` | `not_started` | Registered, and no `up` has ever finished. |

Containers decide over history. A first `up` that fails with one service
running is `partly running`, and one whose only service crashed is `failed`. A
service that exited with code 0, such as a one-shot `migrate` or `seed`, has
done its job: it counts as neither running nor failed, so `web` running beside
a finished `migrate` is `running`, and a worktree whose services all exited
cleanly is `stopped`.

History is what the registration records: `stopped` and `not started` both mean
nothing runs, and are told apart by whether an `up` ever finished (the
registration is complete, or still pending), not by whether containers exist,
because `down` removes them. A removal that got as far as destroying data makes
the registration pending again, so that the next `up` seeds the worktree
afresh; it shows `removing` until it is finished (or until an `up` calls it
off).

Every combination of service states maps to exactly one of these, which a test
enumerates.

### Directory missing

`directory missing` is shown only on positive evidence. All of these hold:

- Git ran against a reachable repository and lists the worktree as prunable and
  not locked;
- the directory itself does not exist;
- its parent directory exists, is not empty, and is not below an automount
  placeholder in the machine's mount table (`/proc/self/mountinfo` on Linux,
  `getfsstat(2)`, which `getmntinfo(3)` wraps, on macOS), read once per run.

An unmounted volume's mount point is an ordinary empty directory, so an empty
or missing parent is taken as a sign that the volume may simply not be mounted.
Everything short of the evidence above is `unknown`, with a hint that names the
cause:

| Hint | When |
|---|---|
| `Its parent directory is missing or empty: is the volume mounted?` | the parent is missing, empty or below an automount placeholder |
| `Git's records for it are damaged: 'git worktree repair' may fix them.` | Git calls the worktree prunable but its directory exists |
| `Git has it locked, so its directory may be on a drive that is not connected: connect it, or run 'git worktree unlock'.` | the directory is absent and the worktree is locked |
| `treerig couldn't check it: <the error>.` | a permission error, Git or the mount table failing (a mount table with a line treerig can't read counts as failing) |

Git can't tell a directory moved without `git worktree move` from a deleted one:
a moved worktree reads `directory missing` too, and `git worktree repair`
points Git at the new place.

A worktree Git no longer lists at all (after `git worktree remove` or `git
worktree prune`) is judged the same way, from the directory the last `up`
recorded for it. With no directory recorded it is `unknown`: `Git does not list
it and no directory was recorded for it: run 'treerig worktree remove <slug>' if
it is gone.` It is also `unknown`, not `directory missing`, while Git lists any
worktree whose identity could not be read, that could not be examined, or that
treerig has no identity for somewhere else: that one might be this worktree,
moved. The result records whether the verdict came from Git's entry for the
worktree's own identity or from the recorded directory alone.

Only the repository a command runs in is asked: another repository's worktrees
are listed with the state their containers and history give.

## A service

A service (one container of it) is one of these. `status` prints the text
word; `status --json` prints the JSON value, and `exitCode` beside `exited`.

| Text | JSON | Means |
|---|---|---|
| `running` | `running` | Running, and healthy where it has a health check. |
| `starting` | `starting` | Running, with its health check still pending. |
| `unhealthy` | `unhealthy` | Running, and failing its health check. |
| `restarting` | `restarting` | Docker keeps restarting it. |
| `exited (code N)` | `exited` | It ran and ended with code N. Exit 137 counts as a crash only when Docker reports the kernel's out-of-memory killer did it. |
| `stopped` | `stopped` | A container that exists and does not run, and did not crash: created and never started, paused, dead, being removed, or ended by `SIGKILL` or `SIGTERM` (exit 137 or 143, which `docker compose stop`, a reboot or a daemon restart leave behind). |
| `not created` | `not_created` | Declared, and no container at all, as after `down`. |
| `unknown` | `unknown` | A state treerig has no word for; `detail` is Docker's own. |

A scaled service has a container per replica. `status` shows it as its worst
replica; the worktree's state counts every replica.

Every command asks Docker about each container that exited 137, to tell a stop
from an out-of-memory kill: `status` through `docker compose ps`,
`worktree list` and `index` through `docker ps`, followed by one `docker
inspect` of each such container (concurrently, under one shared deadline). Exit
137 is `stopped` when Docker says the kernel did not do it, `exited (code 137)`
when it did, and `unknown` (`Whether <service> was stopped or killed for running
out of memory (it exited 137) couldn't be read.`) when the inspect failed.
Exit 143 is always `stopped`.

The list and the overview also know only the services the last `up` recorded in
the worktree's index card, which holds the routed ones; `status` asks Compose for
every declared service. A service that is not routed and was never started
therefore makes `status` say `partly running` where `worktree list` says
`running`. Recording every declared service in the card is a follow-up (#204).

## Where each command shows them

- **`status`** prints the worktree's state beside its host, then each service.
  With Docker not answering the worktree is `unknown`, the table is still
  printed, and the command exits 1. If Compose, not Docker, cannot read the
  worktree (a bad Compose file, a Compose that is too old), the hint says
  `Compose couldn't read this worktree: …` instead. `status --json` carries
  `state` (and `hint` for an unknown one) and each service's `state`. Shared
  services use the service words.
- **`worktree list`** has a STATE column, and `--json` a `state` and, for an
  unknown one, a `hint` (on stderr in the text form, unless it is the one cause
  already given once). It asks Docker once per label namespace, with a budget of
  about five seconds each, and Git once for the current repository. With Docker
  not answering every worktree is `unknown`, the table is still printed, and
  the command exits 1 with the cause once.
- **`index`** and the page at `http://treerig.localhost` show the state as a
  pill, and `index --json` has it as `entries[].status` (the key predates the
  words). `index` makes the same Docker budget and exits 1 the same way when
  Docker does not answer; the page it wrote says so.
- **`doctor`** warns about a registration Git no longer lists, as `directory
  missing`, or as unknown, with the cause.

Their machine-readable forms are in [Machine-readable output](json-output.md).

## Older builds

Earlier builds used three vocabularies. They map onto these words as follows;
the JSON values changed with them ([Machine-readable output](json-output.md)).

| Was | Where | Is now |
|---|---|---|
| `ready` | `worktree list` | `running`, `stopped`, `partly running`, `failed` or `starting`, by what the containers say |
| `pending` | `worktree list` | `not started` (or `partly running`, `failed`, … when containers exist) |
| `missing` | `worktree list`, the overview | `directory missing`, or `unknown` when treerig could not be sure |
| `absent` | `status`, shared services | `not created` |
| `running` with `detail` `unhealthy` | `status`, shared services | `unhealthy` |
| `starting` for a restarting container | `status`, shared services | `restarting` |
| `stopped` for an exited container | `status`, shared services | `exited (code N)`, or `stopped` after a stop |
| `Running`, `Partial`, `Stopped`, `Unknown` | the overview | `running`, `partly running`, `stopped` or `not started`, `unknown` |

A removal run by a 0.1.x pinned teardown binary does not write the `removing`
marker, so an interrupted one shows `not started` or `stopped`. A leftover
marker that such a binary's removal did not delete is harmless: a registration
that is gone is not listed, and an `up` clears the marker.
