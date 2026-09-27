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
after an unclean exit. `internal/run/inherit_guard_test.go`'s
`TestOnlyDockerInheritsLocks` walks every non-test `.go` file in the module
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
  development-session lock at `…/<wt>.dev`.
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
  worktree's own locks (`lifecycle.Requests.Router`).
- Routing (#55) splits the same way. A worktree's own route fragments
  (`<repo>.<wt>.<service>.yaml` in the router's `dynamic/`) are written by
  `up` and removed by `down` and `worktree remove`, and the router's
  attachment to that worktree's own network is made and undone by the same
  three, all under that worktree's operation lock alone: the names carry
  the worktree's IDs, so no other worktree contends for them, and a slow
  `compose up` never holds the machine-wide lock. What spans every worktree
  — reattaching the router to each labelled worktree network, and removing
  fragments whose network is gone — runs inside `Router.Ensure`, under the
  router lock. A router recreated between `up`'s `Ensure` and its own
  network connect loses that one attachment until the next `Ensure`
  restores it.
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
   lock every 250 ms for up to 1 s; the router lock every 250 ms for up to
   20 minutes — enough to cover `router up`'s own worst case: a plain start,
   one recreate if it drifted, and every probe around them. (A `Lock` used
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
  descendant holding the output pipe cannot hang `Wait`. Interactive
  children get no session and are signalled by their own PID.
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

- `signal.Notify` covers SIGINT and SIGTERM, skipping any signal
  `signal.Ignored` already reports.
- The first signal cancels the root context with the cause
  `Interrupted{sig}`. Later signals print one "still stopping" line.
- `Hold(ctx, env, requests, action)`:
  1. Acquires the requested locks in order: operation, then development,
     then registry.
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
- The action's own error is reported first; a release or quiesce failure
  rides along as its `Cause` (if it had none) and always in `Suppressed`, so
  a lock that could not be released is never silent.

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
without cgo.
