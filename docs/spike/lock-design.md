# Go spike: lock layer and process lifecycle

This is the design the Go build implements in `internal/procid`, `internal/run/osrun`,
`internal/lifecycle` and `internal/lock`. It replaces the TypeScript build's
`operation-lock.ts`, `lock-recovery.ts`, `process-table.ts`, `process-runner.ts`,
`signal-lifecycle.ts`, `commands/mutation.ts` and `realProcess` in `commands/context.ts`.

## Decisions

The kernel's `flock(2)` on the existing target file is the only lock primitive.
It is held by an open file description, so:
- the kernel frees it the moment its holder dies, even with SIGKILL, and leaves nothing stale;
- it survives setsid, fork and exec for a child that inherited the descriptor.

That removes:
- the mtime heartbeat and the stale windows;
- the 12-second second look and the clock-drift checks;
- the `.recovery` mutex and the `.lock` directories;
- the macOS birth-time workaround.

It also fixes four known bugs:
- **#22:** a compromised lock did not cancel in-flight work.
- **#23:** `down` did not defer signals.
- **#24:** `forceExit` let an exit hook remove the lock directory.
- **#12:** the router lock was per control home.

**Owner decisions:**
- **Docker children inherit the lock.** `docker` and `docker compose` get the worktree lock's descriptor through `ExtraFiles`, as fd 3 for the operation lock. Hooks do not. A hook's leftover helper would otherwise keep the worktree locked after a successful `up`; those leftovers are caught by the process scan after an unclean exit.
- **Local control homes only.** A control home on NFS, SMB, FUSE (including virtiofs) or 9p is refused, and the message tells the user to point `BERTH_HOME` at a local directory. Locks there may be local to one machine or kernel, or not work at all. So is one on a cluster filesystem whose locks may be local to one client: Lustre, GPFS, CephFS, AFS, Coda, OCFS2 and GFS2.
- **No Windows.**

## Verified kernel facts

