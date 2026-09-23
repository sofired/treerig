package lock

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWatchdog(t *testing.T) {
	t.Run("cancels the action's context when the lock file is replaced while held", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "worktree")
		scope := Scope{Target: target, Description: "worktree main"}
		opts := testOptions(t)
		opts.Watchdog = 20 * time.Millisecond
		lk := New(scope, opts)
		ctx, err := lk.Acquire(context.Background(), "berth up")
		if err != nil {
			t.Fatal(err)
		}

		// Berth itself never unlinks a lock target; this simulates something
		// outside berth doing so, which #22 says must cancel in-flight work.
		if err := os.Remove(target); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, nil, 0o600); err != nil {
			t.Fatal(err)
		}

		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
			t.Fatal("expected the watchdog to cancel the action context")
		}
		if !lk.Lost() || lk.Err() == nil {
			t.Fatalf("Lost() = %v, Err() = %v", lk.Lost(), lk.Err())
		}
		_ = lk.Abandon()
	})

	t.Run("never marks a lost lock's record released: it may be another holder's now", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "worktree")
		scope := Scope{Target: target, Description: "worktree main"}
		opts := testOptions(t)
		opts.Watchdog = 20 * time.Millisecond
		lk := New(scope, opts)
		ctx, err := lk.Acquire(context.Background(), "berth up")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(target); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
			t.Fatal("expected the watchdog to cancel the action context")
		}
		// A caller that releases anyway, as the lifecycle does once its children quiesce.
		if err := lk.Release(); err != nil {
			t.Fatal(err)
		}
		rec, err := readRecord(target + ".holder")
		if err != nil || rec == nil {
			t.Fatalf("record = %v, %v", rec, err)
		}
		if rec.ReleasedAt != nil {
			t.Fatal("a lost lock's record was marked released")
		}
	})

	t.Run("never fires while nothing about the lock changes", func(t *testing.T) {
		dir := t.TempDir()
		scope := Scope{Target: filepath.Join(dir, "worktree"), Description: "worktree main"}
		opts := testOptions(t)
		opts.Watchdog = 20 * time.Millisecond
		lk := New(scope, opts)
		ctx, err := lk.Acquire(context.Background(), "berth up")
		if err != nil {
			t.Fatal(err)
		}
		defer lk.Release()

		select {
		case <-ctx.Done():
			t.Fatal("did not expect the context to be cancelled")
		case <-time.After(150 * time.Millisecond):
		}
		if lk.Lost() {
			t.Fatal("did not expect the lock to be lost")
		}
	})
}
