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

	stopped   bool
	resetSig  os.Signal
	killed    []killCall
	exitCodes []int
}

type killCall struct {
	pid int
	sig syscall.Signal
}

func newFakeOS() *fakeOS { return &fakeOS{ignored: map[os.Signal]bool{}} }

func (f *fakeOS) Notify(c chan<- os.Signal, sig ...os.Signal) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ch = c
}

func (f *fakeOS) Stop(c chan<- os.Signal) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = true
}

func (f *fakeOS) Ignored(sig os.Signal) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ignored[sig]
}

func (f *fakeOS) Reset(sig os.Signal) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resetSig = sig
}

func (f *fakeOS) Kill(pid int, sig syscall.Signal) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killed = append(f.killed, killCall{pid, sig})
	return nil
}

func (f *fakeOS) Getpid() int { return 4242 }

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

func (f *fakeOS) exitCalls() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.exitCodes...)
}

func (f *fakeOS) killCalls() []killCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]killCall(nil), f.killed...)
}
