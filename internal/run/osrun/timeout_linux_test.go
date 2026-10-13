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

	"github.com/sofired/berth/internal/run"
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
	// A short-lived child is orphaned at once, when the shell that started
	// it exits, so it is handed to this process, not left to a shell that
	// would reap it. The script itself then hangs until the timeout.
	body := "#!/bin/sh\n" +
		"sh -c 'true & echo $! > \"$1\"' sh \"" + pidPath + "\"\n" +
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
		t.Cleanup(func() { _, _ = syscall.Wait4(pid, nil, 0, nil) }) // reap the zombie
	}

	if err == nil || !strings.Contains(err.Error(), "The hang hook left processes running (process group") {
		t.Fatalf("err = %v, want the drain's \"left processes running\" error", err)
	}
	if result.TimedOut {
		t.Fatalf("result = %+v: the drain's error must replace the timeout, not accompany it", result)
	}
}
