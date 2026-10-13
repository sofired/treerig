package osrun

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/run"
	"github.com/sofired/berth/internal/text"
)

// weHoldForeground reports whether this process's own process group is
// currently fd's foreground group — the condition under which an
// interactive child can be handed the terminal outright and atomically, via
// SysProcAttr.Foreground, rather than merely getting a process group of its
// own.
func weHoldForeground(fd int) bool {
	return foregroundGroupIs(fd, unix.Getpgrp())
}

// foregroundGroupIs reports whether pgid is currently fd's foreground
// group; false if fd is not a terminal.
func foregroundGroupIs(fd, pgid int) bool {
	pgrp, err := unix.IoctlGetInt(fd, unix.TIOCGPGRP)
	if err != nil {
		return false
	}
	return pgrp == pgid
}

// reclaimFrom takes fd 0's foreground back for ourPgid unless another live
// group holds it. Once the shell has resumed berth in the background (`bg`
// after a Ctrl-Z), the terminal belongs to the shell, and setForeground,
// which ignores SIGTTOU, would otherwise take it from the shell. The
// terminal is still taken back from pid's own group, or from a group that no
// longer exists: macOS clears the foreground group once its last member is
// gone, so after the child exits it may name no group at all.
func reclaimFrom(pid, ourPgid int) {
	fg, err := unix.IoctlGetInt(0, unix.TIOCGPGRP)
	if err != nil || fg == ourPgid {
		return
	}
	if fg == pid || errors.Is(unix.Kill(-fg, 0), unix.ESRCH) {
		_ = setForeground(0, ourPgid)
	}
}

// interactiveAttr is the SysProcAttr an interactive child starts with: given
// the terminal outright (its own process group, made fd 0's foreground
// group as part of the same fork+exec, atomically) when fd 0 is a terminal
// this process currently holds the foreground of, or just its own process
// group otherwise — a pipe, CI, or a job already running in the background.
// foreground reports which one was chosen, since that also decides how Run
// waits for this child.
func interactiveAttr() (attr *syscall.SysProcAttr, foreground bool) {
	if stdinIsTerminal() && weHoldForeground(0) {
		return &syscall.SysProcAttr{Setpgid: true, Foreground: true, Ctty: 0}, true
	}
	return &syscall.SysProcAttr{Setpgid: true}, false
}

// foregroundMu serializes the part of an interactive, foreground child's run
// that touches SIGTTOU's process-wide disposition (setForeground) and this
// process's own terminal foreground: package-level, not per-Runner, since
// the disposition it guards is process-wide too — only one such child may
// be mid-flight in this whole process at a time, however many Runners
// exist. A non-interactive or non-foreground child never touches it. Held
// for the whole of one foreground child's run in osrun.Run, not just around
// each individual setForeground call.
var foregroundMu sync.Mutex

// setForeground makes pgid fd's foreground process group. tcsetpgrp raises
// SIGTTOU on a caller that is not already the terminal's foreground group —
// which this process always is, right here, since every call site is
// reclaiming the terminal from a child it started or handing it back to
// one. Ignoring the signal's disposition for the call, rather than adding a
// second listener via signal.Notify, is the point: internal/lifecycle is
// the only thing in this process allowed to do that (see AGENTS.md). Ignore
// and Reset are process-wide, not per-goroutine, so this is safe only with
// foregroundMu held for the whole of one interactive, foreground child's
// run — enforced in Run, not here. If SIGTTOU was already ignored before
// this call (some parent shell's own choice), that disposition is left
// exactly alone: it is never this call's place to un-ignore it.
func setForeground(fd, pgid int) error {
	if !signal.Ignored(syscall.SIGTTOU) {
		signal.Ignore(syscall.SIGTTOU)
		defer signal.Reset(syscall.SIGTTOU)
	}
	return unix.IoctlSetPointerInt(fd, unix.TIOCSPGRP, pgid)
}

// stopSelf suspends this whole process with SIGSTOP, which — unlike
// SIGTSTP — can never be caught, blocked or ignored, and returns once
// something outside this process (typically the real shell that launched
// berth, answering the user's own `fg`) resumes it with SIGCONT. This is
// how job control expects a foreground process to answer its own child's
// Ctrl-Z: by actually stopping, the way any other job would. No signal
// package call is involved on this path either way.
func stopSelf() {
	_ = syscall.Kill(os.Getpid(), syscall.SIGSTOP)
}

