// Package osrun is the operating-system run.Runner: every subprocess treerig
// spawns for real goes through here — a session per child, and its own
// SIGTERM-then-SIGKILL timer.
package osrun

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/lock"
	"github.com/sofired/treerig/internal/run"
	"golang.org/x/sys/unix"
)

// waitDelay bounds how long Wait spends waiting for a descendant that kept
// the output pipe open after the direct child has already exited.
const waitDelay = 2 * time.Second

// defaultKillAfter is how long a SIGTERMed child gets before SIGKILL when
// Spec.KillAfter is zero.
const defaultKillAfter = 10 * time.Second

// quiesceInterval is how often Quiesce polls; quiesceBudget bounds the whole wait.
const (
	quiesceInterval = 50 * time.Millisecond
	quiesceBudget   = 15 * time.Second
)

// errTimedOut and errOutputOverflow are internal cancellation causes: they
// tell Run why its own derived context ended, distinct from the caller's ctx
// ending for a reason of its own (a signal, say).
var (
	errTimedOut       = errors.New("run: timed out")
	errOutputOverflow = errors.New("run: output overflow")
)

// target is one child this Runner has started, tracked by its process group
// (every child leads one: Setsid for a regular child, Setpgid for an
// interactive one) until Quiesce or drainGroup proves that group empty.
type target struct {
	pid int
}

// Runner is the operating-system run.Runner.
type Runner struct {
	mu      sync.Mutex
	targets map[target]struct{}
	// drainBudget, when set, replaces drainBudget as how long a drain of a
	// child's process group waits in all: for tests that need the drain to
	// give up without waiting out quiesceBudget.
	drainBudget func(killAfter time.Duration, killed bool) time.Duration
}

// New is a Runner ready to spawn real subprocesses.
func New() *Runner {
	return &Runner{targets: make(map[target]struct{})}
}

var _ run.Runner = (*Runner)(nil)

// track records t (a child's process group) as still running, so Quiesce and
// allGone know to wait for it.
func (r *Runner) track(t target) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.targets[t] = struct{}{}
}

