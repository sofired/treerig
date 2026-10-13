# Lock layer and process lifecycle

`internal/lock`, `internal/procid`, `internal/run/osrun`, and `internal/lifecycle`
implement berth's synchronization and process handling.

## Design

The kernel's `flock(2)` on the lock's own target file is the only lock
primitive. It is held by an open file description, so:
- the kernel frees it the moment its holder dies, even by SIGKILL, leaving
  nothing stale;
- it survives setsid, fork, and exec for a child that inherited the
  descriptor.

This removes the mtime heartbeat and its stale windows, the twelve-second
second look and clock-drift checks, the `.recovery` mutex and `.lock`
directories, and the macOS birth-time workaround the heartbeat design needed.

It also fixes four known bugs directly: #22 (a compromised lock did not
cancel in-flight work), #23 (`down` did not defer signals), #24 (`forceExit`
let an exit hook remove the lock directory), and #12 (the router lock was per
control home): #39 moved the router onto this design, in its own state
directory rather than any control home, so every control home sharing that
state directory now waits on the same lock instead of each holding its own.
As of #80, that state directory is `<BERTH_HOME>/router` — the berth root,
not a fixed per-user-account location — so it is shared by every control
home under the same berth root, not necessarily by every control home on
the machine; a scratch `BERTH_HOME` used to isolate a control home also
gets its own copy of the router's lock and state, though not its own router
container (one per Docker daemon regardless of berth root — see
`internal/layout`'s `RouterStateDir`/`RouterLayout` comments, and AGENTS.md's
"Running things safely").

Docker children inherit the lock: `docker` and `docker compose` get the
worktree lock's descriptor through `ExtraFiles`, as fd 3. Hooks do not — a
hook's leftover helper would otherwise keep the worktree locked after a
successful `up`, so those leftovers are instead caught by the process scan
after an unclean exit. The same goes for `berth run`'s commands (#94): they
run interactively, and `Spec.InheritLocks` does not combine with
`Output: Interactive` — the interactive spec wins and no descriptor is handed
over, since an interactive child's whole process group is drained before
`Run` returns and the lock is released only after that.
`internal/run/inherit_guard_test.go`'s `TestOnlyDockerInheritsLocks` walks every non-test `.go` file in the module
and fails the build if anything outside `internal/docker` sets
`Spec.InheritLocks`.

