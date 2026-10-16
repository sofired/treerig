package hooks_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/config"
	"github.com/sofired/treerig/internal/hooks"
	"github.com/sofired/treerig/internal/run/fakerun"
	"github.com/sofired/treerig/internal/run/osrun"
)

// timeouts is a [hooks.timeout] table with the given hooks limited.
func timeouts(prepare, seed, verify string) *config.HookTimeouts {
	set := func(s string) *string {
		if s == "" {
			return nil
		}
		return &s
	}
	return &config.HookTimeouts{Prepare: set(prepare), Seed: set(seed), Verify: set(verify)}
}

// specTimeout is the Timeout the single call a runner recorded asked for.
func specTimeout(t *testing.T, runner *fakerun.Runner) time.Duration {
	t.Helper()
	calls := runner.Calls()
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	return calls[0].Timeout
}

func TestRunHookTimeout(t *testing.T) {
	ctx := context.Background()

	t.Run("a hook with no [hooks.timeout] entry has no limit", func(t *testing.T) {
		runner := recordingRunner(nil)
		hctx := newContext(runner, config.Hooks{Prepare: str("./p.sh"), Timeout: timeouts("", "5m", "")})
		if _, err := hooks.RunHook(ctx, config.Prepare, hctx, hooks.RunOptions{}); err != nil {
			t.Fatal(err)
		}
		if got := specTimeout(t, runner); got != 0 {
			t.Fatalf("Timeout = %s, want none", got)
		}
	})

	t.Run("each hook is limited by its own entry", func(t *testing.T) {
		for stage, want := range map[config.HookStage]time.Duration{
			config.Prepare: 30 * time.Minute, config.Seed: 5 * time.Minute, config.Verify: 90 * time.Second,
		} {
			runner := recordingRunner(nil)
			hctx := newContext(runner, config.Hooks{
				Prepare: str("./p.sh"), Seed: str("./s.sh"), Verify: str("./v.sh"),
				Timeout: timeouts("30m", "5m", "90s"),
			})
			if _, err := hooks.RunHook(ctx, stage, hctx, hooks.RunOptions{}); err != nil {
				t.Fatal(err)
			}
			if got := specTimeout(t, runner); got != want {
				t.Errorf("%s: Timeout = %s, want %s", stage, got, want)
			}
		}
	})

	t.Run("applies to every hook kind, backup included", func(t *testing.T) {
		runner := recordingRunner(nil)
		hctx := newContext(runner, config.Hooks{Backup: str("pg_dump"), Timeout: &config.HookTimeouts{Backup: str("15m")}})
		arg := "/bundle"
		if _, err := hooks.RunHook(ctx, config.Backup, hctx, hooks.RunOptions{Argument: &arg}); err != nil {
			t.Fatal(err)
		}
		if got := specTimeout(t, runner); got != 15*time.Minute {
			t.Fatalf("Timeout = %s, want 15m", got)
		}
	})

	t.Run("an explicit RunOptions.Timeout replaces the configured one", func(t *testing.T) {
		runner := recordingRunner(nil)
		hctx := newContext(runner, config.Hooks{Prepare: str("./p.sh"), Timeout: timeouts("30m", "", "")})
		if _, err := hooks.RunHook(ctx, config.Prepare, hctx, hooks.RunOptions{Timeout: time.Second}); err != nil {
			t.Fatal(err)
		}
		if got := specTimeout(t, runner); got != time.Second {
			t.Fatalf("Timeout = %s, want 1s", got)
		}
	})

	t.Run("the lifecycle gives every stage its own limit", func(t *testing.T) {
		runner := recordingRunner(nil)
		hctx := newContext(runner, config.Hooks{Prepare: str("a"), Seed: str("b"), Timeout: timeouts("1m", "", "")})
		if _, err := hooks.RunLifecycle(ctx, hctx, hooks.LifecycleOptions{Stages: hooks.UpStages()}); err != nil {
			t.Fatal(err)
		}
		calls := runner.Calls()
		if len(calls) != 2 || calls[0].Timeout != time.Minute || calls[1].Timeout != 0 {
			t.Fatalf("Timeouts = %v, want [1m 0]", []time.Duration{calls[0].Timeout, calls[1].Timeout})
		}
	})

	t.Run("a timed-out hook fails as a timeout (exit 124) naming the configured limit, and stops the lifecycle", func(t *testing.T) {
		runner := brokenRunner("TIMEOUT")
		hctx := newContext(runner, config.Hooks{Prepare: str("a"), Seed: str("b"), Timeout: timeouts("10m", "", "")})
		ran, err := hooks.RunLifecycle(ctx, hctx, hooks.LifecycleOptions{Stages: hooks.UpStages()})
		requireMessage(t, err, 124, "The prepare hook timed out after 10m and was terminated.")
		if len(ran) != 0 || len(runner.Calls()) != 1 {
			t.Fatalf("ran %v after %d calls, want seed never started", ran, len(runner.Calls()))
		}
	})

	t.Run("writes no limit as 0s", func(t *testing.T) {
		hctx := newContext(brokenRunner("TIMEOUT"), config.Hooks{Verify: str("v")})
		_, err := hooks.RunHook(ctx, config.Verify, hctx, hooks.RunOptions{})
		requireMessage(t, err, 124, "The verify hook timed out after 0s and was terminated.")
	})

	t.Run("writes a limit the way the config does", func(t *testing.T) {
		for limit, want := range map[string]string{
			"250ms": "250ms", "30s": "30s", "10m": "10m", "90s": "1m30s", "1h": "1h", "90m": "1h30m", "24h": "24h",
			"1h30s": "1h30s", "1500ms": "1s500ms", "61m": "1h1m", "1h1m1s1ms": "1h1m1s1ms",
		} {
			hctx := newContext(brokenRunner("TIMEOUT"), config.Hooks{Verify: str("v"), Timeout: timeouts("", "", limit)})
			_, err := hooks.RunHook(ctx, config.Verify, hctx, hooks.RunOptions{})
			requireMessage(t, err, 124, "The verify hook timed out after "+want+" and was terminated.")
		}
	})
}

