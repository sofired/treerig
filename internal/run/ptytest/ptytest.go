//go:build integration

// Package ptytest is the shared machinery for tests that run a program under
// a real pseudo-terminal and play the part of the shell around it: reading
// what it prints without a read deadline (a pty master can't take one on
// macOS), watching its process's wait4 events, and typing Ctrl-C or Ctrl-Z
// at it. internal/run/osrun's interactive tests and internal/cli's `run`
// tests both use it, so the timing hard-won here (see SendTerminalSignal) is
// written down once.
//
// The process under test is always the pty's session leader, started with
// pty.Start, so its pid is also its process group id: every function taking
// a helperPid means that.
package ptytest

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// output is everything read so far from one pty's master side, filled by
// a single background reader. The master can't take a read deadline on
// macOS, where Go's poller doesn't register terminal devices ("file type does
// not support deadline"), so reads block in their own goroutine and callers
// poll the buffer against their own timeout instead.
type output struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	err      error
	consumed int
}

// outputs holds each pty master's output, so every ReadUntil on one
// pty shares the one reader that owns its bytes.
var outputs sync.Map

// outputOf returns ptmx's output, starting its reader on first use. The
// reader ends when the pty's slave side closes or ptmx itself is closed.
func outputOf(ptmx *os.File) *output {
	fresh := &output{}
	value, loaded := outputs.LoadOrStore(ptmx, fresh)
	out := value.(*output)
	if !loaded {
		go func() {
			chunk := make([]byte, 256)
			for {
				n, err := ptmx.Read(chunk)
				out.mu.Lock()
				out.buf.Write(chunk[:n])
				if err != nil {
					out.err = err
				}
				out.mu.Unlock()
				if err != nil {
					return
				}
			}
		}()
	}
	return out
}

