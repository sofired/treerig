# Go port spike: decision report

## Recommendation

**Go.** Port berth to Go. Gate the first full-port work package on one macOS CI run of the lock layer.

The spike ported a vertical slice to Go and compared it against the TypeScript build:
- **The slice:** the whole command tree, config, the registry, `worktree list`/`urls`/`status`/`down`/`worktree remove`, and the full lock and process lifecycle.
- **Output:** byte-identical to TypeScript everywhere it was compared.
- **Speed:** about 10 to 59 times faster to start, depending on the command, on a sixteenth of the memory. Every command measured saves at least 465 ms.
- **Distribution:** 4 MB static binaries.
- **Lock issues:** the new design fixes three of the four open lock issues (#22, #23, #24). The fourth, #12 (a machine-wide router lock), becomes possible but is only done when the router is ported.
- **Clean break:** Go's own `worktree remove` completely removes a worktree the TypeScript build created, verified against a real Docker daemon.

Every hard gate that can run on Linux passed. The macOS gates cannot run without pushing, which was out of bounds for this spike; they are the one open gate. Two scored targets were missed (code and test size), and the accepted differences exceed their target; see [Scored targets](#scored-targets).

## What was ported

| Area | Go packages | Replaces (TypeScript) |
|---|---|---|
| CLI shell | `internal/cli`, `cmd/berth` | `cli/main.ts` and commander 15 (a hand-written parser, help renderer and "did you mean"; no framework) |
| Config | `internal/config`, `internal/shape` | `config/*`, zod 4 (a zod-compatible validator: same paths, messages and ordering) |
| Registry | `internal/registry`, `internal/files`, `internal/checkoutindex/card`, `internal/layout` | the read path and record writes of `worktree-registry.ts`, `files.ts`, `layout.ts`, `card.ts` (read) |
| Docker | `internal/docker`, `internal/backend`, `internal/worktree` | `docker.ts`, `runtime/compose.ts` (status, down, inventory, verify, reap), `hooks/protocol.ts` (environment) |
| Locks and processes | `internal/lock`, `internal/procid`, `internal/run/osrun`, `internal/lifecycle` | `operation-lock.ts`, and from PR #4 `lock-recovery.ts` and `process-table.ts`; also `process-runner.ts` (execa), `signal-lifecycle.ts`, `mutation.ts`, proper-lockfile |
| Verbs | `internal/commands` | `list`, `urls`, `status`, `down`, `worktree remove`, `config validate`/`show`, the prompt |

**Not ported:**
- the verbs `up`, `index`, `doctor`, `router *`, `worktree unlock` and `dev`: they are stubs that exit 2 and name the TypeScript berth, with help identical to TypeScript's;
- registry `ensure`, the hooks runner, compose override generation, the router and the checkout index.

The spike branched from `main` before the unlock PR (#4) merged, and `main` was merged in afterwards. The Go lock layer ports that PR's holder judgement, as the design required; the `unlock` command itself waits for work package 6.

## The lock redesign

Locks are kernel `flock(2)` locks on the existing target files, with a sidecar holder record (`<target>.holder`). This replaces:
- the mtime heartbeat and the stale windows;
- the 12-second second look and the clock-drift checks;
- the `.recovery` mutex and the `.lock` directories;
- the macOS birth-time workaround.

The design is in [lock-design.md](lock-design.md). How it behaves:
- **A holder that dies frees the lock.** The kernel releases it immediately, even on SIGKILL. The next acquirer finds the record not marked released and judges the old holder by machine, boot, PID namespace, and whether the PID is gone or reused. It then scans for processes still carrying the worktree's markers. If in doubt, it refuses and never steals the lock.
- **Docker children keep the lock.** `docker` and `docker compose` inherit the lock's descriptor, so the lock outlives a killed berth for exactly as long as they run. Hooks do not inherit it, by your decision: a helper a hook left running would otherwise hold the worktree. A standing test fails if anything but the Docker wrapper asks for inheritance.
- **Every command is covered.** One process-wide lifecycle defers SIGINT and SIGTERM for every mutation, including `down` (#23). It proves children have quiesced before releasing, and re-raises the signal so the process dies by it.
- **Failures are handled safely:**
  - If quiescence cannot be proven, berth abandons the lock: it closes the descriptor without marking the record released (#24).
  - A watchdog cancels the running action if its lock is lost (#22). A lost lock is then abandoned, never marked released, since its record may now be someone else's.
  - The router lock can become machine-wide (#12) when the router is ported.
- **Unsafe filesystems are refused.** A control home on NFS, SMB, FUSE/virtiofs or 9p, or on a cluster filesystem whose locks may be local to one client (Lustre, GPFS, CephFS, AFS, Coda, OCFS2, GFS2), is refused, with a message telling the user to point `BERTH_HOME` at a local directory. A two-open self-test catches other filesystems where locking does nothing.
- **A machine with no readable identity** (no machine ID and no boot ID) gets a one-time warning when it takes a lock: if it dies holding one, nothing can later prove it gone.

## Measurements

Measured on an idle 8-core Linux 6.8 x86_64 machine with `go run ./tools/spikebench`: 50 runs after 5 warm-up runs, on identical registered state. That state is three git worktrees registered through the TypeScript build's own registry, plus 17 records for other repositories. The raw results are in [bench.json](bench.json).

| Scenario | TS median | TS p90 | Go median | Go p90 | Saved | TS RSS | Go RSS | Same output |
|---|---:|---:|---:|---:|---:|---:|---:|:---:|
| `--help` | 473 ms | 529 ms | 8.1 ms | 9.4 ms | 465 ms | 105.2 MB | 6.1 MB | yes |
| `config validate` | 492 ms | 552 ms | 8.3 ms | 10.2 ms | 484 ms | 105.8 MB | 6.1 MB | yes |
| `worktree list` | 678 ms | 788 ms | 67.9 ms | 81.7 ms | 611 ms | 116.0 MB | 7.4 MB | yes |
| `worktree list --json` | 744 ms | 825 ms | 69.6 ms | 84.4 ms | 675 ms | 115.9 MB | 7.4 MB | yes |
| `worktree list --all` | 711 ms | 790 ms | 61.9 ms | 73.9 ms | 649 ms | 117.1 MB | 7.5 MB | yes |
| `urls` | 539 ms | 598 ms | 20.8 ms | 24.6 ms | 518 ms | 107.4 MB | 7.4 MB | yes |
| `status` (instant fake docker) | 573 ms | 684 ms | 25.1 ms | 29.9 ms | 548 ms | 108.4 MB | 7.5 MB | yes |

Go's remaining time on `list` is git: one `git worktree list` plus a `rev-parse` per worktree. The TypeScript build pays about 480 ms before it runs any of them.

**How to read the table:**
- TypeScript and Go runs alternate, round by round, and which goes first alternates too. Background load therefore lands on both alike.
- RSS is `ru_maxrss` from `wait4`: the largest single process among the command and the children it waited for, not their sum. For `status` that includes the fake `docker` script.
- The fake `docker` answers instantly. Against a real daemon, both builds pay the same `docker` time on top, so the absolute saving stays and the ratio shrinks.
- The CI `bench` job runs the same tool on every change, but only for information: shared runners are too noisy to gate on.

**Binaries** (`CGO_ENABLED=0 -trimpath -ldflags "-s -w"`; two third-party modules linked):

| Target | Stripped | gzip |
|---|---:|---:|
| linux/amd64 | 3.7 MB | 1.6 MB |
| linux/arm64 | 3.6 MB | 1.5 MB |
| darwin/amd64 | 3.8 MB | 1.6 MB |
| darwin/arm64 | 3.5 MB | 1.5 MB |

**Code size.** Every figure counts code lines only: non-blank and non-comment. With comments, the TypeScript the spike replaces is about 4,700 lines.
- **Go product code: 7,752 lines.** The TypeScript it replaces is about 3,750, so 2.1×.
- **The excess is mostly dependencies reimplemented in Go:** commander's parser and help (655 lines), zod's validator (424), an order-preserving TOML decoder, execa's process handling, and proper-lockfile. Without those, the ratio is about 1.6×.
- **Go test code: 8,646 lines**, against 3,486 lines of TypeScript tests for the same behaviour (2.5×). Go's table-free subtests are verbose. The Go suite also adds tests the TypeScript one never had: real-process lock scenarios, a real-daemon decoy suite, TypeScript-versus-Go parity, and 217 generated oracle files.

**Tests:** 754 Go tests pass under `-race`.
- 717 are unit tests (`go test -race ./...`).
- 37 more need `-tags integration`: real git, real processes and signals, real Docker, and parity runs against the TypeScript build.
- The lock, lifecycle and runner packages also pass at `-count=20 -race`.
- The TypeScript suite still passes unchanged: 565 tests, on `main` as merged in.

## Hard gates

| Gate | Result |
|---|---|
| **Safety:** every decoy refuses removal | **Pass.** Unit tests with a stateful fake daemon, plus `TestDecoysOnARealDaemon` against real Docker. These are all refused before anything is touched: the wrong project, the wrong worktree, a missing project label on a non-image, and a resource in the project carrying none of the ownership labels. Another namespace is never even seen. |
| **Safety:** a TypeScript-era worktree is fully removed | **Pass.** `TestTypeScriptWritesGoRemoves`: the TypeScript build's own modules bring a worktree up against real Docker, then the Go CLI removes it: container, volume, network, home, record and identity marker. The control-home marker is upgraded to the Go format, and the TypeScript `worktree list --all --json` then prints `[]`. |
| **Safety:** no test path steals a lock | **Pass.** No acquisition path removes or bypasses a held lock. |
| **Signals** | **Pass on Linux.** `TestInterruptedMutation`: `worktree remove` and `down`, interrupted by SIGINT or SIGTERM mid-`compose down`, die by that signal. The child's process group is gone, and the lock is free with its record released. Lock and lifecycle signal tests pass at `-count=20`. **macOS: not run.** |
| **macOS:** flock inheritance, `kern.procargs2`, exact start time | **Open.** The code compiles and vets for darwin/amd64 and darwin/arm64 but has never run on a Mac. This is the gate for the first full-port work package. |
| **Parity:** help and parse errors | **Pass.** 71 cases byte-identical to the TypeScript CLI (stdout, stderr, exit code). |
| **Parity:** config issues | **Pass.** 66 cases, identical paths and messages (TOML syntax-error detail excepted). |
| **Parity:** `config show`, `list --json` | **Pass.** Byte-identical, hostile strings included. |
| **Parity:** streams contract | **Pass.** The TypeScript `tests/cli/streams.test.ts`, run against the Go binary: 9 pass, 6 skipped (unported verbs and the shebang check). |
| **Parity:** docker argv transcripts | **Pass.** `TestParityWithTheTypeScriptBuild`: `status`, `urls`, `list`, `down`, `remove` and five failure modes of `remove` run on the same fixture with the same fake `docker`. Stdout, stderr, exit code and every `docker` call match; `remove` alone makes 29. |

## Scored targets

| Measure | Target | Result | |
|---|---|---|---|
| Startup, `--help` / `config validate` | Go median ≤ 20 ms | 8.1 / 8.3 ms | **Met** |
| Startup, `list` / `status` | ≥ 450 ms faster than TypeScript | 611 / 548 ms faster | **Met** |
| Binary size | ≤ 10 MB per target | 3.5–3.8 MB | **Met** |
| Go product code | ≤ 1.6× the TypeScript it replaces | 2.1× | **Missed.** The excess is the reimplemented dependencies above; without them the ratio is about 1.6×. |
| Go tests | ≤ 1.5× the TypeScript tests they replace | 2.5× | **Missed.** The subtests are verbose, and the Go suite has test classes the TypeScript one lacks. Coverage is what grew, not repetition. |
| Remaining port, projected | ≤ 5× the spike's effort | 1–1.5× | **Met** |
| Accepted differences | ≤ 3, each approved by you | 6 | **Missed.** All six are listed below for your approval. Four change only wording (1, 2, 3, 5), one is the prompt's line editing (4), and one is edge cases no current fixture reaches (6). |

## Accepted differences (for your approval)

1. **`BERTH_DEBUG` output.** It prints the error's chain of causes instead of a JavaScript stack trace, which Go does not have. For one of berth's own errors with no cause, it adds nothing.
2. **TOML syntax errors.** The detail after `is not valid TOML:` is BurntSushi's wording, not smol-toml's. The source excerpt with a caret under the error is also missing.
3. **Lock notices and advice.** The heartbeat is gone, so its wording is too: "its last heartbeat was…", the two-look confirmation, and the `rmdir` instructions.
   New wording covers:
   - the processes that currently hold the lock (Linux);
   - `kill -CONT` / `kill <pid>` advice;
   - a refusal when the filesystem breaks locking;
   - a warning when the machine's identity cannot be read.
4. **The confirmation prompt** reads a line through the terminal's own line editing: backspace, Ctrl-U and Ctrl-W work, but arrow keys and history do not. Node's `readline` supports both. The answer is the worktree's hostname, typed exactly, so a typo can still be erased before Enter. An arrow key inserts an escape sequence instead of moving the cursor, and the mismatched answer is refused safely.
5. **Operating-system error wording.** An unexpected file error reads as Go words it (`open /x: permission denied`), not as Node does (`EACCES: permission denied, open '/x'`). This is the same error in the same place; only the words differ.
6. **Edge cases no fixture reaches:**
   - Sorting of non-ASCII service names: byte order instead of UTF-16 order.
   - `Unrecognized key` quoting for a key containing a control character: Go's `%q`.
   - A hook with no `#!` line: 126 in Go, where Node's `execvp` would have run it with `/bin/sh`. Hooks are not ported yet.

## Risk register

| Risk | Status |
|---|---|
| Process lifecycle: sessions, group kills, deferral, re-raising | **Retired on Linux.** Real bugs found and fixed:<br>- The re-raise raced `os.Exit` and exited 130 instead of dying by the signal.<br>- Abandoning a lock cancelled the action's context, which SIGTERMed children that had inherited the lock.<br>- A command that finished uninterrupted left its signal handler registered, which only matters where `Main` runs more than once in a process. |
| Lock correctness under crashes, stops and PID reuse | **Retired on Linux.** Real-process scenarios run at `-count=20`. They cover a grandchild keeping the lock through an inherited descriptor, a Go chain, a survivor that closed it, the contradiction check, and abandoning when quiescence fails. Real bugs found and fixed:<br>- The first lock of a new repository failed its filesystem self-test.<br>- Releasing a lock the watchdog had found lost would have marked another holder's record released. |
| flock on macOS (APFS), inheritance through `docker` | **Open.** Needs one macOS CI run. |
| A daemon with no markers | **Accepted; same as TypeScript.** Suppose a process started under the lock daemonizes out of berth's process tree, closes the lock's descriptor, and carries neither marker (the Compose project as an argv token, or `BERTH_WORKTREE_ID`/`COMPOSE_PROJECT_NAME` in its environment). Recovery cannot see it, and judges the lock free while it runs. The TypeScript marker scan has the same blind spot. |
| Holders in a devcontainer and on the host sharing one control home | **Mitigated by refusal.** A holder recorded from another PID namespace or boot cannot be judged, so the next acquirer refuses rather than guess. Until `worktree unlock` is ported (work package 6), clearing such a lock means removing its `.holder` file by hand, after checking the holder is gone. |
| A machine with no readable identity | **Mitigated by warning.** With no machine ID and no boot ID, a holder that dies cannot be proven gone, so its lock also needs clearing by hand. Berth says so once, when it takes the lock. |
| Byte-identical UI without a CLI framework | **Retired.** |
| zod message parity | **Retired** for the config schema. The review found one more divergence, now fixed: zod's `int()` is a safe integer, with its own "Too big"/"Too small" issue past 2^53−1. Infinity is also rejected as a number. |
| Docker ownership safety | **Retired.** Real daemon. |
| Leaving TypeScript-era state behind | **Retired.** Real daemon. |
| Checkout index byte-identical HTML (CSP hashes, embedded JavaScript, ICU sorting) | **Open.** Not in the spike; it is the next biggest unknown. |
| Router stack verification port | **Open.** Large, but it is deterministic comparison code. |

## Projection

The TypeScript build on `main` is about 6,500 code lines. The spike replaced about 3,750 of them, so about 2,750 remain. The largest pieces are:
- the checkout index (about 800 lines);
- the router (about 570);
- `doctor` (about 460) and `worktree unlock` (about 100);
- the write paths of the registry and the Compose runtime (about 330).

That is about three-quarters the size of what the spike covered, and its hardest parts carry forward: the lock layer, lifecycle, runner and parity harness are done.

The remaining work packages, in order:
1. **Harden and merge the spike.**
2. **Write path.** Registry `ensure`, hooks runner, override generation.
3. **Router.** Protocol 4, plus the machine-wide lock (#12). Also covers #13 and #14.
4. **Checkout index.** Byte-identical HTML and ICU sorting. Covers #15–#18.
5. **`up`/`down` end to end.** The Go binary becomes daily-usable here.
6. **`doctor` and `unlock` on flock,** plus doctor's line for TypeScript-era state.
7. **Parity closure and releases.** Covers #8, and #7 via a binary installed outside the checkout.
8. **Retire TypeScript.** Help and error output then no longer need to match commander. Replace the hand-written commander-compatible parser with spf13/cobra (+0.95 MB, two modules), which deletes most of `internal/cli`'s 655 lines, and re-record the help and error test cases from Go's own output.

The estimate is 1 to 1.5 times the spike's effort, well under the 5× threshold. The checkout index is the widest error bar.

## How to reproduce

```sh
go build -o dist/berth ./cmd/berth
go test -race ./...                                   # unit
go test -race -tags integration ./...                 # real git, processes, signals, Docker, parity (needs node 24)
BERTH_CLI=$PWD/dist/berth BERTH_CLI_UNPORTED=doctor,router,unlock pnpm exec vitest run tests/cli/streams.test.ts
go run ./tools/spikebench -go dist/berth              # timings
go run ./tools/spikebench -sizes                      # binary sizes
```
