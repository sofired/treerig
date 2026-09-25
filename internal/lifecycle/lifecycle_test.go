package lifecycle

import (
	"context"
	"syscall"
	"testing"
	"time"

	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/logger"
)

// waitFor polls condition until it is true, failing the test if it never
// becomes true within a short deadline.
func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for condition")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestLifecycleSignals(t *testing.T) {
	t.Run("the first signal cancels the root context with its cause", func(t *testing.T) {
		os := newFakeOS()
		rec := &logger.Recording{}
		l := NewLifecycle(os, rec)
		l.Start()

		os.send(syscall.SIGINT)
		waitFor(t, func() bool { return l.Context().Err() != nil })

		cause := context.Cause(l.Context())
		cancelled, ok := cause.(*clierr.Cancelled)
		if !ok || cancelled.Signal != syscall.SIGINT {
			t.Fatalf("got %#v", cause)
		}
		if l.Pending() != syscall.SIGINT {
			t.Fatalf("Pending() = %v", l.Pending())
		}
	})

	t.Run("a later signal prints one still-stopping line and does not replace the pending signal", func(t *testing.T) {
		os := newFakeOS()
		rec := &logger.Recording{}
		l := NewLifecycle(os, rec)
		l.Start()

		os.send(syscall.SIGINT)
		waitFor(t, func() bool { return l.Pending() == syscall.SIGINT })
		os.send(syscall.SIGTERM)
		os.send(syscall.SIGTERM)
		waitFor(t, func() bool {
			lines, _ := rec.Snapshot()
			return len(lines) > 0
		})
		time.Sleep(20 * time.Millisecond) // let a second stray notice land, if it would

		if l.Pending() != syscall.SIGINT {
			t.Fatalf("Pending() = %v, want the first signal to stick", l.Pending())
		}
		lines, _ := rec.Snapshot()
		count := 0
		for _, line := range lines {
			if contains(line, "Still stopping") {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("got %d still-stopping lines, want 1: %v", count, lines)
		}
	})

	t.Run("skips a signal already ignored at startup", func(t *testing.T) {
		os := newFakeOS()
		os.ignored[syscall.SIGINT] = true
		os.ignored[syscall.SIGTERM] = true
		l := NewLifecycle(os, logger.Silent{})
		l.Start()
		if os.ch != nil {
			t.Fatal("did not expect Notify to be called when every signal is ignored")
		}
	})

	t.Run("Exit re-raises the pending signal and falls back if the process survives", func(t *testing.T) {
		os := newFakeOS()
		l := NewLifecycle(os, logger.Silent{})
		l.Start()
		os.send(syscall.SIGTERM)
		waitFor(t, func() bool { return l.Pending() == syscall.SIGTERM })

		l.Exit()

		if os.resetSig != syscall.SIGTERM {
			t.Fatalf("Reset was called with %v", os.resetSig)
		}
		calls := os.killCalls()
		if len(calls) != 1 || calls[0].pid != 4242 || calls[0].sig != syscall.SIGTERM {
			t.Fatalf("got %+v", calls)
		}
		exits := os.exitCalls()
		if len(exits) != 1 || exits[0] != 128+int(syscall.SIGTERM) {
			t.Fatalf("got %v", exits)
		}
	})

	t.Run("Exit does nothing when no signal is pending", func(t *testing.T) {
		os := newFakeOS()
		l := NewLifecycle(os, logger.Silent{})
		l.Exit()
		if len(os.killCalls()) != 0 || len(os.exitCalls()) != 0 {
			t.Fatal("did not expect Exit to act with no pending signal")
		}
	})
}

// contains reports whether substr occurs anywhere in s.
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (func() bool {
		for i := 0; i+len(substr) <= len(s); i++ {
			if s[i:i+len(substr)] == substr {
				return true
			}
		}
		return false
	})()
}
