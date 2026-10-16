package lifecycle

import (
	"context"
	"errors"
	"syscall"
	"testing"
	"time"

	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/logger"
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

	t.Run("Repeated stays open after one signal and closes after two", func(t *testing.T) {
		os := newFakeOS()
		l := NewLifecycle(os, logger.Silent{})
		l.Start()

		os.send(syscall.SIGINT)
		waitFor(t, func() bool { return l.Pending() == syscall.SIGINT })

		select {
		case <-l.Repeated():
			t.Fatal("Repeated() closed after only one signal")
		default:
		}

		os.send(syscall.SIGTERM)
		waitFor(t, func() bool {
			select {
			case <-l.Repeated():
				return true
			default:
				return false
			}
		})
	})

	t.Run("skips a signal already ignored at startup", func(t *testing.T) {
		os := newFakeOS()
		os.ignored[syscall.SIGINT] = true
		os.ignored[syscall.SIGTERM] = true
		os.ignored[syscall.SIGHUP] = true
		l := NewLifecycle(os, logger.Silent{})
		l.Start()
		if os.ch != nil {
			t.Fatal("did not expect Notify to be called when every signal is ignored")
		}
	})

	t.Run("a closed terminal's SIGHUP is caught like SIGINT and SIGTERM, and re-raised as itself", func(t *testing.T) {
		os := newFakeOS()
		l := NewLifecycle(os, logger.Silent{})
		l.Start()

		os.send(syscall.SIGHUP)
		waitFor(t, func() bool { return l.Context().Err() != nil })

		cancelled, ok := context.Cause(l.Context()).(*clierr.Cancelled)
		if !ok || cancelled.Signal != syscall.SIGHUP {
			t.Fatalf("cause = %#v", context.Cause(l.Context()))
		}
		l.Exit()
		if calls := os.killCalls(); len(calls) != 1 || calls[0].sig != syscall.SIGHUP {
			t.Fatalf("re-raised %+v, want SIGHUP", calls)
		}
	})

	t.Run("back-to-back SIGHUPs are one hangup: no still-stopping line, and Repeated stays open", func(t *testing.T) {
		os := newFakeOS()
		rec := &logger.Recording{}
		l := NewLifecycle(os, rec)
		l.Start()

		os.send(syscall.SIGHUP)
		waitFor(t, func() bool { return l.Pending() == syscall.SIGHUP })
		os.send(syscall.SIGHUP)
		os.send(syscall.SIGHUP)
		time.Sleep(50 * time.Millisecond) // let a stray second notice land, if it would

		select {
		case <-l.Repeated():
			t.Fatal("Repeated closed after a second SIGHUP")
		default:
		}
		if lines, _ := rec.Snapshot(); len(lines) != 0 {
			t.Fatalf("logged %v, want nothing", lines)
		}
		// A SIGINT after the hangup is a real second signal.
		os.send(syscall.SIGINT)
		waitFor(t, func() bool {
			select {
			case <-l.Repeated():
				return true
			default:
				return false
			}
		})
		if l.Pending() != syscall.SIGHUP {
			t.Fatalf("Pending() = %v, want the hangup to stick", l.Pending())
		}
	})

	t.Run("a SIGHUP already ignored, as under nohup, is left ignored while the others are still caught", func(t *testing.T) {
		os := newFakeOS()
		os.ignored[syscall.SIGHUP] = true
		l := NewLifecycle(os, logger.Silent{})
		l.Start()

		if len(os.notified) != 2 || os.notified[0] != syscall.SIGINT || os.notified[1] != syscall.SIGTERM {
			t.Fatalf("Notify asked for %v, want SIGINT and SIGTERM only", os.notified)
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

// TestFinish covers Finish's plain-versus-not split: a plain err (nil, or a
// bare *clierr.Cancelled) ends the process by a pending signal exactly as
// Exit alone would, reporting nothing of its own; anything else — a
// different error, or one attach wrapped a Cancelled inside, the shape a
// release failure racing a signal takes — is reported once and returns the
// exit code it calls for, whether or not a signal happened to be pending
// too, and never reaches Exit.
func TestFinish(t *testing.T) {
	abandoned := clierr.New("The sh command left processes running (process group 123), and waiting for them was abandoned after a second interrupt; stop them with kill -KILL -123.")
	// The shape Hold's own attach leaves a release failure that raced a
	// signal in: a *clierr.Error whose Cause, not its own type, is the
	// *clierr.Cancelled — so it must not read as plain either.
	racedRelease := attach(&clierr.Cancelled{Signal: syscall.SIGINT}, []error{errors.New("could not release the lock")})

	tests := []struct {
		name       string
		err        error
		withSignal bool
		wantCode   int
		wantReport bool
		wantExit   bool
	}{
		{
			name:     "nil error, no signal pending: returns 0, reports nothing, never exits",
			err:      nil,
			wantCode: 0,
		},
		{
			name:       "nil error, a signal pending: ends the process by it, reports nothing",
			err:        nil,
			withSignal: true,
			wantExit:   true,
		},
		{
			name:       "a bare Cancelled, a signal pending: ends the process by it, reports nothing",
			err:        &clierr.Cancelled{Signal: syscall.SIGINT},
			withSignal: true,
			wantExit:   true,
		},
		{
			name:       "a non-Cancelled error, no signal pending: reported, returns its own code",
			err:        abandoned,
			wantCode:   1,
			wantReport: true,
		},
		{
			name:       "a non-Cancelled error, a signal pending too: still reported, returns its own code, never exits",
			err:        abandoned,
			withSignal: true,
			wantCode:   1,
			wantReport: true,
		},
		{
			name:       "attach wrapped a Cancelled inside a release failure, no signal pending: reported, returns its own code",
			err:        racedRelease,
			wantCode:   1,
			wantReport: true,
		},
		{
			name:       "attach wrapped a Cancelled inside a release failure, a signal pending too: still reported, returns its own code, never exits",
			err:        racedRelease,
			withSignal: true,
			wantCode:   1,
			wantReport: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			os := newFakeOS()
			l := NewLifecycle(os, logger.Silent{})
			if tt.withSignal {
				l.Start()
				os.send(syscall.SIGINT)
				waitFor(t, func() bool { return l.Pending() == syscall.SIGINT })
			}

			var reportCalls int
			var reported error
			code := l.Finish(tt.err, func(e error) {
				reportCalls++
				reported = e
			})

			if reportCalls != 0 != tt.wantReport {
				t.Fatalf("report called %d time(s), want called=%v", reportCalls, tt.wantReport)
			}
			if tt.wantReport && reported != tt.err {
				t.Fatalf("report was called with %v, want the same error Finish was given (%v)", reported, tt.err)
			}
			exited := len(os.exitCalls()) != 0
			if exited != tt.wantExit {
				t.Fatalf("Exit called = %v, want %v (exitCalls=%v)", exited, tt.wantExit, os.exitCalls())
			}
			if !tt.wantExit && code != tt.wantCode {
				t.Fatalf("Finish returned %d, want %d", code, tt.wantCode)
			}
		})
	}
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
