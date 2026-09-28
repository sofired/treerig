//go:build integration

package osrun

// Real terminals, real job control: these tests run an interactive child
// under a real pty, exactly as issue #104 asks. Since fd 0's terminal-ness
// and foreground group are properties of *this process*, not something a
// mock can stand in for, the "berth" side of each scenario is this same
// test binary, re-executed under BERTH_PTY_HELPER=1 (a package-scoped
// TestMain, the same pattern os/exec's own tests use for a helper process)
// so it runs with the pty as its own real fd 0 and reports what
// osrun.New().Run() did to a result file, since the pty's own stream is
// busy carrying the interactive child's own terminal session.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"

	"github.com/sofired/berth/internal/lifecycle"
	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/run"
)

// argSep joins BERTH_PTY_ARGS entries: a unit separator, so an argument
// containing a space or comma still round-trips exactly.
const argSep = "\x1f"

// TestMain lets this test binary also serve as the "berth" stand-in these
// tests run under a pty: with BERTH_PTY_HELPER=1 it runs one
// osrun.New().Run(Output: Interactive) call and exits, never entering the
// normal test machinery.
func TestMain(m *testing.M) {
	if os.Getenv("BERTH_PTY_HELPER") == "1" {
		os.Exit(ptyHelperMain())
	}
	os.Exit(m.Run())
}

// ptyResult is what a helper process reports about its one Run call, read
// back by the test that started it.
type ptyResult struct {
	ExitCode int `json:"exitCode"`
	Signal   int `json:"signal"`
	// Foreground is fd 0's foreground process group as this helper itself
	// sees it right after Run returns — read from inside, since checking it
	// from outside after the helper (a session leader) has actually exited
	// is unreliable: exiting a session's leader can itself reset the
	// terminal's foreground group, which would prove nothing about whether
	// Run gave the terminal back before that happened.
	Foreground int    `json:"foreground"`
	Err        string `json:"err"`
}

// ptyHelperMain is the whole of the "berth" stand-in: a real Lifecycle,
// wired the way #104 asks — its Repeated() channel carried on the context
// via run.WithRepeated, never through a second signal.Notify — running one
// interactive child, reporting the result to BERTH_PTY_RESULT, and finally
// routing through Lifecycle.Finish exactly as internal/cli's runMain does:
// a richer error (an abandoned or timed-out process-group drain) printed
// to this process's own stderr — the pty, same as everything else here —
// with the exit code it calls for, or a plain interruption re-raised as
// the signal that caused it, same as the real CLI.
func ptyHelperMain() int {
	var args []string
	if raw := os.Getenv("BERTH_PTY_ARGS"); raw != "" {
		args = strings.Split(raw, argSep)
	}
	var killAfter time.Duration
	if raw := os.Getenv("BERTH_PTY_KILLAFTER"); raw != "" {
		killAfter, _ = time.ParseDuration(raw)
	}

	lc := lifecycle.NewLifecycle(lifecycle.New(), logger.Silent{})
	lc.Start()
	ctx := run.WithRepeated(lc.Context(), lc.Repeated())

	result, runErr := New().Run(ctx, run.Spec{
		// AllowFailure: an interactive session's own exit code or death by
		// signal is not berth's failure to report — Failure's "Command
		// failed (…)" wrapping is for a command whose success or failure
		// berth itself judges, and every test below reads the child's own
		// outcome from Result, not from runErr.
		Command: os.Getenv("BERTH_PTY_CMD"), Args: args, Output: run.Interactive, KillAfter: killAfter,
		AllowFailure: true,
	})

	foreground, _ := unix.IoctlGetInt(0, unix.TIOCGPGRP)
	res := ptyResult{ExitCode: result.ExitCode, Signal: int(result.Signal), Foreground: foreground}
	if runErr != nil {
		res.Err = runErr.Error()
	}
	data, err := json.Marshal(res)
	if err != nil {
		return 1
	}
	if err := os.WriteFile(os.Getenv("BERTH_PTY_RESULT"), data, 0o600); err != nil {
		return 1
	}

	return lc.Finish(runErr, func(e error) { fmt.Fprintln(os.Stderr, e.Error()) })
}