// ReadUntil waits up to 10s for want to appear in ptmx's output after
// whatever an earlier ReadUntil on the same pty already matched.
func ReadUntil(t *testing.T, ptmx *os.File, want string) {
	t.Helper()
	out := outputOf(ptmx)
	deadline := time.Now().Add(10 * time.Second)
	for {
		out.mu.Lock()
		unread := out.buf.Bytes()[out.consumed:]
		if i := bytes.Index(unread, []byte(want)); i >= 0 {
			out.consumed += i + len(want)
			out.mu.Unlock()
			return
		}
		seen, err := string(unread), out.err
		out.mu.Unlock()
		if err != nil {
			t.Fatalf("read: %v (saw %q)", err, seen)
		}
		if time.Now().After(deadline) {
			t.Fatalf("never saw %q, only %q", want, seen)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// WaitFile polls for path to exist with non-blank content, returning it
// trimmed — the same synchronisation internal/cli's own signal integration
// tests use for a background helper's marker files.
func WaitFile(t *testing.T, path string) string {
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

// StartHelperWatch reaps the helper process's own next wait4(WUNTRACED)
// event — exited, ended by a signal, or merely stopped — the same call
// runForegroundChild makes for an interactive child, and delivers it on the
// returned channel. The test plays the role of the real shell that would
// otherwise be doing this for berth itself. Wait4 has no timeout of its
// own, so every caller bounds its own wait externally; a goroutine left
// blocked in it past a failed assertion is the same trade the rest of this
// package's tests already make (see osrun_test.go's own "skips the
// SIGKILL" comment) — harmless in a test binary that is about to exit.
func StartHelperWatch(pid int) <-chan unix.WaitStatus {
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

// WaitHelperEvent bounds StartHelperWatch's event at 10s, failing the test
// if none arrives.
func WaitHelperEvent(t *testing.T, pid int) unix.WaitStatus {
	t.Helper()
	select {
	case status := <-StartHelperWatch(pid):
		return status
	case <-time.After(10 * time.Second):
		t.Fatalf("never reaped an event for %d", pid)
		return unix.WaitStatus(0)
	}
}

// WaitForegroundHandoff polls the pty's foreground process group (TIOCGPGRP
// on the master) until it is something other than helperPid — berth's own
// group — and returns that value, understood to be the interactive child's
// own pgid. It is a direct assertion that the handoff itself happened, kept
// separate from SendTerminalSignal's write-retries: those exist for a host
// timing race in delivering a signal to an already-correct foreground group
// (see SendTerminalSignal's own doc), not to paper over berth being slow,
// or wrong, about making that group the foreground in the first place —
// that must fail loudly and promptly, here, rather than be absorbed by a
// handful of retried writes.
func WaitForegroundHandoff(t *testing.T, ptmx *os.File, helperPid int) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if fg := liveForeground(int(ptmx.Fd()), helperPid); fg != 0 {
			return fg
		}
		if time.Now().After(deadline) {
			fg, err := unix.IoctlGetInt(int(ptmx.Fd()), unix.TIOCGPGRP)
			t.Fatalf("terminal foreground pgid never left berth's own group %d (last saw %d, err=%v)", helperPid, fg, err)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// SendTerminalSignal writes b — a control character the pty's line
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
// process exit. WaitForegroundHandoff, above, is what actually keeps this
// loud rather than silently generous: a slow or wrong handoff in berth's
// own code fails there, promptly, before this retry ever runs.
//
// A write is repeated only while the child's group still holds the
// terminal. Once berth has taken it back, the child has already had the
// signal, and a further byte would go to berth's own group instead: under
// -race berth takes about a second to exit, so a byte written in that
// window used to interrupt berth itself, killing it with SIGINT on macOS.
// From then on this only waits for the helper's own event.
func SendTerminalSignal(t *testing.T, ptmx *os.File, helperPid int, b byte) unix.WaitStatus {
	t.Helper()
	events := StartHelperWatch(helperPid)
	const attempts = 60
	const perAttempt = 50 * time.Millisecond
	for i := 0; i < attempts; i++ {
		if i > 0 && !ChildHoldsTerminal(ptmx, helperPid) {
			select {
			case status := <-events:
				return status
			case <-time.After(10 * time.Second):
				t.Fatalf("byte %#x reached %d's child, but %d never reported an event", b, helperPid, helperPid)
				return unix.WaitStatus(0)
			}
		}
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

// ChildHoldsTerminal reports whether the pty's foreground process group is
// still some live group other than berth's own (helperPid): the interactive
// child's. No group at all, an error, or a group nobody is left in means no
// one holds it any more, which also means the child's group has gone. The
// last matters on macOS, which does not report 0 for a terminal whose
// foreground group has emptied but a placeholder (NO_PID, 100000) that names
// no group: taken at face value, it would have the caller keep typing at a
// terminal berth is about to take back.
func ChildHoldsTerminal(ptmx *os.File, helperPid int) bool {
	return liveForeground(int(ptmx.Fd()), helperPid) != 0
}

// liveForeground is the pty's foreground process group when it is a group
// other than helperPid that still has a member, and 0 otherwise.
func liveForeground(fd, helperPid int) int {
	fg, err := unix.IoctlGetInt(fd, unix.TIOCGPGRP)
	if err != nil || fg <= 0 || fg == helperPid {
		return 0
	}
	if errors.Is(unix.Kill(-fg, 0), unix.ESRCH) {
		return 0
	}
	return fg
}

// Transcript is everything ptmx's reader has collected so far, consumed by a
// ReadUntil or not, for a failure message: what the program printed is what
// names the signal or error behind an unexpected exit status. Called once
// the program has exited, it first waits (up to 2s) for the reader to see the
// slave side close, so the last line is not lost to a read still in flight.
func Transcript(ptmx *os.File) string {
	out := outputOf(ptmx)
	deadline := time.Now().Add(2 * time.Second)
	for {
		out.mu.Lock()
		text, done := out.buf.String(), out.err != nil
		out.mu.Unlock()
		if done || time.Now().After(deadline) {
			return text
		}
		time.Sleep(10 * time.Millisecond)
	}
}