// Run implements run.Runner.
func (r *Runner) Run(ctx context.Context, spec run.Spec) (run.Result, error) {
	if err := ctx.Err(); err != nil {
		return run.Result{}, context.Cause(ctx)
	}

	if startErr := checkDir(spec.Dir); startErr != nil {
		startErr.Command = spec.Command
		return run.Result{}, startErr
	}

	resolved, startErr := run.ResolveExecutable(spec.Command, spec.Env)
	if startErr != nil {
		return run.Result{}, startErr
	}

	cmd := &exec.Cmd{
		Path: resolved,
		Args: append([]string{spec.Command}, spec.Args...),
		Dir:  spec.Dir,
	}
	if spec.Env != nil {
		cmd.Env = spec.Env
	}
	interactive := spec.Output == run.Interactive
	var foreground bool
	switch {
	case interactive:
		cmd.SysProcAttr, foreground = interactiveAttr()
		if foreground {
			// Held for the whole of this child's run, not just Start: every
			// tcsetpgrp call this run makes (taking the terminal for the
			// child, reclaiming it on a stop or an exit, handing it back on
			// a resume) shares SIGTTOU's process-wide disposition with
			// setForeground. Package-level, not per-Runner: the disposition
			// it guards is process-wide too, so only one foreground child
			// may be mid-flight in this whole process at a time, regardless
			// of how many Runners exist. See foregroundMu's own doc.
			foregroundMu.Lock()
			defer foregroundMu.Unlock()
		}
	default:
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	}
	cmd.WaitDelay = waitDelay

	var overflowOnce sync.Once
	runCtx, cancelCause := context.WithCancelCause(ctx)
	defer cancelCause(nil)
	overflow := func() {
		overflowOnce.Do(func() { cancelCause(errOutputOverflow) })
	}

	var stdout, stderr *limitWriter
	switch {
	case interactive:
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	default:
		limit := run.OutputLimit
		if spec.OutputLimit > 0 {
			limit = spec.OutputLimit
		}
		stdout = newLimitWriter(limit, overflow)
		stderr = newLimitWriter(limit, overflow)
		switch spec.Output {
		case run.Tee:
			cmd.Stdout = io.MultiWriter(os.Stdout, stdout)
			cmd.Stderr = io.MultiWriter(os.Stderr, stderr)
		case run.TeeToStderr:
			cmd.Stdout = io.MultiWriter(os.Stderr, stdout)
			cmd.Stderr = io.MultiWriter(os.Stderr, stderr)
		default: // run.Capture
			cmd.Stdout = stdout
			cmd.Stderr = stderr
		}
		if spec.Input != nil {
			cmd.Stdin = bytes.NewReader(spec.Input)
		}
	}

	// Interactive wins over InheritLocks (see run.Spec.InheritLocks): an
	// interactive child's whole process group is drained before Run returns,
	// so the lock it could otherwise carry past treerig's own exit is never
	// wanted, and a caller that asked for both gets the drain.
	if spec.InheritLocks && !interactive {
		cmd.ExtraFiles = lock.Inherited(ctx)
	}

	var timeoutTimer *time.Timer
	if spec.Timeout > 0 {
		timeoutTimer = time.AfterFunc(spec.Timeout, func() { cancelCause(errTimedOut) })
		defer timeoutTimer.Stop()
	}

	if err := cmd.Start(); err != nil {
		return run.Result{}, classifyExecError(spec.Command, err)
	}
	// Captured once, and used in place of cmd.Process.Pid from here on:
	// Process.Release (below, for a foreground child) sets Process.Pid to
	// -1 — a documented historical wart of os.Process, not a nil-out this
	// code can rely on being absent.
	started := target{pid: cmd.Process.Pid}
	r.track(started)

	killAfter := spec.KillAfter
	if killAfter <= 0 {
		killAfter = defaultKillAfter
	}

	var leaderExitCode int
	var leaderSignal syscall.Signal
	var killed bool
	var waitErr error
	if foreground {
		// Its own wait4(pid, WUNTRACED) loop, not cmd.Wait(): only WUNTRACED
		// can see this child stop, which it must for Ctrl-Z to work.
		leaderExitCode, leaderSignal, killed = runForegroundChild(runCtx, started.pid, unix.Getpgrp(), killAfter)
		_ = cmd.Process.Release() // reaped above; os/exec must never wait for it too
	} else {
		// cmd.Wait() runs on its own goroutine so this one can select
		// between it and runCtx.Done() without racing either against the
		// other: killTimer is only ever touched from here, never from a
		// second goroutine.
		waitDone := make(chan error, 1)
		go func() { waitDone <- cmd.Wait() }()

		done := runCtx.Done()
		var repeated <-chan struct{}
		var killTimer *time.Timer
	waitLoop:
		for {
			select {
			case waitErr = <-waitDone:
				break waitLoop
			case <-done:
				done = nil // fire the signal at most once; keep waiting for waitDone
				if interactive {
					forwardToChild(started.pid, context.Cause(runCtx))
					// A second SIGINT or SIGTERM ends the kill-after grace
					// at once; a hook keeps its plain timer.
					repeated = run.Repeated(runCtx)
				} else {
					signalChild(started.pid, forwardSignal(context.Cause(runCtx), false))
				}
				killTimer = time.AfterFunc(killAfter, func() {
					signalChild(started.pid, syscall.SIGKILL)
				})
			case <-repeated:
				repeated = nil
				killed = true
				signalChild(started.pid, syscall.SIGKILL)
			}
		}
		// A timer that had already fired has SIGKILLed the group, so the
		// drain below must not start a second grace period on top of it.
		if killTimer != nil && !killTimer.Stop() {
			killed = true
		}
	}

	// An interactive child's descendants must never outlive this call,
	// whether or not treerig itself was ever signalled: SIGTERM (then SIGKILL
	// after killAfter) whatever the group still holds, and do not return
	// until it is gone or this wait is itself abandoned. So must those of a
	// child stopped for its timeout: "timed out and was terminated" is not
	// true while a grandchild that ignored the SIGTERM runs on. A
	// non-interactive child that ended on its own leaves what it left
	// deliberately — Docker left running, say — and that stays Quiesce's
	// job alone.
	//
	// When the drain fails (something in the group survives even SIGKILL for
	// the whole budget, or a second interrupt abandons the wait) Run returns
	// that error and no Result, so it replaces TimedOut: a caller never sees
	// "timed out" for a run whose group is still alive. The wait after a
	// timeout is at most about killAfter, or killAfter plus the drain
	// budget's grace when the group ignores SIGKILL's reaping: the main
	// loop's kill timer has fired by then, so the drain starts with SIGKILL
	// rather than a second SIGTERM and a fresh grace.
	var drainErr error
	if interactive || errors.Is(context.Cause(runCtx), errTimedOut) {
		label := spec.Label
		if label == "" {
			label = filepath.Base(spec.Command) + " command"
		}
		budget := drainBudget(killAfter, killed)
		if r.drainBudget != nil {
			budget = r.drainBudget(killAfter, killed)
		}
		drainErr = drainGroup(ctx, started.pid, label, killAfter, killed, budget)
	}
	// Reaped, and its whole group with it: nothing left for Quiesce to wait
	// on. Kept only while something it started lingers in the group — and
	// never left to linger, lest its PID be reused and read as still running.
	r.untrackIfGone(started)

	if drainErr != nil {
		return run.Result{}, drainErr
	}

	// The caller's own context ending is the story here, not a Result: a
	// signal deferred no lock past this point, and Failure would otherwise
	// bury it under "Command failed", which is not what happened.
	if err := ctx.Err(); err != nil {
		return run.Result{}, context.Cause(ctx)
	}

	result := run.Result{}
	if stdout != nil {
		result.Stdout = stdout.String()
	}
	if stderr != nil {
		result.Stderr = stderr.String()
	}

	switch cause := context.Cause(runCtx); {
	case errors.Is(cause, errOutputOverflow):
		result.OutputOverflow = true
		result.ExitCode = 1
	case errors.Is(cause, errTimedOut):
		result.TimedOut = true
		result.ExitCode = 1
	default:
		exitCode, sig := leaderExitCode, leaderSignal
		if !foreground {
			exitCode, sig = exitStatus(cmd.ProcessState, waitErr)
		}
		result.ExitCode = exitCode
		if sig != 0 {
			result.Signal = sig
		}
	}

	return run.Finish(spec, result)
}

