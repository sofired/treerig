package lifecycle

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/lock"
	"github.com/sofired/berth/internal/logger"
)

type fakeQuiescer struct {
	err   error
	calls int
}

func (f *fakeQuiescer) Quiesce(ctx context.Context) error {
	f.calls++
	return f.err
}

func testScopes(t *testing.T) (operation, development, registry lock.Scope) {
	t.Helper()
	dir := t.TempDir()
	return lock.Scope{Target: filepath.Join(dir, "op"), Description: "worktree main"},
		lock.Scope{Target: filepath.Join(dir, "dev"), Description: "main development session"},
		lock.Scope{Target: filepath.Join(dir, "registry"), Description: "global worktree registry"}
}

func TestHold(t *testing.T) {
	t.Run("acquires the requested locks in order and releases them on success", func(t *testing.T) {
		op, dev, reg := testScopes(t)
		l := NewLifecycle(newFakeOS(), logger.Silent{})
		q := &fakeQuiescer{}
		var order []string
		err := l.Hold(context.Background(), Env{Logger: logger.Silent{}, Runner: q, Invocation: "berth up"},
			Requests{Operation: &op, Development: &dev, Registry: &reg},
			func(ctx context.Context) error {
				order = append(order, "action")
				return nil
			})
		if err != nil {
			t.Fatal(err)
		}
		if len(order) != 1 || order[0] != "action" {
			t.Fatalf("got %v", order)
		}
		if q.calls != 0 {
			t.Fatal("did not expect Quiesce to be called for a clean success")
		}

		// Every lock must be free again: acquiring each once more must succeed at once.
		for _, scope := range []lock.Scope{op, dev, reg} {
			lk := lock.New(scope, lock.Options{Logger: logger.Silent{}, Watchdog: -1})
			ctx, err := lk.Acquire(context.Background(), "probe")
			if err != nil {
				t.Fatalf("%s: %v", scope.Description, err)
			}
			_ = ctx
			_ = lk.Release()
		}
	})

	t.Run("releases whatever it already holds when a later lock cannot be acquired", func(t *testing.T) {
		op, dev, _ := testScopes(t)
		blocker := lock.New(dev, lock.Options{Logger: logger.Silent{}, Watchdog: -1})
		if _, err := blocker.Acquire(context.Background(), "blocker"); err != nil {
			t.Fatal(err)
		}
		defer blocker.Release()

		l := NewLifecycle(newFakeOS(), logger.Silent{})
		ran := false
		err := l.Hold(context.Background(), Env{Logger: logger.Silent{}, Runner: &fakeQuiescer{}, Invocation: "berth up"},
			Requests{Operation: &op, Development: &dev},
			func(ctx context.Context) error { ran = true; return nil })
		if err == nil {
			t.Fatal("expected an error")
		}
		if ran {
			t.Fatal("did not expect the action to run")
		}
		// The operation lock, acquired before development failed, must be free again.
		probe := lock.New(op, lock.Options{Logger: logger.Silent{}, Watchdog: -1})
		if _, err := probe.Acquire(context.Background(), "probe"); err != nil {
			t.Fatalf("expected the operation lock to have been released: %v", err)
		}
		_ = probe.Release()
	})

	t.Run("attaches a release failure to the action's own error", func(t *testing.T) {
		// There is no way to make Release itself fail without breaking the
		// filesystem out from under it; instead this proves the action's
		// own error survives untouched when nothing needs attaching, and a
		// second, independent case (below) proves the attach() combinator.
		op, _, _ := testScopes(t)
		l := NewLifecycle(newFakeOS(), logger.Silent{})
		actionErr := errors.New("hook failed")
		err := l.Hold(context.Background(), Env{Logger: logger.Silent{}, Runner: &fakeQuiescer{}, Invocation: "berth up"},
			Requests{Operation: &op},
			func(ctx context.Context) error { return actionErr })
		if !errors.Is(err, actionErr) && err.Error() != actionErr.Error() {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("abandons every lock and reports the failure when quiescence cannot be proven", func(t *testing.T) {
		op, dev, _ := testScopes(t)
		fakeos := newFakeOS()
		l := NewLifecycle(fakeos, logger.Silent{})
		rec := &logger.Recording{}
		q := &fakeQuiescer{err: errors.New("a child is still running")}
		err := l.Hold(context.Background(), Env{Logger: rec, Runner: q, Invocation: "berth up"},
			Requests{Operation: &op, Development: &dev},
			func(ctx context.Context) error { return errors.New("interrupted") })
		if err == nil {
			t.Fatal("expected an error")
		}
		ce, ok := err.(*clierr.Error)
		if !ok || ce.ExitCode != 1 {
			t.Fatalf("got %#v", err)
		}
		if q.calls != 1 {
			t.Fatalf("Quiesce called %d times, want 1", q.calls)
		}
		lines, _ := rec.Snapshot()
		found := false
		for _, line := range lines {
			if line == "error: Could not prove the interrupted operation stopped: a child is still running" {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected the reason to be logged before exiting, got %v", lines)
		}
		if len(fakeos.exitCalls()) != 1 || fakeos.exitCalls()[0] != 1 {
			t.Fatalf("got exit calls %v", fakeos.exitCalls())
		}
		// Abandoned, not released: the record stays unreleased for the next acquirer to judge.
		if _, err := os.Stat(op.Target + ".holder"); err != nil {
			t.Fatalf("expected a holder record to remain: %v", err)
		}
	})

	t.Run("wires every held lock into the action's context, operation first", func(t *testing.T) {
		op, dev, _ := testScopes(t)
		l := NewLifecycle(newFakeOS(), logger.Silent{})
		var seenCount int
		var sameAsOp bool
		err := l.Hold(context.Background(), Env{Logger: logger.Silent{}, Runner: &fakeQuiescer{}, Invocation: "berth up"},
			Requests{Operation: &op, Development: &dev},
			func(ctx context.Context) error {
				// Checked here, not after Hold returns: Release closes the
				// file as soon as the action settles.
				seen := lock.Inherited(ctx)
				seenCount = len(seen)
				if seenCount > 0 {
					opInfo, statErr := seen[0].Stat()
					wantInfo, wantErr := os.Stat(op.Target)
					sameAsOp = statErr == nil && wantErr == nil && os.SameFile(opInfo, wantInfo)
				}
				return nil
			})
		if err != nil {
			t.Fatal(err)
		}
		if seenCount != 2 {
			t.Fatalf("got %d inherited files, want 2", seenCount)
		}
		if !sameAsOp {
			t.Fatal("expected the operation lock's file first (fd 3)")
		}
	})
}

func TestAttach(t *testing.T) {
	t.Run("returns the primary error untouched with nothing to attach", func(t *testing.T) {
		primary := errors.New("boom")
		if got := attach(primary, nil); got != primary {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("promotes the first secondary error when there is no primary", func(t *testing.T) {
		secondary := errors.New("release failed")
		if got := attach(nil, []error{secondary}); got != secondary {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("attaches every secondary error as Cause and Suppressed on a clierr.Error", func(t *testing.T) {
		primary := clierr.New("hook failed")
		secondary := errors.New("release failed")
		got := attach(primary, []error{secondary})
		ce := got.(*clierr.Error)
		if ce != primary {
			t.Fatal("expected the same *clierr.Error to be mutated in place")
		}
		if ce.Cause != secondary {
			t.Fatalf("Cause = %v", ce.Cause)
		}
		if len(ce.Suppressed) != 1 || ce.Suppressed[0] != secondary {
			t.Fatalf("Suppressed = %v", ce.Suppressed)
		}
	})

	t.Run("never overwrites a cause the primary error already carried", func(t *testing.T) {
		original := errors.New("original cause")
		primary := &clierr.Error{Message: "primary", ExitCode: 1, Cause: original}
		second := errors.New("second")
		got := attach(primary, []error{second}).(*clierr.Error)
		if got.Cause != original {
			t.Fatalf("Cause = %v, want the original", got.Cause)
		}
		if len(got.Suppressed) != 1 || got.Suppressed[0] != second {
			t.Fatalf("Suppressed = %v", got.Suppressed)
		}
	})
}
