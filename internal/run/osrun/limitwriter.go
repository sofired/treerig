package osrun

import (
	"bytes"
	"sync"
)

// limitWriter captures up to limit bytes of a stream, then silently drops
// the rest — this process still owns the pipe and must keep draining it so
// the child is not blocked on a full pipe buffer — and calls overflow once,
// the first time the limit is crossed.
type limitWriter struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	limit    int
	overflow func()
	over     bool
}

func newLimitWriter(limit int, overflow func()) *limitWriter {
	return &limitWriter{limit: limit, overflow: overflow}
}

func (w *limitWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.over {
		return len(p), nil
	}
	remaining := w.limit - w.buf.Len()
	if remaining <= 0 {
		w.over = true
	} else if len(p) > remaining {
		w.buf.Write(p[:remaining])
		w.over = true
	} else {
		w.buf.Write(p)
	}
	if w.over && w.overflow != nil {
		w.overflow()
	}
	return len(p), nil
}

func (w *limitWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}