// checkDir is a working-directory check done up front because a
// SysProcAttr on the child skips os/exec's.
func checkDir(dir string) *run.StartError {
	if dir == "" {
		return nil
	}
	info, err := os.Stat(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &run.StartError{Code: "ENOENT", Detail: fmt.Sprintf("has no working directory: %s does not exist (ENOENT).", dir), ExitCode: 126}
		}
		return &run.StartError{Detail: fmt.Sprintf("has no working directory: %s (%s).", dir, err), ExitCode: 126}
	}
	if !info.IsDir() {
		return &run.StartError{Code: "ENOTDIR", Detail: fmt.Sprintf("has no working directory: %s is not a directory (ENOTDIR).", dir), ExitCode: 126}
	}
	return nil
}

// classifyExecError names what a Start failure discovered only at the real
// exec(2) call — chiefly ENOEXEC, since run.ResolveExecutable already
// checked existence and the execute bit.
func classifyExecError(command string, err error) *run.StartError {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return &run.StartError{Command: command, Detail: fmt.Sprintf("failed to spawn: %s.", strings.TrimSuffix(err.Error(), ".")), ExitCode: 126}
	}
	switch errno {
	case syscall.ENOEXEC:
		return &run.StartError{Command: command, Code: "ENOEXEC", Detail: "is not a valid executable (ENOEXEC) — a script needs a #! line.", ExitCode: 126}
	case syscall.EACCES:
		return &run.StartError{Command: command, Code: "EACCES", Detail: "is not executable (EACCES).", ExitCode: 126}
	case syscall.ENOENT:
		detail := "is not on PATH (ENOENT)."
		if strings.Contains(command, "/") {
			detail = "does not exist — or the interpreter on its #! line is missing, as when the file has CRLF line endings (ENOENT)."
		}
		return &run.StartError{Command: command, Code: "ENOENT", Detail: detail, ExitCode: 127}
	default:
		return &run.StartError{Command: command, Code: errno.Error(), Detail: fmt.Sprintf("failed to spawn (%s).", errno), ExitCode: 126}
	}
}

// exitStatus reads the child's exit code and terminating signal, if any, off
// its ProcessState: 128+n for a child a signal ended, as a shell reports it.
func exitStatus(state *os.ProcessState, waitErr error) (exitCode int, signal syscall.Signal) {
	if state == nil {
		return 1, 0
	}
	if ws, ok := state.Sys().(syscall.WaitStatus); ok {
		if ws.Signaled() {
			return 128 + int(ws.Signal()), ws.Signal()
		}
		return ws.ExitStatus(), 0
	}
	return state.ExitCode(), 0
}

// signalChild sends sig to pid's whole process group: every child this
// Runner starts leads one.
func signalChild(pid int, sig syscall.Signal) {
	_ = syscall.Kill(-pid, sig)
}

// Which reports whether an executable regular file by that name is on PATH.
func (r *Runner) Which(name string) bool {
	_, err := run.ResolveExecutable(name, nil)
	return err == nil
}

// Quiesce returns once every child this Runner has started has been reaped
// and kill(-pgid, 0) answers ESRCH for each one's process group, bounded at
// about 15 s.
func (r *Runner) Quiesce(ctx context.Context) error {
	deadline := time.Now().Add(quiesceBudget)
	for {
		if r.allGone() {
			return nil
		}
		if time.Now().After(deadline) {
			r.mu.Lock()
			n := len(r.targets)
			r.mu.Unlock()
			return clierr.Newf("%d process group(s) still running after %s", n, quiesceBudget)
		}
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-time.After(quiesceInterval):
		}
	}
}

// untrackIfGone drops t from the tracked set once nothing is left of it, so
// a later Quiesce does not keep waiting on a child that already exited.
func (r *Runner) untrackIfGone(t target) {
	if gone(t) {
		r.mu.Lock()
		delete(r.targets, t)
		r.mu.Unlock()
	}
}

// gone reports whether a child's process group has no process left in it.
func gone(t target) bool {
	return errors.Is(syscall.Kill(-t.pid, 0), syscall.ESRCH)
}

// allGone reports whether every tracked target has exited, untracking each
// one it finds gone along the way.
func (r *Runner) allGone() bool {
	r.mu.Lock()
	targets := make([]target, 0, len(r.targets))
	for t := range r.targets {
		targets = append(targets, t)
	}
	r.mu.Unlock()

	allGone := true
	for _, t := range targets {
		if !gone(t) {
			allGone = false
			continue
		}
		r.mu.Lock()
		delete(r.targets, t)
		r.mu.Unlock()
	}
	return allGone
}
