package progress_test

import (
	"errors"
	"regexp"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sofired/berth/internal/progress"
)

// recordingLogger sends every Info message to a channel, so a test can block
// on the Nth heartbeat instead of guessing how long one takes.
type recordingLogger struct {
	lines chan string
}

// Info sends message on the channel, blocking until a test receives it.
func (r recordingLogger) Info(message string) { r.lines <- message }

// Warn discards the message.
func (recordingLogger) Warn(string) {}

// Error discards the message.
func (recordingLogger) Error(string) {}

// Output discards the message.
func (recordingLogger) Output(string) {}

// syncLogger records every Info message behind a mutex instead of a channel,
// so it never blocks a heartbeat goroutine that is racing to shut down —
// unlike recordingLogger, it is safe to leave completely undrained.
//
// When want is positive, reached is closed once that many messages have been
// recorded, so a test can wait for ticks instead of sleeping next to them.
// A want of zero or less disables reached: it is never closed.
type syncLogger struct {
	mu      sync.Mutex
	lines   []string
	want    int
	reached chan struct{}
}

// newSyncLogger returns a syncLogger whose reached channel closes once want
// messages have been recorded.
func newSyncLogger(want int) *syncLogger {
	return &syncLogger{want: want, reached: make(chan struct{})}
}

// Info appends message under the mutex and returns immediately.
func (s *syncLogger) Info(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lines = append(s.lines, message)
	if len(s.lines) == s.want {
		close(s.reached)
	}
}

// Warn discards the message.
func (*syncLogger) Warn(string) {}

// Error discards the message.
func (*syncLogger) Error(string) {}

// Output discards the message.
func (*syncLogger) Output(string) {}

// count returns how many Info messages have been recorded so far.
func (s *syncLogger) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.lines)
}

// failsafe bounds every wait on a message that must arrive. It is a hang
// guard only: nothing is expected to take anywhere near this long, and no
// assertion depends on it.
const failsafe = 30 * time.Second

// heartbeatLine matches a heartbeat for the test phase at any whole elapsed
// second from 1s up, so a slow or loaded machine cannot change what is
// asserted while the 1s floor stays pinned (0s is rejected).
var heartbeatLine = regexp.MustCompile(`^Preparing services \([1-9]\d*s elapsed\)\.$`)

// lateLogger holds a heartbeat inside the logger until the test releases it,
// so a WithProgress that returns without waiting for its heartbeat goroutine
// is caught without any timing guess. From its third Info call on, Info
// signals that it has started and blocks until release is closed.
type lateLogger struct {
	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
}

// Info counts the call; from the third on it signals started and blocks
// until the test closes release.
func (l *lateLogger) Info(string) {
	if l.calls.Add(1) < 3 {
		return
	}
	select {
	case <-l.started:
	default:
		close(l.started)
	}
	<-l.release
}

// Warn discards the message.
func (*lateLogger) Warn(string) {}

// Error discards the message.
func (*lateLogger) Error(string) {}

// Output discards the message.
func (*lateLogger) Output(string) {}

func TestWithProgress(t *testing.T) {
	t.Run("reports elapsed time while a phase is still running and stops after completion", func(t *testing.T) {
		testHeartbeat := 15 * time.Millisecond
		defer progress.SetHeartbeatForTest(testHeartbeat)()

		log := recordingLogger{lines: make(chan string, 4096)}
		release := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- progress.WithProgress(log, "Preparing services", func() error {
				<-release
				return nil
			})
		}()

		mustReceive := func() string {
			t.Helper()
			select {
			case line := <-log.lines:
				return line
			case <-time.After(failsafe):
				t.Fatal("timed out waiting for a progress message")
				return ""
			}
		}

		if got := mustReceive(); got != "Preparing services." {
			t.Fatalf("got %q, want the opening line", got)
		}
		if got := mustReceive(); !heartbeatLine.MatchString(got) {
			t.Fatalf("got %q, want the first heartbeat", got)
		}
		if got := mustReceive(); !heartbeatLine.MatchString(got) {
			t.Fatalf("got %q, want a second heartbeat", got)
		}

		close(release)
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("WithProgress: %v", err)
			}
		case <-time.After(failsafe):
			t.Fatal("timed out waiting for WithProgress to return")
		}

		// Heartbeats that fired between the last receive and the return are
		// legitimately buffered; WithProgress has joined its goroutine, so
		// none can be added. Drain them, then require silence across several
		// further heartbeat intervals.
		for drained := false; !drained; {
			select {
			case <-log.lines:
			default:
				drained = true
			}
		}
		select {
		case line := <-log.lines:
			t.Fatalf("unexpected message after completion: %q", line)
		case <-time.After(3 * testHeartbeat):
		}
	})

	t.Run("logs no heartbeat once WithProgress has returned, racing a very short heartbeat", func(t *testing.T) {
		defer progress.SetHeartbeatForTest(200 * time.Microsecond)()

		// The opening line plus five heartbeats: action returns only once the
		// heartbeat goroutine has ticked at least five times at 200µs, which
		// exercises the shutdown path with a busy ticker. The deterministic
		// guard that WithProgress waits for its goroutine is the late-logger
		// subtest below; this one checks the lines and that the count is
		// stable after return.
		log := newSyncLogger(6)
		if err := progress.WithProgress(log, "Preparing services", func() error {
			select {
			case <-log.reached:
				return nil
			case <-time.After(failsafe):
				return errors.New("timed out waiting for heartbeats")
			}
		}); err != nil {
			t.Fatalf("WithProgress: %v", err)
		}

		log.mu.Lock()
		for i, line := range log.lines {
			if i == 0 {
				if line != "Preparing services." {
					t.Errorf("first line = %q, want the opening line", line)
				}
				continue
			}
			if !heartbeatLine.MatchString(line) {
				t.Errorf("line %d = %q, want a heartbeat", i, line)
			}
		}
		log.mu.Unlock()

		settled := log.count()
		time.Sleep(20 * time.Millisecond)
		if after := log.count(); after != settled {
			t.Fatalf("%d message(s) logged after WithProgress returned (had %d, now %d)", after-settled, settled, after)
		}
	})

	t.Run("waits for an in-flight heartbeat before returning", func(t *testing.T) {
		defer progress.SetHeartbeatForTest(200 * time.Microsecond)()

		log := &lateLogger{started: make(chan struct{}), release: make(chan struct{})}
		actionDone := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- progress.WithProgress(log, "Preparing services", func() error {
				// Return while a heartbeat's Info call is blocked inside the
				// logger: WithProgress must wait for it to finish.
				defer close(actionDone)
				select {
				case <-log.started:
					return nil
				case <-time.After(failsafe):
					return errors.New("timed out waiting for a heartbeat")
				}
			})
		}()

		select {
		case <-actionDone:
		case <-time.After(failsafe):
			close(log.release)
			t.Fatal("timed out waiting for the action to return")
		}
		// The heartbeat is still blocked in Info, so WithProgress must still
		// be waiting for it. This wait can only miss a broken join, never
		// fail a correct one.
		select {
		case err := <-done:
			close(log.release)
			t.Fatalf("WithProgress returned (%v) while a heartbeat was still in Info", err)
		case <-time.After(20 * time.Millisecond):
		}
		close(log.release)
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("WithProgress: %v", err)
			}
		case <-time.After(failsafe):
			t.Fatal("WithProgress did not return once the heartbeat finished")
		}
	})
}
