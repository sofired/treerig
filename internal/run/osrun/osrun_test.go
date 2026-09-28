package osrun

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sofired/berth/internal/lock"
	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/run"
)

// scratch returns a fresh directory for a test's spawned commands to run in.
func scratch(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// withPath sets PATH to dir for the duration of the test, restoring whatever
// PATH held (or clearing it, if it was unset) on cleanup.
func withPath(t *testing.T, dir string) {
	t.Helper()
	previous, had := os.LookupEnv("PATH")
	os.Setenv("PATH", dir)
	t.Cleanup(func() {
		if had {
			os.Setenv("PATH", previous)
		} else {
			os.Unsetenv("PATH")
		}
	})
}

func TestRunnerStartFailures(t *testing.T) {
	t.Run("says a missing executable could not be started", func(t *testing.T) {
		withPath(t, scratch(t))
		_, err := New().Run(context.Background(), run.Spec{Command: "berth-definitely-missing-tool"})
		se, ok := err.(*run.StartError)
		if !ok {
			t.Fatalf("got %T: %v", err, err)
		}
		if se.ExitCode != 127 || se.Code != "ENOENT" {
			t.Fatalf("got %+v", se)
		}
		if err.Error() != "`berth-definitely-missing-tool` could not be started: it is not on PATH (ENOENT)." {
			t.Fatalf("got %q", err.Error())
		}
	})

	t.Run("says a file that is not executable could not be started", func(t *testing.T) {
		dir := scratch(t)
		script := filepath.Join(dir, "hook")
		if err := os.WriteFile(script, []byte("#!/bin/sh\necho hi\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := New().Run(context.Background(), run.Spec{Command: script})
		se, ok := err.(*run.StartError)
		if !ok {
			t.Fatalf("got %T: %v", err, err)
		}
		if se.ExitCode != 126 || se.Code != "EACCES" {
			t.Fatalf("got %+v", se)
		}
		want := "`" + script + "` could not be started: it is not executable (EACCES)."
		if err.Error() != want {
			t.Fatalf("got %q, want %q", err.Error(), want)
		}
	})

	t.Run("says a bare name found on PATH without its execute bit is not executable, not missing", func(t *testing.T) {
		dir := scratch(t)
		if err := os.WriteFile(filepath.Join(dir, "berth-noexec-tool"), []byte("#!/bin/sh\necho hi\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		withPath(t, dir)
		_, err := New().Run(context.Background(), run.Spec{Command: "berth-noexec-tool"})
		se, ok := err.(*run.StartError)
		if !ok {
			t.Fatalf("got %T: %v", err, err)
		}
		if se.ExitCode != 126 || se.Code != "EACCES" {
			t.Fatalf("got %+v", se)
		}
	})

	t.Run("names a working directory that does not exist, on one line", func(t *testing.T) {
		gone := filepath.Join(scratch(t), "gone")
		_, err := New().Run(context.Background(), run.Spec{Command: "sh", Args: []string{"-c", "true"}, Dir: gone})
		se, ok := err.(*run.StartError)
		if !ok {
			t.Fatalf("got %T: %v", err, err)
		}
		want := "`sh` could not be started: it has no working directory: " + gone + " does not exist (ENOENT)."
		if err.Error() != want {
			t.Fatalf("got %q, want %q", err.Error(), want)
		}
		if se.ExitCode != 126 {
			t.Fatalf("ExitCode = %d, want 126", se.ExitCode)
		}
	})

	t.Run("blames a working directory that is a file the same way", func(t *testing.T) {
		file := filepath.Join(scratch(t), "berth.toml")
		if err := os.WriteFile(file, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := New().Run(context.Background(), run.Spec{Command: "sh", Args: []string{"-c", "true"}, Dir: file})
		want := "`sh` could not be started: it has no working directory: " + file + " is not a directory (ENOTDIR)."
		if err == nil || err.Error() != want {
			t.Fatalf("got %v, want %q", err, want)
		}
	})

	t.Run("tells a script without a #! line what it is missing", func(t *testing.T) {
		dir := scratch(t)
		script := filepath.Join(dir, "seed")
		if err := os.WriteFile(script, []byte("not a script\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		_, err := New().Run(context.Background(), run.Spec{Command: script})
		se, ok := err.(*run.StartError)
		if !ok {
			t.Fatalf("got %T: %v", err, err)
		}
		if se.ExitCode != 126 || se.Code != "ENOEXEC" {
			t.Fatalf("got %+v", se)
		}
		want := "`" + script + "` could not be started: it is not a valid executable (ENOEXEC) — a script needs a #! line."
		if err.Error() != want {
			t.Fatalf("got %q, want %q", err.Error(), want)
		}
	})
}

func TestRunnerExitCodes(t *testing.T) {
	t.Run("still returns a plain exit code for a command that ran and failed", func(t *testing.T) {
		result, err := New().Run(context.Background(), run.Spec{Command: "sh", Args: []string{"-c", "exit 3"}, AllowFailure: true})
		if err != nil {
			t.Fatal(err)
		}
		if result.ExitCode != 3 {
			t.Fatalf("got %+v", result)
		}
	})

	t.Run("captures stdout and stderr separately", func(t *testing.T) {
		result, err := New().Run(context.Background(), run.Spec{
			Command: "sh", Args: []string{"-c", "echo out; echo err >&2"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if result.Stdout != "out\n" || result.Stderr != "err\n" {
			t.Fatalf("got %+v", result)
		}
	})

	t.Run("captures output even when teed", func(t *testing.T) {
		result, err := New().Run(context.Background(), run.Spec{
			Command: "sh", Args: []string{"-c", "echo out; echo err >&2"}, Output: run.Tee,
		})
		if err != nil {
			t.Fatal(err)
		}
		if result.Stdout != "out\n" || result.Stderr != "err\n" {
			t.Fatalf("got %+v", result)
		}
	})

	t.Run("reports a child ended by a signal with the shell's exit code", func(t *testing.T) {
		result, err := New().Run(context.Background(), run.Spec{
			Command: "sh", Args: []string{"-c", "kill -KILL $$"}, AllowFailure: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if result.ExitCode != 137 || result.Signal != syscall.SIGKILL {
			t.Fatalf("got %+v", result)
		}
	})

	t.Run("rejects a failed command by default, with the reason in the message", func(t *testing.T) {
		_, err := New().Run(context.Background(), run.Spec{Command: "sh", Args: []string{"-c", "echo boom >&2; exit 3"}})
		if err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("got %v", err)
		}
	})
}

func TestRunnerTimeout(t *testing.T) {
	result, err := New().Run(context.Background(), run.Spec{
		Command: "sleep", Args: []string{"30"}, Timeout: 100 * time.Millisecond, KillAfter: 200 * time.Millisecond, AllowFailure: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.TimedOut || result.ExitCode != 1 {
		t.Fatalf("got %+v", result)
	}
}

func TestRunnerAlreadyCancelledContext(t *testing.T) {
	dir := scratch(t)
	marker := filepath.Join(dir, "ran")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := New().Run(ctx, run.Spec{Command: "sh", Args: []string{"-c", `touch "$1"`, "sh", marker}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("expected no subprocess to have run at all, got stat err %v", statErr)
	}
}

func TestRunnerInteractiveOutput(t *testing.T) {
	t.Run("wires the child straight to this process's stdio instead of capturing it", func(t *testing.T) {
		read, write, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		previous := os.Stdout
		os.Stdout = write
		defer func() { os.Stdout = previous }()

		result, runErr := New().Run(context.Background(), run.Spec{
			Command: "sh", Args: []string{"-c", "echo on the terminal"}, Output: run.Interactive,
		})
		write.Close()
		if runErr != nil {
			t.Fatal(runErr)
		}

		seen, err := io.ReadAll(read)
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(string(seen)) != "on the terminal" {
			t.Fatalf("this process's stdout saw %q, want the child's output", seen)
		}
		if result.Stdout != "" || result.Stderr != "" {
			t.Fatalf("expected nothing captured for an interactive command, got %+v", result)
		}
	})

	t.Run("tracks an interactive child by the process group it now leads", func(t *testing.T) {
		runner := New()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		// A failed assertion below skips the SIGKILL at the end; cancelling
		// still stops the child, and waiting keeps it from outliving the test.
		t.Cleanup(func() {
			cancel()
			<-done
		})
		go func() {
			_, _ = runner.Run(ctx, run.Spec{
				Command: "sleep", Args: []string{"30"}, Output: run.Interactive,
			})
			close(done)
		}()

		// Polls the tracked set itself, rather than a side channel like a
		// marker file: the only thing this subtest needs to be true, and the
		// only way to avoid a race against how fast the child is scheduled.
		var pid int
		deadline := time.Now().Add(5 * time.Second)
		for {
			runner.mu.Lock()
			n := len(runner.targets)
			for tracked := range runner.targets {
				pid = tracked.pid
			}
			runner.mu.Unlock()
			if n == 1 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the child was never tracked")
			}
			time.Sleep(5 * time.Millisecond)
		}
		// An interactive child now gets Setpgid regardless of whether it
		// also gets the terminal's foreground (this process's own stdin
		// under `go test` is rarely a terminal it holds), so it always
		// leads a process group of its own — its pgid is its own pid.
		pgid, err := syscall.Getpgid(pid)
		if err != nil {
			t.Fatal(err)
		}
		if pgid != pid {
			t.Fatalf("Getpgid(%d) = %d, want the child to lead its own group", pid, pgid)
		}

		if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		<-done
	})
}

func TestRunnerOutputOverflow(t *testing.T) {
	if testing.Short() {
		t.Skip("writes over the 100 MB output limit")
	}
	overflow := run.Spec{Command: "dd", Args: []string{"if=/dev/zero", "bs=1M", "count=150"}, AllowFailure: true}

	result, err := New().Run(context.Background(), overflow)
	if err != nil {
		t.Fatal(err)
	}
	if !result.OutputOverflow || result.ExitCode != 1 {
		t.Fatalf("got overflow=%v exitCode=%d len(stdout)=%d", result.OutputOverflow, result.ExitCode, len(result.Stdout))
	}
	if len(result.Stdout) > run.OutputLimit {
		t.Fatalf("captured %d bytes, want at most %d", len(result.Stdout), run.OutputLimit)
	}

	// Rejected the same way, its wording names the overflow rather than
	// burying it under a plain exit code or a signal.
	overflow.AllowFailure = false
	_, err = New().Run(context.Background(), overflow)
	want := "Command failed (dd if=/dev/zero bs=1M count=150): produced more than 100 MB of output and was stopped"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
}

func TestRunnerEnv(t *testing.T) {
	t.Run("passes a supplied environment through verbatim instead of merging this process in", func(t *testing.T) {
		os.Setenv("BERTH_TEST_LEAK", "from-the-parent")
		defer os.Unsetenv("BERTH_TEST_LEAK")

		script := "[ -z \"${BERTH_TEST_LEAK+x}\" ] && echo false || echo true"
		verbatim, err := New().Run(context.Background(), run.Spec{
			Command: "sh", Args: []string{"-c", script}, Env: []string{"PATH=" + os.Getenv("PATH")},
		})
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(verbatim.Stdout) != "false" {
			t.Fatalf("got %q", verbatim.Stdout)
		}

		inherited, err := New().Run(context.Background(), run.Spec{Command: "sh", Args: []string{"-c", script}})
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(inherited.Stdout) != "true" {
			t.Fatalf("got %q", inherited.Stdout)
		}
	})

	t.Run("looks the command up on the child's own PATH when Env is set", func(t *testing.T) {
		dir := scratch(t)
		tool := filepath.Join(dir, "tool")
		if err := os.WriteFile(tool, []byte("#!/bin/sh\necho found\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		result, err := New().Run(context.Background(), run.Spec{Command: "tool", Env: []string{"PATH=" + dir}})
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(result.Stdout) != "found" {
			t.Fatalf("got %q", result.Stdout)
		}
	})
}

func TestWhich(t *testing.T) {
	dir := scratch(t)
	if err := os.WriteFile(filepath.Join(dir, "tool"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plain"), []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "folder"), 0o755); err != nil {
		t.Fatal(err)
	}
	withPath(t, dir)
	runner := New()
	if !runner.Which("tool") {
		t.Error("expected tool to be found")
	}
	if runner.Which("plain") {
		t.Error("did not expect an unexecutable file to be found")
	}
	if runner.Which("folder") {
		t.Error("did not expect a directory to be found")
	}
	if runner.Which("absent") {
		t.Error("did not expect a missing name to be found")
	}
}

func TestQuiesce(t *testing.T) {
	t.Run("returns once a started child and its group are gone", func(t *testing.T) {
		runner := New()
		_, err := runner.Run(context.Background(), run.Spec{Command: "sh", Args: []string{"-c", "true"}})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := runner.Quiesce(ctx); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("waits for a cancelled child's whole group to actually exit", func(t *testing.T) {
		runner := New()
		ready := filepath.Join(scratch(t), "ready")
		runCtx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			_, _ = runner.Run(runCtx, run.Spec{Command: "sh", Args: []string{"-c", `: > "$0"; exec sleep 30`, ready},
				KillAfter: 100 * time.Millisecond, AllowFailure: true})
			close(done)
		}()
		// Cancelled only once the child is certainly running, so the test proves what its title says.
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, err := os.Stat(ready); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the child never started")
			}
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
		<-done

		ctx, qcancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer qcancel()
		if err := runner.Quiesce(ctx); err != nil {
			t.Fatal(err)
		}
	})
}

func TestUntracking(t *testing.T) {
	t.Run("stops tracking a child whose whole group is gone once it is reaped", func(t *testing.T) {
		runner := New()
		if _, err := runner.Run(context.Background(), run.Spec{Command: "true"}); err != nil {
			t.Fatal(err)
		}
		runner.mu.Lock()
		tracked := len(runner.targets)
		runner.mu.Unlock()
		if tracked != 0 {
			t.Fatalf("%d target(s) still tracked after a clean exit", tracked)
		}
	})

	t.Run("keeps tracking a group something it started still lingers in", func(t *testing.T) {
		runner := New()
		pidFile := filepath.Join(scratch(t), "pid")
		// The leader exits at once; the sleep it started stays in its group.
		if _, err := runner.Run(context.Background(), run.Spec{Command: "sh", Args: []string{"-c", `sleep 30 & echo $! > "$0"`, pidFile}}); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(pidFile)
		if err != nil {
			t.Fatal(err)
		}
		lingering, _ := strconv.Atoi(strings.TrimSpace(string(data)))
		t.Cleanup(func() { _ = syscall.Kill(lingering, syscall.SIGKILL) })

		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		if err := runner.Quiesce(ctx); err == nil {
			t.Fatal("quiesced with a group member still running")
		}
		_ = syscall.Kill(lingering, syscall.SIGKILL)
		ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel2()
		if err := runner.Quiesce(ctx2); err != nil {
			t.Fatal(err)
		}
	})
}

func TestInheritLocks(t *testing.T) {
	dir := scratch(t)
	scope := lock.Scope{Target: filepath.Join(dir, "op"), Description: "worktree main"}
	lk := lock.New(scope, lock.Options{Logger: logger.Silent{}, Watchdog: -1})
	ctx, err := lk.Acquire(context.Background(), "berth up")
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()

	heldCtx := lock.WithHeld(ctx, lk)
	// fd 3 is the first ExtraFiles entry; a shell can only "test -e" a
	// descriptor that is actually open.
	result, err := New().Run(heldCtx, run.Spec{
		Command: "sh", Args: []string{"-c", "test -e /dev/fd/3 && echo inherited"},
		InheritLocks: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(result.Stdout) != "inherited" {
		t.Fatalf("got %+v", result)
	}

	// Without InheritLocks, the child gets no such descriptor.
	result, err = New().Run(heldCtx, run.Spec{
		Command: "sh", Args: []string{"-c", "test -e /dev/fd/3 && echo inherited || echo absent"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(result.Stdout) != "absent" {
		t.Fatalf("got %+v", result)
	}
}
