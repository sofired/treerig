//go:build integration

package lifecycle

// Scenario 10 from the design doc, the part exercised without re-executing
// this binary as a helper: a real child (sleep) held under a real lock,
// SIGINT delivered through the injectable OS so the test process's own
// signal handling is never touched, proving children get SIGTERM on their
// group, the lock is released, and its record is marked released.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/lock"
	"github.com/sofired/treerig/internal/logger"
	"github.com/sofired/treerig/internal/run"
	"github.com/sofired/treerig/internal/run/osrun"
)

func TestSignalDuringHeldAction(t *testing.T) {
	dir := t.TempDir()
	op := lock.Scope{Target: filepath.Join(dir, "op"), Description: "worktree main"}
	ready := filepath.Join(dir, "child.ready")

	runner := osrun.New()
	fakeos := newFakeOS()
	l := NewLifecycle(fakeos, logger.Silent{})
	l.Start()

	actionDone := make(chan error, 1)
	go func() {
		err := l.Hold(l.Context(), Env{Logger: logger.Silent{}, Runner: runner, Invocation: "treerig up"},
			Requests{Operation: &op},
			func(ctx context.Context) error {
				result, err := runner.Run(ctx, run.Spec{Command: "sh", Args: []string{"-c", `: > "$0"; exec sleep 30`, ready}, AllowFailure: true})
				if err != nil {
					return err
				}
				if result.Signal == 0 {
					return errors.New("expected the child to have been signalled")
				}
				return nil
			})
		actionDone <- err
	}()

	// Signalled only once the child is certainly running under the action.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the action's child never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	fakeos.send(syscall.SIGINT)

	var err error
	select {
	case err = <-actionDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Hold never returned after SIGINT")
	}
	if err == nil {
		t.Fatal("expected an error after SIGINT")
	}
	var cancelled *clierr.Cancelled
	if !errors.As(err, &cancelled) {
		t.Fatalf("got %v (%T)", err, err)
	}
	if cancelled.Signal != syscall.SIGINT {
		t.Fatalf("Signal = %v", cancelled.Signal)
	}

	// The lock must be free, with its record marked released.
	rec, rerr := readHolderRecordForTest(op.Target)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if rec == nil || rec["releasedAt"] == nil {
		t.Fatalf("expected a released record, got %v", rec)
	}
	probe := lock.New(op, lock.Options{Logger: logger.Silent{}, Watchdog: -1})
	if _, err := probe.Acquire(context.Background(), "probe"); err != nil {
		t.Fatalf("expected the lock to be free: %v", err)
	}
	_ = probe.Release()
}

// readHolderRecordForTest avoids depending on lock's unexported Record type
// from this package; the released marker is all this test needs.
func readHolderRecordForTest(target string) (map[string]any, error) {
	data, err := os.ReadFile(target + ".holder")
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return m, nil
}
