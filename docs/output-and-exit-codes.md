# Output and exit codes

Which stream each command writes to, and the exit codes treerig uses.

The commands that produce data — `index`, `status`, `urls`, `router status`,
`shared status`, `route ls`, `config show`, `config validate`, `worktree list`, `doctor`, `teardown status` — write that data to stdout
and nothing else, so `treerig index > page.txt` and
`treerig worktree list --json | jq` get what they asked for. Progress, waiting
notices, confirmation prompts, warnings and failures go to stderr. Eight of
these (`index`, `status`, `urls`, `shared status`, `route ls`, `worktree list`, `doctor` and `teardown status`) take `--json`:
see [Machine-readable output](json-output.md).

`worktree rename` writes only its notice, to stderr, and nothing to stdout.

`teardown pin` writes only its notices to stderr, and nothing to stdout.
`teardown status` writes its report to stdout, and takes `--json` (see
[Teardown pin](teardown-pin.md)); a problem is an error on stderr that exits 1,
and so is no pin on a development build. On a release build, no pin is not a
problem: it exits 0 and says on stderr that none is needed. The report's last
line, `worktree remove: will run this binary`, `worktree remove: would refuse` or
`worktree remove: will run this build (a release; no pin needed)`, is part of the
text form only: the `--json` document does not carry it. Anything that would make removal refuse
besides the pin's own integrity (a control home inside a checkout, a pin store
others can write, a `noexec` mount) is a warning on stderr in both forms, with
exit 0, so stdout stays the report or the document.

`up`, `dev`, `down`, `backup`, `restore`, `router down` and `worktree remove`
are the exception: they pass your hooks' and Compose's own stdout through as
those programs wrote it, and `up` adds its URL table there. `worktree remove`
prints the two lines of its teardown gate when a pin is used (the pinned binary it
runs, and the pinned binary saying it ran), and any refusal of the gate, on stderr,
never on stdout. `dev` is `up` and then the followed logs, which keep their streams: a
container's stdout on treerig's stdout, its stderr on treerig's stderr, and one
notice on stderr when following starts. `backup` and `restore` write their own closing line, and any
warning, to stderr, so a script that needs a bundle's path passes it a
directory. `shared up` passes Compose's stdout through the same way; `shared
down` writes only notices and warnings, to stderr. `run` is the same, more
strictly: the command it runs writes straight to treerig's stdout and stderr, and
treerig adds nothing while it runs, so `treerig run plan > out` holds only what
`plan` wrote.

`router status` has a fourth vocabulary, about the shared router rather than
any checkout: `running on <address>, <mode> mode`, `not running (would use
<address>)`, `degraded on <address>, <mode> mode` when a container is down or
unhealthy, and nothing at all on stdout when Docker could not be asked —
that case is a `Router state unknown:` line on stderr. Each reported problem is
its own `Router problem:` line on stderr, followed by the container's last
log lines for one that is not running or not healthy. A running router whose
container carries no mode label reads `running on <address>, mode unknown`.
Both `running` and `not running` exit 0, because "is it there?" is the question and both are answers; `degraded` and
an unreachable Docker exit 1. Whenever the status line is printed, a second
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
