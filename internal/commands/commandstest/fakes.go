package commandstest

import (
	"context"
	"sync"

	"github.com/sofired/treerig/internal/commands"
)

var (
	_ commands.Router = (*FakeRouter)(nil)
	_ commands.Index  = (*FakeIndex)(nil)
)

// FakeRouter is a commands.Router double for `up` tests. BeforeEnsure runs a
// hook just before Ensure answers, so a test can simulate "the router took a
// while, and something else happened to the registration meanwhile."
type FakeRouter struct {
	mu      sync.Mutex
	port    int
	before  func()
	err     error
	ensures int
}

// NewFakeRouter answers port exactly, and ensures cleanly by default.
func NewFakeRouter(port int) *FakeRouter {
	return &FakeRouter{port: port}
}

// Port is the fixed port NewFakeRouter was given.
func (r *FakeRouter) Port() int { return r.port }

// Ensure runs BeforeEnsure's hook, if one is set, then answers FailEnsure's
// error, if one is set, or nil otherwise. Every call counts toward Ensures.
func (r *FakeRouter) Ensure(context.Context) error {
	r.mu.Lock()
	before, err := r.before, r.err
	r.ensures++
	r.mu.Unlock()
	if before != nil {
		before()
	}
	return err
}

// BeforeEnsure runs f the next time (and every time) Ensure is called, just
// before it answers.
func (r *FakeRouter) BeforeEnsure(f func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.before = f
}

// FailEnsure makes every later Ensure call answer err.
func (r *FakeRouter) FailEnsure(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

// Ensures is how many times Ensure was called.
func (r *FakeRouter) Ensures() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ensures
}

// FakeIndex is a commands.Index double: it records every refresh reason
// instead of touching a real overview.
type FakeIndex struct {
	mu      sync.Mutex
	reasons []string
}

// RefreshBestEffort records reason instead of touching a real overview; see Reasons.
func (i *FakeIndex) RefreshBestEffort(_ context.Context, reason string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.reasons = append(i.reasons, reason)
}

// Reasons is every reason RefreshBestEffort was called with, in order.
func (i *FakeIndex) Reasons() []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]string(nil), i.reasons...)
}