These were verified on Linux with throwaway programs.
- A descriptor passed to a child in its own session keeps the flock held after the holder is SIGKILLed, and frees it only when that child dies.
- Without passing it (Go's default: every open is `O_CLOEXEC`), the lock is freed at once.
- A child that closes the descriptor (a daemon) frees it.
- A stopped (SIGSTOP) holder still holds the lock.
- A thread blocked in `flock(LOCK_EX)` is not woken when another thread closes the descriptor. **Blocking flock cannot be cancelled, so waiters poll with `LOCK_NB`.**
- `LOCK_UN` from anyone sharing the open description releases it for everyone. **Release must only close(), never `LOCK_UN`.**
- `/proc/locks` keeps the PID that took the lock, not the current holders.
- Which programs keep an inherited descriptor:
  - Kept by: bash `cmd &`, `nohup … &`, `setsid -f`, the `docker` CLI and its `docker-compose` plugin (Go never closes inherited descriptors).
  - Dropped by: Node and Python children.

**Go facts to rely on:**
- `ExtraFiles` are dup'd onto fd 3+i without close-on-exec.
- `os/exec` skips its `Dir` pre-check when `SysProcAttr` is set.
- `WaitDelay`'s kill hits only the leader, not the group.
- `signal.Reset` followed by `syscall.Kill(self, sig)` makes the runtime die by that signal.
- A SIGINT or SIGHUP ignored at startup stays ignored only if you never `Notify` it, so skip ignored signals.

**Not verifiable here (macOS):**
- flock on APFS;
- `kern.procargs2` visibility of other users' processes;
- whether `ps` is setuid;
- `mach_absolute_time` across sleep.

Mark these in tests and the report as unverified; there is no macOS CI run in the spike.

## `internal/procid`

- **Linux:** port `src/process-table.ts` as is:
  - `/proc/<pid>/stat`, parsed after the last `)`: state and start ticks (field 22);
  - `/proc/sys/kernel/random/boot_id`;
  - `/etc/machine-id`, then `/var/lib/dbus/machine-id`;
  - `readlink /proc/self/ns/pid`;
  - the scan of `/proc/*/{cmdline,environ,ns/pid}`, with the TypeScript build's owner and hidepid rules.

  Add `Holding(dev, ino)`, which scans `/proc/*/fd/*` for descriptors on the lock file and lists the processes holding it.
- **macOS:**
  - `unix.SysctlKinfoProc("kern.proc.pid", pid)`: `P_starttime` (exact to the microsecond) and `P_stat` (SSTOP=4, SZOMB=5);
  - `SysctlKinfoProcSlice("kern.proc.all")` for the scan;
  - `SysctlRaw("kern.procargs2", pid)` for exact argv and environment;
  - `unix.Sysctl("kern.bootsessionuuid")`;
  - `ioreg -rd1 -c IOPlatformExpertDevice` for the machine ID.

  If `procargs2` refuses another user's process, fall back to `ps -A -ww -E -o pid=,command=` for those, as today.

```go
package procid
type Identity struct { MachineID, BootID, PIDNS string; StartTicks *uint64; StartUs *int64 } // json-tagged
type State int // Absent, Zombie, Stopped, Running
type Status struct { State State; StartTicks *uint64; StartUs *int64 }
type Proc struct { PID int; Args, Env []string; EnvRead bool }
type Scan struct { Procs []Proc; Unreadable int; OthersHidden bool }
type Markers struct { Project, WorktreeID string }
type Table interface {
	Self() (Identity, error)
	Inspect(pid int) (Status, error)
	Scan() (Scan, error)
	Holding(dev, ino uint64) ([]Proc, error) // darwin: errors.ErrUnsupported
}
func Platform() Table
func Marked(procs []Proc, m Markers) []Proc // project as a whole argv token; BERTH_WORKTREE_ID=<wt> or COMPOSE_PROJECT_NAME=<p> in env
```

## `internal/lock`

**Scopes and files.**
- **Target files:** unchanged from today. Registry: `<home>/locks/registry`. Worktree operation: `<home>/locks/worktrees/<repo>/<wt>`. Development session: `…/<wt>.dev`.
- **Router:** machine-wide, as the router is. Per user and project, in the user's runtime directory, not the control home. Out of the spike; only needed when the router verbs are ported.
- **Opening:** create the target file 0600 (parent 0700) if it is missing, open it `O_RDWR`, and keep a strong reference to the `*os.File` for the whole hold. The GC finalizer would otherwise close it.

**Holder record.**
- A sidecar file `<target>.holder`, written atomically (temp file, then rename) while the lock is held.
- The target file itself is not written, because children that inherit fd 3 could write to it.
- Fields:

```json
{"pid":1,"host":"h","command":"berth up","startedAt":"…","releasedAt":"…?","file":{"dev":1,"ino":2},
 "machineId":"…","bootId":"…","pidNs":"pid:[…]","startTicks":123,"startUs":456}
```

- If the record cannot be written, give the lock back and fail, as today.

**Acquire(ctx, scope, wait, command).**
1. **Poll.** Loop on `LOCK_EX|LOCK_NB`:
   - Backoff: 50 ms doubling to 250 ms.
   - Timeouts: the worktree operation lock 25 min, the registry lock 60 s at 50 ms, the development lock 1 s.
   - Notices: after 2 s, then every 30 s, using the TypeScript build's wording. The notice says who holds the lock (from the record), how long, and on Linux the processes holding it.
   - The deadline uses Go's monotonic clock.
   - `ctx.Done()` (a signal) ends the wait with `Interrupted`; nothing is held then.
2. **Check the file is still the path.** After getting the lock, compare `fstat(fd)` with `lstat(path)` (same dev and ino, link count above 0). If they differ, reopen and retry. Berth never unlinks lock files.
3. **Check the filesystem.** On first use per scope, call `CheckFS(dir)`. `statfs` refuses NFS, SMB/CIFS, FUSE, 9p, and the cluster filesystems above on Linux; `nfs`, `smbfs`, `afpfs`, `webdav`, FUSE and 9p on macOS. A two-open self-test must see `EWOULDBLOCK` on the second open. A lock target that is not a regular file (a symlink, directory, FIFO, socket or device) is refused.
4. **Previous holder exited without releasing.** If the existing record has no `releasedAt`, judge it while holding the flock:
   - Port `judgeHolder` from `src/lock-recovery.ts`: same machine or boot, same PID namespace, PID absent, zombie or reused.
   - Then run the marker scan, rescanning once after 1 s.
   - **Refuse on doubt**, naming the holder and the processes still running (`kill -CONT`, `kill <pid>` advice). Never steal the lock.
   - **Contradiction:** the flock is free, but the record names a running holder with the same identity. Look again after 1 s, then refuse: locking is broken here.
   - A holder that is stopped or running gets the TypeScript build's advice.
5. **TypeScript leftovers.** A `<target>.lock` directory plus a TypeScript holder record inside `<target>` counts as "not released". Judge and scan as in step 4, then `rmdir` the directory and empty `<target>`, all while holding the flock.

**Held.**
- `Release()`: write `releasedAt`, then `close()`. If a descendant still holds the lock afterwards (probe with a new `LOCK_NB` open), warn that "processes started by this command still hold the lock". A lock the watchdog found lost is only closed: its record may now be another holder's.
- `Abandon()`: `close()` only, so the record stays unreleased.
- A process that cannot read its machine's identity (no machine ID and no boot ID) warns once when it first takes a lock: if it dies holding one, nothing can later prove it gone.
- `File()`: the `*os.File`, for `ExtraFiles`.
- `Lost()` and `Err()`: a watchdog checks every 5 s that the path's inode and the record are still ours. If not, it cancels the action's context. This fixes #22.
- `WithHeld(ctx, held...)` and `Inherited(ctx)`: carry the held files, so the runner can pass them to Docker children with the operation lock first.
- `Describe(scope, table)`: a read-only probe (`LOCK_SH|LOCK_NB`, then close at once) for `doctor`, later.

**Unlock.** Out of the spike. The command stays a stub.

## `internal/run/osrun`

Implements `run.Runner`.

- **Children and cancellation.**
  - Every non-interactive child gets `SysProcAttr{Setsid: true}`.
  - Cancelling (ctx done) sends SIGTERM to `-pgid`. Berth's own `time.AfterFunc(KillAfter)` then sends SIGKILL to `-pgid`.
  - `WaitDelay` is about 2 s, so a descendant holding the output pipe cannot hang `Wait`.
  - Interactive children get no Setsid, and are signalled by their own PID.
- **Start failures.** Port the message table from `src/process-runner.ts:57-96` exactly:
  - LookPath not found → 127 `is not on PATH (ENOENT).`
  - A path that does not exist, or whose `#!` interpreter is missing → 127, with the TypeScript wording.
  - EACCES → 126 `is not executable (EACCES).`
  - ENOEXEC → 126 `is not a valid executable (ENOEXEC) — a script needs a #! line.`
  - Check `Dir` first: 126 `has no working directory: <dir> does not exist (ENOENT).`, or `is not a directory (ENOTDIR)`.
- **Exit codes.**
  - A child killed by signal n reports 128+n, with `Signal` set.
  - A timeout is cancelled through `context.WithCancelCause`: `TimedOut`, reported as 124 by `run.Failure`.
  - Output overflow: a capped buffer at `run.OutputLimit` cancels with its own cause, and sets `OutputOverflow`.
- **Output.**
  - Tee: stdout to this process's stdout, stderr to its stderr.
  - TeeToStderr: both to stderr.
  - Either way, both are captured.
- **Environment.** `Env == nil` inherits berth's. When `Env` is set, look the command up on the child's own `PATH`, as Node does.
- **Other methods.** `Which(name)` walks `PATH` for an executable regular file. `Quiesce(ctx)` returns once every child has been reaped and `kill(-pgid, 0)` answers ESRCH for each group, bounded at about 15 s.

## `internal/lifecycle`

One lifecycle for the whole process, started in `main`.

- `signal.Notify` for SIGINT and SIGTERM, skipping any signal `signal.Ignored` reports.
- The first signal cancels the root context with the cause `Interrupted{sig}`. Later signals print one "still stopping" line.
- `Hold(ctx, env, requests, action)`:
  1. Acquire the locks in order: operation, then development, then registry.
  2. Run the action with a context that is also cancelled when a lock is lost.
  3. On an interrupt or error, call `runner.Quiesce`.
  4. If quiescent, release the locks in reverse order. If not, `Abandon` them, and exit 1 with `Could not prove the interrupted operation stopped: …`.
  5. `Exit()` re-raises the pending signal with `signal.Reset(sig)` and `syscall.Kill(getpid, sig)`. If the process survives that, it falls back to `os.Exit(128+n)`.
- The action's own error comes first. Release and quiesce errors are attached as `Cause` and `Suppressed`, as in `src/signal-lifecycle.ts:95-104`.

## Tests (real processes, `-tags integration`)

The test binary re-executes itself as a helper, chosen by an environment variable. It reports readiness over an `ExtraFiles` pipe. Cleanup kills exact PIDs only.

1. **Holder SIGKILLed.** The lock is free at once, and the next acquire succeeds after judging the holder gone.
2. **Holder SIGSTOPped.** A 1 s acquire times out with the `kill -CONT` advice.
3. **Grandchild in its own session carrying the markers.**
   - (a) If it inherited the descriptor, the lock stays held until it dies.
   - (b) If it closed the descriptor, acquire refuses, naming it; after it is killed, acquire succeeds.
4. **Holder exits (`os.Exit`) while holding.** The record is unreleased, and the next acquire judges and proceeds.
5. **PID reuse.** A record naming a live PID with a wrong start time is judged "exited; that PID now belongs to a later process".
6. **Go chain.** A child Go process starts a grandchild, which keeps fd 3.
7. **Daemon that closed its descriptors and carries no markers.** The lock is released; this documents the blind spot.
8. **Hung holder.** The timeout advice names `kill <pid>`.
9. **Contradiction.**
10. **Signals.** SIGINT during a held action: children get SIGTERM on their group, the helper dies by SIGINT, and the lock is free with the record released. SIGINT while waiting: exits by SIGINT within 1 s. An ignored SIGINT stays ignored.
11. **Not quiescent.** Leads to `Abandon`, and a child holding the descriptor keeps the lock held.
12. **Lock file replaced mid-hold.** The watchdog cancels the action.
13. **Waiter whose file was unlinked.** It reopens, and there is never a second holder.
14. **Runner exit codes.** 127 and 126 (every case above), 124, 137 with `Signal` set, output overflow, and `Env` nil versus a replaced list.
15. **Filesystem table.**
16. **TypeScript leftover.**
