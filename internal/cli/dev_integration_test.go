//go:build integration

package cli_test

// `dev` (#5), through the real binary: the fake docker the `up` tests use,
// whose `compose logs` follows until signalled or, on request, ends at once.
// Signals are sent for real; the terminal test runs the binary under a real
// pty, playing the shell around it.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/run/ptytest"
)

// developmentLock is the development-session lock of the fixture's one
// registration.
func (f upFixture) developmentLock(t *testing.T) string {
	t.Helper()
	read := func(name string) string {
		data, err := os.ReadFile(filepath.Join(f.checkout, ".git", "berth", name))
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(data))
	}
	return layout.WorktreeLayout(f.controlHome, read("repository-id"), read("worktree-id")).DevelopmentLockPath
}

// startDev starts `berth dev` in f's checkout and returns once the log
// follower is running, with the berth process and its stdout and stderr as
// they go. A cleanup registered on t kills whatever the test left behind,
// the follower's grandchild included.
func startDev(t *testing.T, f upFixture, binary string) (cmd *exec.Cmd, stdout, stderr *syncBuffer) {
	t.Helper()
	cmd = f.command(binary, "dev")
	stdout, stderr = &syncBuffer{}, &syncBuffer{}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		for _, name := range []string{"hung.pid", "follower.pid"} {
			if data, err := os.ReadFile(filepath.Join(f.state, name)); err == nil {
				if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
					_ = unix.Kill(pid, syscall.SIGKILL)
				}
			}
		}
		_ = cmd.Wait()
	})
	waitForFile(t, filepath.Join(f.state, "follower.pid"))
	return cmd, stdout, stderr
}

