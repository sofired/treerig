package lifecycle

import (
	"os"
	"sync"
	"syscall"
)

// fakeOS is an injectable OS for tests: it never touches real signal
// delivery and never actually exits.
type fakeOS struct {
	mu      sync.Mutex
	ch      chan<- os.Signal
	ignored map[os.Signal]bool
	// notified is the signals the last Notify asked for.
	notified []os.Signal

	stopped   bool
	resetSig  os.Signal
	killed    []killCall
	exitCodes []int
}

type killCall struct {
	pid int
	sig syscall.Signal
}

// newFakeOS returns a fakeOS with no signals ignored, ready to hand to
// NewLifecycle in place of New().
func newFakeOS() *fakeOS { return &fakeOS{ignored: map[os.Signal]bool{}} }

// Notify records the channel Start subscribed, so a later send can deliver
// to it the way the real signal package would.
func (f *fakeOS) Notify(c chan<- os.Signal, sig ...os.Signal) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ch = c
	f.notified = sig
}

// Stop records that Stop was called; it does not close or clear the channel.
func (f *fakeOS) Stop(c chan<- os.Signal) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = true
}

// Ignored reports the disposition a test set up via f.ignored, never real
// process state.
func (f *fakeOS) Ignored(sig os.Signal) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ignored[sig]
}

// Reset records the last signal reset, for a test to assert on.
func (f *fakeOS) Reset(sig os.Signal) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resetSig = sig
}

// Kill records the call instead of sending a real signal, and always
// succeeds.
func (f *fakeOS) Kill(pid int, sig syscall.Signal) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killed = append(f.killed, killCall{pid, sig})
	return nil
}

// Getpid returns a fixed, recognisable fake pid.
func (f *fakeOS) Getpid() int { return 4242 }

// Exit records the exit code instead of ending the process, so a test can
// keep running after the code under test calls Exit.
func (f *fakeOS) Exit(code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exitCodes = append(f.exitCodes, code)
}

// send simulates signal delivery: it writes directly to the channel Notify
// was given, exactly as the real signal package would deliver to it.
func (f *fakeOS) send(sig os.Signal) {
	f.mu.Lock()
	ch := f.ch
	f.mu.Unlock()
	ch <- sig
}

// exitCalls returns a copy of every code passed to Exit, in call order.
func (f *fakeOS) exitCalls() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.exitCodes...)
}

// killCalls returns a copy of every pid/signal pair passed to Kill, in call
// order.
func (f *fakeOS) killCalls() []killCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]killCall(nil), f.killed...)
}