// helperEnv is the environment one BERTH_PTY_HELPER=1 run of this same
// binary needs: which command to run interactively, and where to leave its
// result.
func helperEnv(resultPath, command string, args []string, killAfter time.Duration) []string {
	env := append(os.Environ(),
		"BERTH_PTY_HELPER=1",
		"BERTH_PTY_RESULT="+resultPath,
		"BERTH_PTY_CMD="+command,
		"BERTH_PTY_ARGS="+strings.Join(args, argSep),
	)
	if killAfter > 0 {
		env = append(env, "BERTH_PTY_KILLAFTER="+killAfter.String())
	}
	return env
}

// startPTYHelper launches this test binary, in helper mode, under a fresh
// pty — the "berth" stand-in for one command run interactively — and
// returns the pty's master end, the helper's PID (which, since pty.Start
// makes it a session and process-group leader of its own, is also its own
// pgid), and the path its result will land in.
func startPTYHelper(t *testing.T, command string, args []string, killAfter time.Duration) (ptmx *os.File, helperPid int, resultPath string) {
	t.Helper()
	resultPath = filepath.Join(t.TempDir(), "result.json")
	cmd := exec.Command(os.Args[0])
	cmd.Env = helperEnv(resultPath, command, args, killAfter)

	var err error
	ptmx, err = pty.Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	helperPid = cmd.Process.Pid
	t.Cleanup(func() { _ = ptmx.Close() })
	// A helper this test leaves running (a failed assertion, or a berth bug
	// that never returns) must not outlive the test: its own process group,
	// since anything it started as an interactive child leads a group of
	// its own, distinct from the helper's.
	t.Cleanup(func() { _ = syscall.Kill(-helperPid, syscall.SIGKILL) })
	return ptmx, helperPid, resultPath
}

// readUntil reads from ptmx until want has appeared in what it has seen so
// far, failing the test if it does not within a short deadline —
// synchronising with the helper's child without a fixed sleep.
func readUntil(t *testing.T, ptmx *os.File, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	if err := ptmx.SetReadDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	var seen bytes.Buffer
	chunk := make([]byte, 256)
	for !bytes.Contains(seen.Bytes(), []byte(want)) {
		n, err := ptmx.Read(chunk)
		seen.Write(chunk[:n])
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("never saw %q, only %q", want, seen.String())
			}
			t.Fatalf("read: %v (saw %q)", err, seen.String())
		}
	}
}

// waitFile polls for path to exist with non-blank content, returning it
// trimmed — the same synchronisation internal/cli's own signal integration
// tests use for a background helper's marker files.
func waitFile(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(data))) > 0 {
			return strings.TrimSpace(string(data))
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", path)
	return ""
}

// readResult polls resultPath for a helper's JSON report, failing the test
// if none appears in time.
func readResult(t *testing.T, resultPath string) ptyResult {
	t.Helper()
	data := waitFile(t, resultPath)
	var report ptyResult
	if err := json.Unmarshal([]byte(data), &report); err != nil {
		t.Fatalf("result file %s: %v (%q)", resultPath, err, data)
	}
	return report
}

// startHelperWatch reaps the helper process's own next wait4(WUNTRACED)
// event — exited, ended by a signal, or merely stopped — the same call
// runForegroundChild makes for an interactive child, and delivers it on the
// returned channel. The test plays the role of the real shell that would
// otherwise be doing this for berth itself. Wait4 has no timeout of its
// own, so every caller bounds its own wait externally; a goroutine left
// blocked in it past a failed assertion is the same trade the rest of this
// package's tests already make (see osrun_test.go's own "skips the
// SIGKILL" comment) — harmless in a test binary that is about to exit.
func startHelperWatch(pid int) <-chan unix.WaitStatus {
	events := make(chan unix.WaitStatus, 1)
	go func() {
		for {
			var status unix.WaitStatus
			wpid, err := unix.Wait4(pid, &status, unix.WUNTRACED, nil)
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if err != nil || wpid == pid {
				events <- status
				return
			}
		}
	}()
	return events
}

