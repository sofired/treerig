package progress_test

import (
	"sync"
	"testing"
	"time"

	"github.com/sofired/berth/internal/progress"
)

// recordingLogger sends every Info message to a channel, so a test can block
// on the Nth heartbeat instead of guessing how long one takes.
type recordingLogger struct {
	lines chan string
}

func (r recordingLogger) Info(message string) { r.lines <- message }
func (recordingLogger) Warn(string)           {}
func (recordingLogger) Error(string)          {}
func (recordingLogger) Output(string)         {}

// syncLogger records every Info message behind a mutex instead of a channel,
// so it never blocks a heartbeat goroutine that is racing to shut down —
// unlike recordingLogger, it is safe to leave completely undrained.
type syncLogger struct {
	mu    sync.Mutex
	lines []string
}

func (s *syncLogger) Info(message string) {
	s.mu.Lock()
	s.lines = append(s.lines, message)
	s.mu.Unlock()
}
func (*syncLogger) Warn(string)   {}
func (*syncLogger) Error(string)  {}
func (*syncLogger) Output(string) {}

func (s *syncLogger) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.lines)
}

func TestWithProgress(t *testing.T) {
	t.Run("reports elapsed time while a phase is still running and stops after completion", func(t *testing.T) {
		testHeartbeat := 15 * time.Millisecond
		defer progress.SetHeartbeatForTest(testHeartbeat)()

		log := recordingLogger{lines: make(chan string, 16)}
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
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for a progress message")
				return ""
			}
		}

		if got := mustReceive(); got != "Preparing services." {
			t.Fatalf("got %q, want the opening line", got)
		}
		if got := mustReceive(); got != "Preparing services (1s elapsed)." {
			t.Fatalf("got %q, want the first heartbeat", got)
		}
		if got := mustReceive(); got != "Preparing services (1s elapsed)." {
			t.Fatalf("got %q, want a second heartbeat at the same rounded second", got)
		}

		close(release)
		if err := <-done; err != nil {
			t.Fatalf("WithProgress: %v", err)
		}

		// No further heartbeat arrives once the phase has completed.
		select {
		case line := <-log.lines:
			t.Fatalf("unexpected message after completion: %q", line)
		case <-time.After(3 * testHeartbeat):
		}
	})

	t.Run("logs no heartbeat once WithProgress has returned, racing a very short heartbeat", func(t *testing.T) {
		defer progress.SetHeartbeatForTest(200 * time.Microsecond)()

		log := &syncLogger{}
		// Long enough, next to a 200µs heartbeat, that several ticks are
		// reliably pending right as action returns — the exact window the
		// fix's second, non-blocking done check and the wait for the
		// heartbeat goroutine to actually exit are both there for.
		if err := progress.WithProgress(log, "Preparing services", func() error {
			time.Sleep(5 * time.Millisecond)
			return nil
		}); err != nil {
			t.Fatalf("WithProgress: %v", err)
		}

		settled := log.count()
		time.Sleep(20 * time.Millisecond)
		if after := log.count(); after != settled {
			t.Fatalf("%d message(s) logged after WithProgress returned (had %d, now %d)", after-settled, settled, after)
		}
	})
}
