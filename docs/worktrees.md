# Worktrees

Development sessions, removing a worktree, and renaming one.

## Development sessions

`dev` is `up` followed by `docker compose logs --follow` on the worktree's
project: the last 100 lines of each container, then new ones. It is `up` all
the way through (the same checks, router, shared services, hooks and URL
table), and if `up` fails, `dev` fails the same way, with the same exit code,
and follows nothing. What it adds is a session: while it is attached,
`worktree remove` and a second `dev` refuse, naming the session, and telling
you to press Ctrl-C in the terminal running it. `up`, `down`, `run` and the
other verbs still work meanwhile.

The session lasts until you end it. What `treerig down` from another terminal
does to it is up to Compose's follower, which varies: it may end once the
containers are gone, and then so does the session, with a message; or it may
keep waiting for new containers (Compose 5.5.0 has done so after a
`restart: always` service restarted), and then `dev` says once that no
container is left and idles, and a later `treerig up` may show the new
containers' logs. An idle session still refuses `worktree remove` until
Ctrl-C. The services keep running when it ends; `treerig down` stops them.

Ctrl-C, SIGTERM and SIGHUP stop the log follower's whole process group and
release the lock, print `The development session of <host> ended (<signal>).
Its services are still running; "treerig down" stops them.` as a notice rather
than an error (`<signal>` names the one that ended it, SIGINT for Ctrl-C; the
second sentence is left out when no container is left), and exit with the
status a shell reports for that signal: 130 for Ctrl-C, 143 for SIGTERM, 129
for SIGHUP (see
[Output and exit codes](output-and-exit-codes.md)). A follower that ends on its
own, because Docker went away or every container is gone, ends the session
with a message on stderr and its own exit code (1 when it exited 0). A
`kill -9` of treerig itself frees the lock with the process but leaves the log
follower running on its own; end it with `kill`.

## Running without a treerig.toml

`worktree list`, `worktree unlock` and `worktree remove <slug|uuid>` work from
labels and records alone, so they need no `treerig.toml`, and you can run them
from any directory, inside a repository or not. So does the completion of
worktree names. They still take `--config <path>` and use that file when it
parses; a `--config` that names a file that is not there is an error, as for
every other command.

- **`worktree list`** lists this repository's worktrees when you run it inside
  a Git repository, whether or not it has a usable `treerig.toml`. When treerig
  knows none there, it says `No worktrees are registered for this repository.`
  and, on stderr, `Every repository: 'treerig worktree list --all'`. Outside
  every Git work tree there is no "this repository", so it lists every
  registered worktree on the machine, exactly as `--all` does; an empty
  registry then says `No worktrees are registered on this machine.` Outside a
  repository the table has no live Git state to read, so BRANCH and ROOT show
  what `up` last recorded, or `-`.
- **`worktree unlock <slug|uuid>`** finds the worktree by slug or UUID on the
  whole machine. With no name it unlocks the worktree you are in, and outside
  a Git worktree it says so and asks for a name.
- **`worktree remove <slug|uuid>`** finds the worktree the same way and takes
  the Docker label namespace of its resources from its registration, then from
  its index card, and never from the resources it is about to delete. When
  neither records it, and no `treerig.toml` can say, it stops with nothing
  removed and tells you to run `treerig worktree remove <slug>` from inside the
  repository that created the worktree, or to pass `--label-namespace`. With no
  name it removes the worktree you are in; outside a Git worktree it says so
  and asks for a name. Run from outside the repository, it leaves the
  worktree's identity marker in its directory, which `worktree remove` run from
  inside the repository would remove.
- **A `treerig.toml` that does not parse** is set aside with a warning
  (`Ignoring treerig.toml for this command: <path> is not valid TOML: …`)
  rather than stopping these commands: removing a worktree whose branch broke
  its config, or clearing its lock, must still work. The commands that start or
  inspect a project (`up`, `status`, `config validate` and the rest) still
  refuse it.

On a development build with no pin, the pin check comes before the check for
a worktree to remove, so a bare `worktree remove` outside a repository asks for
a pin first.

A pinned teardown binary from before this change (0.1.x) still reads a
`treerig.toml` first, so a removal through such a pin from a directory without
one stops with `No treerig.toml found`. When the pin is a release older than the
build you run, treerig says before it runs the pin that it may need one: run
the removal from inside the worktree's repository, or pass `--config <that
repository's treerig.toml>` (see [Teardown pin](teardown-pin.md)).

## Removing a worktree

