//go:build integration

package lifecycle

// Scenario 11 from the design doc: quiescence forced to fail through the
// Quiescer seam. Abandon must leave the record unreleased, and a real child
// that inherited the lock's descriptor must keep it held after the helper
// itself is torn down by a real os.Exit(1) — proving Hold's forceExit path
// end to end, including WithHeld wiring the lock into the action's context
// (so InheritLocks has something to give the child), and the reason logged
// before abandoning and exiting.
//
// The test binary re-executes itself as a helper, chosen by
// BERTH_LIFECYCLE_HELPER_ROLE. Cleanup kills exact PIDs only.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sofired/berth/internal/lock"
	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/procid"
	"github.com/sofired/berth/internal/run"
	"github.com/sofired/berth/internal/run/osrun"
)

const (
	helperRoleEnv        = "BERTH_LIFECYCLE_HELPER_ROLE"
	helperTargetEnv      = "BERTH_LIFECYCLE_HELPER_TARGET"
	helperChildReadyEnv  = "BERTH_LIFECYCLE_HELPER_CHILD_READY"
	forcedQuiesceMessage = "forced quiesce failure (test seam)"
)

// park blocks this helper until it is killed from outside. Not select{}: with
// every goroutine blocked and no timer pending, Go's runtime declares the
// process deadlocked and exits it. On Linux a cgo build (os/user links cgo)
// has an extra thread that keeps that check from firing; on macOS, or on
// Linux with CGO_ENABLED=0, nothing does, and a helper that exits closes the
// very descriptor it was started to keep open.
func park() {
	for {
		time.Sleep(time.Hour)
	}
}

func TestMain(m *testing.M) {
	if role := os.Getenv(helperRoleEnv); role != "" {
		os.Exit(runScenario11Helper(role))
	}
	os.Exit(m.Run())
}

// alwaysFailQuiescer is the seam scenario 11 forces failure through: Hold's
// Env.Runner is a Quiescer, and any type satisfying that one method works,
// with no need to make a real osrun.Runner actually fail to reap a child.
type alwaysFailQuiescer struct{}

// Quiesce always fails, forcing Hold down its abandon-and-force-exit path
// regardless of what it is asked to wait for.
func (alwaysFailQuiescer) Quiesce(context.Context) error {
	return errors.New(forcedQuiesceMessage)
}

// runScenario11Helper runs this re-executed test binary as one of scenario
// 11's helper roles, selected by BERTH_LIFECYCLE_HELPER_ROLE, and returns the
// process exit code. "not-quiescent" holds the lock through Hold with a
// Quiescer that always fails and spawns the "child-hold-fd" role, which
// writes a ready file, then parks holding the inherited lock descriptor open
// until it is killed from outside.
func runScenario11Helper(role string) int {
	switch role {
	case "not-quiescent":
		op := lock.Scope{Target: os.Getenv(helperTargetEnv), Description: "helper"}
		l := NewLifecycle(New(), logger.NewConsole("helper", os.Stdout, os.Stderr))
		err := l.Hold(context.Background(),
			Env{Logger: logger.NewConsole("helper", os.Stdout, os.Stderr), Runner: alwaysFailQuiescer{}, Invocation: "helper"},
			Requests{Operation: &op},
			func(ctx context.Context) error {
				self, err := os.Executable()
				if err != nil {
					return err
				}
				childEnv := filterHelperEnv(os.Environ())
				childEnv = append(childEnv, helperRoleEnv+"=child-hold-fd", helperChildReadyEnv+"="+os.Getenv(helperChildReadyEnv))
				go func() {
					_, _ = osrun.New().Run(ctx, run.Spec{
						Command: self, Env: childEnv, InheritLocks: true, AllowFailure: true,
					})
				}()
				// The action fails only once the child has said it is running
				// with the lock, so Hold certainly has something to quiesce.
				deadline := time.Now().Add(5 * time.Second)
				for {
					if data, err := os.ReadFile(os.Getenv(helperChildReadyEnv)); err == nil && len(data) > 0 {
						break
					}
					if time.Now().After(deadline) {
						return errors.New("the child never reported ready")
					}
					time.Sleep(5 * time.Millisecond)
				}
				return errors.New("action failed, forcing a quiesce check")
			})
		// Unreachable in production: l.os.Exit(1) inside Hold already ended
		// this process. Only the real OS's Kill/Exit end it for real, so
		// this only matters if that ever changes.
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		return 1

	case "child-hold-fd":
		// Never touch fd 3: keep the inherited lock descriptor open and park.
		if err := writeReadyFile(os.Getenv(helperChildReadyEnv), os.Getpid()); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		park()

	default:
		fmt.Fprintln(os.Stderr, "unknown role: "+role)
		return 2
	}
	return 0 // not reached: a role that parks is only ever killed from outside
}

