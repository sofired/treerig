package osrun

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sofired/treerig/internal/run"
	"golang.org/x/sys/unix"
)

// TestRunnerTimeoutWhenTheDrainFails runs a Docker-shaped caller (no
// KillAfter, AllowFailure, captured output) whose child times out and leaves
// a group member nothing can remove: a zombie, kept by making this process
// the reaper of orphans and never waiting for it. The drain gives up, and
// Run returns its error instead of a Result, so "timed out" is never
// reported for a group that is still there.
//
// It sets the process-wide PR_SET_CHILD_SUBREAPER, so it must not run in
// parallel with other tests in this package (none calls t.Parallel).
func TestRunnerTimeoutWhenTheDrainFails(t *testing.T) {
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		t.Skipf("cannot become a subreaper: %v", err)
	}
	t.Cleanup(func() { _ = unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 0, 0, 0, 0) })

	dir := scratch(t)
	pidPath := filepath.Join(dir, "zombie.pid")
	script := filepath.Join(dir, "leave-a-zombie.sh")
	// The background child outlives the shell that started it, so it is
	// always orphaned to this process when that shell exits. A child that
	// ended first could be reaped by that shell (dash does, sometimes),
	// leaving no zombie and an empty group (#8's CI flake). The drain's
	// SIGKILL then makes it a zombie nothing reaps. The script itself hangs
	// until the timeout.
	body := "#!/bin/sh\n" +
		"sh -c 'sleep 60 & echo $! > \"$1\"' sh \"" + pidPath + "\"\n" +
		"while :; do sleep 1; done\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	runner := New()
	runner.drainBudget = func(time.Duration, bool) time.Duration { return 500 * time.Millisecond }
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := runner.Run(ctx, run.Spec{
		Command: script, Output: run.Capture, AllowFailure: true,
		Timeout: 2 * time.Second, Label: "hang hook",
	})

	raw, readErr := os.ReadFile(pidPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if pid, convErr := strconv.Atoi(strings.TrimSpace(string(raw))); convErr == nil {
		t.Cleanup(func() {
			// Killed first, in case a failed run left it alive, so the wait
			// that reaps the zombie cannot block.
			_ = syscall.Kill(pid, syscall.SIGKILL)
			_, _ = syscall.Wait4(pid, nil, 0, nil)
		})
	}

	if err == nil || !strings.Contains(err.Error(), "The hang hook left processes running (process group") {
		t.Fatalf("err = %v, want the drain's \"left processes running\" error", err)
	}
	if result.TimedOut {
		t.Fatalf("result = %+v: the drain's error must replace the timeout, not accompany it", result)
	}
}
