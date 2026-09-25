//go:build integration

// #14: a `docker` that never answers must not hang the Compose backend's
// bounded calls, or a mutation holding the worktree lock around one,
// forever. Mirrors internal/router's own TestRouterUpTimesOutOnAHungDocker
// and internal/checkoutindex's own TestServiceRefreshTimesOutOnAHungDocker:
// a real, hanging subprocess (found only through this test's own PATH
// override), run through the real osrun.Runner, so the real
// timeout-and-kill machinery in internal/run/osrun is what is exercised —
// not a hand-rolled fake that merely claims to time out. Needs no live
// Docker daemon: the fake `docker` on PATH is the only thing either test
// ever runs.
package backend

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/lifecycle"
	"github.com/sofired/berth/internal/lock"
	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/procid"
	"github.com/sofired/berth/internal/registry/registrytest"
	"github.com/sofired/berth/internal/run/osrun"
	"golang.org/x/sys/unix"
)

// hangDocker plants a `docker` on PATH that never answers, and returns its
// directory.
func hangDocker(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	fakeDocker := filepath.Join(dir, "docker")
	if err := os.WriteFile(fakeDocker, []byte("#!/bin/sh\nsleep 999999\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func TestInventoryTimesOutOnAHungDocker(t *testing.T) {
	hangDocker(t)
	controlHome := registrytest.TempDir(t)
	rt := NewCompose(Options{
		ComposeFile: "/work/acme/docker/compose.yaml", ProjectRoot: "/work/acme",
		ConfigFile: "/work/acme/berth.toml", ProjectName: "acme", Domain: "acme.localhost",
		ControlHome: controlHome, LabelNamespace: ns, Runner: osrun.New(), Logger: logger.Silent{},
		// Well short of osrun's own default kill grace, so this test does
		// not wait long, and well short of the 10s ceiling below it is
		// checked against.
		Timeouts: Timeouts{Probe: 300 * time.Millisecond},
	})

	started := time.Now()
	_, err := rt.Inventory(context.Background(), ref)
	elapsed := time.Since(started)

	if err == nil {
		t.Fatal("expected the hung `docker` to be caught as a failure rather than hanging Inventory forever")
	}
	if elapsed > 10*time.Second {
		t.Fatalf("expected the timeout to end this well under 10s, took %s (err: %v)", elapsed, err)
	}
	if clierr.Code(err) != 124 {
		t.Fatalf("code = %d, want 124 (a timeout), err: %v", clierr.Code(err), err)
	}
	if !strings.Contains(err.Error(), "timed out after 300 ms") {
		t.Fatalf("error = %v, want it to name the timeout plainly", err)
	}
	t.Logf("Inventory failed after %s, as expected: %v", elapsed, err)
}

// TestBoundedCallUnderTheWorktreeLockTimesOutAndReleasesIt is the "Done
// when" of #14 end to end, the way internal/commands' own mutate() holds a
// worktree's operation lock around a runtime call: lifecycle.Hold only
// releases a lock once it can prove every subprocess the action started has
// been reaped (see Hold's own comment) — abandoning it otherwise — so this
// also proves the timeout's SIGTERM/SIGKILL actually stops the hung
// `docker`, not only that Inventory itself returns.
func TestBoundedCallUnderTheWorktreeLockTimesOutAndReleasesIt(t *testing.T) {
	hangDocker(t)
	controlHome := registrytest.TempDir(t)
	runner := osrun.New()
	rt := NewCompose(Options{
		ComposeFile: "/work/acme/docker/compose.yaml", ProjectRoot: "/work/acme",
		ConfigFile: "/work/acme/berth.toml", ProjectName: "acme", Domain: "acme.localhost",
		ControlHome: controlHome, LabelNamespace: ns, Runner: runner, Logger: logger.Silent{},
		Timeouts: Timeouts{Probe: 300 * time.Millisecond},
	})

	operation, _ := lock.WorktreeScopes(controlHome, ref.RepositoryID, ref.WorktreeID, ref.Slug,
		procid.Markers{Project: project, WorktreeID: ref.WorktreeID})
	life := lifecycle.NewLifecycle(lifecycle.New(), logger.Silent{})

	started := time.Now()
	err := life.Hold(context.Background(), lifecycle.Env{Logger: logger.Silent{}, Runner: runner, Invocation: "test worktree remove"},
		lifecycle.Requests{Operation: &operation},
		func(ctx context.Context) error {
			_, err := rt.Inventory(ctx, ref)
			return err
		})
	elapsed := time.Since(started)

	if err == nil {
		t.Fatal("expected the hung `docker` to fail the held action rather than hang it forever")
	}
	if elapsed > 10*time.Second {
		t.Fatalf("expected the timeout to end this well under 10s, took %s (err: %v)", elapsed, err)
	}

	lockPath := layout.WorktreeLayout(controlHome, ref.RepositoryID, ref.WorktreeID).LockPath
	file, err := os.OpenFile(lockPath, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("could not open the lock file to check it: %v", err)
	}
	defer file.Close()
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("the worktree lock is still held after the timeout: %v", err)
	}
}
