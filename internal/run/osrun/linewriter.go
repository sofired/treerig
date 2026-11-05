package osrun

import (
	"bytes"
	"io"
	"sync"
)

// maxPendingLine is the longest unterminated line a lineWriter holds back:
// past it the bytes are judged and passed on as a line of their own, so a
// child that never writes a line break cannot grow the buffer without bound.
const maxPendingLine = 64 * 1024

// lineWriter passes a child's output on a line at a time. Each complete line
// is handed to observe and shown on dst (a nil dst shows nothing), unless hide
// says no: a hidden line is neither shown nor observed. With neither hide nor observe it is dst itself.
type lineWriter struct {
	mu      sync.Mutex
	dst     io.Writer
	hide    func(line string) bool
	observe func(line string)
	pending []byte
}

// newLineWriter is the constructor Run uses.
func newLineWriter(dst io.Writer, hide func(line string) bool, observe func(line string)) *lineWriter {
	return &lineWriter{dst: dst, hide: hide, observe: observe}
}

// Write forwards the complete lines of p, holding back a trailing partial line
// until its line break, flush, or maxPendingLine arrives. It reports len(p)
// whatever dst does, as a tee must: showing the output is never a reason to
// fail the child.
func (w *lineWriter) Write(p []byte) (int, error) {
	if w.hide == nil && w.observe == nil {
		if w.dst != nil {
			_, _ = w.dst.Write(p)
		}
		return len(p), nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pending = append(w.pending, p...)
	for {
		end := bytes.IndexByte(w.pending, '\n')
		if end < 0 {
			break
		}
		w.emit(w.pending[:end+1])
		w.pending = w.pending[end+1:]
	}
	if len(w.pending) > maxPendingLine {
		w.emit(w.pending)
		w.pending = nil
	}
	return len(p), nil
}

// flush forwards a trailing partial line, once the child has ended.
func (w *lineWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.pending) > 0 {
		w.emit(w.pending)
		w.pending = nil
	}
}

// emit shows one line, break included, unless hide rejects it, and observes it.
func (w *lineWriter) emit(line []byte) {
	text := string(bytes.TrimRight(line, "\r\n"))
	if w.hide != nil && w.hide(text) {
		return
	}
	if w.observe != nil {
		w.observe(text)
	}
	if w.dst != nil {
		_, _ = w.dst.Write(line)
	}
}