// waitHelperEvent bounds startHelperWatch's event at 10s, failing the test
// if none arrives.
func waitHelperEvent(t *testing.T, pid int) unix.WaitStatus {
	t.Helper()
	select {
	case status := <-startHelperWatch(pid):
		return status
	case <-time.After(10 * time.Second):
		t.Fatalf("never reaped an event for %d", pid)
		return unix.WaitStatus(0)
	}
}

// waitForegroundHandoff polls the pty's foreground process group (TIOCGPGRP
// on the master) until it is something other than helperPid — berth's own
// group — and returns that value, understood to be the interactive child's
// own pgid. It is a direct assertion that the handoff itself happened, kept
// separate from sendTerminalSignal's write-retries: those exist for a host
// timing race in delivering a signal to an already-correct foreground group
// (see sendTerminalSignal's own doc), not to paper over berth being slow,
// or wrong, about making that group the foreground in the first place —
// that must fail loudly and promptly, here, rather than be absorbed by a
// handful of retried writes.
func waitForegroundHandoff(t *testing.T, ptmx *os.File, helperPid int) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		fg, err := unix.IoctlGetInt(int(ptmx.Fd()), unix.TIOCGPGRP)
		if err == nil && fg != 0 && fg != helperPid {
			return fg
		}
		if time.Now().After(deadline) {
			t.Fatalf("terminal foreground pgid never left berth's own group %d (last saw %d, err=%v)", helperPid, fg, err)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// sendTerminalSignal writes b — a control character the pty's line
// discipline turns into a signal for its foreground process group, such as
// Ctrl-C or Ctrl-Z — and returns once helperPid has answered with a wait4
// event, retrying the write if none arrives within a short interval.
//
// The retry is load-bearing, not defensive filler: on this host, a byte
// written this way can be lost if it lands in the narrow window right
// after the foreground child's own fork+exec of what it runs next — this
// reproduces identically with no berth code involved at all (a bare
// pty.Start(exec.Command("sh", "-c", "echo ready; cat")), Ctrl-C written
// the instant "ready" is read, loses the byte on a clean host too;
// https://github.com/sofired/berth/issues/104). It is a characteristic of
// this kernel/shell/scheduling combination reacting to a signal-generating
// byte sent immediately after the previous command's own fork, not a berth
// defect — a human pressing Ctrl-C or Ctrl-Z is never this fast, and once
// the signal is actually delivered, berth's own handling of it (proven by
// the rest of this file) is correct. Retrying the write is how a real
// terminal emulator's own user would "solve" the same race by construction
// (they cannot type faster than this).
//
// The budget below is wider than the kernel-level race itself needs (tens
// of milliseconds is typically enough) because this test binary runs under
// -race, and a -race binary's process exit itself lags os.Exit by about a
// second — measured here directly, on the helper this retry is waiting on:
// attempts tight enough for the race alone (10 × 50 ms) reproducibly
// starved before that unrelated exit latency had even elapsed once, and the
// observed ceiling across repeated runs was about 1.12 s. Nothing to do
// with host contention or load; the race detector's own bookkeeping on
// process exit. waitForegroundHandoff, above, is what actually keeps this
// loud rather than silently generous: a slow or wrong handoff in berth's
// own code fails there, promptly, before this retry ever runs.
func sendTerminalSignal(t *testing.T, ptmx *os.File, helperPid int, b byte) unix.WaitStatus {
	t.Helper()
	events := startHelperWatch(helperPid)
	const attempts = 60
	const perAttempt = 50 * time.Millisecond
	for i := 0; i < attempts; i++ {
		if _, err := ptmx.Write([]byte{b}); err != nil {
			t.Fatal(err)
		}
		select {
		case status := <-events:
			return status
		case <-time.After(perAttempt):
		}
	}
	t.Fatalf("byte %#x had no effect on %d after %d attempts (%s total)", b, helperPid, attempts, attempts*perAttempt)
	return unix.WaitStatus(0)
}

func TestInteractivePTYCtrlC(t *testing.T) {
	ptmx, helperPid, resultPath := startPTYHelper(t, "sh", []string{"-c", "echo ready; cat"}, 2*time.Second)
	readUntil(t, ptmx, "ready")
	waitForegroundHandoff(t, ptmx, helperPid)

	status := sendTerminalSignal(t, ptmx, helperPid, 0x03) // Ctrl-C
	if !status.Exited() || status.ExitStatus() != 0 {
		t.Fatalf("helper did not exit cleanly after its child's Ctrl-C: %+v", status)
	}
	report := readResult(t, resultPath)
	if report.Signal != int(syscall.SIGINT) {
		t.Fatalf("child's reported signal = %d, want SIGINT (%d): %+v", report.Signal, syscall.SIGINT, report)
	}
	// The shell gets the terminal back: berth's own group — its own pid,
	// since pty.Start made it lead both its session and its process group —
	// is fd 0's foreground group again, not left with the now-dead child's.
	if report.Foreground != helperPid {
		t.Fatalf("terminal foreground group = %d, want berth's own %d back: %+v", report.Foreground, helperPid, report)
	}
}

func TestInteractivePTYPromptReadsInput(t *testing.T) {
	ptmx, helperPid, resultPath := startPTYHelper(t, "sh", []string{"-c", `echo ready; read line; echo "got:$line"`}, 2*time.Second)
	readUntil(t, ptmx, "ready")

	if _, err := ptmx.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	readUntil(t, ptmx, "got:hello")

	status := waitHelperEvent(t, helperPid)
	if !status.Exited() || status.ExitStatus() != 0 {
		t.Fatalf("helper did not exit cleanly: %+v", status)
	}
	if report := readResult(t, resultPath); report.ExitCode != 0 {
		t.Fatalf("got %+v, want a clean exit", report)
	}
}

func TestInteractivePTYCtrlZThenFgResumes(t *testing.T) {
	ptmx, helperPid, resultPath := startPTYHelper(t, "sh", []string{"-c", "echo ready; read _; echo done"}, 2*time.Second)
	readUntil(t, ptmx, "ready")
	waitForegroundHandoff(t, ptmx, helperPid)

	status := sendTerminalSignal(t, ptmx, helperPid, 0x1a) // Ctrl-Z
	if !status.Stopped() {
		t.Fatalf("helper did not stop when its child did: %+v", status)
	}

	// The terminal is already berth's own group's again: runForegroundChild
	// reclaims it (setForeground) before stopSelf. This test process is not
	// itself in the pty's session (it only holds the master end), so unlike
	// a real outer shell it cannot call tcsetpgrp on this terminal at all —
	// ENOTTY, since TIOCSPGRP requires the caller's own controlling
	// terminal. `fg` here is just the SIGCONT a real shell would also send.
	if err := syscall.Kill(-helperPid, syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}

	if _, err := ptmx.Write([]byte("\n")); err != nil {
		t.Fatal(err)
	}
	readUntil(t, ptmx, "done")

	status = waitHelperEvent(t, helperPid)
	if !status.Exited() || status.ExitStatus() != 0 {
		t.Fatalf("helper did not exit cleanly after resuming: %+v", status)
	}
	if report := readResult(t, resultPath); report.ExitCode != 0 {
		t.Fatalf("got %+v, want a clean exit", report)
	}
}

func TestInteractivePTYGrandchildIgnoringSIGTERMIsKilledAfterTheTimer(t *testing.T) {
	dir := t.TempDir()
	grandchild := filepath.Join(dir, "grandchild")
	// Ignores SIGTERM outright, so only SIGKILL — sent after KillAfter —
	// can end it: proves drainGroup escalates rather than waiting forever.
	if err := os.WriteFile(grandchild, []byte("#!/bin/sh\ntrap '' TERM\necho $$ > \"$1\"\nsleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(dir, "grandchild.pid")
	killAfter := 300 * time.Millisecond

	// The leader backgrounds the grandchild and exits, leaving it as the
	// only thing left in the group for drainGroup to deal with — but only
	// once the grandchild's own trap is actually installed: drainGroup
	// SIGTERMs the group the instant the leader is gone, and a bare fork
	// between "&" and "trap" would otherwise lose the race to the default
	// disposition. The leader's own busy-wait for the pid file is exactly
	// that handshake.
	ptmx, helperPid, resultPath := startPTYHelper(t, "sh",
		[]string{"-c", `echo ready; "$0" "$1" & while [ ! -s "$1" ]; do :; done; exit 0`, grandchild, pidFile},
		killAfter)
	readUntil(t, ptmx, "ready")

	grandchildPid, err := strconv.Atoi(waitFile(t, pidFile))
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	status := waitHelperEvent(t, helperPid)
	elapsed := time.Since(start)
	if !status.Exited() || status.ExitStatus() != 0 {
		t.Fatalf("helper did not exit cleanly: %+v", status)
	}
	if elapsed < killAfter-50*time.Millisecond {
		t.Fatalf("returned after %s, before KillAfter (%s) could have escalated to SIGKILL", elapsed, killAfter)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("took %s to drain one grandchild — want well under Quiesce's own 15 s budget", elapsed)
	}

	if err := unix.Kill(grandchildPid, 0); !errors.Is(err, unix.ESRCH) {
		t.Fatalf("grandchild %d still exists after the run returned (Kill: %v)", grandchildPid, err)
	}
	if report := readResult(t, resultPath); report.ExitCode != 0 {
		t.Fatalf("got %+v, want the leader's own clean exit", report)
	}
}

// TestInteractivePTYAbandonedDrainReportsAndExits1 pins #104's round-1 fix:
// an abandoned drain's message must reach the user, not be lost to
// Lifecycle.Exit's own signal death (see internal/lifecycle's Finish and
// internal/cli's runMain, which this helper's own tail exercises exactly).
// A lingering grandchild that ignores SIGTERM keeps drainGroup's wait open
// well past killAfter (a minute away) and Quiesce's own 15 s budget, so two
// real signals sent to the helper's own pid — not through the pty, the way
// something outside berth's own terminal session would signal it — land
// during the drain and abandon it: SIGINT then SIGTERM, distinct signal
// numbers, since two of the same in quick succession are not guaranteed to
// arrive as two separate deliveries at all (POSIX signals of one number
// are not queued past the first still pending).
func TestInteractivePTYAbandonedDrainReportsAndExits1(t *testing.T) {
	dir := t.TempDir()
	grandchild := filepath.Join(dir, "grandchild")
	if err := os.WriteFile(grandchild, []byte("#!/bin/sh\ntrap '' TERM\necho $$ > \"$1\"\nsleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(dir, "grandchild.pid")
	leaderPidFile := filepath.Join(dir, "leader.pid")
	// An hour, not the default 10 s or even the grandchild test's own
	// 300 ms: KillAfter's own escalation must not be what ends this test,
	// the second signal must be, and this proves it — an assertion this
	// test would otherwise not be making at all.
	const killAfter = time.Hour

	ptmx, helperPid, resultPath := startPTYHelper(t, "sh",
		[]string{"-c", `echo $$ > "$2"; echo ready; "$0" "$1" & while [ ! -s "$1" ]; do :; done; exit 0`, grandchild, pidFile, leaderPidFile},
		killAfter)
	readUntil(t, ptmx, "ready")

	leaderPid, err := strconv.Atoi(waitFile(t, leaderPidFile))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := strconv.Atoi(waitFile(t, pidFile)); err != nil {
		t.Fatal(err)
	}
	// The grandchild, in the leader's own group, outlives an abandoned
	// drain by design — this test's whole point — so it needs its own
	// cleanup; startPTYHelper's already kills the helper's own group,
	// which the leader (Setpgid) never joined.
	t.Cleanup(func() { _ = syscall.Kill(-leaderPid, syscall.SIGKILL) })

	// The leader is reaped only once the grandchild's pid file exists (its
	// own busy-wait loop, above), which is also proof drainGroup has
	// started polling a group that will not clear on its own.
	deadline := time.Now().Add(5 * time.Second)
	for !errors.Is(unix.Kill(leaderPid, 0), unix.ESRCH) {
		if time.Now().After(deadline) {
			t.Fatal("leader was never reaped")
		}
		time.Sleep(2 * time.Millisecond)
	}

	if err := syscall.Kill(helperPid, syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(helperPid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}

	status := waitHelperEvent(t, helperPid)
	if !status.Exited() || status.ExitStatus() != 1 {
		t.Fatalf("helper did not exit 1 after an abandoned drain: %+v", status)
	}
	// The message reaching the user is the whole point: Lifecycle.Finish
	// must have reported it before whatever ended this process, rather
	// than the drain's own error being lost to a signal death that prints
	// nothing of its own.
	readUntil(t, ptmx, "abandoned after a second interrupt")

	report := readResult(t, resultPath)
	if !strings.Contains(report.Err, "abandoned after a second interrupt") {
		t.Fatalf("result file's own error = %q, want it to name the abandon too", report.Err)
	}
}

// TestInteractiveNonTerminalStdin runs the helper with no pty at all — its
// stdin is /dev/null, exec.Cmd's own answer for a nil Stdin — proving an
// interactive child still runs to completion (the Setpgid-alone branch,
// cmd.Wait() rather than runForegroundChild) instead of hanging on a
// terminal that was never there. No /proc dependence, and nothing pty- or
// job-control-specific, so this one holds equally on Linux and macOS.
func TestInteractiveNonTerminalStdin(t *testing.T) {
	resultPath := filepath.Join(t.TempDir(), "result.json")
	cmd := exec.Command(os.Args[0])
	cmd.Env = helperEnv(resultPath, "sh", []string{"-c", "echo hi"}, 0)
	cmd.Stdin = nil // exec.Cmd gives a nil Stdin /dev/null: never a terminal
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output

	done := make(chan error, 1)
	go func() { done <- cmd.Run() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("helper failed: %v\noutput: %s", err, output.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("helper hung with stdin not a terminal")
	}

	if report := readResult(t, resultPath); report.ExitCode != 0 {
		t.Fatalf("got %+v, want a clean exit", report)
	}
	if !strings.Contains(output.String(), "hi") {
		t.Fatalf("the child's own output is missing from the helper's stdout: %q", output.String())
	}
}

// TestDrainGroupAbandonedBySecondSignal exercises drainGroup directly,
// bypassing the pty and Lifecycle machinery the tests above go through: a
// lingering grandchild that ignores SIGTERM, and a repeated channel closed
// by hand to stand in for a real second Ctrl-C or SIGTERM, proving the wait
// gives up early — reporting the group, not just running out its full
// 15 s budget — rather than only ever timing out.
func TestDrainGroupAbandonedBySecondSignal(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "stubborn")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntrap '' TERM\nsleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })

	repeated := make(chan struct{})
	close(repeated) // stands in for "a second Ctrl-C or SIGTERM already arrived"
	ctx := run.WithRepeated(context.Background(), repeated)

	start := time.Now()
	err := drainGroup(ctx, pid, "stubborn", time.Minute)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected the abandoned drain to report an error")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("took %s to give up after a repeated signal, want near-immediate", elapsed)
	}
	if !strings.Contains(err.Error(), "stubborn") {
		t.Fatalf("error does not name the group: %v", err)
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}
