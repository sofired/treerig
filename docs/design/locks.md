# Lock layer and process lifecycle

`internal/lock`, `internal/procid`, `internal/run/osrun`, and `internal/lifecycle`
implement berth's synchronization and process handling. They replace the
TypeScript build's `operation-lock.ts`, `lock-recovery.ts`, `process-table.ts`,
`process-runner.ts`, `signal-lifecycle.ts`, `commands/mutation.ts`, and
`realProcess` in `commands/context.ts`.

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

It also fixes three known bugs directly: #22 (a compromised lock did not
cancel in-flight work), #23 (`down` did not defer signals), #24 (`forceExit`
let an exit hook remove the lock directory). A fourth, #12 (the router lock
was per control home), becomes possible once the router is ported — the
router lock itself is future work, #39.

Docker children inherit the lock: `docker` and `docker compose` get the
worktree lock's descriptor through `ExtraFiles`, as fd 3. Hooks do not — a
hook's leftover helper would otherwise keep the worktree locked after a
successful `up`, so those leftovers are instead caught by the process scan
after an unclean exit. `internal/run/inherit_guard_test.go`'s
`TestOnlyDockerInheritsLocks` walks every non-test `.go` file in the module
and fails the build if anything outside `internal/docker` sets
`Spec.InheritLocks`.

A control home on NFS, SMB, FUSE (including virtiofs), or 9p is refused; the
message tells the user to point `BERTH_HOME` at a local directory. So is one
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
  `hidepid` rules the TypeScript build used.

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
- The router lock is machine-wide, per user and project, in the user's
  runtime directory rather than the control home — future work, once the
  router verbs are ported (#39).
- Opening a target creates it `0600` (parent `0700`) if missing, opens it
  `O_RDWR`, and keeps a strong reference to the `*os.File` for the whole
  hold — the GC finalizer would otherwise close it. A target that is not a
  plain file (a symlink, directory, named pipe, socket, or device) is
  refused rather than followed or replaced.

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
   lock every 250 ms for up to 1 s. (A `Lock` used with neither a timeout nor
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
   (`judgeHolder`, ported from `lock-recovery.ts`). It then scans for the
   previous holder's own processes still carrying the worktree's markers,
   rescanning once after a second before concluding. On any doubt it
   refuses, naming why the holder cannot be checked — never stealing the
   lock. If the flock is free but the holder's identity checks say it is
   still running (a stopped holder still holds the OS lock, so this is not
   itself proof of anything broken) it looks again after a second, then
   refuses, saying locking does not work on this filesystem.
5. **TypeScript-era leftovers.** A `<target>.lock` directory is what the
   TypeScript build's own lock looks like:
   - if the TypeScript record inside `<target>` names a holder that is gone,
     it is judged and scanned exactly as step 4, then the `.lock` directory
     is removed and `<target>` is emptied, all while still holding the
     flock;
   - if that holder is judged alive, the acquire is refused, naming the
     TypeScript build explicitly and explaining that this build cannot wait
     on that build's locks;
   - a `.lock` directory with no record inside `<target>` is refused
     outright — a TypeScript berth takes its lock directory before writing
     its record, so this could be a live holder caught mid-write, and the
     two builds' locks cannot see each other to tell — with advice to
     `rmdir` it once its absence is confirmed by hand.

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
  close at once), for `doctor` — future work, #43.

**Unlock** stays a stub; it is future work, #43.

## `internal/run/osrun`

Implements `run.Runner`: every subprocess berth spawns for real goes through
here. It adapts `src/process-runner.ts` to Go's process model — a session
per child instead of Execa's process-tree tracking, and its own
SIGTERM-then-SIGKILL timer instead of Execa's `forceKillAfterDelay`.

- Every non-interactive child gets `SysProcAttr{Setsid: true}`. Cancelling
  its context sends SIGTERM to `-pgid`; `time.AfterFunc(KillAfter)` (10 s by
  default) then sends SIGKILL to `-pgid`. `WaitDelay` is about 2 s, so a
  descendant holding the output pipe cannot hang `Wait`. Interactive
  children get no session and are signalled by their own PID.
- Start failures use the message table ported word for word from
  `src/process-runner.ts:57-96`: not on `PATH` or a missing `#!`
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
  rides along as its `Cause` (if it had none) and always in `Suppressed`, as
  `src/signal-lifecycle.ts:95-104` attached a lock-release failure.

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
file was unlinked out from under it; every `osrun` exit code; the
filesystem table; and a TypeScript-era leftover. The lock, lifecycle, and
runner packages also pass at `-count=20 -race` on Linux; macOS CI runs them
once with `-race` and once without cgo.