A control home, or the router's own state directory, on NFS, SMB, FUSE
(including virtiofs), or 9p is refused; the message names whichever
environment variable actually governs that directory (`BERTH_CONTROL_HOME`
for the control home, `BERTH_HOME` for the router's — `Scope.HomeEnvVar`,
#80) and tells the user to point it at a local directory instead. So is one
on a cluster filesystem whose locks may be local to one client: Lustre, GPFS,
CephFS, AFS, Coda, OCFS2, GFS2. There is no Windows support.

## Kernel facts this design relies on

- A descriptor passed to a child in its own session keeps the flock held
  after the holder is SIGKILLed, and frees it only when that child dies.
- Without passing it (Go's default: every open is `O_CLOEXEC`), the lock is
  freed at once.
- A child that closes the descriptor (a daemon) frees it.
- A stopped (SIGSTOP) holder still holds the lock.
- A thread blocked in `flock(LOCK_EX)` is not woken when another thread
  closes the descriptor: blocking `flock` cannot be cancelled, so waiters
  poll with `LOCK_NB` instead.
- `LOCK_UN` from anyone sharing the open file description releases it for
  everyone, including a child holding an inherited copy: release only ever
  closes the descriptor, never calls `LOCK_UN`.
- `/proc/locks` keeps the PID that took the lock, not its current holders.
- Which programs keep an inherited descriptor: bash `cmd &`, `nohup … &`,
  `setsid -f`, the `docker` CLI, and its `docker-compose` plugin keep it (Go
  never closes inherited descriptors); Node and Python children drop it.

Go specifics the implementation relies on:
- `ExtraFiles` are dup'd onto fd 3+i without close-on-exec.
- `os/exec` skips its `Dir` pre-check when `SysProcAttr` is set, so `osrun`
  checks `Dir` itself before starting the child.
- `WaitDelay`'s kill hits only the process group leader, not the group —
  `osrun` sends its own signals to `-pgid`.
- `signal.Reset` followed by `syscall.Kill(self, sig)` makes the runtime die
  by that signal.
- A SIGINT or SIGHUP ignored at startup stays ignored only if it is never
  passed to `Notify`, so `lifecycle.Start` skips any signal `signal.Ignored`
  already reports.

## `internal/procid`

Answers what can be proven about a process from outside it, and nothing
more. A lock holder writes its identity into its record when it takes a
lock; a later reader checks that identity against the machine, this boot of
it, and the process table — never a wall clock, which can step.

**Linux** reads `/proc` directly (no `ps`, which slim images lack and
BusyBox answers differently):
- `/proc/<pid>/stat`, parsed after the last `)`, for state and start ticks
  (field 22);
- `/proc/sys/kernel/random/boot_id`;
- `/etc/machine-id`, then `/var/lib/dbus/machine-id`;
- `readlink /proc/self/ns/pid` for the PID namespace;
- a scan of `/proc/*/{cmdline,environ,ns/pid}`, honoring the same owner and
  `hidepid` rules any unprivileged reader of `/proc` must.

`Holding(dev, ino)` scans `/proc/*/fd/*` for descriptors on the lock file and
lists the processes holding it. This is Linux-only.

**macOS** asks the kernel through `sysctl`, since there is no `/proc`:
- `unix.SysctlKinfoProc("kern.proc.pid", pid)` for `P_starttime` (exact to
  the microsecond) and `P_stat` (stopped or zombie);
- `SysctlKinfoProcSlice("kern.proc.all")` for the scan;
- `SysctlRaw("kern.procargs2", pid)` for exact argv and environment; a
  process `procargs2` refuses to another user without root falls back to a
  single `ps -A -ww -E -o pid=,command=` call;
- `unix.Sysctl("kern.bootsessionuuid")` for the boot ID;
- `ioreg -rd1 -c IOPlatformExpertDevice` for the machine ID.

`Holding` is Linux-only by design: macOS has no `/proc` to enumerate open
descriptors from, and `lsof` is not guaranteed present or unprivileged.
`DarwinTable.Holding` returns `errors.ErrUnsupported`; a lock notice on
macOS never lists who holds it, only what the record itself says.

**macOS CI** (`macos-latest`) passes: flock inheritance through `osrun` and
through a plain `exec` chain, the `kern.procargs2` marker scan, identity and
exact start time, and stopped-holder detection. Inheritance through a real
`docker` is proven on Linux only — the macOS runner has no Docker, so those
tests skip there.

## `internal/lock`

**Scopes and files.**
- Target files: the registry lock at `<home>/locks/registry`; a worktree's
  operation lock at `<home>/locks/worktrees/<repo>/<wt>`; its
  development-session lock at `…/<wt>.dev`; a repository's shared-services
  lock at `<home>/locks/shared/<repo>` (#123, below).
- Which verbs take which: `up`, `down`, `run`, `backup` and `restore` take the
  operation lock only, so they work during a development session;
  `worktree remove` takes the development lock as well, and is refused while
  one is attached.
- The survivor scan (`procid.Marked`) counts a process as a worktree's own when
  its environment holds `BERTH_WORKTREE_ID=<wt>` or `COMPOSE_PROJECT_NAME=<p>`.
  Hooks and commands now set `COMPOSE_PROJECT_NAME` (docs/hooks.md), and
  already carried `BERTH_WORKTREE_ID`, so the set of processes counted is
  unchanged: a hook's children were the worktree's processes before and are
  now marked twice.
- The router lock's target is `layout.RouterState`'s `LockPath`,
  `<BERTH_HOME>/router/lock` (#80) — never any control home's own `locks/`,
  so every control home under the same berth root waits on the same lock
  (#12, #13). A berth root is not a fixed, per-user-account location the
  way it was before #80 (previously `$XDG_STATE_HOME/berth/router/lock`, or
  `$HOME/.local/state/berth/router/lock` when that is unset, on Linux;
  `$HOME/Library/Application Support/berth/router/lock` on macOS): a
  different `BERTH_HOME` reads and writes a different lock file entirely,
  which does *not* exclude the first — yet the router container this lock
  guards is still one per Docker daemon regardless of berth root (its
  Compose project name is fixed). Two berth roots' `router up`s therefore
  never wait on each other. What stops the second from recreating the
  first's router out from under it is not the lock but a label (#82): both
  router containers carry `dev.berth.router-home`, the berth root whose
  `Ensure` last started them, and `Ensure` refuses — before it reads or
  writes anything — a running router whose label names another root. The
  way out is explicit: `router down`, which is never gated on ownership,
  stops it from any root, and the next root's `router up` starts its own.
  A router carrying no label is refused the same way, since this berth root
  did not start it either; `doctor` reports the owner read-only. What the
  label does not close is the cold
  start: two roots that each find no router running can still both go on
  to create the one container, since their locks are different files. A
  machine-wide lock target would close that; it is not this design. See
  `internal/router/stack.go`'s `RouterHomeLabel` comment and `router.go`'s
  `checkOwnership`. `router up` takes its own lock alone, never alongside a
  worktree's own locks (`lifecycle.Requests.Router`). The one edge from a
  worktree-level lock to this one is a `dev` session's best-effort index
  refresh (see the development session, below); the router lock is never held
  while waiting for a worktree-level lock.
- Routing splits the same way. A worktree's own route files
  (`<repo>.<wt>.<service>.yaml` in the router's `dynamic/`) are written by
  `up` and removed by `down` and `worktree remove`, under that worktree's
  operation lock alone: the names carry the worktree's IDs, so no other
  worktree contends for them, and a slow `compose up` never holds the
  machine-wide lock. There is no network to attach or detach: a route file
  names a loopback port. What spans every worktree runs inside
  `Router.Ensure`, under the router lock: allocating the checkout index's and
  the ping entrypoint's ports into `<BERTH_HOME>/router/state.json` (below),
  and repairing route files that disagree with a registration. That repair
  reads registrations without the registry lock and reads a record again
  immediately before it removes or rewrites a file, which narrows the window
  against a port `up` has just committed without closing it. A removal in
  that window is repaired by the writer's next `up`, and a rewrite is
  eventually consistent with the writer, which writes the same port. Files
  this control home holds no record of are never touched. The router lock and
  the registry lock are never nested.
- Opening a target creates it `0600` (parent `0700`) if missing, opens it
  `O_RDWR`, and keeps a strong reference to the `*os.File` for the whole
  hold — the GC finalizer would otherwise close it. A target that is not a
  plain file (a symlink, directory, named pipe, socket, device, or other
  special file) is refused rather than followed or replaced.

**Holder record.**
- A sidecar file `<target>.holder`, written atomically (temp file, then
  rename) while the lock is held. The target file itself is never written,
  since children that inherit fd 3 could write to it.
- Fields: `pid`, `host`, `command`, `startedAt`, `releasedAt` (once
  released), `file` (`dev`/`ino`), `machineId`, `bootId`, `pidNs`,
  `startTicks` (Linux), `startUs` (macOS).
- If the record cannot be written, the lock is given back and the acquire
  fails.

**`Acquire(ctx, command)`.**
1. Polls `LOCK_EX|LOCK_NB` at a fixed interval bounded by the scope's
   timeout: the worktree operation lock polls every 250 ms for up to 25
   minutes; the registry lock every 50 ms for up to 60 s; the development
   lock every 250 ms for up to 1 s; the shared lock every 250 ms for up to 15
   minutes; the router lock every 250 ms for up to 20 minutes — enough to
   cover `router up`'s own worst case: a start and every Docker probe around
   it. (A `Lock` used
   with neither a timeout nor
   a retry interval set instead backs off from 50 ms to a 250 ms ceiling
   over about 4.6 s.) While it waits, a notice fires after 2 s and then every
   30 s, naming the holder and how long it has held the lock — a stopped
   holder is told to `kill -CONT <pid>`, a running one can be waited for or
   sent `kill <pid>` — and, on Linux, listing the processes holding the
   descriptor. Giving up returns an error with the same advice, its action
   ending in "then retry". The deadline uses Go's monotonic clock.
   `ctx.Done()` ends the wait with the context's own cancellation cause;
   nothing is held then.
2. Once the flock is taken, compares `fstat(fd)` with `lstat(path)` (same
   dev and ino). Berth never unlinks lock files itself, so a mismatch is a
   tight race with something else replacing the path; it reopens and
   retries.
3. Checks the filesystem once per scope (`CheckFS`): `statfs` refuses NFS,
   SMB/CIFS, FUSE (including virtiofs), 9p, and the cluster filesystems
   above — by magic number on Linux, by `f_fstypename` on macOS (`nfs`,
   `smbfs`, `afpfs`, `webdav`, `fusefs`/`macfuse`/`osxfuse`, `9p`, `afs`). A
   two-open self-test then proves a second opener actually sees
   `EWOULDBLOCK`.
4. If the existing record has no `releasedAt`, judges the previous holder
   while still holding the flock: same machine or boot, same PID namespace,
   and whether its PID is absent, a zombie, or has been reused
   (`judgeHolder`). It then scans for the previous holder's own processes
   still carrying the worktree's markers, rescanning once after a second
   before concluding. On any doubt it refuses, naming why the holder cannot
   be checked — never stealing the lock. If the flock is free but the
   holder's identity checks say it is still running (a stopped holder still
   holds the OS lock, so this is not itself proof of anything broken) it
   looks again after a second, then refuses, saying locking does not work on
   this filesystem.

**Held.**
- `Release()` writes `releasedAt`, then closes the descriptor — never
  `LOCK_UN`, which would free the lock for anyone else sharing the open file
  description, including a child holding an inherited copy. If a descendant
  still holds it afterward (probed with a fresh `LOCK_NB` open), it warns
  rather than waits. A lock the watchdog already found lost is only closed,
  since its record may now belong to whoever holds it.
- `Abandon()` closes the descriptor only, leaving the record unreleased for
  the next acquirer to judge.
- A process that cannot read its machine's identity (no machine ID and no
  boot ID) warns once, the first time it takes a lock: if it dies holding
  one, nothing can later prove it gone.
- `File()` is the `*os.File`, for `ExtraFiles`.
- A watchdog checks every 5 s that the path's inode and the record are still
  this process's (`Lost()`/`Err()`); if not, it cancels the action's
  context. This is what fixes #22.
- `WithHeld(ctx, held...)` and `Inherited(ctx)` carry the held locks on the
  context, so `osrun` can hand their descriptors to a Docker child,
  operation lock first, at fd 3.
- `Describe(scope, table)` is a read-only probe (`LOCK_SH|LOCK_NB`, then
  close at once) for `doctor`'s Locks section: held, unreleased-but-gone
  ("recovered automatically the next time this lock is taken"), or a
  `lock.Describe` read failure of its own ("could not be checked"). A
  shared lock, not the exclusive one `ProbeHeld`/`Clear` take below:
  `Describe` never mutates anything, so it only needs to prove nobody holds
  the flock exclusively, and a shared probe can run alongside another (a
  second `doctor`, say) without either waiting on the other.
- `ProbeHeld(ctx, scope, table)` is `Clear`'s own non-blocking, read-only
  probe of one scope's flock (`LOCK_EX|LOCK_NB`, then close), exported so
  `worktree unlock` can check every one of a target's locks before clearing
  any of them.
- `Clear(ctx, scopes, opts)` is `worktree unlock`'s core: given every scope a
  target might have (a worktree's operation and development locks, or,
  with `--registry`/`--router`, the one machine-wide scope those flags name
  — `commands.UnlockGlobal`, #21), it holds each one's free flock through
  judging every holder, one shared marker survivor scan, and only then
  committing each — marking a judged record's `releasedAt` — so a refusal
  on one scope (an alive contradiction, doubt without
  `--holder-unverifiable`, or a survivor) never leaves another already
  committed. A scope with no markers of its own (the
  registry lock, which guards only brief writes) skips the survivor scan
  outright and says so, rather than claiming a check that never ran. That
  scope's guarantee is narrower than a worktree's or the router's for
  exactly this reason: with no processes of its own to look for, its holder
  is judged only by identity (its record's PID, boot and machine id against
  this machine), never against a live scan of what it started — because it
  never starts anything to scan for. `ctx` cancellation ends its own waits
  (the contradiction's second look, the survivor rescan) the same way
  `Acquire`'s does.

## `internal/run/osrun`

Implements `run.Runner`: every subprocess berth spawns for real goes through
here — a session per child instead of Execa's process-tree tracking, and
its own SIGTERM-then-SIGKILL timer instead of Execa's `forceKillAfterDelay`.

- Every non-interactive child gets `SysProcAttr{Setsid: true}`. Cancelling
  its context sends SIGTERM to `-pgid`; `time.AfterFunc(KillAfter)` (10 s by
  default) then sends SIGKILL to `-pgid`. `WaitDelay` is about 2 s, so a
  descendant holding the output pipe cannot hang `Wait`.
- An interactive child always gets `SysProcAttr{Setpgid: true}` — its own
  process group, never berth's. When its stdin is a terminal and berth
  currently holds that terminal's foreground, it also gets `Foreground:
  true` (with `Ctty: 0`), handing it the terminal atomically as part of the
  same fork+exec; otherwise (piped, backgrounded, `ssh` without `-t`, CI) it
  gets the process group alone. Only the foreground case waits with its own
  `runForegroundChild`, a `wait4(pid, WUNTRACED)` loop instead of
  `cmd.Wait()` — the only way to see the child stop. A stop hands the
  terminal back to berth's own group and stops this whole process with
  SIGSTOP, the way a real shell answers Ctrl-Z on a job it started; the
  resume (SIGCONT from outside, typically the real shell answering `fg`)
  hands the terminal back to the child's group and `SIGCONT`s it.
  Reclaiming or handing back the terminal (`tcsetpgrp`) needs SIGTTOU
  ignored for the call — a background group's own tcsetpgrp otherwise stops
  it — done by toggling the signal's process-wide disposition rather than a
  second `signal.Notify`, and serialized by a package-level mutex, since
  only one foreground interactive child may be mid-flight in this process
  at a time.
- Once an interactive child's leader is gone, `drainGroup` always runs,
  whether or not berth itself was ever signalled: SIGTERM to whatever the
  group still holds, SIGKILL after `KillAfter`, bounded at `drainBudget`:
  `quiesceBudget` (about 15 s, the way `Quiesce` bounds its own wait), or
  `KillAfter` plus 5 s when a command's `kill-after` is longer than that
  leaves room for, so a minute's grace is never given up on at 15 s. A
  second SIGINT or SIGTERM (never SIGHUP) — read off the run's context via
  `run.Repeated`, never a second `signal.Notify` — abandons that wait early
  instead of running out the full budget. Both errors name the child by its
  `Spec.Label` ("apply command"), never its arguments or an absolute path,
  and the group to clear: `The apply command left processes running (process
  group 4242) after 15s; stop them with kill -KILL -4242.`, or, when
  abandoned, `… and waiting for them was abandoned after a second interrupt;
  stop them with kill -KILL -4242.`
- The same second signal also ends an interactive leader's kill-after grace:
  while the leader is still running after the first signal was forwarded, it
  is sent SIGKILL to the group at once (`runForegroundChild` for a foreground
  child, the `cmd.Wait` loop otherwise), and the drain that follows waits for
  the kill to take rather than treating that same signal as an abandon. A
  forwarded signal is followed by SIGCONT, so a child stopped by SIGTTIN acts
  on it. SIGHUP is forwarded as SIGTERM.
- Start failures use a fixed message table: not on `PATH` or a missing `#!`
  interpreter exits 127; not executable (`EACCES`) or not a valid
  executable (`ENOEXEC`, needs a `#!` line) exits 126; a working directory
  that does not exist or is not a directory exits 126, checked before the
  child ever starts (`os/exec` skips its own `Dir` check once
  `SysProcAttr` is set).
- A child killed by signal `n` reports exit `128+n`, with `Signal` set. A
  timeout cancels through `context.WithCancelCause` and is reported as exit
  124 by `run.Failure`. Output past `run.OutputLimit` cancels the run and
  sets `OutputOverflow`.
- Output can be captured only, teed to this process's matching stream, or
  teed to stderr for both streams; either way both are captured.
- `Env == nil` inherits berth's own environment; when `Env` is set, the
  command is looked up on the child's own `PATH`, as Node does.
- `Which(name)` walks `PATH` for an executable regular file. `Quiesce(ctx)`
  returns once every child this Runner started has been reaped and
  `kill(-pgid, 0)` answers `ESRCH` for each group, bounded at about 15 s.

## `internal/lifecycle`

One lifecycle for the whole process, started once in `main`.

- `signal.Notify` covers SIGINT, SIGTERM and SIGHUP (a closed terminal, which
  `berth run` forwards to its command), skipping any signal
  `signal.Ignored` already reports.
- The first signal cancels the root context with the cause
  `Interrupted{sig}`. Later SIGINT or SIGTERM signals print one "still
  stopping" line and close `Repeated()`; a later SIGHUP is ignored, because a
  hangup often arrives twice within milliseconds.
- `Hold(ctx, env, requests, action)`:
  1. Acquires the requested locks in order: operation, then development,
     then shared, then registry.
  2. Runs `action` with a context that is also cancelled if a held lock is
     later lost.
  3. On an interrupt or error, calls `runner.Quiesce`.
  4. If quiescent, releases the locks in reverse order. If not, abandons
     them instead and exits 1 with `Could not prove the interrupted
     operation stopped: …` — locks are never released while something they
     guarded might still be running.
  5. `Exit()` re-raises the pending signal with `signal.Reset(sig)` and
     `syscall.Kill(getpid, sig)`, so the process dies by it as a shell
     expects; if it somehow survives that, it falls back to
     `os.Exit(128+n)`.
- `HoldThen(ctx, env, requests, action, then)` is `Hold` for a command that
  needs the operation lock only for `action` and keeps the later locks for
  `then` (see the development session, below).
- The action's own error is reported first; a release or quiesce failure
  rides along as its `Cause` (if it had none) and always in `Suppressed`, so
  a lock that could not be released is never silent.
- `Finish(err, report)` is how the process's own top-level error becomes
  its exit code — called once, from `internal/cli`'s `runMain`, after the
  command has run and `Stop` has been called. `err` is "plain" when it is
  nil or unwraps to `*clierr.Cancelled` — nothing more than the fact that a
  signal already interrupted this process, which a signal death already
  says on its own. A plain `err`, with a signal pending, ends the process
  through `Exit()` above, exactly as before; anything else — a
  non-`Cancelled` error, or one `Hold`'s own `attach` wrapped a `Cancelled`
  inside (a release failure racing a signal, say) — is reported through
  `report` first, and `Finish` returns the exit code it calls for
  (`clierr.Code(err)`) instead of ending the process by the signal, so it is
  never lost to a signal death that prints nothing of its own. A bounded
  wait elsewhere in the process reaches `Finish` this way too: an
  interactive child's own process-group drain (`internal/run/osrun`'s
  `drainGroup`) selects on the second signal via `Repeated()`
  (`internal/run`'s `WithRepeated`/`Repeated`) to abandon early instead of
  running out its own budget, and the error it returns on that path is
  exactly the kind `Finish` reports rather than silently re-raising.

## The development session (`dev`, #5)

`dev` is `up` followed by `docker compose logs --follow` on the worktree's
project, and it holds the worktree's development-session lock from before
`up`'s first change until the log follower is gone. The lock's only job is to
make a verb that needs the worktree to stay put refuse while someone is
working in it. Which verbs take it:

| Verb | Operation lock | Development lock |
|---|---|---|
| `dev` | during `up`'s work only | whole session |
| `worktree remove` | yes | yes (refuses during a session) |
| `up`, `down`, `run` (`lock = true`) | yes | no |
| `backup`, `restore` (#10) | yes | no |

`worktree remove` waits the lock's 1 s, then refuses, naming the session's
holder and telling you to end it with Ctrl-C in the terminal running
`berth dev`; a second `dev` does the same. (A second `dev` first runs `up`'s
router and shared-service steps and may wait on the operation lock before the
1 s refusal; it changes nothing of the worktree.) `up`, `down`, `run`,
`backup` and `restore` work during a session; a `backup` or `restore` of a
worktree being developed is the operator's call, as it is for `run`.

The order stays operation, then development: `lifecycle.HoldThen` acquires the
operation and development locks together, in that order, runs `up`'s mutation
under both, then **releases the operation lock** and runs its second phase, the
log follower, holding the development lock alone. The operation lock is held
only for `up`'s work, so a `down` from another terminal is not kept waiting for
the whole session. If the mutation fails or the context ends first, the second
phase never runs, the context's cause is returned, and everything is released
as `Hold` releases it. This depends on `Lock.Release` stopping a lock's
watchdog without cancelling the context `Acquire` derived: the development
lock's context is a child of the operation lock's. After the second phase,
quiescence is proven as for `Hold` and the development lock is released; a
failure to prove it abandons the development lock, never the operation lock,
which is already back and marked released.

The follower is run `Interactive` (`internal/run/osrun`), so it is in its own
process group, nothing is captured, and the runner drains the group before
`Run` returns, whether berth was signalled or not; that is the "stop the
group, then release the lock" the session needs. A SIGINT, SIGTERM or SIGHUP
that reaches berth cancels the context, the runner forwards it to the group
(SIGHUP as SIGTERM), a notice names the signal, and berth dies by the signal
once the lock is released, as in `run`. A Ctrl-C at a terminal goes to the
follower's foreground group instead, so berth sees the follower end by SIGINT
(or exit 130, which is Compose handling the interrupt itself) and ends the
session itself with exit 130 and the same notice (`clierr.Ended`, printed as
information, not as an error). Any other end of the follower is the session
dying on its own, Docker lost, say: a message on stderr, the follower's exit
code, or 1 for exit 0.

Whether Compose's follower ends when the project's containers go away varies.
On Compose 5.5.0 it has kept waiting for new containers when a `restart:
always` service had restarted under it (it depends on where in the restart
cycle the containers were removed), and otherwise exits 0 once they are gone,
ending the session with a message; Compose 2.38.2 always exits. A session may
therefore idle after a `berth down`, and a later `berth up` may show the new
containers' logs. An idle session keeps refusing `worktree remove` until
Ctrl-C. The real-daemon test, which includes a `restart: always` service,
accepts either outcome and checks that the lock, the follower and the refusal
agree. A poll of `compose ps` every 5 s while following prints one notice when
no container is left, saying that `worktree remove` refuses until the session
ends; it is read-only, bounded by the probe timeout, and joined before the
session returns. The session-end notice then omits "its services are still
running", since the last look found none.
A `kill -9` of berth frees the lock with the process (the follower is not
given the lock) but leaves the follower running on its own.

**One sanctioned exception to "the router lock is taken alone".** After
`up`'s work, `dev` prints the URLs and then refreshes the checkout index (best
effort, up to a 60 s wait) while still holding the development lock, because a
session that left the index page stale until it ended would hide the worktree
it just started. This adds the single edge development, then router, and the
rule becomes: the router lock is never held while waiting for a worktree-level
lock. The router's `Ensure` and the index refresh read registrations and Docker
without any worktree lock, so the order stays acyclic, and the refresh gives up
rather than waiting out a router start. A test pins that the refresh runs with
the development lock held and the operation lock not.

## Loopback port allocation (#119)

No new lock, and no new order. `up` allocates a routed service's port inside
its `Hold` of the worktree's operation lock, by calling
`registry.Store.SyncPorts`, which takes the registry lock for its own short
read-scan-write: operation, then registry, the order every other registry
write already uses. The scan of every registration in the control home that
picks a free port, the loopback probes, and the write of the worktree's
record all happen under that one registry hold, so two worktrees of one
control home cannot pick the same port. `worktree remove` releases a
worktree's ports with `Store.ReleasePorts` in the same order, after the
containers are reaped and before the record is deleted. Nothing takes the
router lock and the registry lock together.

The router's own index and ping ports (#120) are allocated from the same
range under the router lock, not the registry lock, and the two locks are
never nested. `Router.Ensure` reads the registrations without the registry
lock (every registry writer goes through `files.AtomicJSON`, a temporary file
then a rename) to keep its pick clear of their ports, and stores both ports
in one file, `<BERTH_HOME>/router/state.json`, written once and kept. `up`
passes that file's ports as `PortPlan.Reserved`, so `SyncPorts` skips them,
and `Ensure` runs before the allocation in the same `up`. Within one control
home a worktree allocation and a first-ever index/ping allocation can still
race for the same port, because neither holds the other's lock. That race is
accepted, like the two-control-home race below; the losing side is caught by
Docker's bind error and by the pre-start port check in `Ensure`.

The registry lock is per control home, as it is for hostnames: two control
homes on one machine can pick the same port at the same moment. That race is
accepted (docs/design/routing.md, decision 3a); the second `up`'s bind error
is its catch, and `up` re-probes and names the holder rather than parsing
Docker's message.

## Renaming a worktree (#11)

`worktree rename` takes the worktree's operation lock and its
development-session lock (`mutate` with `exclusive`), so it cannot overlap
an `up`, a `down` or an attached `dev`; it refuses from the worktree's own
hooks like the other verbs. The registry lock is taken once, inside, by
`registry.Store.Rename`: operation, development, registry, the same order as
every other registry write. Under that one hold it reads the record, scans
every registry on the machine, checks the slug is free in the record's
domain, runs the migration callback and replaces the record, so two renames
(or a rename and a first registration) cannot mint the same hostname. The
callback (`moveRoutes`, `moveIndexCard`) removes the worktree's route files and
rewrites the index card, with plain file operations; it
takes no lock and does no Docker call, so the registry hold stays short. The
inventory that refuses a rename while containers exist runs before, under the
operation lock alone.

Crash order: derived state first, the record last. The record is the commit
point. Route files are removed and the card is rewritten from the card's own
host to the record's new one, so each step is idempotent and does not depend
on which slug the failed attempt was given. A crash before the record leaves
the old slug registered, with at worst the route files gone and the card
already at the first attempt's host; a retry, with that slug or another,
redoes both, and `up` rewrites both anyway. A crash after it leaves nothing to
finish. The record is not marked pending first, as `worktree remove` marks it:
a rename destroys no data, and a pending mark would make the next `up` seed
again.

## Shared services (#123)

One new scope, `lock.SharedScope(controlHome, repositoryID)`, target
`<control home>/locks/shared/<repo>`: a single lock for all of a repository's
shared services, with the shared Compose project's name
(`berth-shared-<24 hex>`) as its survivor-scan marker. Its timeout is 15
minutes, the longest thing done under it: a `compose up --wait` of five
minutes, a pull and a recreate. `lifecycle.Requests` gains `Shared`, and the
order everywhere is operation, development, shared, registry, with the router
still alone (bar that `dev` refresh). **Nothing takes the shared lock and then a worktree lock.** That
one rule is what keeps the order acyclic, and it shapes every caller:

- `berth up` starts or reuses the instance under the shared lock *alone*,
  before `mutate`, so a worktree's 25-minute operation lock is never held
  waiting on a slow shared start.
- Connecting a worktree runs inside that worktree's operation lock and takes
  the shared lock nested after it, for the connect only: `Lifecycle.Nest`
  acquires the extra scopes beside the ones `Hold` took, hands all of them to
  a Docker child in order (`lock.AddHeld`), and refuses a request for an
  operation, development or router scope, and records the highest kind
  already held on the action's context so a request that does not come after
  it (an inversion) is refused as a bug in berth. The operation lock is what excludes this worktree's own
  disconnect, so the shared container is never attached to a network that is
  being removed.
- Disconnecting (`down`, `worktree remove`) needs the operation lock only, so
  a removal never waits behind a slow shared start.
- The reconnect sweep, after a shared container is created or recreated, runs
  only after the shared lock is released: one `Hold(Operation, Shared)` per
  registered worktree, each with `Requests.Timeout = lock.ReconnectTimeout`
  (5 s) and `Requests.Quiet` (no "waiting" notice). A worktree whose lock is
  busy yields a `*lock.BusyError` (a transparent subtype of the `clierr.Error` it
  wraps: same message, printed once, `Suppressed` kept; `errors.As` finds
  either), which the sweep reads by type, not by text: it is skipped with a
  warning and repaired by its own next `up`, by `berth
  shared up` (which always sweeps) or found by `doctor`. A sweep run from
  inside a worktree's own hook skips that worktree without waiting, since the
  lock is its caller's.
- A shared service's route file (`shared.<repo>.<name>.yaml`, routing.md
  decision 19) is written and removed under the shared lock alone, so it
  excludes a concurrent start or `shared down` of the same repository. Route
  repair (`Router.Ensure`, doctor) takes no shared lock and, like a worktree's
  route repair, reads the record again immediately before it rewrites or
  removes a file. Repair never creates a file: a record whose file is not
  written yet stays unrouted until `up` or `shared up` writes it. The index
  refresh and `Router.Ensure` take the router lock, so they run only after the
  shared lock is released.
- Port allocation for a shared service is `registry.Store.SyncSharedPorts`,
  which takes the registry lock for its own short read-scan-write inside the
  shared lock, the order every other registry write already uses. The skip set
  it reads, `takenPorts`, now includes every shared-service record as well as
  every registration, and a worktree's allocation reads the same set, so
  neither can take a port the other holds.

The shared lock is per control home, like the registry's. Two control homes on
one Docker daemon can both start the stack; the loser gets "container name …
is already in use" (verified), which `stack.Up` returns as a
`*shared.ConflictError` and the caller answers by inspecting the container by
label again: running means it is theirs to use. A running instance found with
no local record is reported and its ports are never re-allocated. Docker calls
under the lock are bounded (30 s for an inspection, a minute for a removal);
an inspect that fails, or a container state berth does not know, fails the
command, because a container that cannot be proven absent must not be assumed
absent: that would start a duplicate or recreate a running instance.

There is no age-based cleanup. The flock frees on death, and the recovery is
`berth worktree unlock --shared`, which runs the same `lock.ProbeHeld` and
`lock.Clear` checks as a worktree's lock against this scope, scanning for
processes carrying the shared project's name.

## Real-process tests

`//go:build integration` tests in `internal/lock`, `internal/lifecycle`, and
`internal/run/osrun` re-execute the test binary as a helper, chosen by an
environment variable, and report readiness over an inherited pipe. Cleanup
kills exact PIDs only, never by name. They cover: a holder SIGKILLed or
SIGSTOPped mid-hold; a holder that exits without releasing; a grandchild in
its own session that inherits or closes the descriptor; PID reuse; a
Go-to-Go inheritance chain; a daemon that closes its descriptors and carries
no markers, which documents recovery's one blind spot; a hung holder; the
contradiction case; SIGINT delivered mid-action and while waiting for a
lock; an unquiesced action; a lock file replaced mid-hold; a waiter whose
file was unlinked out from under it; every `osrun` exit code; and the
filesystem table. The lock, lifecycle, and runner packages also pass at
`-count=20 -race` on Linux; macOS CI runs them once with `-race` and once
without cgo. `internal/commands` has one more: two worktrees of a repository
run their first `up` in two processes at the same moment against a real
daemon, and exactly one starts the shared service.