// forwardSignal is the signal Run sends a still-running child when its own
// context ends: for an interactive child, the one berth itself received —
// SIGINT stays SIGINT — so a shell answers it the way it would under a real
// terminal. Anything else (a hook, Docker, a timeout, an output overflow)
// keeps the plain SIGTERM contract regardless of what stopped berth.
func forwardSignal(cause error, interactive bool) syscall.Signal {
	if interactive {
		var cancelled *clierr.Cancelled
		if errors.As(cause, &cancelled) && cancelled.Signal != syscall.SIGHUP {
			return cancelled.Signal
		}
	}
	return syscall.SIGTERM
}

// forwardToChild sends an interactive child's group the signal berth
// received (forwardSignal), then SIGCONT: a child stopped by SIGTTIN or
// SIGTSTP (berth backgrounded on a terminal, say) would otherwise sit on the
// pending signal until its kill-after ran out.
func forwardToChild(pid int, cause error) {
	signalChild(pid, forwardSignal(cause, true))
	signalChild(pid, syscall.SIGCONT)
}

// waitStopped reports whether status is a stop, whatever signal caused it.
// WaitStatus.Stopped can't be used for this: on macOS and the other BSDs it
// is false for a stop by SIGSTOP, which Go reports as Continued instead.
// Both kernels encode a stop the same way, 0x7f in the low byte with the
// signal above it; Linux's own continued status, 0xffff, doesn't match.
func waitStopped(status unix.WaitStatus) bool {
	return status&0xff == 0x7f
}

// foregroundEvent is one state change wait4 reaped for a foreground child:
// either it stopped (Ctrl-Z), or it is gone for good (err is set only if
// the wait itself failed, which normally only happens if something else
// already reaped this pid).
type foregroundEvent struct {
	status unix.WaitStatus
	err    error
}

// watchForeground reaps pid on its own goroutine with its own wait4(pid,
// WUNTRACED) loop, since that blocking call must run concurrently with
// runForegroundChild selecting on ctx and the events it produces. It quits
// once pid is truly gone (exited or ended by a signal); a mere stop keeps it
// going, since the next event — resumed, then eventually gone — still needs
// reaping.
func watchForeground(pid int) <-chan foregroundEvent {
	events := make(chan foregroundEvent)
	go func() {
		for {
			var status unix.WaitStatus
			var wpid int
			var err error
			for {
				wpid, err = unix.Wait4(pid, &status, unix.WUNTRACED, nil)
				if !errors.Is(err, unix.EINTR) {
					break
				}
			}
			if err != nil {
				events <- foregroundEvent{err: err}
				return
			}
			if wpid != pid {
				continue
			}
			events <- foregroundEvent{status: status}
			if status.Exited() || status.Signaled() {
				return
			}
			// Stopped: loop for the next event, which is this child's
			// eventual resume-and-continue-running, not reported here
			// since WUNTRACED alone never reports WCONTINUED.
		}
	}()
	return events
}

// runForegroundChild waits for pid — an interactive child holding the
// terminal's foreground — with its own wait4(pid, WUNTRACED) loop instead
// of cmd.Wait(), because only WUNTRACED can see a stop: cmd.Wait() blocks
// straight through Ctrl-Z. ourPgid is this process's own process group,
// reclaimed as fd 0's foreground on every path out while the child's group
// still holds it (reclaimFrom). A stop hands the
// terminal back to berth and stops this whole process with it, the way a
// real shell answers Ctrl-Z on a job it started; the resume (SIGCONT from
// outside, typically the real shell answering `fg`) hands the terminal back
// to pid's own group and continues it. Cancelling runCtx forwards the
// signal berth received to pid's group once, escalating to SIGKILL after
// killAfter the way the rest of this Runner already does for every child.
// killed reports that a second SIGINT or SIGTERM cut that grace short with
// SIGKILL to the group, which the caller's drain then need not treat as a
// request to abandon its wait.
func runForegroundChild(runCtx context.Context, pid, ourPgid int, killAfter time.Duration) (exitCode int, sig syscall.Signal, killed bool) {
	events := watchForeground(pid)
	done := runCtx.Done()
	// Enabled once the first signal has been forwarded: a second SIGINT or
	// SIGTERM while the child is still within its kill-after grace ends the
	// grace at once (never a SIGHUP, which the lifecycle does not count).
	var repeated <-chan struct{}
	var killTimer *time.Timer
	defer func() {
		if killTimer != nil {
			killTimer.Stop()
		}
	}()
	for {
		select {
		case ev := <-events:
			switch {
			// wait4 fails here only if something else already reaped pid —
			// unreachable in practice, since Run never calls cmd.Wait() on
			// this path and nothing else in this process holds its
			// *os.Process. Kept as a defensive fallback rather than a
			// panic: a plain failure, the same 1-and-no-signal exitStatus
			// falls back to for a nil ProcessState.
			case ev.err != nil:
				reclaimFrom(pid, ourPgid)
				return 1, 0, killed
			case waitStopped(ev.status):
				// A stop reported after the run was cancelled (the child's
				// own SIGTERM handler stopping itself, say) is shutdown, not
				// job control: berth's own signal is already being
				// answered, and stopping berth now would leave it and the
				// lock stuck until someone typed fg. Resume the child's group
				// so it can finish and skip the terminal dance.
				if runCtx.Err() != nil {
					_ = syscall.Kill(-pid, syscall.SIGCONT)
					continue
				}
				reclaimFrom(pid, ourPgid)
				stopSelf()
				// Resumed. After `fg`, berth holds the terminal again and
				// hands it back to the child's own group (its pgid equals
				// pid, since it led its own group). After `bg`, the shell
				// keeps it, and the child just continues in the
				// background: a read from the terminal stops it again, and
				// berth with it, until the next `fg`.
				if weHoldForeground(0) {
					_ = setForeground(0, pid)
				}
				_ = syscall.Kill(-pid, syscall.SIGCONT)
			case ev.status.Exited():
				reclaimFrom(pid, ourPgid)
				return ev.status.ExitStatus(), 0, killed
			case ev.status.Signaled():
				reclaimFrom(pid, ourPgid)
				s := ev.status.Signal()
				return 128 + int(s), s, killed
			}
		case <-done:
			done = nil // fire the forwarded signal at most once
			forwardToChild(pid, context.Cause(runCtx))
			killTimer = time.AfterFunc(killAfter, func() { signalChild(pid, syscall.SIGKILL) })
			repeated = run.Repeated(runCtx)
		case <-repeated:
			repeated = nil
			killed = true
			signalChild(pid, syscall.SIGKILL)
		}
	}
}

