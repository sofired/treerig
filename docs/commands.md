# State words

What `status`, `list`, the overview page and their `--json`
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

`status` asks Git about the repository of the worktree it runs in (a worktree
of another repository is judged by its recorded directory alone). `list --all`,
`list` outside a repository, and the overview ask Git about every repository
that has a worktree: the one the command runs in, and any other through a
directory one of its worktrees last ran from. Each repository answers within
five seconds, and one that does not shows its worktrees as `unknown` with the
cause. A repository whose recorded directories are all gone is judged by those
directories alone.

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
`list` and the overview through `docker ps`, followed by one `docker
inspect` of each such container (concurrently, under one shared deadline). Exit
137 is `stopped` when Docker says the kernel did not do it, `exited (code 137)`
when it did, and `unknown` (`Whether <service> was stopped or killed for running
out of memory (it exited 137) couldn't be read.`) when the inspect failed.
Exit 143 is always `stopped`.

`list` and the overview also know only the services the last `up` recorded in
the worktree's index card, which holds the routed ones; `status` asks Compose for
every declared service. A service that is not routed and was never started
therefore makes `status` say `partly running` where `list` says `running`.
Recording every declared service in the card needs a new version of the card
(see [stored state](design/state.md)), and is tracked in #227.

## A host route

A [host route](host-routes.md) has no container, so it has two words of its own,
which `status` prints in the STATE column and `status --json` spells with an
underscore:

| Text | JSON | Means |
|---|---|---|
| `listening` | `listening` | A connection to its port on `127.0.0.1` succeeded. Something listens there, not necessarily the process this route was made for. |
| `not listening` | `not_listening` | None did within about a second. Nothing is wrong with treerig: the dev server is not running. |

The ports of all of a worktree's host routes are tried together, at most eight
at a time, within that one second.

## Where each command shows them

- **`status [worktree]`** prints the worktree's state beside its host, then a
  table of SERVICE, STATE and URL:

  ```text
  feature-x.myapp.localhost   partly running
    SERVICE           STATE            URL
    web               running          http://feature-x.myapp.localhost
    worker            exited (code 3)  -
    app (host route)  listening        http://app.feature-x.myapp.localhost
    mail (shared)     running          http://mail.shared.myapp.localhost
  ```

  A name carries its kind: `(host route)` for a route to a process on your
  machine (the words above) and `(shared)` for one of the repository's shared
  services, whose state uses the service words. A shared service this worktree
  uses but the shared container is not attached to reads `running (not
  connected)`. For a connection problem run `treerig doctor`, and for
  addresses, the definition and who started it run `treerig shared status`
  (`status --json` has them too).

  A URL is shown only when its service is running (a host route: listening)
  **and** the router's container is up, started by this treerig root and
  serving the port the URLs name; otherwise the column says `-`. Nothing is
  fetched over HTTP, so a URL shown is a URL that should load, not one that was
  checked. Stderr says why when something is routed and the router is the
  reason:

  | The router is | stderr |
  |---|---|
  | stopped | `The router is stopped, so no URL loads. Run 'treerig up' to start it.` |
  | on another port | `The router runs on port N, not M. Run 'treerig router up'.` |
  | another treerig root's | `The router belongs to a different treerig root, so no URL is shown. Run 'treerig router down', then 'treerig up'.` |
  | unreadable | `The router's state couldn't be read, so no URL is shown. Run 'treerig doctor' to see why.` |

  With Docker not answering the worktree is `unknown`, the header is still
  printed, and the command exits 1 with the cause on stderr. If Compose, not
  Docker, cannot read the worktree (a bad Compose file, a Compose that is too
  old), the hint says `Compose couldn't read this worktree: …` instead.
  `status --json` carries `state` (and `hint` for an unknown one), `router`,
  `dockerError`, and each service's `state` and `url`. Shared services use the
  service words.

  `status` exits 0 whatever the state, and 1 when it couldn't read the
  worktree: Docker didn't answer, Compose couldn't read it, its treerig.toml
  wouldn't load, or its Compose file is too large or not a regular file. Every
  `status` asks Compose for the services with `docker compose config
  --no-interpolate`, so no variable of the file is needed or expanded (if a
  Compose release refuses that, it asks once more without the flag). Name
  another worktree by its UUID, its host, its slug or a path, which may point
  inside its directory. For one of another repository, `status` reads that
  repository's `treerig.toml` and Compose file from the directory the worktree
  was last seen in, and takes the worktree's label namespace from its
  registration. The directory is judged as the listing judges it: gone only
  when it is absent and its parent is a real, non-empty directory, otherwise
  `unknown` with the cause. A directory that is missing, undetermined, now in
  another repository, or never recorded gets its header (`directory missing` or
  `unknown`), a line on stderr that ends with `Directory: <path>` (or, for a
  worktree no `up` has recorded a directory for, says that treerig has no
  record of where it lives), and nothing more, exit 0. Neither file is read if
  it is not a regular file or is larger than 1 MiB. In a worktree that is not
  registered, `status` says so on stderr, prints nothing on stdout and exits 0.
- **`list`** (`ls`) prints a table of the repository's worktrees, and with
  `--all`, or outside a repository, of every registered worktree on the
  machine, with a REPOSITORY column first. `*` marks the one you are in:

  ```text
     WORKTREE   STATE              URL                          DIRECTORY
  *  main       running            http://main.myapp.localhost  /home/sam/src/myapp
     feature-x  stopped            -                            /home/sam/src/myapp.feature-x
     old-spike  directory missing  -                            /home/sam/src/myapp.old-spike
  ```

  The URL column follows the rule of `status`: the first routed service that
  runs, with `+2 more` when others do, and `-` when none does, because the
  worktree is stopped or the router is not serving this root's URLs (stderr says
  why, as `status` does). Only the services the last `up` routed are known
  (above); a host route is not asked about. A directory is absolute, `-` when
  nothing recorded one. `--json` has a `state` and, for an unknown one, a
  `hint` (on stderr in the text form, unless it is the one cause already given
  once), and every URL.

  `list` asks Docker once per label namespace, with a budget of about five
  seconds each, and Git once per repository, eight at a time and within five
  seconds together. With Docker not answering every worktree is `unknown`, the
  table is still printed, and the command exits 1 with the cause once. On a
  terminal, stderr ends with how many directories are gone and, inside a
  repository, with the overview's address (`Every repository: 'treerig list
  --all', or http://treerig.localhost`) when the router serves this root's URLs;
  with `--all` it does not, since it already lists everything. Neither line is
  printed into a pipe or with `--json`. `list --all --refresh` (or `list
  --refresh` outside a repository) rebuilds the [overview](overview.md) page
  under the router's lock; nothing else about `list` takes a lock or writes. It
  needs no `treerig.toml`.
- **The overview** at `http://treerig.localhost` shows the state as a pill, and
  `list --all --json` has it as `entries[].state`. The page it writes says so
  when Docker does not answer.
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
