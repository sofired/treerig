package logger

import (
	"bytes"
	"sync"
	"testing"
)

// recordingWriter records each call to Write as one entry, so a test can
// assert that a message reached its stream as a single write rather than
// several concatenated ones.
type recordingWriter struct {
	mu     sync.Mutex
	writes []string
}

// Write records p as one entry and always reports the full write as successful.
func (w *recordingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes = append(w.writes, string(p))
	return len(p), nil
}

// all returns a copy of the writes recorded so far.
func (w *recordingWriter) all() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.writes...)
}

// Which stream a line goes to is a contract: anything the tool says about
// itself has to stay out of output someone is piping.

func TestConsoleStreamRouting(t *testing.T) {
	t.Run("Output and Raw go to stdout and stderr respectively, and nowhere else", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		c := NewConsole("berth", &stdout, &stderr)

		c.Output(`{"project":"acme"}`)
		if got, want := stdout.String(), "{\"project\":\"acme\"}\n"; got != want {
			t.Errorf("stdout = %q, want %q", got, want)
		}
		if stderr.Len() != 0 {
			t.Errorf("Output wrote to stderr: %q", stderr.String())
		}

		stdout.Reset()
		c.Raw("goroutine 1 [running]:")
		if stdout.Len() != 0 {
			t.Errorf("Raw wrote to stdout: %q", stdout.String())
		}
		if got, want := stderr.String(), "goroutine 1 [running]:\n"; got != want {
			t.Errorf("stderr = %q, want %q", got, want)
		}
	})

	t.Run("Info, Warn and Error go to stderr only", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		c := NewConsole("berth", &stdout, &stderr)

		c.Info("Waiting for the shared router lock.")
		c.Warn("Could not refresh the checkout index.")
		c.Error("Docker is not available.")

		if stdout.Len() != 0 {
			t.Errorf("Info/Warn/Error wrote to stdout: %q", stdout.String())
		}
		want := "[berth] Waiting for the shared router lock.\n" +
			"[berth] WARNING: Could not refresh the checkout index.\n" +
			"[berth] ERROR: Docker is not available.\n"
		if got := stderr.String(); got != want {
			t.Errorf("stderr = %q, want %q", got, want)
		}
	})
}

func TestConsolePrefixes(t *testing.T) {
	t.Run("answers to the name it was invoked as", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		NewConsole("duke", &stdout, &stderr).Info("Ready.")
		if got, want := stderr.String(), "[duke] Ready.\n"; got != want {
			t.Errorf("stderr = %q, want %q", got, want)
		}
	})

	t.Run("Warn and Error add their own label after the prefix", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		c := NewConsole("berth", &stdout, &stderr)
		c.Warn("a")
		c.Error("b")
		want := "[berth] WARNING: a\n[berth] ERROR: b\n"
		if got := stderr.String(); got != want {
			t.Errorf("stderr = %q, want %q", got, want)
		}
	})
}

// Borrowed text is anything a subprocess, another process's lock record, or
// a config file put into a message. It must not be able to drive the
// terminal, and it must not be able to forge a line that looks like one this
// tool wrote.

func TestConsoleLineSafety(t *testing.T) {
	t.Run("neutralizes escape sequences and directional overrides from borrowed text", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		c := NewConsole("berth", &stdout, &stderr)
		c.Warn("fatal: \u001b[2Knot a git repository")
		c.Error("held by \u202eevil")

		want := "[berth] WARNING: fatal: �[2Knot a git repository\n" +
			"[berth] ERROR: held by �evil\n"
		if got := stderr.String(); got != want {
			t.Errorf("stderr = %q, want %q", got, want)
		}
	})

	t.Run("leaves Output exactly as composed, unfiltered", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		c := NewConsole("berth", &stdout, &stderr)
		c.Output("{\"branch\":\"feature/\u202ex\"}")
		if got, want := stdout.String(), "{\"branch\":\"feature/\u202ex\"}\n"; got != want {
			t.Errorf("stdout = %q, want %q", got, want)
		}
	})

	t.Run("keeps a message that is several lines several lines, indenting continuations", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		c := NewConsole("berth", &stdout, &stderr)
		c.Error("berth.toml is not valid:\n  project.name: required\n  compose.file: \u001b[2Kmissing")

		want := "[berth] ERROR: berth.toml is not valid:\n" +
			"    project.name: required\n" +
			"    compose.file: �[2Kmissing\n"
		if got := stderr.String(); got != want {
			t.Errorf("stderr = %q, want %q", got, want)
		}
	})

	t.Run("will not let borrowed text forge a line of its own", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		NewConsole("berth", &stdout, &stderr).Error("docker said:\n[berth] ERROR: everything is fine")

		want := "[berth] ERROR: docker said:\n  [berth] ERROR: everything is fine\n"
		if got := stderr.String(); got != want {
			t.Errorf("stderr = %q, want %q", got, want)
		}
	})

	t.Run("will not let a separator that is not a newline forge a line either", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		// U+2028 is a line terminator no newline split sees.
		NewConsole("berth", &stdout, &stderr).Error("docker said: [berth] ERROR: everything is fine")

		want := "[berth] ERROR: docker said:�[berth] ERROR: everything is fine\n"
		if got := stderr.String(); got != want {
			t.Errorf("stderr = %q, want %q", got, want)
		}
		if bytes.ContainsRune(stderr.Bytes(), ' ') {
			t.Errorf("stderr still contains U+2028: %q", stderr.String())
		}
	})

	t.Run("Raw filters per line without adding a prefix or indent", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		NewConsole("berth", &stdout, &stderr).Raw("panic: boom\n\u001b[2Kgoroutine 1 [running]:\nmain.go:1")

		want := "panic: boom\n�[2Kgoroutine 1 [running]:\nmain.go:1\n"
		if got := stderr.String(); got != want {
			t.Errorf("stderr = %q, want %q", got, want)
		}
	})
}