// killGroupOf registers a cleanup that SIGKILLs the whole process group of
// the grandchild whose pid the file at path holds, so a test that fails
// (before or after the pid is written) never leaves its loop running. The
// file is read when the cleanup runs, as it may not exist before then.
func killGroupOf(t *testing.T, path string) {
	t.Helper()
	t.Cleanup(func() {
		raw, err := os.ReadFile(path)
		if err != nil {
			return
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err != nil {
			return
		}
		if pgid, err := syscall.Getpgid(pid); err == nil && pgid != syscall.Getpgrp() {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
	})
}

// TestRunHookTimeoutOverARealSubprocess runs a real hung hook over the real
// runner: the hook fails as a timeout after its limit, and its descendant is
// gone when RunHook returns. (A descendant that ignores SIGTERM is covered
// where the grace can be shortened: osrun's TestRunnerTimeoutStopsTheWholeGroup.)
func TestRunHookTimeoutOverARealSubprocess(t *testing.T) {
	dir := t.TempDir()
	pids := filepath.Join(dir, "grandchild.pid")
	killGroupOf(t, pids)
	script := filepath.Join(dir, "hang.sh")
	// The hook starts a grandchild, waits until it has said who it is, then
	// hangs on its own.
	body := "#!/bin/sh\n" +
		"sh -c 'echo $$ > \"$1\"; while :; do sleep 1; done' sh \"" + pids + "\" >/dev/null 2>&1 </dev/null &\n" +
		"while [ ! -s \"" + pids + "\" ]; do sleep 0.01; done\n" +
		"while :; do sleep 1; done\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	command := script
	hctx := newContext(osrun.New(), config.Hooks{Seed: &command, Timeout: &config.HookTimeouts{Seed: str("2s")}})
	hctx.Source.ProjectRoot = dir

	start := time.Now()
	// Bounded, so a regression that never stops the hook fails the test
	// instead of hanging it.
	bounded, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ran, err := hooks.RunHook(bounded, config.Seed, hctx, hooks.RunOptions{})
	elapsed := time.Since(start)

	if ran {
		t.Error("RunHook reported the hung hook as having run")
	}
	var failure *clierr.Error
	if !errors.As(err, &failure) || failure.Message != "The seed hook timed out after 2s and was terminated." || failure.ExitCode != 124 {
		t.Fatalf("got %v, want the seed hook timing out after 2s with exit 124", err)
	}
	if elapsed < 2*time.Second || elapsed > 12*time.Second {
		t.Errorf("RunHook took %s, want about the 2s limit", elapsed)
	}
	raw, readErr := os.ReadFile(pids) // the hook only hangs once this is written
	if readErr != nil {
		t.Fatal(readErr)
	}
	pid, convErr := strconv.Atoi(strings.TrimSpace(string(raw)))
	if convErr != nil {
		t.Fatalf("pid file %q: %v", raw, convErr)
	}
	// kill(pid, 0) reports a zombie as alive; this assumes the orphaned
	// grandchild is reaped by an init that reaps.
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("the hook's grandchild %d is still running (kill 0: %v)", pid, err)
	}
}