// filterHelperEnv strips this test's own role/target/ready-file variables
// out of env, so a spawned helper's own role selection isn't confused by
// its parent's.
func filterHelperEnv(env []string) []string {
	out := make([]string, 0, len(env))
next:
	for _, entry := range env {
		for _, key := range []string{helperRoleEnv, helperTargetEnv, helperChildReadyEnv} {
			if strings.HasPrefix(entry, key+"=") {
				continue next
			}
		}
		out = append(out, entry)
	}
	return out
}

// writeReadyFile records pid at path so the parent test can tell the child
// helper is running and holding the lock; it writes to a temp file first and
// renames into place so the parent never observes a partial write.
func writeReadyFile(path string, pid int) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(fmt.Sprintf("%d\n", pid)), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func TestNotQuiescentAbandonsAndKeepsChildHolding(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "worktree")
	childReady := filepath.Join(dir, "child.ready")

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd := exec.Command(self)
	cmd.Env = append(os.Environ(),
		helperRoleEnv+"=not-quiescent", helperTargetEnv+"="+target, helperChildReadyEnv+"="+childReady)
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// If the helper hangs, the test's timeout is what ends it: this makes
	// sure it and the child it reports do not outlive the test either way.
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	t.Cleanup(func() {
		if data, err := os.ReadFile(childReady); err == nil {
			var pid int
			if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid); err == nil && pid > 0 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})

	waitErr := cmd.Wait()
	exitErr, ok := waitErr.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected the helper to exit non-zero via a real os.Exit(1), got %v", waitErr)
	}
	if exitErr.ExitCode() != 1 {
		t.Fatalf("exit code = %d, want 1 (stderr: %s)", exitErr.ExitCode(), stderr.String())
	}
	wantLine := "Could not prove the interrupted operation stopped: " + forcedQuiesceMessage
	if !strings.Contains(stderr.String(), wantLine) {
		t.Fatalf("expected the reason logged before exiting; stderr = %q", stderr.String())
	}

	var childPID int
	waitFor(t, func() bool {
		data, err := os.ReadFile(childReady)
		if err != nil {
			return false
		}
		_, scanErr := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &childPID)
		return scanErr == nil && childPID > 0
	})

	scope := lock.Scope{Target: target, Description: "worktree main"}
	probe := lock.New(scope, lock.Options{Logger: logger.Silent{}, Timeout: 500 * time.Millisecond, Retry: 25 * time.Millisecond, Watchdog: -1})
	if _, err := probe.Acquire(context.Background(), "probe"); err == nil {
		t.Fatal("expected the lock to still be held by the child that inherited fd 3")
	}

	// Abandoned, not released: the record stays unreleased for the next acquirer to judge.
	desc, err := lock.Describe(scope, procid.Platform())
	if err != nil {
		t.Fatal(err)
	}
	if desc.Record == nil || desc.Record.ReleasedAt != nil {
		t.Fatalf("expected an unreleased (abandoned) record, got %+v", desc.Record)
	}

	_ = syscall.Kill(childPID, syscall.SIGKILL)
	waitFor(t, func() bool {
		status, err := procid.Platform().Inspect(childPID)
		return err == nil && (status.State == procid.Absent || status.State == procid.Zombie)
	})

	if _, err := probe.Acquire(context.Background(), "probe2"); err != nil {
		t.Fatalf("expected the lock to free once the child is gone: %v", err)
	}
	_ = probe.Release()
}