`worktree remove` is the only destructive verb. It inventories everything
the worktree owns, verifies the full [ownership tuple](concepts.md#ownership-labels) on every resource, and
refuses the whole operation if a single one fails — before it asks you to
type the hostname to confirm (`--yes` skips the prompt). Then, in an order
chosen so that an interrupted removal can simply be run again: Docker
resources, the home directory, the registration, the identity marker.
`treerig worktree remove <slug|uuid>` removes another worktree, including one
whose directory is already gone, and needs no `treerig.toml`
([below](#running-without-a-treerigtoml)); `--label-namespace` supplies the
Docker label namespace of its resources when nothing on disk records it.

A SIGINT, SIGTERM or SIGHUP that arrives once the removal has started
destroying things leaves the worktree `pending`, and treerig says so on stderr
before it dies by the signal: `Interrupted (<signal>) while removing <host>.
Removed so far (at least): <what>. It is still registered, now pending:
'treerig worktree remove' finishes the removal, 'treerig up' starts it again and
runs the seed hook again.` The counts are a lower bound (the resource being
removed when the signal came is not counted), and "nothing confirmed yet"
stands when none is known to be gone. The command names the target as you
typed it (`'treerig worktree remove <slug|uuid> --domain <domain>'`) and leaves out
`up` when it is another worktree's removal that was interrupted. A signal
during the earlier `compose down` destroys nothing and prints no such notice,
and neither does one that also left a lock unreleased: that is reported as
an error, and [Lock recovery](lock-recovery.md) applies. `doctor` deliberately
does not flag a `pending` worktree: a new worktree is `pending` until its
first `up` finishes, and so is one whose first `up` failed, and nothing in
the registration tells either from an interrupted removal, so a check could
only warn about the ordinary case. `worktree list` shows it.

Before `worktree remove` does anything else, it decides which code does the removal.

- **A pin wins, for every build.** If you pinned a teardown binary
  (`treerig teardown pin`, [Teardown pin](teardown-pin.md)), it checks the pinned
  file's sha256 against the pin, then replaces itself with that file, passing the
  same arguments and environment. The pinned binary lives in the control home,
  outside every checkout, and the control home is refused if it is inside a git
  work tree or if its pin store is owned by someone else or writable by others.
  The exec drops the `LD_*`, `DYLD_*`, `GODEBUG` and `GOFLAGS` variables from the
  environment, and prints two lines on stderr: one before the exec naming the file
  and the release, one from the pinned binary after it, naming its version. A pin
  that is broken (a pin it cannot read, a stored binary that is missing or whose
  bytes changed, no binary for your platform, a filesystem that cannot run
  programs) refuses, removes nothing, and says what to run; an exec that fails is
  never worked around by running the invoked build. The refusal also says when the
  build you invoked is newer than the pin, as `teardown status` does.
- **No pin, a release build:** it removes with itself. You have nothing to set up.
  It is first copied, as itself, into a content-addressed store under the control
  home (`bin/`, outside any checkout), and the removal is handed to the copy, so
  deleting the checkout cannot delete the running program.
- **No pin, a development build** (built from source, a snapshot, anything
  `0.0.0-…`): it refuses, because a development build may carry code from a
  branch. Pin a build ([Getting a pin](teardown-pin.md#getting-a-pin)) or use
  `--dev-teardown`.

`worktree remove --dev-teardown` removes with the build you invoked, whatever
its kind and whether or not a pin exists, for work on removal itself. It is a
flag and nothing else: no setting, no `treerig.toml` key and no environment
variable turns it on, and nothing remembers it. It prints a warning on every run
that names the version and the sha256 of the code that is about to run. That
build is first copied, as itself, into a content-addressed store under the
control home (`bin/`, outside any checkout), and the removal is handed to the
copy, so deleting the checkout cannot delete the running program either (an
unpinned release build does the same). The copy is exactly the invoked binary
and no more: a build made from a branch still runs that branch's code. Copies
are content-addressed, so a rebuild that changes nothing installs nothing new;
one that has not been installed or reused in the last 30 days, or that is not
among the 5 most recently used besides the current one, whichever rule catches
it first, is swept away automatically: disk hygiene for a workflow that rebuilds
often, not something to manage by hand. That count is a steady-state target, not
a hard ceiling: the sweep only runs as part of a later `--dev-teardown` removal,
so a burst of removals from distinct builds in quick succession can briefly
leave more than 5 until the next one catches up. A copy installed or reused in
the last 10 minutes is never swept away by either rule, however old or however
far past the count it would otherwise be: that window exists for another
removal, running at the same time, that might be mid-way through installing or
about to run the very copy this one's own sweep would otherwise be free to take.
A copy that cannot be installed or run, including on a control home whose
filesystem cannot execute programs at all, is reported on stderr as it happens,
and the removal continues with the invoked binary. The pinned binary cannot be
run with `--dev-teardown` (it refuses), and the pin itself is untouched.

## Renaming a worktree

`treerig worktree rename <new-slug>` renames the worktree you run it from: it
changes the slug it is registered under, so it routes as `<new-slug>.<domain>`
from then on. The slug must be a lowercase hostname label (letters, digits and
inner hyphens, at most 63 characters), must not be `shared`, and must not be
held by another worktree in the same domain, whichever repository it belongs
to. It is also how to reclaim a bare name that an earlier collision suffixed.

Everything keyed by the worktree's IDs stays exactly as it is: the Compose
project name, and with it every container, network and volume name, the home
directory, the locks and the pinned loopback ports. treerig renames and deletes
no volume. What carries the slug moves to the new one: the registration and
the worktree's index entry, and the checkout index is refreshed. Route files
are removed, not moved: a worktree with no containers has none that are live,
and `up` writes them at the new hostname. A Compose file that builds
`${TREERIG_SLUG}` or `${TREERIG_HOST}` into a volume name would name a new volume
on the next `up`; treerig cannot see inside the file, so the old volume is left
in place, and `docker volume ls --filter label=<namespace>.worktree-uuid=<id>`
(the namespace defaults to `dev.treerig`) lists every volume the worktree owns.
`worktree remove` removes the old one along with the rest.

A worktree with containers, running or stopped, is refused: they were created
with the old `TREERIG_SLUG` and `TREERIG_HOST`. Run `treerig down` first, which keeps
the data, then rename, then `treerig up` at the new hostname. The rename takes
the worktree's operation and development-session locks, so it cannot overlap
an `up`, a `down` or a `dev` session, and it cannot run from the worktree's
own hooks. If it is interrupted, the old slug stays registered until the
record is replaced, last; run the command again, with the same slug or a
different one. Hooks are not told of a rename; the next `up` runs them with
the new `TREERIG_SLUG` and `TREERIG_HOST`.
