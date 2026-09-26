package commandstest

import (
	"context"
	"sync"

	"github.com/sofired/berth/internal/commands"
)

var (
	_ commands.Router = (*FakeRouter)(nil)
	_ commands.Index  = (*FakeIndex)(nil)
)

// FakeRouter is a commands.Router double for `up` tests. BeforeEnsure runs a
// hook just before Ensure answers, so a test can simulate "the router took a
// while, and something else happened to the registration meanwhile."
type FakeRouter struct {
	mu               sync.Mutex
	port             int
	network          string
	before           func()
	err              error
	ensures          int
	connected        []string
	connectErr       error
	disconnected     []string
	disconnectErr    error
	beforeDisconnect func()
}

// NewFakeRouter answers port and network exactly, and ensures cleanly by default.
func NewFakeRouter(port int, network string) *FakeRouter {
	return &FakeRouter{port: port, network: network}
}

// Port is the fixed port NewFakeRouter was given.
func (r *FakeRouter) Port() int { return r.port }

// Network is the fixed network name NewFakeRouter was given.
func (r *FakeRouter) Network() string { return r.network }

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

// ConnectNetwork records network and answers FailConnectNetwork's error, if
// one is set, or nil otherwise.
func (r *FakeRouter) ConnectNetwork(_ context.Context, network string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.connected = append(r.connected, network)
	return r.connectErr
}

// Connected is every network ConnectNetwork was called with, in order.
func (r *FakeRouter) Connected() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.connected...)
}

// FailConnectNetwork makes every later ConnectNetwork call answer err.
func (r *FakeRouter) FailConnectNetwork(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.connectErr = err
}

// DisconnectNetwork runs BeforeDisconnectNetwork's hook, if one is set, then
// records network and answers FailDisconnectNetwork's error, if one is set,
// or nil otherwise.
func (r *FakeRouter) DisconnectNetwork(_ context.Context, network string) error {
	r.mu.Lock()
	before, err := r.beforeDisconnect, r.disconnectErr
	r.disconnected = append(r.disconnected, network)
	r.mu.Unlock()
	if before != nil {
		before()
	}
	return err
}

// BeforeDisconnectNetwork runs f the next time (and every time)
// DisconnectNetwork is called, just before it answers — so a test can
// check what else has already happened (e.g. that this worktree's own
// fragments are already gone) at the exact moment of the call.
func (r *FakeRouter) BeforeDisconnectNetwork(f func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.beforeDisconnect = f
}

// Disconnected is every network DisconnectNetwork was called with, in order.
func (r *FakeRouter) Disconnected() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.disconnected...)
}

// FailDisconnectNetwork makes every later DisconnectNetwork call answer err.
func (r *FakeRouter) FailDisconnectNetwork(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.disconnectErr = err
}

// FakeIndex is a commands.Index double: it records every refresh reason
// instead of touching a real checkout index.
type FakeIndex struct {
	mu      sync.Mutex
	reasons []string
}

// RefreshBestEffort records reason instead of touching a real checkout index; see Reasons.
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
