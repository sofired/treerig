package progress_test

import (
	"errors"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sofired/treerig/internal/logger"
	"github.com/sofired/treerig/internal/progress"
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

// Detail discards the message.
func (recordingLogger) Detail(string) {}

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

// Detail discards the message.
func (*syncLogger) Detail(string) {}

// Output discards the message.
func (*syncLogger) Output(string) {}

// failsafe bounds every wait on a message that must arrive. It is a hang
// guard only: nothing is expected to take anywhere near this long, and no
// assertion depends on it.
const failsafe = 30 * time.Second

// heartbeatLine matches a heartbeat for the test phase at any elapsed time
// formatElapsed can print, "Ns" from 1s up or "Mm SSs" from a minute on, so a
// slow or loaded machine cannot change what is asserted while the 1s floor
// stays pinned (0s is rejected).
var heartbeatLine = regexp.MustCompile(`^still preparing services, ([1-9]\d*s|[1-9]\d*m [0-5]\ds)$`)

// lateLogger holds a heartbeat inside the logger until the test releases it,
// so a Phase that returns without waiting for its heartbeat goroutine
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

// Detail discards the message.
func (*lateLogger) Detail(string) {}

// Output discards the message.
func (*lateLogger) Output(string) {}

func TestPlainPhase(t *testing.T) {
	t.Run("reports elapsed time while a phase is still running and stops after completion", func(t *testing.T) {
		testHeartbeat := 15 * time.Millisecond
		defer progress.SetHeartbeatForTest(testHeartbeat)()

		log := recordingLogger{lines: make(chan string, 4096)}
		release := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- progress.NewPlain(log).Phase(progress.PhaseSpec{Verb: "Preparing", What: "services"}, func() error {
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
				t.Fatalf("Phase: %v", err)
			}
		case <-time.After(failsafe):
			t.Fatal("timed out waiting for Phase to return")
		}

		// Heartbeats that fired between the last receive and the return are
		// legitimately buffered; Phase has joined its goroutine, so
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

	t.Run("logs no heartbeat once Phase has returned, racing a very short heartbeat", func(t *testing.T) {
		defer progress.SetHeartbeatForTest(200 * time.Microsecond)()

		// The opening line plus five heartbeats: action returns only once the
		// heartbeat goroutine has ticked at least five times at 200µs, which
		// exercises the shutdown path with a busy ticker. The deterministic
		// guard that Phase waits for its goroutine is the late-logger
		// subtest below; this one checks the lines and that the count is
		// stable after return.
		log := newSyncLogger(6)
		if err := progress.NewPlain(log).Phase(progress.PhaseSpec{Verb: "Preparing", What: "services"}, func() error {
			select {
			case <-log.reached:
				return nil
			case <-time.After(failsafe):
				return errors.New("timed out waiting for heartbeats")
			}
		}); err != nil {
			t.Fatalf("Phase: %v", err)
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
	})

	t.Run("waits for an in-flight heartbeat before returning", func(t *testing.T) {
		defer progress.SetHeartbeatForTest(200 * time.Microsecond)()

		log := &lateLogger{started: make(chan struct{}), release: make(chan struct{})}
		actionDone := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- progress.NewPlain(log).Phase(progress.PhaseSpec{Verb: "Preparing", What: "services"}, func() error {
				// Return while a heartbeat's Info call is blocked inside the
				// logger: Phase must wait for it to finish.
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
		// The heartbeat is still blocked in Info, so Phase must still
		// be waiting for it. This wait can only miss a broken join, never
		// fail a correct one.
		select {
		case err := <-done:
			close(log.release)
			t.Fatalf("Phase returned (%v) while a heartbeat was still in Info", err)
		case <-time.After(20 * time.Millisecond):
		}
		close(log.release)
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Phase: %v", err)
			}
		case <-time.After(failsafe):
			t.Fatal("Phase did not return once the heartbeat finished")
		}
	})
}

func TestPlainPhaseResult(t *testing.T) {
	t.Run("announces the phase and returns the action's error", func(t *testing.T) {
		defer progress.SetHeartbeatForTest(time.Hour)()
		log := newSyncLogger(0)
		want := errors.New("boom")
		if got := progress.NewPlain(log).Phase(progress.PhaseSpec{Verb: "Stopping", What: "web"}, func() error { return want }); got != want {
			t.Fatalf("Phase = %v, want %v", got, want)
		}
		if len(log.lines) != 1 || log.lines[0] != "Stopping web." {
			t.Fatalf("lines = %q", log.lines)
		}
	})

	t.Run("words the heartbeat as still doing the thing, with the seconds", func(t *testing.T) {
		defer progress.SetHeartbeatForTest(time.Millisecond)()
		log := newSyncLogger(2)
		if err := progress.NewPlain(log).Phase(progress.PhaseSpec{Verb: "Starting", What: "web for feature-x"}, func() error {
			select {
			case <-log.reached:
				return nil
			case <-time.After(failsafe):
				return errors.New("no heartbeat")
			}
		}); err != nil {
			t.Fatal(err)
		}
		if !regexp.MustCompile(`^still starting web for feature-x, [1-9]\d*s$`).MatchString(log.lines[1]) {
			t.Fatalf("heartbeat = %q", log.lines[1])
		}
	})
}

func TestPhaseNote(t *testing.T) {
	t.Run("carries the last thing the action said, safe to print and cut short", func(t *testing.T) {
		defer progress.SetHeartbeatForTest(time.Millisecond)()
		log := newSyncLogger(2)
		var latest progress.Latest
		latest.Set("")
		latest.Set(" Container web \x1b[2K " + strings.Repeat("x", 300))
		if err := progress.NewPlain(log).Phase(progress.PhaseSpec{Verb: "Starting", What: "web", Note: latest.Get}, func() error {
			select {
			case <-log.reached:
				return nil
			case <-time.After(failsafe):
				return errors.New("no heartbeat")
			}
		}); err != nil {
			t.Fatal(err)
		}
		line := log.lines[1]
		if !strings.HasPrefix(line, "still starting web, ") || !strings.Contains(line, ": Container web \ufffd[2K xxx") || !strings.HasSuffix(line, "...") {
			t.Fatalf("heartbeat = %q", line)
		}
		if note := line[strings.Index(line, ": ")+2:]; len([]rune(note)) > 120 {
			t.Fatalf("note has %d characters", len([]rune(note)))
		}
	})

	t.Run("hides the secrets in a note", func(t *testing.T) {
		defer progress.SetHeartbeatForTest(time.Millisecond)()
		log := newSyncLogger(2)
		var latest progress.Latest
		latest.Set("pulling https://u:p@registry.example/img with TOKEN=abc123")
		_ = progress.NewPlain(log).Phase(progress.PhaseSpec{Verb: "Starting", What: "web", Note: latest.Get}, func() error {
			<-log.reached
			return nil
		})
		if strings.Contains(log.lines[1], "u:p@") || strings.Contains(log.lines[1], "abc123") || !strings.Contains(log.lines[1], "https://<redacted>@registry.example/img with TOKEN=<redacted>") {
			t.Fatalf("heartbeat = %q", log.lines[1])
		}
	})

	t.Run("adds nothing when there is no note", func(t *testing.T) {
		defer progress.SetHeartbeatForTest(time.Millisecond)()
		log := newSyncLogger(2)
		_ = progress.NewPlain(log).Phase(progress.PhaseSpec{Verb: "Starting", What: "web", Note: func() string { return "  " }}, func() error {
			<-log.reached
			return nil
		})
		if strings.Contains(log.lines[1], ":") {
			t.Fatalf("heartbeat = %q", log.lines[1])
		}
	})
}

func TestOr(t *testing.T) {
	plain := progress.NewPlain(logger.Silent{})
	if progress.Or(plain, nil) != progress.Progress(plain) {
		t.Error("Or replaced a writer it was given")
	}
	if _, ok := progress.Or(nil, logger.Silent{}).(*progress.Plain); !ok {
		t.Error("Or(nil) is not the plain writer")
	}
}
