# Output and exit codes

Which stream each command writes to, and the exit codes treerig uses.

The commands that produce data — `list`, `status`, `router status`,
`shared status`, `route ls`, `config show`, `config validate`, `doctor`, `teardown status` — write that data to stdout
and nothing else, so `treerig list > worktrees.txt` and
`treerig list --json | jq` get what they asked for. Progress, waiting
notices, confirmation prompts, warnings, failures and footers go to stderr.
Six of these (`list`, `status`, `shared status`, `route ls`, `doctor` and `teardown status`) take `--json`:
see [Machine-readable output](json-output.md).

`list` adds up to two lines to stderr only when stderr is a terminal, so that a
person sees them and a script that captures stderr does not: how many
directories are gone (`1 directory is gone: 'treerig worktree remove old-spike'
removes its worktree.`), and where the rest of the machine is. On a terminal,
inside a repository, `treerig list` ends with the page's address when the router
serves this root's URLs (`Every repository: 'treerig list --all', or
http://treerig.localhost`, with the router's port when it is not 80); with
`--all` it does not, since it already lists everything. `--json` prints neither.
`treerig worktree list` and `treerig index` are hidden stubs for one release:
each prints a pointer to `list` (`[treerig] ERROR: 'treerig index' is now
'treerig list --all'.`) on stderr, nothing on stdout, and exits 1.

`worktree rename` writes only its notice, to stderr, and nothing to stdout.

`teardown pin` writes only its notices to stderr, and nothing to stdout.
`teardown status` writes its report to stdout, and takes `--json` (see
[Teardown pin](teardown-pin.md)). Exit 0 means `worktree remove` would run: the
pin is intact, or there is none and this is a release build, which needs none.
Anything else exits 1 with its reason on stderr, no pin on a development build
included. The report's last line, `worktree remove: will run this binary`,
`worktree remove: would refuse` or `worktree remove: will run this build (a
release; no pin needed)`, is part of the text form only; the `--json` document
carries the same answer as `"removal"`: `"pin"`, `"this-build"` or `"refuse"`,
in every case. What would make removal refuse besides the pin's own integrity (a
control home inside a checkout, a pin store others can write, a `noexec` mount)
is a warning on stderr in both forms, and exit 1, so stdout stays the report or
the document.

