package lock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/procid"
)

// testOptions is a fast, deterministic Options for locking tests: a fake
// table identifying this process, a silent logger, and the watchdog
// disabled so tests don't race its ticker.
func testOptions(t *testing.T) Options {
	t.Helper()
	return Options{Table: newFakeTable(procid.Identity{MachineID: "m", BootID: "b", PIDNS: "pid:[1]"}), Logger: logger.Silent{}, Watchdog: -1}
}

// skipIfRoot skips t when running as root, which bypasses the file
// permissions a mode-based test stages — mirrors internal/router's own
// helper (router_test.go).
func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Getuid() == 0 {
		t.Skip("running as root bypasses the file permissions this test tampers with")
	}
}

func TestLockAcquireRelease(t *testing.T) {
	t.Run("acquires an uncontended lock and writes a holder record", func(t *testing.T) {
		dir := t.TempDir()
		scope := Scope{Target: filepath.Join(dir, "worktree"), Description: "worktree main"}
		lk := New(scope, testOptions(t))
		ctx, err := lk.Acquire(context.Background(), "berth up")
		if err != nil {
			t.Fatal(err)
		}
		if ctx.Err() != nil {
			t.Fatal("expected a live context")
		}
		rec, err := readRecord(scope.Target + ".holder")
		if err != nil || rec == nil {
			t.Fatalf("record = %+v, err = %v", rec, err)
		}
		if rec.PID != os.Getpid() || rec.Command != "berth up" || rec.ReleasedAt != nil {
			t.Fatalf("got %+v", rec)
		}
		if err := lk.Release(); err != nil {
			t.Fatal(err)
		}
		rec, err = readRecord(scope.Target + ".holder")
		if err != nil || rec == nil || rec.ReleasedAt == nil {
			t.Fatalf("expected a released record, got %+v, err %v", rec, err)
		}
	})

	t.Run("serializes the same scope and releases it for the next waiter", func(t *testing.T) {
		dir := t.TempDir()
		scope := Scope{Target: filepath.Join(dir, "worktree"), Description: "worktree main"}
		first := New(scope, testOptions(t))
		ctx1, err := first.Acquire(context.Background(), "first")
		if err != nil {
			t.Fatal(err)
		}
		_ = ctx1

		var events []string
		var mu sync.Mutex
		done := make(chan error, 1)
		second := New(scope, testOptions(t))
		go func() {
			delay := 200 * time.Millisecond
			second.opts.Watchdog = -1
			second.opts.Timeout = 2 * time.Second
			second.opts.Retry = 25 * time.Millisecond
			_, err := second.Acquire(context.Background(), "second")
			mu.Lock()
			events = append(events, "acquired")
			mu.Unlock()
			_ = delay
			done <- err
		}()

		time.Sleep(100 * time.Millisecond)
		mu.Lock()
		gotEarly := len(events)
		mu.Unlock()
		if gotEarly != 0 {
			t.Fatal("second should not have acquired yet")
		}
		if err := first.Release(); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})

	t.Run("gives up with an error naming the holder when the wait budget runs out", func(t *testing.T) {
		dir := t.TempDir()
		scope := Scope{Target: filepath.Join(dir, "worktree"), Description: "worktree busy"}
		holder := New(scope, testOptions(t))
		if _, err := holder.Acquire(context.Background(), "berth up api"); err != nil {
			t.Fatal(err)
		}
		defer holder.Release()

		waiterOpts := testOptions(t)
		waiterOpts.Timeout = 150 * time.Millisecond
		waiterOpts.Retry = 25 * time.Millisecond
		delay := time.Second
		waiterOpts.NoticeDelay = &delay
		waiter := New(scope, waiterOpts)
		_, err := waiter.Acquire(context.Background(), "waiter")
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "Another process holds the worktree busy lock") {
			t.Fatalf("got %q", err)
		}
		if !strings.Contains(err.Error(), `"berth up api"`) {
			t.Fatalf("expected the holder's command in %q", err)
		}
	})

	t.Run("ends the wait at once when the context is already done", func(t *testing.T) {
		dir := t.TempDir()
		scope := Scope{Target: filepath.Join(dir, "worktree"), Description: "worktree x"}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		lk := New(scope, testOptions(t))
		_, err := lk.Acquire(ctx, "x")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("judges a previous holder that exited without releasing, and proceeds", func(t *testing.T) {
		dir := t.TempDir()
		scope := Scope{Target: filepath.Join(dir, "worktree"), Description: "worktree main"}
		opts := testOptions(t)
		lk := New(scope, opts)
		if _, err := lk.Acquire(context.Background(), "first"); err != nil {
			t.Fatal(err)
		}
		if err := lk.Abandon(); err != nil { // leaves the record unreleased
			t.Fatal(err)
		}

		next := New(scope, testOptions(t)) // fresh fake table: PID absent by default
		ctx, err := next.Acquire(context.Background(), "second")
		if err != nil {
			t.Fatalf("expected the dead holder to be judged gone, got %v", err)
		}
		if ctx.Err() != nil {
			t.Fatal("expected a live context")
		}
	})

	t.Run("catches the contradiction of a free flock whose record still looks alive", func(t *testing.T) {
		// This can only happen if locking itself is broken (a bypassed
		// flock, say) — the design calls it a contradiction, refused after
		// one more look, never treated as an ordinary busy lock.
		dir := t.TempDir()
		scope := Scope{Target: filepath.Join(dir, "worktree"), Description: "worktree main"}
		opts := testOptions(t)
		lk := New(scope, opts)
		if _, err := lk.Acquire(context.Background(), "first"); err != nil {
			t.Fatal(err)
		}
		if err := lk.Abandon(); err != nil {
			t.Fatal(err)
		}

		alive := testOptions(t)
		table := alive.Table.(*fakeTable)
		table.states[os.Getpid()] = procid.Status{State: procid.Running, StartTicks: ptr(uint64(0))}
		// The record's own StartTicks must match what Inspect reports for "alive" to hold.
		next := New(scope, alive)
		start := time.Now()
		_, err := next.Acquire(context.Background(), "second")
		if err == nil {
			t.Fatal("expected a refusal")
		}
		if time.Since(start) < judgeDelay {
			t.Fatal("expected the contradiction's second look to wait")
		}
		if !strings.Contains(err.Error(), "Locking does not work on this filesystem") {
			t.Fatalf("got %q", err)
		}
	})
}

func TestOpenTargetRefusesASymlinkOrDirectory(t *testing.T) {
	t.Run("refuses a lock target that is a FIFO: only a regular file is ours", func(t *testing.T) {
		target := filepath.Join(t.TempDir(), "worktree")
		if err := syscall.Mkfifo(target, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := openTarget(target, "test"); err == nil || !strings.Contains(err.Error(), "Refusing to use "+target+" for the test lock: it is not a plain file (a named pipe stands in its place).") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("refuses a lock target that is a symbolic link", func(t *testing.T) {
		dir := t.TempDir()
		elsewhere := filepath.Join(dir, "elsewhere")
		if err := os.WriteFile(elsewhere, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(dir, "worktree")
		if err := os.Symlink(elsewhere, target); err != nil {
			t.Fatal(err)
		}
		scope := Scope{Target: target, Description: "linked worktree"}
		_, err := New(scope, testOptions(t)).Acquire(context.Background(), "x")
		if err == nil || !strings.Contains(err.Error(), "for the linked worktree lock: it is not a plain file (a symbolic link stands in its place).") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("refuses a lock target that is a directory", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "worktree")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		scope := Scope{Target: target, Description: "odd worktree"}
		_, err := New(scope, testOptions(t)).Acquire(context.Background(), "x")
		if err == nil || !strings.Contains(err.Error(), "for the odd worktree lock: it is not a plain file (a directory stands in its place).") {
			t.Fatalf("got %v", err)
		}
	})
}

func TestCheckFS(t *testing.T) {
	t.Run("accepts an ordinary local directory", func(t *testing.T) {
		if err := CheckFS(t.TempDir(), brand.HomeEnv); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("the first lock of a new repository finds its own directory not yet created", func(t *testing.T) {
		// Regression: CheckFS's self-test used to run in the target's
		// directory before anything had created it, so the very first lock
		// under locks/worktrees/<new repo>/ failed with ENOENT.
		dir := t.TempDir()
		target := filepath.Join(dir, "worktrees", "a-new-repository-id", "a-worktree-id")
		if _, err := os.Stat(filepath.Dir(target)); !os.IsNotExist(err) {
			t.Fatalf("test setup: %s must not exist yet, stat err = %v", filepath.Dir(target), err)
		}
		scope := Scope{Target: target, Description: "worktree main"}
		lk := New(scope, testOptions(t))
		if _, err := lk.Acquire(context.Background(), "berth up"); err != nil {
			t.Fatalf("expected the lock to be acquired despite its directory being new, got %v", err)
		}
		defer lk.Release()
	})
}

func TestDescribe(t *testing.T) {
	t.Run("describes a free lock with no record", func(t *testing.T) {
		dir := t.TempDir()
		scope := Scope{Target: filepath.Join(dir, "worktree"), Description: "worktree main"}
		desc, err := Describe(scope, newFakeTable(procid.Identity{}))
		if err != nil {
			t.Fatal(err)
		}
		if desc.State != Free {
			t.Fatalf("got %+v", desc)
		}
	})

	t.Run("describes a held lock and its record without disturbing it", func(t *testing.T) {
		dir := t.TempDir()
		scope := Scope{Target: filepath.Join(dir, "worktree"), Description: "worktree main"}
		holder := New(scope, testOptions(t))
		if _, err := holder.Acquire(context.Background(), "berth up"); err != nil {
			t.Fatal(err)
		}
		defer holder.Release()

		desc, err := Describe(scope, newFakeTable(procid.Identity{}))
		if err != nil {
			t.Fatal(err)
		}
		if desc.State != Held || desc.Record == nil || desc.Record.Command != "berth up" {
			t.Fatalf("got %+v", desc)
		}
		// Held is untouched by the probe: this process can still acquire again once released.
		if err := holder.Release(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("describes a TypeScript build's lock directory, with and without its record", func(t *testing.T) {
		dir := t.TempDir()
		scope := Scope{Target: filepath.Join(dir, "worktree"), Description: "worktree main"}
		if err := os.Mkdir(scope.Target+".lock", 0o700); err != nil {
			t.Fatal(err)
		}

		desc, err := Describe(scope, newFakeTable(procid.Identity{}))
		if err != nil {
			t.Fatal(err)
		}
		if desc.State != Free || !desc.Legacy || desc.LegacyRecord != nil || desc.Record != nil {
			t.Fatalf("got %+v, want a free lock with a TypeScript lock directory naming no holder", desc)
		}

		legacy := `{"pid":999999,"host":"old-host","command":"berth up","startedAt":"` + time.Now().Format(time.RFC3339Nano) + `"}`
		if err := os.WriteFile(scope.Target, []byte(legacy), 0o600); err != nil {
			t.Fatal(err)
		}
		desc, err = Describe(scope, newFakeTable(procid.Identity{}))
		if err != nil {
			t.Fatal(err)
		}
		if !desc.Legacy || desc.LegacyRecord == nil || desc.LegacyRecord.PID != 999999 || desc.LegacyRecord.Command != "berth up" {
			t.Fatalf("got %+v, want the TypeScript holder's record", desc)
		}
	})
}

func TestWithHeldInherited(t *testing.T) {
	dir := t.TempDir()
	opScope := Scope{Target: filepath.Join(dir, "op"), Description: "worktree main"}
	devScope := Scope{Target: filepath.Join(dir, "dev"), Description: "main development session"}
	opLock := New(opScope, testOptions(t))
	devLock := New(devScope, testOptions(t))
	if _, err := opLock.Acquire(context.Background(), "up"); err != nil {
		t.Fatal(err)
	}
	defer opLock.Release()
	if _, err := devLock.Acquire(context.Background(), "up"); err != nil {
		t.Fatal(err)
	}
	defer devLock.Release()

	ctx := WithHeld(context.Background(), opLock, devLock)
	files := Inherited(ctx)
	if len(files) != 2 {
		t.Fatalf("got %d files, want 2", len(files))
	}
	if files[0] != opLock.File() || files[1] != devLock.File() {
		t.Fatal("expected operation lock's file first")
	}
}