// waitForOutput polls out until it contains want, failing the test if it
// never does within 20 seconds.
func waitForOutput(t *testing.T, out *syncBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !strings.Contains(out.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("%q never appeared in %q", want, out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestDevThroughTheBinary(t *testing.T) {
	binary := buildBinary(t)

	t.Run("starts the worktree as up does, follows its logs, and holds the development lock alone meanwhile", func(t *testing.T) {
		f := newUpFixture(t)
		_, stdout, stderr := startDev(t, f, binary)

		waitForOutput(t, stdout, "api-1  | following")
		for _, line := range []string{
			"[berth] Starting all services for feature-login.acme.localhost.\n",
			"[berth] feature-login.acme.localhost is up.\n",
			"[berth] Following the logs of feature-login.acme.localhost. Press Ctrl-C to end the session.\n",
		} {
			if !strings.Contains(stderr.String(), line) {
				t.Fatalf("stderr = %q, want it to contain %q", stderr.String(), line)
			}
		}
		if got := f.stages(t); strings.Join(got, " ") != "prepare seed" {
			t.Fatalf("stages = %v", got)
		}
		if !strings.Contains(stdout.String(), "api   http://feature-login.acme.localhost:18080\n") {
			t.Fatalf("stdout = %q, want the URL table", stdout.String())
		}
		var starts, follows []string
		for _, call := range f.calls(t) {
			switch {
			case strings.HasPrefix(call, "compose ") && strings.Contains(call, " up ") && !strings.Contains(call, "--project-name berth-router "):
				starts = append(starts, call)
			case strings.HasPrefix(call, "compose ") && strings.Contains(call, " logs "):
				follows = append(follows, call)
			}
		}
		if len(starts) != 1 {
			t.Fatalf("worktree starts = %q, want one", starts)
		}
		if len(follows) != 1 || !strings.HasSuffix(follows[0], " logs --follow --tail 100") || !strings.Contains(follows[0], "--project-name berth-") {
			t.Fatalf("log followers = %q, want one `logs --follow --tail 100` on the worktree's project", follows)
		}
		if !lockHeld(t, f.developmentLock(t)) {
			t.Fatal("the development lock is free during the session")
		}
		if lockHeld(t, f.worktreeLock(t)) {
			t.Fatal("the operation lock is still held during the session")
		}
	})

	t.Run("a second dev, and worktree remove, refuse while a session is attached, and up and down still work", func(t *testing.T) {
		f := newUpFixture(t)
		startDev(t, f, binary)

		for _, args := range [][]string{{"dev"}, {"worktree", "remove", "--yes"}} {
			_, stderr, code := runFixture(t, f, binary, nil, args...)
			if code == 0 {
				t.Fatalf("%v succeeded during a session", args)
			}
			for _, want := range []string{"feature-login development session", `"berth dev"`, "End it with Ctrl-C in the terminal running `berth dev`, then retry."} {
				if !strings.Contains(stderr, want) {
					t.Fatalf("%v: stderr = %q, want it to name %q", args, stderr, want)
				}
			}
		}
		if _, err := os.Stat(filepath.Join(f.checkout, ".git", "berth", "worktree-id")); err != nil {
			t.Fatalf("the refused removal took the identity: %v", err)
		}
		for _, args := range [][]string{{"up"}, {"down"}} {
			if _, stderr, code := runFixture(t, f, binary, nil, args...); code != 0 {
				t.Fatalf("%v during a session: exit %d\n%s", args, code, stderr)
			}
		}
		if !lockHeld(t, f.developmentLock(t)) {
			t.Fatal("the development lock was lost to another verb")
		}
	})

	t.Run("a failing up fails dev the same way, and no logs are followed", func(t *testing.T) {
		f := newUpFixture(t)
		if err := os.WriteFile(filepath.Join(f.state, "up-fails"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		_, upErr, upCode := runFixture(t, f, binary, nil, "up")
		_, devErr, devCode := runFixture(t, f, binary, nil, "dev")
		if upCode == 0 || devCode != upCode {
			t.Fatalf("up exits %d, dev exits %d, want the same non-zero code", upCode, devCode)
		}
		if !strings.Contains(upErr, "boom: cannot start") || !strings.Contains(devErr, "boom: cannot start") {
			t.Fatalf("up said %q, dev said %q, want both to carry Compose's reason", upErr, devErr)
		}
		for _, call := range f.calls(t) {
			if strings.Contains(call, " logs ") {
				t.Fatalf("logs were followed after a failed up: %q", call)
			}
		}
		requireReleased(t, f.developmentLock(t))
		requireReleased(t, f.worktreeLock(t))
	})

	t.Run("exits non-zero with a message, and releases the lock, when the follower ends on its own", func(t *testing.T) {
		for _, tc := range []struct {
			exit     string
			wantCode int
			wantText string
		}{
			{"7", 7, "The log follower of feature-login.acme.localhost ended on its own (exit 7), so the development session is over; \"berth status\" shows what is running."},
			{"0", 1, "The log follower of feature-login.acme.localhost ended on its own (exit 0), usually because none of its containers is left running, so the development session is over; \"berth status\" shows what is running."},
		} {
			f := newUpFixture(t)
			if err := os.WriteFile(filepath.Join(f.state, "logs-exit"), []byte(tc.exit), 0o600); err != nil {
				t.Fatal(err)
			}
			stdout, stderr, code := runFixture(t, f, binary, nil, "dev")
			if code != tc.wantCode {
				t.Fatalf("follower exit %s: dev exits %d, want %d\n%s", tc.exit, code, tc.wantCode, stderr)
			}
			if !strings.Contains(stderr, tc.wantText) {
				t.Fatalf("follower exit %s: stderr = %q, want it to contain %q", tc.exit, stderr, tc.wantText)
			}
			if !strings.Contains(stdout, "api-1  | following") {
				t.Fatalf("stdout = %q, want the follower's output", stdout)
			}
			requireReleased(t, f.developmentLock(t))
			requireReleased(t, f.worktreeLock(t))
		}
	})
}

// A signal stops the log follower's process group, and the process dies by
// that signal only once it is gone, with the development lock released.
func TestDevInterrupted(t *testing.T) {
	binary := buildBinary(t)
	// A signal ignored when berth starts stays ignored; catching them here
	// hands berth the defaults whatever this test was started with, as
	// TestRunInterrupted does.
	caught := make(chan os.Signal, 1)
	signal.Notify(caught, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	t.Cleanup(func() { signal.Stop(caught) })
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(fmt.Sprintf("dies by %v, with the follower's group gone and both locks released", sig), func(t *testing.T) {
			f := newUpFixture(t)
			cmd, _, stderr := startDev(t, f, binary)
			follower, _ := strconv.Atoi(waitForFile(t, filepath.Join(f.state, "follower.pid")))
			hung, _ := strconv.Atoi(waitForFile(t, filepath.Join(f.state, "hung.pid")))

			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			err := cmd.Wait()
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("exit = %v\n%s", err, stderr.String())
			}
			if status := exit.Sys().(syscall.WaitStatus); !status.Signaled() || status.Signal() != sig {
				t.Fatalf("status = %v, want death by %v\n%s", status, sig, stderr.String())
			}
			for _, pid := range []int{follower, hung} {
				if alive(pid) {
					t.Fatalf("the follower's group survived: %d is alive", pid)
				}
			}
			notice := fmt.Sprintf("[berth] The development session of feature-login.acme.localhost ended (%s). Its services are still running; \"berth down\" stops them.\n", clierr.SignalName(sig))
			if !strings.Contains(stderr.String(), notice) {
				t.Fatalf("stderr = %q, want the notice %q", stderr.String(), notice)
			}
			requireReleased(t, f.developmentLock(t))
			requireReleased(t, f.worktreeLock(t))
		})
	}
}

// Ctrl-C at a terminal goes to the follower's own foreground group, not to
// berth: the session ends, with the documented status, and the lock is free.
func TestDevOnATerminal(t *testing.T) {
	binary := buildBinary(t)

	t.Run("Ctrl-C ends the session with exit 130 once the follower's group is gone, and the lock is released", func(t *testing.T) {
		f := newUpFixture(t)
		ptmx, pid := startBerthOnTerminal(t, f, binary, "dev")
		ptytest.ReadUntil(t, ptmx, "api-1  | following")
		follower, _ := strconv.Atoi(waitForFile(t, filepath.Join(f.state, "follower.pid")))
		hung, _ := strconv.Atoi(waitForFile(t, filepath.Join(f.state, "hung.pid")))
		ptytest.WaitForegroundHandoff(t, ptmx, pid)
		if !lockHeld(t, f.developmentLock(t)) {
			t.Fatal("the development lock is free during the session")
		}

		status := ptytest.SendTerminalSignal(t, ptmx, pid, 0x03) // Ctrl-C
		if !status.Exited() || status.ExitStatus() != 128+int(syscall.SIGINT) {
			t.Fatalf("status = %+v, want exit %d\nberth printed: %q", status, 128+int(syscall.SIGINT), ptytest.Transcript(ptmx))
		}
		ptytest.ReadUntil(t, ptmx, `[berth] The development session of feature-login.acme.localhost ended (SIGINT). Its services are still running; "berth down" stops them.`)
		if transcript := ptytest.Transcript(ptmx); strings.Contains(transcript, "ERROR") {
			t.Fatalf("a session the user ended was reported as an error: %q", transcript)
		}
		for _, pid := range []int{follower, hung} {
			if alive(pid) {
				t.Fatalf("the follower's group survived: %d is alive", pid)
			}
		}
		requireReleased(t, f.developmentLock(t))
		requireReleased(t, f.worktreeLock(t))
	})
}