Every command that runs `docker compose up` or `down` (`up`, `dev`, `down`,
`router up`, `router down`, `shared up`, `worktree remove` and `worktree
rename`) tells what happened in treerig's own words, on stderr, and `up` adds
its URL table on stdout. Docker Compose's own progress (`Container web
Started`) is not passed through: pass `-v` to see it, on stderr (see [Verbose
output](#verbose-output)). A failure still quotes the last 50 lines of what
Compose said. **Changed in 1.0:** the output of every hook (`prepare`, `seed`,
`backup`, `restore` and `verify`) goes to stderr whichever stream the hook wrote
it on, so `dir=$(treerig backup)` gets the path `backup` wrote and nothing else;
a script that read a hook's output from stdout adds `2>&1` to read it from the
same pipe, which also carries treerig's own notices and warnings (see
[Hooks](hooks.md#output-and-exit-codes)). `worktree remove` prints two lines
naming the pinned binary when a pin is used (the one it runs, and the pinned
binary saying it ran), and any refusal of the teardown check, on stderr, never
on stdout. `dev` is `up` and then the followed logs, which keep their streams: a
container's stdout on treerig's stdout, its stderr on treerig's stderr, and one
notice on stderr when following starts. `backup` writes the path of the bundle
it wrote to stdout and its closing line, and any warning, to stderr; `restore`
writes only to stderr. `shared down` names services in its progress lines; the
`--volumes` prompt still lists the containers and volumes it will remove, since
that list is the proof of what goes. `run` is the stricter case: the command it
runs writes straight to treerig's stdout and stderr, and treerig adds nothing
while it runs, so `treerig run plan > out` holds only what `plan` wrote.

`down` says `<host> is not running. Nothing to stop.` when the worktree has
nothing running, and `This worktree is not registered. Nothing to stop.` when it
was never brought up. Both exit 0. While a stopped container or a network of the
worktree remains, `down` still runs `docker compose down` to remove it, and
says nothing about it but that nothing was running. `router down` says `Router
stopped.`, and, when worktrees had route files, `Router stopped. 2 worktrees
were routed through it; their URLs don't load until you run 'treerig up' in
each.` (a route file can outlive a service that was stopped by hand, so it does
not say they run); with no router running it says `The router is not running.
Nothing to stop.`

A long step prints one line when it starts (`Starting all services for
feature-x.myapp.localhost.`) and a line every ten seconds while it runs (`still
starting all services for feature-x.myapp.localhost, 40s: <the last line Compose
wrote>`), so a pipe, CI or an agent never sees a silent minute and a hung start
leaves its last words. A terminal gets the same lines until the live view
arrives (#208). `CI` set, a stderr that is not a terminal, `TERM=dumb` and
`--progress=plain` select this form; it is the only one today.

`status` and `list` share one set of [state
words](commands.md). When Docker cannot be asked, each of them still prints
everything it can (the states that need Docker read `unknown`) and then exits 1
with the cause on stderr, once. `status` does the same when Compose, not Docker,
cannot read the worktree. `list` gives Docker about five seconds a label
namespace, and Git five seconds a repository.

`status` prints its table (or, with `--json`, its document) whatever the state.
It exits 0 whatever the state, and 1 when it couldn't read the worktree: Docker
didn't answer, Compose couldn't read it, its treerig.toml wouldn't load, or its
Compose file is too large or not a regular file. A failed or stopped worktree
and a missing directory are states it reports with exit 0, since `doctor` is
the check that fails. A worktree that is not registered is the notice
`<slug> is not registered. Run 'treerig up' to create its environment.` on
stderr, nothing on stdout (`--json`: `{"registered": false, "services": []}`)
and exit 0. When something is routed and the router is the reason no URL loads
(its container is stopped, runs on another port, belongs to another treerig
root or could not be read), `status` says which on stderr and shows `-` for
every URL. It fetches nothing over HTTP. The command it replaced, `urls`, is a
hidden stub that prints `'treerig urls' is now 'treerig status'.` on stderr and
exits 1 for one release, whatever flags it is given (`help urls` prints the same
text on stdout, exit 0).

`router status` has a vocabulary of its own, about the shared router rather than
any worktree: `running on <address>, <mode> mode`, `not running (would use
<address>)`, `degraded on <address>, <mode> mode` when a container is down or
unhealthy, and nothing at all on stdout when Docker could not be asked —
that case is a `Router state unknown:` line on stderr. Each reported problem is
its own `Router problem:` line on stderr, followed by the container's last
log lines for one that is not running or not healthy. A running router whose
container carries no mode label reads `running on <address>, mode unknown`.
Both `running` and `not running` exit 0, because "is it there?" is the
question and both are answers; `degraded` and an unreachable Docker exit 1. Whenever the status line is printed, a second
stdout line follows it saying which port was resolved and from where —
`port <n> from <settings file>`, `port <n> from TREERIG_ROUTER_PORT (overriding
<settings file>)` or `port <n> by default (<settings file> sets none)`; it too
is absent when Docker could not be asked.

`doctor`'s report and exit rules are in [doctor.md](doctor.md).

`init` writes nothing to stdout: that it wrote the file, and a warning when no
Compose file was found, are notices on stderr. It exits 1, writing nothing,
outside a Git checkout or when `treerig.toml` is already at the root, and 127
when Git is not on `PATH`, as for any command that cannot start. A kill in the
middle of the write can leave a truncated `treerig.toml`; `init` then refuses, and
you delete the file and run it again.

`worktree unlock` writes nothing to stdout: what it verified and what it
cleared are notices on stderr. A refusal is an error that exits 1 and clears
nothing: a holder that may be alive, a process it left running, or a lock that
changed while it looked.

Every failure the tool itself reports, a bad command line included, ends in a
non-zero exit code and a `[treerig] ERROR:` line on stderr saying why. Set
`TREERIG_DEBUG=1` to see the stack trace behind a failure. A failing hook's own
exit code becomes treerig's, and so does a `run` command's, a `backup` or `restore`
hook's — see [`hooks.md`](hooks.md) — except that a hook which outlives
its `[hooks.timeout]` limit is stopped and treerig exits 124, not the hook's own
code ([`hooks.md`](hooks.md#timeouts)). A `dev` session that you end
ends with the status of the signal that ended it: 130 for Ctrl-C, 143 for
SIGTERM, 129 for SIGHUP.

## Verbose output

`-v` (`--verbose`), accepted by every command, adds to stderr:

- Docker Compose's transcript of every command that runs `docker compose up` or
  `down` (see above), as Compose wrote it;
- each Docker and Compose command treerig runs, with treerig's own arguments
  only (`[treerig] docker compose --project-name treerig-… down
  --remove-orphans`);
- for a hook, its exit code and how long it ran.

It never prints the environment, anything after a `--`, the arguments of a hook
or a `[commands]` entry, or the output of `docker compose config`. In treerig's
own verbose lines, the value after a flag such as `--password` or `--token`, the
value of a pair after `-e`, `--env` or `--build-arg`, the value of a `--flag=value`
whose flag name looks like a secret, the value of a `KEY=value` pair whose name
is upper case or contains pass, secret, token, key, auth, cred and similar
words, an `Authorization` header, a bearer token, and the user and password of a
URL are replaced by `<redacted>`. **Compose's transcript is not redacted**, and
neither is your hooks' output: treerig cannot redact what it does not write, and
a log line can carry a secret. `TREERIG_DEBUG=1` is separate and means only the
stack trace behind a failure.

Not shown yet: the paths, IDs and digests that other commands know (#211,
#215), and `doctor`'s every check under `-v` (#215).
