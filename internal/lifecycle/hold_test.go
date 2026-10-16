package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/lock"
	"github.com/sofired/treerig/internal/logger"
)

// fakeQuiescer is a Runner that counts its calls and returns a fixed error.
type fakeQuiescer struct {
	err   error
	calls int
}

// Quiesce records the call and returns the fake's configured error.
func (f *fakeQuiescer) Quiesce(ctx context.Context) error {
	f.calls++
	return f.err
}

// testScopes returns three distinct lock.Scope values, rooted in a fresh
// temp directory, for the three request kinds Hold coordinates.
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
		err := l.Hold(context.Background(), Env{Logger: logger.Silent{}, Runner: q, Invocation: "treerig up"},
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

	t.Run("takes a router request alone, with its own timeout", func(t *testing.T) {
		dir := t.TempDir()
		router := lock.Scope{Target: filepath.Join(dir, "router"), Description: "shared router"}
		l := NewLifecycle(newFakeOS(), logger.Silent{})
		ran := false
		err := l.Hold(context.Background(), Env{Logger: logger.Silent{}, Runner: &fakeQuiescer{}, Invocation: "treerig router up"},
			Requests{Router: &router},
			func(ctx context.Context) error { ran = true; return nil })
		if err != nil {
			t.Fatal(err)
		}
		if !ran {
			t.Fatal("expected the action to run")
		}
		probe := lock.New(router, lock.Options{Logger: logger.Silent{}, Watchdog: -1})
		if _, err := probe.Acquire(context.Background(), "probe"); err != nil {
			t.Fatalf("expected the router lock to have been released: %v", err)
		}
		_ = probe.Release()
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
		err := l.Hold(context.Background(), Env{Logger: logger.Silent{}, Runner: &fakeQuiescer{}, Invocation: "treerig up"},
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
		err := l.Hold(context.Background(), Env{Logger: logger.Silent{}, Runner: &fakeQuiescer{}, Invocation: "treerig up"},
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
		err := l.Hold(context.Background(), Env{Logger: rec, Runner: q, Invocation: "treerig up"},
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
		err := l.Hold(context.Background(), Env{Logger: logger.Silent{}, Runner: &fakeQuiescer{}, Invocation: "treerig up"},
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

	t.Run("a hangup delivered twice takes the graceful path: the action settles, quiesces and releases, with no abandon", func(t *testing.T) {
		op, _, _ := testScopes(t)
		fos := newFakeOS()
		l := NewLifecycle(fos, logger.Silent{})
		l.Start()
		q := &fakeQuiescer{}
		var abandoned bool
		err := l.Hold(l.Context(), Env{Logger: logger.Silent{}, Runner: q, Invocation: "treerig up"},
			Requests{Operation: &op},
			func(ctx context.Context) error {
				fos.send(syscall.SIGHUP)
				fos.send(syscall.SIGHUP)
				<-ctx.Done()
				// The wait a runner would make for its child's group: it
				// gives up only on Repeated.
				select {
				case <-l.Repeated():
					abandoned = true
				case <-time.After(100 * time.Millisecond):
				}
				return context.Cause(ctx)
			})
		var cancelled *clierr.Cancelled
		if !errors.As(err, &cancelled) || cancelled.Signal != syscall.SIGHUP {
			t.Fatalf("err = %v, want the hangup", err)
		}
		if abandoned {
			t.Fatal("two SIGHUPs took the abandon path")
		}
		if q.calls != 1 {
			t.Fatalf("Quiesce called %d times, want 1", q.calls)
		}
		if exits := fos.exitCalls(); len(exits) != 0 {
			t.Fatalf("exited %v: the locks were abandoned", exits)
		}
		lk := lock.New(op, lock.Options{Logger: logger.Silent{}, Timeout: 200 * time.Millisecond, Watchdog: -1})
		if _, err := lk.Acquire(context.Background(), "probe"); err != nil {
			t.Fatalf("the lock was not released: %v", err)
		}
		_ = lk.Release()
	})

	t.Run("reports a lock lost during the action instead of releasing it as if nothing happened", func(t *testing.T) {
		restore := SetWatchdogForTest(20 * time.Millisecond)
		defer restore()

		op, _, _ := testScopes(t)
		l := NewLifecycle(newFakeOS(), logger.Silent{})
		err := l.Hold(context.Background(), Env{Logger: logger.Silent{}, Runner: &fakeQuiescer{}, Invocation: "treerig up"},
			Requests{Operation: &op},
			func(ctx context.Context) error {
				// Treerig itself never unlinks a lock target; this simulates
				// something outside treerig doing so, the way
				// internal/lock/watchdog_test.go's own "cancels the
				// action's context" case does, but here end to end
				// through Hold. Written to a temp file in the same
				// directory and renamed over the target, atomically,
				// rather than removed and then recreated: the watchdog
				// runs concurrently with this and must never be able to
				// see the target briefly missing, which reports removed
				// rather than replaced.
				replacement, err := os.CreateTemp(filepath.Dir(op.Target), "replacement-*")
				if err != nil {
					return err
				}
				temp := replacement.Name()
				if err := replacement.Close(); err != nil {
					return err
				}
				if err := os.Rename(temp, op.Target); err != nil {
					return err
				}
				select {
				case <-ctx.Done():
				case <-time.After(10 * time.Second):
					t.Fatal("the watchdog never cancelled the action's context after the target was replaced")
				}
				// Deliberately ignores the cancellation and reports
				// success anyway, as an action that never watches its
				// own context would: Hold itself must still catch the
				// loss.
				return nil
			})
		if err == nil {
			t.Fatal("expected the lost lock to be reported")
		}
		if !strings.Contains(err.Error(), "was replaced while this command held it") {
			t.Fatalf("got %v", err)
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

	t.Run("keeps the wrapper's text when the primary error wraps a clierr.Error", func(t *testing.T) {
		wrapped := fmt.Errorf("while holding the lock: %w", clierr.WithCode("hook failed", 3))
		secondary := errors.New("release failed")
		got := attach(wrapped, []error{secondary})
		if got.Error() != "while holding the lock: hook failed" {
			t.Fatalf("message = %q, the wrapper's text was lost", got.Error())
		}
		if code := clierr.Code(got); code != 3 {
			t.Fatalf("exit code = %d, want 3", code)
		}
		var ce *clierr.Error
		if !errors.As(got, &ce) || len(ce.Suppressed) != 1 || ce.Suppressed[0] != secondary {
			t.Fatalf("Suppressed not kept: %+v", ce)
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

func TestSharedLock(t *testing.T) {
	scope := func(t *testing.T, name string) lock.Scope {
		t.Helper()
		return lock.Scope{Target: filepath.Join(t.TempDir(), name), Description: name}
	}
	probe := func(t *testing.T, s lock.Scope) (free bool) {
		t.Helper()
		lk := lock.New(s, lock.Options{Logger: logger.Silent{}, Watchdog: -1, Timeout: 20 * time.Millisecond})
		if _, err := lk.Acquire(context.Background(), "probe"); err != nil {
			return false
		}
		_ = lk.Release()
		return true
	}

	t.Run("orders the shared lock after development and before the registry", func(t *testing.T) {
		op, dev, shared, reg := scope(t, "op"), scope(t, "dev"), scope(t, "shared"), scope(t, "registry")
		got := Requests{Registry: &reg, Shared: &shared, Development: &dev, Operation: &op}.ordered()
		want := []*lock.Scope{&op, &dev, &shared, &reg}
		if len(got) != len(want) {
			t.Fatalf("got %d scopes, want %d", len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("scope %d = %s, want %s", i, got[i].Description, want[i].Description)
			}
		}
	})

	t.Run("gives the shared lock the shared timeout", func(t *testing.T) {
		shared := scope(t, "shared")
		requests := Requests{Shared: &shared}
		if got := optionsFor(&shared, requests, Env{}).Timeout; got != lock.SharedTimeout {
			t.Errorf("timeout = %v, want %v", got, lock.SharedTimeout)
		}
		requests.Timeout = lock.ReconnectTimeout
		if got := optionsFor(&shared, requests, Env{}).Timeout; got != lock.ReconnectTimeout {
			t.Errorf("overridden timeout = %v, want %v", got, lock.ReconnectTimeout)
		}
	})

	t.Run("nests the shared lock inside a held operation lock and hands both to a child", func(t *testing.T) {
		op, shared := scope(t, "op"), scope(t, "shared")
		l := NewLifecycle(newFakeOS(), logger.Silent{})
		env := Env{Logger: logger.Silent{}, Runner: &fakeQuiescer{}, Invocation: "treerig up"}
		var inherited, innerHeld int
		err := l.Hold(context.Background(), env, Requests{Operation: &op}, func(ctx context.Context) error {
			if !probe(t, shared) {
				t.Error("the shared lock was held before Nest")
			}
			err := l.Nest(ctx, env, Requests{Shared: &shared}, func(inner context.Context) error {
				inherited = len(lock.Inherited(inner))
				if probe(t, shared) {
					t.Error("the shared lock was free inside Nest")
				}
				innerHeld = len(lock.Inherited(ctx))
				return nil
			})
			if probe(t, op) {
				t.Error("the operation lock was released by Nest")
			}
			if !probe(t, shared) {
				t.Error("the shared lock was still held after Nest")
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if inherited != 2 || innerHeld != 1 {
			t.Errorf("inner action inherited %d locks and the outer ctx %d, want 2 and 1", inherited, innerHeld)
		}
		if !probe(t, op) || !probe(t, shared) {
			t.Error("a lock was still held after Hold")
		}
	})

	t.Run("Nest refuses a request that would invert the order", func(t *testing.T) {
		op := scope(t, "op")
		l := NewLifecycle(newFakeOS(), logger.Silent{})
		ran := false
		err := l.Nest(context.Background(), Env{Logger: logger.Silent{}}, Requests{Operation: &op}, func(context.Context) error {
			ran = true
			return nil
		})
		if err == nil || ran {
			t.Fatalf("Nest(Operation) = %v, ran = %v", err, ran)
		}
	})

	t.Run("a short timeout turns a held lock into a BusyError without waiting out the scope's own", func(t *testing.T) {
		op := scope(t, "op")
		holder := lock.New(op, lock.Options{Logger: logger.Silent{}, Watchdog: -1})
		if _, err := holder.Acquire(context.Background(), "holder"); err != nil {
			t.Fatal(err)
		}
		defer holder.Release()

		l := NewLifecycle(newFakeOS(), logger.Silent{})
		started := time.Now()
		ran := false
		err := l.Hold(context.Background(), Env{Logger: logger.Silent{}, Runner: &fakeQuiescer{}, Invocation: "treerig shared up"},
			Requests{Operation: &op, Timeout: 100 * time.Millisecond, Quiet: true},
			func(context.Context) error { ran = true; return nil })
		var busy *lock.BusyError
		if !errors.As(err, &busy) {
			t.Fatalf("err = %v (%T), want a *lock.BusyError", err, err)
		}
		if ran {
			t.Error("the action ran without the lock")
		}
		if elapsed := time.Since(started); elapsed > 5*time.Second {
			t.Errorf("waited %v", elapsed)
		}
	})
}

func TestNestRefusesAnInversion(t *testing.T) {
	scope := func(name string) lock.Scope {
		return lock.Scope{Target: filepath.Join(t.TempDir(), name), Description: name}
	}
	l := NewLifecycle(newFakeOS(), logger.Silent{})
	env := Env{Logger: logger.Silent{}, Runner: &fakeQuiescer{}, Invocation: "treerig up"}

	t.Run("a registry lock held outside refuses a shared lock inside it", func(t *testing.T) {
		registry, shared := scope("registry"), scope("shared")
		ran := false
		err := l.Hold(context.Background(), env, Requests{Registry: &registry}, func(ctx context.Context) error {
			return l.Nest(ctx, env, Requests{Shared: &shared}, func(context.Context) error { ran = true; return nil })
		})
		if err == nil || ran || !strings.Contains(err.Error(), "invert the lock order") {
			t.Fatalf("err = %v, ran = %v", err, ran)
		}
	})

	t.Run("a shared lock held outside refuses another shared lock inside it", func(t *testing.T) {
		first, second := scope("shared"), scope("shared2")
		err := l.Hold(context.Background(), env, Requests{Shared: &first}, func(ctx context.Context) error {
			return l.Nest(ctx, env, Requests{Shared: &second}, func(context.Context) error { return nil })
		})
		if err == nil {
			t.Fatal("a second shared lock was taken inside the first")
		}
	})

	t.Run("an operation lock outside allows a shared lock inside, and the registry inside that", func(t *testing.T) {
		op, shared, registry := scope("op"), scope("shared"), scope("registry")
		depth := 0
		err := l.Hold(context.Background(), env, Requests{Operation: &op}, func(ctx context.Context) error {
			return l.Nest(ctx, env, Requests{Shared: &shared}, func(inner context.Context) error {
				return l.Nest(inner, env, Requests{Registry: &registry}, func(context.Context) error { depth = 3; return nil })
			})
		})
		if err != nil || depth != 3 {
			t.Fatalf("err = %v, depth = %d", err, depth)
		}
	})

	t.Run("a context with no Hold behind it is not an inversion", func(t *testing.T) {
		shared := scope("shared")
		if err := l.Nest(context.Background(), env, Requests{Shared: &shared}, func(context.Context) error { return nil }); err != nil {
			t.Fatal(err)
		}
	})
}

// held reports whether another open file description holds scope's flock.
func held(t *testing.T, scope lock.Scope) bool {
	t.Helper()
	probe := lock.New(scope, lock.Options{Logger: logger.Silent{}, Watchdog: -1, Timeout: time.Millisecond, Retry: time.Millisecond})
	if _, err := probe.Acquire(context.Background(), "probe"); err != nil {
		return true
	}
	_ = probe.Release()
	return false
}

func TestHoldThen(t *testing.T) {
	env := func(q Quiescer) Env { return Env{Logger: logger.Silent{}, Runner: q, Invocation: "treerig dev"} }

	t.Run("gives the operation lock back before the second phase, which keeps the development lock until it ends", func(t *testing.T) {
		op, dev, _ := testScopes(t)
		l := NewLifecycle(newFakeOS(), logger.Silent{})
		var inAction, inThen [2]bool
		err := l.HoldThen(context.Background(), env(&fakeQuiescer{}), Requests{Operation: &op, Development: &dev},
			func(ctx context.Context) error {
				inAction = [2]bool{held(t, op), held(t, dev)}
				return nil
			},
			func(ctx context.Context) error {
				inThen = [2]bool{held(t, op), held(t, dev)}
				if got := len(lock.Inherited(ctx)); got != 1 {
					t.Errorf("then inherits %d locks, want only the development lock", got)
				}
				return nil
			})
		if err != nil {
			t.Fatal(err)
		}
		if inAction != [2]bool{true, true} {
			t.Fatalf("held during action (operation, development) = %v, want both", inAction)
		}
		if inThen != [2]bool{false, true} {
			t.Fatalf("held during then (operation, development) = %v, want the development lock alone", inThen)
		}
		if held(t, op) || held(t, dev) {
			t.Fatal("a lock is still held after HoldThen")
		}
	})

	t.Run("does not run the second phase when the first fails, and releases everything", func(t *testing.T) {
		op, dev, _ := testScopes(t)
		l := NewLifecycle(newFakeOS(), logger.Silent{})
		q := &fakeQuiescer{}
		actionErr := errors.New("compose up failed")
		ran := false
		err := l.HoldThen(context.Background(), env(q), Requests{Operation: &op, Development: &dev},
			func(ctx context.Context) error { return actionErr },
			func(ctx context.Context) error { ran = true; return nil })
		if !errors.Is(err, actionErr) {
			t.Fatalf("got %v, want the action's error", err)
		}
		if ran {
			t.Fatal("the second phase ran after the first failed")
		}
		if q.calls != 1 {
			t.Fatalf("Quiesce called %d times, want once for a failed action", q.calls)
		}
		if held(t, op) || held(t, dev) {
			t.Fatal("a lock is still held")
		}
	})

	t.Run("reports the second phase's error, proves quiescence, and releases the development lock", func(t *testing.T) {
		op, dev, _ := testScopes(t)
		l := NewLifecycle(newFakeOS(), logger.Silent{})
		q := &fakeQuiescer{}
		thenErr := clierr.WithCode("the follower ended", 7)
		err := l.HoldThen(context.Background(), env(q), Requests{Operation: &op, Development: &dev},
			func(ctx context.Context) error { return nil },
			func(ctx context.Context) error { return thenErr })
		if !errors.Is(err, thenErr) || clierr.Code(err) != 7 {
			t.Fatalf("got %v (code %d), want the second phase's error", err, clierr.Code(err))
		}
		if q.calls != 1 {
			t.Fatalf("Quiesce called %d times, want once", q.calls)
		}
		if held(t, dev) {
			t.Fatal("the development lock is still held")
		}
	})

	t.Run("abandons the development lock, not the operation lock already given back, when quiescence cannot be proven", func(t *testing.T) {
		op, dev, _ := testScopes(t)
		fos := newFakeOS()
		l := NewLifecycle(fos, logger.Silent{})
		q := &fakeQuiescer{err: errors.New("a child is still running")}
		_ = l.HoldThen(context.Background(), env(q), Requests{Operation: &op, Development: &dev},
			func(ctx context.Context) error { return nil },
			func(ctx context.Context) error { return errors.New("follower died") })
		// The operation lock was released cleanly, its record saying so.
		record, err := os.ReadFile(op.Target + ".holder")
		if err != nil || !strings.Contains(string(record), `"releasedAt"`) {
			t.Fatalf("operation holder record = %s, %v, want it released", record, err)
		}
		// The development lock's record is left unreleased for the next acquirer to judge.
		record, err = os.ReadFile(dev.Target + ".holder")
		if err != nil || strings.Contains(string(record), `"releasedAt"`) {
			t.Fatalf("development holder record = %s, %v, want it abandoned", record, err)
		}
	})

	t.Run("returns the context's cause, runs nothing more and releases everything when the context ends during the first phase", func(t *testing.T) {
		op, dev, _ := testScopes(t)
		l := NewLifecycle(newFakeOS(), logger.Silent{})
		q := &fakeQuiescer{}
		ctx, cancel := context.WithCancelCause(context.Background())
		interrupted := &clierr.Cancelled{Signal: syscall.SIGINT}
		ran := false
		err := l.HoldThen(ctx, env(q), Requests{Operation: &op, Development: &dev},
			func(ctx context.Context) error { cancel(interrupted); return nil },
			func(ctx context.Context) error { ran = true; return nil })
		if !errors.Is(err, interrupted) {
			t.Fatalf("got %v, want the context's cause, never nil", err)
		}
		if ran {
			t.Fatal("the second phase ran on a cancelled context")
		}
		if q.calls != 1 {
			t.Fatalf("Quiesce called %d times, want once", q.calls)
		}
		if held(t, op) || held(t, dev) {
			t.Fatal("a lock is still held")
		}
	})

	t.Run("hands the second phase a live context that still carries the development lock, across the hand-off", func(t *testing.T) {
		restore := SetWatchdogForTest(5 * time.Millisecond)
		defer restore()
		op, dev, _ := testScopes(t)
		l := NewLifecycle(newFakeOS(), logger.Silent{})
		err := l.HoldThen(context.Background(), env(&fakeQuiescer{}), Requests{Operation: &op, Development: &dev},
			func(ctx context.Context) error { return nil },
			func(ctx context.Context) error {
				if ctx.Err() != nil {
					t.Errorf("the second phase starts with a dead context: %v", ctx.Err())
				}
				if got := len(lock.Inherited(ctx)); got != 1 {
					t.Errorf("then inherits %d locks, want the development lock", got)
				}
				// Several watchdog ticks of the released operation lock and the
				// live development lock: neither may cancel the phase.
				select {
				case <-ctx.Done():
					t.Errorf("the context ended during a healthy second phase: %v", context.Cause(ctx))
				case <-time.After(100 * time.Millisecond):
				}
				return nil
			})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("cancels the second phase and reports the loss when the development lock is lost during it", func(t *testing.T) {
		restore := SetWatchdogForTest(20 * time.Millisecond)
		defer restore()
		op, dev, _ := testScopes(t)
		l := NewLifecycle(newFakeOS(), logger.Silent{})
		err := l.HoldThen(context.Background(), env(&fakeQuiescer{}), Requests{Operation: &op, Development: &dev},
			func(ctx context.Context) error { return nil },
			func(ctx context.Context) error {
				replacement, err := os.CreateTemp(filepath.Dir(dev.Target), "replacement-*")
				if err != nil {
					return err
				}
				temp := replacement.Name()
				if err := replacement.Close(); err != nil {
					return err
				}
				if err := os.Rename(temp, dev.Target); err != nil {
					return err
				}
				select {
				case <-ctx.Done():
				case <-time.After(10 * time.Second):
					t.Error("the lost development lock never cancelled the second phase")
				}
				return nil
			})
		if err == nil {
			t.Fatal("a lost development lock went unreported")
		}
	})

	t.Run("reports an operation lock lost during the first phase, which also skips the second", func(t *testing.T) {
		restore := SetWatchdogForTest(20 * time.Millisecond)
		defer restore()
		op, dev, _ := testScopes(t)
		l := NewLifecycle(newFakeOS(), logger.Silent{})
		ran := false
		err := l.HoldThen(context.Background(), env(&fakeQuiescer{}), Requests{Operation: &op, Development: &dev},
			func(ctx context.Context) error {
				replacement, err := os.CreateTemp(filepath.Dir(op.Target), "replacement-*")
				if err != nil {
					return err
				}
				temp := replacement.Name()
				if err := replacement.Close(); err != nil {
					return err
				}
				if err := os.Rename(temp, op.Target); err != nil {
					return err
				}
				select {
				case <-ctx.Done():
				case <-time.After(10 * time.Second):
					t.Error("the lost operation lock never cancelled the first phase")
				}
				return nil
			},
			func(ctx context.Context) error { ran = true; return nil })
		if err == nil || ran {
			t.Fatalf("err = %v, second phase ran = %v, want the loss reported and no second phase", err, ran)
		}
	})

	t.Run("refuses a request that cannot be handed off, as a bug in treerig", func(t *testing.T) {
		op, dev, _ := testScopes(t)
		l := NewLifecycle(newFakeOS(), logger.Silent{})
		for name, requests := range map[string]Requests{
			"no operation lock":      {Development: &dev},
			"only an operation lock": {Operation: &op},
		} {
			ran := false
			err := l.HoldThen(context.Background(), env(&fakeQuiescer{}), requests,
				func(ctx context.Context) error { ran = true; return nil },
				func(ctx context.Context) error { ran = true; return nil })
			if err == nil || !strings.Contains(err.Error(), "this is a bug in treerig") || ran {
				t.Fatalf("%s: err = %v, ran = %v, want a refusal that ran nothing", name, err, ran)
			}
		}
	})
}
