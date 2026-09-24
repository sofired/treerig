package progress_test

import (
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
}
