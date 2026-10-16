//go:build integration

// lock.Clear's proof against real processes and the real process table of
// whichever platform runs it (/proc on Linux, ps on macOS). Lives in the
// same lock_test package as integration_test.go — not a separate package —
// so it can reuse that file's helper-process protocol (startHelper,
// killPID, waitForGone, statFileID and the single TestMain that dispatches by
// TREERIG_LOCK_HELPER_ROLE) instead of duplicating it.
package lock_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sofired/treerig/internal/lock"
	"github.com/sofired/treerig/internal/procid"
)

// startMarkedSurvivor starts a real, long-lived process (`sleep`) carrying
// project's Compose-project marker in its environment — a process
// lock.Clear's own marker scan (procid.Marked) must find, the way a
// container a dead holder started would still show up in the process table
// after the holder itself is gone. It needs no part of the helper-role
// protocol: it never touches the lock at all.
func startMarkedSurvivor(t *testing.T, project string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	cmd.Env = append(os.Environ(), "COMPOSE_PROJECT_NAME="+project)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

// startLaterProcess starts a real, long-lived process (`sleep`) and keeps
// restarting it until the process table itself reports a start distinct
// from original's — see the comment at its one call site for why a single
// attempt is not always enough.
func startLaterProcess(t *testing.T, original map[string]any) *exec.Cmd {
	t.Helper()
	table := procid.Platform()
	deadline := time.Now().Add(5 * time.Second)
	for {
		cmd := exec.Command("sleep", "30")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		status, err := table.Inspect(cmd.Process.Pid)
		if err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatal(err)
		}
		if startDistinct(original, status) {
			t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
			return cmd
		}
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for a process whose start does not collide with the dead holder's")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// startToleranceUs mirrors lock.startToleranceUs (unexported, so not
// reachable from this external test package): how far apart a macOS start
// time and a fresh one may be before judgeHolder itself calls a PID reused.
const startToleranceUs = 2_000_000

// startDistinct reports whether status's start is distinguishable from
// original's own recorded startTicks/startUs, the same fields and the same
// comparison judgeHolder itself makes.
func startDistinct(original map[string]any, status procid.Status) bool {
	if ticks, ok := original["startTicks"].(float64); ok && status.StartTicks != nil {
		return uint64(ticks) != *status.StartTicks
	}
	if us, ok := original["startUs"].(float64); ok && status.StartUs != nil {
		diff := *status.StartUs - int64(us)
		if diff < 0 {
			diff = -diff
		}
		return diff > startToleranceUs
	}
	return true
}

func TestWorktreeUnlockAgainstRealProcesses(t *testing.T) {
	t.Run("clears the lock of a holder killed outright", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "worktree")
		cmd, pid := startHelper(t, target, "hold")

		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		_ = cmd.Wait()

		scope := lock.Scope{Target: target, Description: "worktree main"}
		result, err := lock.Clear(context.Background(), []lock.Scope{scope}, lock.ClearOptions{})
		if err != nil {
			t.Fatalf("expected the killed holder to be judged gone and cleared, got %v", err)
		}
		if !result.Scopes[0].Cleared || len(result.Scopes[0].Verified) != 1 {
			t.Fatalf("result = %+v", result)
		}
		if !strings.Contains(result.Scopes[0].Verified[0], "no longer exists") && !strings.Contains(result.Scopes[0].Verified[0], "has exited") {
			t.Fatalf("verified = %q, want it to say the holder is gone", result.Scopes[0].Verified[0])
		}

		// And the lock is free: a fresh Acquire does not wait on it.
		probe := lock.New(scope, lock.Options{Timeout: 500 * time.Millisecond, Watchdog: -1})
		if _, err := probe.Acquire(context.Background(), "probe"); err != nil {
			t.Fatalf("expected the lock to be free after Clear: %v", err)
		}
		probe.Release()
	})

	t.Run("refuses a holder that is only stopped, and keeps its lock", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "worktree")
		cmd, pid := startHelper(t, target, "hold")

		if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGCONT) })
		_ = cmd

		scope := lock.Scope{Target: target, Description: "worktree busy"}
		// The holder still holds its flock while stopped, so Clear's own
		// non-blocking probe finds it held — refused before any judging, the
		// same "there is nothing to unlock" a live holder always gets.
		_, err := lock.Clear(context.Background(), []lock.Scope{scope}, lock.ClearOptions{})
		if err == nil {
			t.Fatal("expected a refusal")
		}
		if !strings.Contains(err.Error(), "is held") {
			t.Fatalf("got %q, want it to say the lock is held", err)
		}
	})

	t.Run("refuses while a process the holder left behind still runs, and clears once it has gone", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "worktree")
		const project = "treerig-clear-it-3"
		cmd, pid := startHelper(t, target, "hold")
		survivor := startMarkedSurvivor(t, project)

		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		_ = cmd.Wait()

		scope := lock.Scope{Target: target, Description: "worktree main", Markers: procid.Markers{Project: project}}
		_, err := lock.Clear(context.Background(), []lock.Scope{scope}, lock.ClearOptions{})
		if err == nil {
			t.Fatal("expected a refusal naming the surviving marked process")
		}
		if !strings.Contains(err.Error(), "still running") {
			t.Fatalf("got %q, want it to name the survivor", err)
		}

		_ = survivor.Process.Kill()
		_ = survivor.Wait()
		waitForGone(t, survivor.Process.Pid)

		result, err := lock.Clear(context.Background(), []lock.Scope{scope}, lock.ClearOptions{})
		if err != nil {
			t.Fatalf("expected Clear to succeed once the survivor is gone: %v", err)
		}
		if !result.Scopes[0].Cleared {
			t.Fatalf("result = %+v", result)
		}
	})

	// The flock design always leaves an unreleased record after any death,
	// exit(2) included: Clear judges an exited holder gone and clears it,
	// exactly as it does a killed one — checked here through a different
	// death path than "clears the lock of a holder killed outright" above.
	t.Run("leaves no record behind when its holder exits while holding it", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "worktree")
		cmd, _ := startHelper(t, target, "exit")
		if err := cmd.Wait(); err != nil {
			t.Fatalf("helper: %v", err)
		}

		scope := lock.Scope{Target: target, Description: "worktree main"}
		desc, err := lock.Describe(scope, procid.Platform())
		if err != nil {
			t.Fatal(err)
		}
		if desc.Record == nil || desc.Record.ReleasedAt != nil {
			t.Fatalf("expected an unreleased record left behind, got %+v", desc.Record)
		}

		result, err := lock.Clear(context.Background(), []lock.Scope{scope}, lock.ClearOptions{})
		if err != nil {
			t.Fatalf("expected the exited holder to be judged gone and cleared: %v", err)
		}
		if !result.Scopes[0].Cleared {
			t.Fatalf("result = %+v", result)
		}
		after, err := lock.Describe(scope, procid.Platform())
		if err != nil {
			t.Fatal(err)
		}
		if after.Record == nil || after.Record.ReleasedAt == nil {
			t.Fatalf("expected the record to be marked released, got %+v", after.Record)
		}
	})

	t.Run("knows a PID now used by a later process is not the holder", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "worktree")
		cmd, pid := startHelper(t, target, "hold")
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		_ = cmd.Wait()
		waitForGone(t, pid)

		data, err := os.ReadFile(target + ".holder")
		if err != nil {
			t.Fatal(err)
		}
		var rec map[string]any
		if err := json.Unmarshal(data, &rec); err != nil {
			t.Fatal(err)
		}

		// A real, running process, later than the dead holder — not
		// necessarily by much wall-clock time, but Linux's start time is
		// only as fine as a clock tick (10ms on a stock HZ=100 kernel), so
		// a process started right after the holder died can land in the
		// very same tick as the holder's own recorded start and be
		// indistinguishable from it. Retry until the new process's own
		// start genuinely differs, so the rewrite below is judged against a
		// start that really is later, not one that only usually is.
		sleeper := startLaterProcess(t, rec)
		rec["pid"] = sleeper.Process.Pid
		out, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target+".holder", append(out, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}

		scope := lock.Scope{Target: target, Description: "worktree main"}
		result, err := lock.Clear(context.Background(), []lock.Scope{scope}, lock.ClearOptions{})
		if err != nil {
			t.Fatalf("expected the reused-PID record to be judged gone: %v", err)
		}
		if !result.Scopes[0].Cleared || len(result.Scopes[0].Verified) != 1 {
			t.Fatalf("result = %+v", result)
		}
		if !strings.Contains(result.Scopes[0].Verified[0], "now belongs to a later process") {
			t.Fatalf("verified = %q, want it to say the PID now belongs to a later process", result.Scopes[0].Verified[0])
		}
	})
}