// drainBudget is how long drainGroup waits in all: quiesceBudget, or a
// little past the SIGKILL when killAfter is longer than that allows for.
// A command configured to get a minute to stop gracefully (`kill-after` of
// a [commands] entry, #94) must not be given up on at 15 seconds, before the
// SIGKILL it asked to wait for has even been sent.
//
// killed says the group has already been sent SIGKILL (a second signal cut
// the leader's grace short): nothing is left to wait out but the reaping, so
// the budget is quiesceBudget whatever killAfter is.
func drainBudget(killAfter time.Duration, killed bool) time.Duration {
	if killed {
		return quiesceBudget
	}
	return max(quiesceBudget, killAfter+drainGrace)
}

// drainGrace is how long past SIGKILL drainGroup keeps waiting for the group
// to be reaped; the default killAfter (10 s) plus this is quiesceBudget.
const drainGrace = 5 * time.Second

// drainGroup returns once pid's whole process group is gone — SIGTERMing
// whatever it still holds at once, SIGKILLing it after killAfter, and
// giving up after budget, which callers take from drainBudget(killAfter,
// killed) (quiesceBudget for the
// default killAfter, the same bound Quiesce uses, and always quiesceBudget
// once a second signal has already SIGKILLed the group) with an error
// naming the command and pgid — since an interactive child's descendants
// must never outlive the call that started them, whether or not berth
// itself was ever signalled. A second Ctrl-C or SIGTERM, read off ctx via
// run.Repeated so this package never calls signal.Notify itself, abandons
// that wait early instead of running out the full budget, with its own,
// differently worded error.
func drainGroup(ctx context.Context, pid int, label string, killAfter time.Duration, killed bool, budget time.Duration) error {
	first := syscall.SIGTERM
	if killed {
		first = syscall.SIGKILL
	}
	signalChild(pid, first)
	killTimer := time.AfterFunc(killAfter, func() { signalChild(pid, syscall.SIGKILL) })
	defer killTimer.Stop()

	// Clamped and made printable once, here, rather than at every call
	// site: this is the only place either message is built. label names the
	// child ("apply command"), never its arguments or an absolute path.
	label = text.OneLine(label, 80)

	// The second signal that already SIGKILLed the group (killed) was this
	// wait's abandon signal too, spent: SIGKILL is not something to give up
	// on, only to wait out.
	repeated := run.Repeated(ctx)
	if killed {
		repeated = nil
	}
	deadline := time.Now().Add(budget)
	for {
		if gone(target{pid: pid}) {
			return nil
		}
		if time.Now().After(deadline) {
			return clierr.Newf("The %s left processes running (process group %d) after %s; stop them with kill -KILL -%d.", label, pid, budget, pid)
		}
		select {
		case <-repeated:
			return clierr.Newf("The %s left processes running (process group %d), and waiting for them was abandoned after a second interrupt; stop them with kill -KILL -%d.", label, pid, pid)
		case <-time.After(quiesceInterval):
		}
	}
}
