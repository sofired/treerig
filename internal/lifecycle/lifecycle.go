// Package lifecycle is one signal handler for the whole process, started
// once in main, plus Hold: the lock-ordered, signal-deferring wrapper every
// mutating command runs its work through.
package lifecycle

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/logger"
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

// Notify delegates to signal.Notify.
func (realOS) Notify(c chan<- os.Signal, sig ...os.Signal) { signal.Notify(c, sig...) }

// Stop delegates to signal.Stop.
func (realOS) Stop(c chan<- os.Signal) { signal.Stop(c) }

// Ignored delegates to signal.Ignored.
func (realOS) Ignored(sig os.Signal) bool { return signal.Ignored(sig) }

// Reset delegates to signal.Reset.
func (realOS) Reset(sig os.Signal) { signal.Reset(sig) }

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

// Getpid delegates to os.Getpid.
func (realOS) Getpid() int { return os.Getpid() }

// Exit delegates to os.Exit; like it, this never returns.
func (realOS) Exit(code int) { os.Exit(code) }

// Lifecycle is the process's one signal handler. Nothing starts listening
// until Start is called, typically once from main.
type Lifecycle struct {
	os     OS
	logger logger.Logger

	ctx    context.Context
	cancel context.CancelCauseFunc

	mu       sync.Mutex
	pending  syscall.Signal
	printed  bool
	sigCh    chan os.Signal
	repeated chan struct{}
}

// NewLifecycle builds a Lifecycle over osIface (New() for the real process) and log.
func NewLifecycle(osIface OS, log logger.Logger) *Lifecycle {
	l := &Lifecycle{os: osIface, logger: log, repeated: make(chan struct{})}
	l.ctx, l.cancel = context.WithCancelCause(context.Background())
	return l
}

// Context is the root context: cancelled, with cause *clierr.Cancelled, by
// the first SIGINT, SIGTERM or SIGHUP this process receives.
func (l *Lifecycle) Context() context.Context { return l.ctx }

// Repeated is a channel that closes the moment a second SIGINT or SIGTERM
// arrives after the first — the signal that means "stop waiting, abandon
// whatever is in flight" rather than merely "stop starting new work," which
// the first signal (cancelling Context) already asked for. A bounded wait
// still in progress when the first signal lands (an interactive child's own
// process-group drain, for instance) selects on this to learn when to give
// up early instead of running out its full budget. Carry it to code that
// cannot import this package with run.WithRepeated (internal/run, not this
// one, so osrun can read it back without importing internal/lifecycle);
// internal/lifecycle remains the only thing in this process that calls
// signal.Notify.
func (l *Lifecycle) Repeated() <-chan struct{} { return l.repeated }

// Start begins listening for SIGINT, SIGTERM and SIGHUP — the last being
// what a closed terminal sends, which `treerig run` must forward to its child
// and wait out like the other two — skipping any of them this process
// already had ignored at startup: Notifying an ignored signal would make it
// stop being ignored, which is never this tool's call to make (a process
// started under nohup keeps ignoring SIGHUP).
func (l *Lifecycle) Start() {
	candidates := []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP}
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
		// A later SIGHUP is nothing: a hangup often arrives twice within
		// milliseconds (the kernel's, then the shell's relay), and the
		// second is not an operator asking to stop waiting. Only a repeated
		// SIGINT or SIGTERM is "Still stopping" and the abandon signal.
		if !first && sig == syscall.SIGHUP {
			l.mu.Unlock()
			continue
		}
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
			close(l.repeated)
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

// Finish turns a command's own top-level error into this process's exit
// code — the one call every entry point should route through once the
// command has run and Stop has been called. err is reported through
// report unless it is nothing more than the plain fact that a signal
// already interrupted this process (err is nil, or unwraps to
// *clierr.Cancelled): that alone stays silent, since a signal death,
// below, already says it. When a signal was received and err is that
// plain, Exit takes it from here and this call never returns: the process
// dies by the signal, as a shell expects. Otherwise Finish returns the
// exit code err itself calls for (clierr.Code(err)), or 0 for a nil err —
// so an error that carries more than the interruption alone (an
// interactive child's own process-group drain that timed out or was
// abandoned by a second signal, say) reaches report and this process
// exits with its own code, rather than being silently lost to a signal
// death that prints nothing of its own. report may be nil.
func (l *Lifecycle) Finish(err error, report func(error)) int {
	var cancelled *clierr.Cancelled
	plain := err == nil || errors.As(err, &cancelled)
	if err != nil && !plain && report != nil {
		report(err)
	}
	if l.Pending() != 0 && plain {
		l.Exit() // real code never returns from this
	}
	if err != nil {
		return clierr.Code(err)
	}
	return 0
}
