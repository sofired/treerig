// Package lifecycle is one signal handler for the whole process, started
// once in main, plus Hold: the lock-ordered, signal-deferring wrapper every
// mutating command runs its work through. It replaces
// src/signal-lifecycle.ts and src/commands/mutation.ts's withWorktreeMutation.
package lifecycle

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/logger"
)

// OS is the process-level surface Lifecycle needs, injectable so tests never
// touch real signal delivery or actually exit.
type OS interface {
	Notify(c chan<- os.Signal, sig ...os.Signal)
	Stop(c chan<- os.Signal)
	// Ignored reports whether sig is currently ignored (SIG_IGN); such a
	// signal stays ignored only if it is never passed to Notify.
	Ignored(sig os.Signal) bool
	// Reset restores sig's default disposition, undoing Notify's handling of it.
	Reset(sig os.Signal)
	Kill(pid int, sig syscall.Signal) error
	Getpid() int
	// Exit ends the process. Real code never returns from it; a test double may.
	Exit(code int)
}

// realOS is the production OS, wrapping the os/os.signal/syscall packages.
type realOS struct{}

// New wires signal.Notify, signal.Stop, signal.Ignored, signal.Reset,
// syscall.Kill, os.Getpid and os.Exit — the real process.
func New() OS { return realOS{} }

func (realOS) Notify(c chan<- os.Signal, sig ...os.Signal) { signal.Notify(c, sig...) }
func (realOS) Stop(c chan<- os.Signal)                     { signal.Stop(c) }
func (realOS) Ignored(sig os.Signal) bool                  { return signal.Ignored(sig) }
func (realOS) Reset(sig os.Signal)                         { signal.Reset(sig) }

// Kill sends sig. Sent to this process, it is delivered on a thread of the
// runtime's choosing, so the caller is held here until it ends the process:
// falling through to Exit at once would win that race and exit 128+n instead
// of dying by the signal.
func (realOS) Kill(pid int, sig syscall.Signal) error {
	if err := syscall.Kill(pid, sig); err != nil {
		return err
	}
	if pid == os.Getpid() {
		time.Sleep(2 * time.Second)
	}
	return nil
}
func (realOS) Getpid() int   { return os.Getpid() }
func (realOS) Exit(code int) { os.Exit(code) }

// Lifecycle is the process's one signal handler. Nothing starts listening
// until Start is called, typically once from main.
type Lifecycle struct {
	os     OS
	logger logger.Logger

	ctx    context.Context
	cancel context.CancelCauseFunc

	mu      sync.Mutex
	pending syscall.Signal
	printed bool
	sigCh   chan os.Signal
}

// NewLifecycle builds a Lifecycle over osIface (New() for the real process) and log.
func NewLifecycle(osIface OS, log logger.Logger) *Lifecycle {
	l := &Lifecycle{os: osIface, logger: log}
	l.ctx, l.cancel = context.WithCancelCause(context.Background())
	return l
}

// Context is the root context: cancelled, with cause *clierr.Cancelled, by
// the first SIGINT or SIGTERM this process receives.
func (l *Lifecycle) Context() context.Context { return l.ctx }

// Start begins listening for SIGINT and SIGTERM, skipping either one this
// process already had ignored at startup — Notifying an ignored signal would
// make it stop being ignored, which is never this tool's call to make.
func (l *Lifecycle) Start() {
	candidates := []os.Signal{syscall.SIGINT, syscall.SIGTERM}
	var toNotify []os.Signal
	for _, s := range candidates {
		if !l.os.Ignored(s) {
			toNotify = append(toNotify, s)
		}
	}
	if len(toNotify) == 0 {
		return
	}
	ch := make(chan os.Signal, 4)
	l.mu.Lock()
	l.sigCh = ch
	l.mu.Unlock()
	l.os.Notify(ch, toNotify...)
	go l.loop(ch)
}

// loop is handed its channel rather than reading sigCh, which Stop clears.
func (l *Lifecycle) loop(ch <-chan os.Signal) {
	for raw := range ch {
		sig, ok := raw.(syscall.Signal)
		if !ok {
			continue
		}
		l.mu.Lock()
		first := l.pending == 0
		if first {
			l.pending = sig
		}
		alreadyPrinted := l.printed
		if !first {
			l.printed = true
		}
		l.mu.Unlock()

		if first {
			l.cancel(&clierr.Cancelled{Signal: sig})
			continue
		}
		if !alreadyPrinted {
			l.logger.Info("Still stopping — the current operation is finishing up before it exits.")
		}
	}
}

// Stop ends listening: the registration with the signal package and the
// goroutine reading it. Main calls it when a command finished without being
// interrupted, so nothing a Lifecycle started outlives the command — which
// matters wherever Main runs more than once in a process, as in tests.
func (l *Lifecycle) Stop() {
	l.mu.Lock()
	ch := l.sigCh
	l.sigCh = nil
	l.mu.Unlock()
	if ch == nil {
		return
	}
	l.os.Stop(ch)
	close(ch)
}

// Pending is the first signal this process received, or zero if none has.
func (l *Lifecycle) Pending() syscall.Signal {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.pending
}

// Exit re-raises the pending signal now that this process's own handling of
// it is done: signal.Reset, then kill(getpid, sig), so the process dies by
// that signal as a shell expects. If it somehow survives that — the test
// double, or a signal the kernel does not actually deliver to self — it
// falls back to Exit(128+n). A process with no pending signal does nothing.
func (l *Lifecycle) Exit() {
	sig := l.Pending()
	if sig == 0 {
		return
	}
	l.mu.Lock()
	ch := l.sigCh
	l.mu.Unlock()
	if ch != nil {
		l.os.Stop(ch)
	}
	l.os.Reset(sig)
	_ = l.os.Kill(l.os.Getpid(), sig)
	l.os.Exit(128 + int(sig))
}
