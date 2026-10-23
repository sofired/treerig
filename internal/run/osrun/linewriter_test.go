package osrun

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/sofired/treerig/internal/run"
)

func TestLineWriter(t *testing.T) {
	hideSecret := func(line string) bool { return strings.Contains(line, "secret") }

	t.Run("forwards everything when nothing is hidden or observed", func(t *testing.T) {
		var out bytes.Buffer
		w := newLineWriter(&out, nil, nil)
		if n, err := w.Write([]byte("a\nb")); n != 3 || err != nil {
			t.Fatalf("Write = %d, %v", n, err)
		}
		if got := out.String(); got != "a\nb" {
			t.Fatalf("got %q, want the bytes as written, partial line included", got)
		}
	})

	t.Run("leaves out the lines the filter rejects and keeps the others whole", func(t *testing.T) {
		var out bytes.Buffer
		w := newLineWriter(&out, hideSecret, nil)
		_, _ = w.Write([]byte("one\nthe secret\r\ntwo\n"))
		if got, want := out.String(), "one\ntwo\n"; got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("joins a line split across writes before judging it", func(t *testing.T) {
		var out bytes.Buffer
		w := newLineWriter(&out, hideSecret, nil)
		_, _ = w.Write([]byte("keep\nthe sec"))
		_, _ = w.Write([]byte("ret\nafter\n"))
		if got, want := out.String(), "keep\nafter\n"; got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("flushes a last line that has no line break, once", func(t *testing.T) {
		var out bytes.Buffer
		w := newLineWriter(&out, hideSecret, nil)
		_, _ = w.Write([]byte("tail"))
		if out.Len() != 0 {
			t.Fatalf("wrote %q before the line ended", out.String())
		}
		w.flush()
		w.flush()
		if got := out.String(); got != "tail" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("observes the lines that are not hidden, and shows nothing without a destination", func(t *testing.T) {
		var seen []string
		w := newLineWriter(nil, hideSecret, func(line string) { seen = append(seen, line) })
		_, _ = w.Write([]byte("a\r\nthe secret\nlast"))
		w.flush()
		if got := strings.Join(seen, "|"); got != "a|last" {
			t.Fatalf("observed %q", got)
		}
	})

	t.Run("passes on a line that never ends once it is longer than the cap", func(t *testing.T) {
		var out bytes.Buffer
		var seen int
		w := newLineWriter(&out, func(string) bool { return false }, func(line string) { seen += len(line) })
		chunk := bytes.Repeat([]byte("x"), 10*1024)
		for i := 0; i < 20; i++ {
			_, _ = w.Write(chunk)
			if len(w.pending) > maxPendingLine {
				t.Fatalf("holding %d bytes, more than the cap", len(w.pending))
			}
		}
		w.flush()
		if out.Len() != 20*len(chunk) || seen != out.Len() {
			t.Fatalf("showed %d bytes and observed %d, want %d of both", out.Len(), seen, 20*len(chunk))
		}
	})

	t.Run("is safe for two writers at once", func(t *testing.T) {
		var out syncBuffer
		w := newLineWriter(&out, hideSecret, func(string) {})
		var wg sync.WaitGroup
		for _, line := range []string{"one\n", "two secret\n"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 500; i++ {
					_, _ = w.Write([]byte(line))
				}
			}()
		}
		wg.Wait()
		if got := strings.Count(out.String(), "one\n"); got != 500 || strings.Contains(out.String(), "secret") {
			t.Fatalf("shown %d kept lines, output has a hidden one: %v", got, strings.Contains(out.String(), "secret"))
		}
	})
}

// syncBuffer is a bytes.Buffer safe to read after concurrent writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write appends p.
func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String is what was written.
func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureStreams replaces os.Stdout and os.Stderr with pipes while f runs and
// returns what each carried. The tests using it do not run in parallel.
func captureStreams(t *testing.T, f func()) (stdout, stderr string) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = outW, errW
	var wg sync.WaitGroup
	var outBytes, errBytes []byte
	wg.Add(2)
	go func() { defer wg.Done(); outBytes, _ = io.ReadAll(outR) }()
	go func() { defer wg.Done(); errBytes, _ = io.ReadAll(errR) }()
	func() {
		defer func() {
			os.Stdout, os.Stderr = oldOut, oldErr
			_ = outW.Close()
			_ = errW.Close()
		}()
		f()
	}()
	wg.Wait()
	return string(outBytes), string(errBytes)
}

// script writes one line to each stream, one to hide on each, and a last line
// without a break on each.
const script = `echo keep-out; echo hide-out; echo keep-err >&2; echo hide-err >&2; printf last-out; printf last-err >&2`

func TestRunShowsAndHidesLines(t *testing.T) {
	hide := func(line string) bool { return strings.HasPrefix(line, "hide") }

	t.Run("Tee shows each stream on its own and leaves out the hidden lines, capturing all", func(t *testing.T) {
		var result run.Result
		stdout, stderr := captureStreams(t, func() {
			var err error
			result, err = New().Run(context.Background(), run.Spec{Command: "sh", Args: []string{"-c", script}, Output: run.Tee, HideLine: hide})
			if err != nil {
				t.Error(err)
			}
		})
		if stdout != "keep-out\nlast-out" || stderr != "keep-err\nlast-err" {
			t.Errorf("stdout = %q, stderr = %q", stdout, stderr)
		}
		if !strings.Contains(result.Stdout, "hide-out") || !strings.Contains(result.Stderr, "hide-err") {
			t.Errorf("captured %+v, want the hidden lines too", result)
		}
	})

	t.Run("TeeToStderr shows both streams on stderr", func(t *testing.T) {
		stdout, stderr := captureStreams(t, func() {
			if _, err := New().Run(context.Background(), run.Spec{Command: "sh", Args: []string{"-c", script}, Output: run.TeeToStderr, HideLine: hide}); err != nil {
				t.Error(err)
			}
		})
		if stdout != "" || !strings.Contains(stderr, "keep-out\n") || !strings.Contains(stderr, "keep-err\n") || strings.Contains(stderr, "hide") || !strings.Contains(stderr, "last-out") || !strings.Contains(stderr, "last-err") {
			t.Errorf("stdout = %q, stderr = %q", stdout, stderr)
		}
	})

	t.Run("Capture shows nothing, and the lines are still observed", func(t *testing.T) {
		var mu sync.Mutex
		var seen []string
		stdout, stderr := captureStreams(t, func() {
			if _, err := New().Run(context.Background(), run.Spec{Command: "sh", Args: []string{"-c", script}, Output: run.Capture,
				Observe: func(line string) { mu.Lock(); seen = append(seen, line); mu.Unlock() }}); err != nil {
				t.Error(err)
			}
		})
		if stdout != "" || stderr != "" {
			t.Errorf("stdout = %q, stderr = %q", stdout, stderr)
		}
		if len(seen) != 6 {
			t.Errorf("observed %q, want all six lines (none is hidden)", seen)
		}
	})
}

func TestTranscript(t *testing.T) {
	if run.Transcript(false) != run.Capture || run.Transcript(true) != run.TeeToStderr {
		t.Fatalf("Transcript(false) = %v, Transcript(true) = %v", run.Transcript(false), run.Transcript(true))
	}
}