// A shared stream, whether the terminal or a pipe, only stays coherent under
// concurrent writers if each message lands in one Write call; anything split
// across two could be interleaved with another goroutine's write.

func TestConsoleOneWritePerMessage(t *testing.T) {
	cases := []struct {
		name string
		call func(c *Console)
	}{
		{"Info", func(c *Console) { c.Info("one line") }},
		{"Warn multi-line", func(c *Console) { c.Warn("line one\nline two\nline three") }},
		{"Error multi-line", func(c *Console) { c.Error("line one\nline two") }},
		{"Output", func(c *Console) { c.Output("line one\nline two") }},
		{"Raw multi-line", func(c *Console) { c.Raw("line one\nline two") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout := &recordingWriter{}
			stderr := &recordingWriter{}
			c := NewConsole("berth", stdout, stderr)
			tc.call(c)

			writes := append(stdout.all(), stderr.all()...)
			if len(writes) != 1 {
				t.Fatalf("got %d writes, want 1: %q", len(writes), writes)
			}
		})
	}
}

func TestConsoleConcurrentWrites(t *testing.T) {
	t.Run("interleaved goroutines never split or merge a message", func(t *testing.T) {
		stdout := &recordingWriter{}
		stderr := &recordingWriter{}
		c := NewConsole("berth", stdout, stderr)

		const n = 50
		var wg sync.WaitGroup
		wg.Add(n * 2)
		for i := 0; i < n; i++ {
			go func() {
				defer wg.Done()
				c.Info("concurrent info")
			}()
			go func() {
				defer wg.Done()
				c.Output("concurrent output")
			}()
		}
		wg.Wait()

		for _, w := range stderr.all() {
			if w != "[berth] concurrent info\n" {
				t.Fatalf("stderr write corrupted: %q", w)
			}
		}
		for _, w := range stdout.all() {
			if w != "concurrent output\n" {
				t.Fatalf("stdout write corrupted: %q", w)
			}
		}
		if got := len(stderr.all()); got != n {
			t.Errorf("stderr writes = %d, want %d", got, n)
		}
		if got := len(stdout.all()); got != n {
			t.Errorf("stdout writes = %d, want %d", got, n)
		}
	})
}

func TestSilentDiscardsEverything(t *testing.T) {
	t.Run("every method is a no-op", func(t *testing.T) {
		var s Silent
		// Must not panic, and there is nothing to observe: Silent holds no
		// writers at all.
		s.Info("a")
		s.Warn("b")
		s.Error("c")
		s.Output("d")
	})
}

func TestRecording(t *testing.T) {
	t.Run("prefixes each level and keeps Output separate from Lines", func(t *testing.T) {
		r := &Recording{}
		r.Info("a")
		r.Warn("b")
		r.Error("c")
		r.Output("d")

		lines, printed := r.Snapshot()
		wantLines := []string{"info: a", "warn: b", "error: c"}
		if len(lines) != len(wantLines) {
			t.Fatalf("Lines = %v, want %v", lines, wantLines)
		}
		for i := range wantLines {
			if lines[i] != wantLines[i] {
				t.Errorf("Lines[%d] = %q, want %q", i, lines[i], wantLines[i])
			}
		}
		if len(printed) != 1 || printed[0] != "d" {
			t.Errorf("Printed = %v, want [%q]", printed, "d")
		}
	})

	t.Run("Snapshot returns copies the caller can mutate safely", func(t *testing.T) {
		r := &Recording{}
		r.Info("a")
		r.Output("x")

		lines, printed := r.Snapshot()
		lines[0] = "tampered"
		printed[0] = "tampered"

		lines2, printed2 := r.Snapshot()
		if lines2[0] != "info: a" {
			t.Errorf("mutating a snapshot changed the recording: Lines = %v", lines2)
		}
		if printed2[0] != "x" {
			t.Errorf("mutating a snapshot changed the recording: Printed = %v", printed2)
		}
	})

	t.Run("stays race-free under concurrent logging and snapshots", func(t *testing.T) {
		r := &Recording{}
		const n = 50
		var wg sync.WaitGroup
		wg.Add(n * 2)
		for i := 0; i < n; i++ {
			go func() {
				defer wg.Done()
				r.Info("x")
				r.Output("y")
			}()
			go func() {
				defer wg.Done()
				r.Snapshot()
			}()
		}
		wg.Wait()

		lines, printed := r.Snapshot()
		if len(lines) != n {
			t.Errorf("Lines has %d entries, want %d", len(lines), n)
		}
		if len(printed) != n {
			t.Errorf("Printed has %d entries, want %d", len(printed), n)
		}
	})
}
