// Package osrun is the operating-system run.Runner: every subprocess berth
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

	"golang.org/x/sys/unix"

	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/lock"
	"github.com/sofired/berth/internal/run"
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

// target is one child this Runner has started, tracked until Quiesce proves
// its whole process group (or, for an interactive child, just its PID) gone.
type target struct {
	pid   int
	group bool
}

// Runner is the operating-system run.Runner.
type Runner struct {
	mu      sync.Mutex
	targets map[target]struct{}
}

// New is a Runner ready to spawn real subprocesses.
func New() *Runner {
	return &Runner{targets: make(map[target]struct{})}
}

var _ run.Runner = (*Runner)(nil)

// track records t (a child's PID or process group) as still running, so
// Quiesce and allGone know to wait for it.
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

	resolved, startErr := resolveCommand(spec.Command, spec.Env)
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
	if !interactive {
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
		stdout = newLimitWriter(run.OutputLimit, overflow)
		stderr = newLimitWriter(run.OutputLimit, overflow)
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

	if spec.InheritLocks {
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
	started := target{pid: cmd.Process.Pid, group: !interactive}
	r.track(started)

	killAfter := spec.KillAfter
	if killAfter <= 0 {
		killAfter = defaultKillAfter
	}

	// cmd.Wait() runs on its own goroutine so this one can select between it
	// and runCtx.Done() without racing either against the other: killTimer
	// is only ever touched from here, never from a second goroutine.
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	done := runCtx.Done()
	var killTimer *time.Timer
	var waitErr error
waitLoop:
	for {
		select {
		case waitErr = <-waitDone:
			break waitLoop
		case <-done:
			done = nil // fire the signal at most once; keep waiting for waitDone
			signalChild(cmd.Process.Pid, !interactive, syscall.SIGTERM)
			killTimer = time.AfterFunc(killAfter, func() {
				signalChild(cmd.Process.Pid, !interactive, syscall.SIGKILL)
			})
		}
	}
	if killTimer != nil {
		killTimer.Stop()
	}
	// Reaped, and its whole group with it: nothing left for Quiesce to wait
	// on. Kept only while something it started lingers in the group — and
	// never left to linger, lest its PID be reused and read as still running.
	r.untrackIfGone(started)

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
		exitCode, signal := exitStatus(cmd.ProcessState, waitErr)
		result.ExitCode = exitCode
		if signal != 0 {
			result.Signal = signal
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

// resolveCommand finds the executable ourselves — rather than leaning on
// os/exec's own lookup, which always reads this process's PATH — so a
// caller-supplied Env is honored for the lookup too, and so the exact
// "not on PATH" wording is ours to give before ever attempting to spawn.
func resolveCommand(command string, env []string) (string, *run.StartError) {
	if strings.Contains(command, "/") {
		info, err := os.Stat(command)
		if err != nil {
			return "", &run.StartError{
				Command: command, Code: "ENOENT",
				Detail:   "does not exist — or the interpreter on its #! line is missing, as when the file has CRLF line endings (ENOENT).",
				ExitCode: 127,
			}
		}
		if info.IsDir() || unix.Access(command, unix.X_OK) != nil {
			return "", &run.StartError{Command: command, Code: "EACCES", Detail: "is not executable (EACCES).", ExitCode: 126}
		}
		return command, nil
	}
	path := pathEnv(env)
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			dir = "."
		}
		candidate := filepath.Join(dir, command)
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() {
			continue
		}
		if unix.Access(candidate, unix.X_OK) != nil {
			continue
		}
		return candidate, nil
	}
	return "", &run.StartError{Command: command, Code: "ENOENT", Detail: "is not on PATH (ENOENT).", ExitCode: 127}
}

// pathEnv reads PATH out of env, the environment a spawned command resolves
// against; a nil env means the command inherits ours, so PATH comes from the
// real environment instead.
func pathEnv(env []string) string {
	if env == nil {
		return os.Getenv("PATH")
	}
	for _, entry := range env {
		if strings.HasPrefix(entry, "PATH=") {
			return entry[len("PATH="):]
		}
	}
	return ""
}

// classifyExecError names what a Start failure discovered only at the real
// exec(2) call — chiefly ENOEXEC, since resolveCommand already checked
// existence and the execute bit.
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

// signalChild sends sig to the child's whole process group (for a session
// leader) or to its bare PID (for an interactive child, left in this
// process's own session).
func signalChild(pid int, group bool, sig syscall.Signal) {
	target := pid
	if group {
		target = -pid
	}
	_ = syscall.Kill(target, sig)
}

// Which reports whether an executable regular file by that name is on PATH.
func (r *Runner) Which(name string) bool {
	_, err := resolveCommand(name, nil)
	return err == nil
}

// Quiesce returns once every child this Runner has started has been reaped
// and kill(-pgid, 0) answers ESRCH for each group (or kill(pid, 0) for an
// interactive child), bounded at about 15 s.
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

// gone reports whether a child's process group (or, interactive, its PID) has no process left.
func gone(t target) bool {
	probe := t.pid
	if t.group {
		probe = -t.pid
	}
	return errors.Is(syscall.Kill(probe, 0), syscall.ESRCH)
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
