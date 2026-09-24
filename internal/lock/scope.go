package lock

import (
	"time"

	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/procid"
)

// Scope identifies one lock target and how to describe it to a person, plus
// the markers that pick this scope's own processes out of a scan.
type Scope struct {
	Target      string
	Description string
	Markers     procid.Markers
}

// Timeouts the design assigns to each of the three scopes.
const (
	// OperationTimeout is how long an operation waits for a busy worktree before giving up.
	OperationTimeout = 25 * time.Minute
	// RegistryTimeout guards a few milliseconds of JSON writes, so contenders
	// poll quickly and wait patiently rather than failing after a handful of slow retries.
	RegistryTimeout = 60 * time.Second
	// RegistryRetry is the registry lock's fixed poll interval.
	RegistryRetry = 50 * time.Millisecond
	// DevelopmentTimeout is held for the length of a dev session, so it is
	// not something to wait out: say so quickly.
	DevelopmentTimeout = 1 * time.Second
	// RouterTimeout is how long `router up` waits for another control home's
	// ensure to finish before giving up. It must cover Ensure's own worst
	// case, or a waiter could give up on a holder that is still working
	// within its own budget: previousProtocol's probe pair (2 * 30s), a
	// `compose up --wait` (5m), a full verify pass across all three
	// services (6 * 30s = 3m), one recreate-and-reverify when it drifted
	// (5m + 3m), and a protocol write. 60s + 5m + 3m + 5m + 3m = 17m, so
	// 20m leaves comfortable room. (The probe and start timeouts referenced
	// here are internal/router's dockerProbeTimeout and startTimeout.)
	//
	// Not every caller of this same lock scope waits this long: the checkout
	// index (internal/checkoutindex.Service) shares RouterScope's target but
	// times its own wait far shorter (60s, matching TypeScript's own
	// INDEX_LOCK_TIMEOUT_MS), since it is best-effort and must never make
	// `down`, `worktree remove`, or `up` itself stall behind a daemon that
	// will not answer.
	RouterTimeout = 20 * time.Minute
)

// RegistryScope is the machine-wide worktree registry lock.
func RegistryScope(controlHome string) Scope {
	return Scope{
		Target:      layout.ControlLayout(controlHome).RegistryLockPath,
		Description: "global worktree registry",
	}
}

// RouterScope is the shared router's lock (#12): one per user account, in
// the router's own state directory rather than any control home, so two
// control homes on the same machine wait on each other instead of taking
// turns recreating the router (#13). projectName marks this lock's own
// Compose children for recovery, the way a worktree's markers do.
// internal/commands' routerLockScope builds the same scope from an
// already-resolved layout; keep the two in step.
func RouterScope(getenv layout.Getenv, projectName string) (Scope, error) {
	state, err := layout.RouterState(getenv)
	if err != nil {
		return Scope{}, err
	}
	return Scope{
		Target:      state.LockPath,
		Description: "shared router",
		Markers:     procid.Markers{Project: projectName},
	}, nil
}

// WorktreeScopes is one worktree's operation and development-session locks,
// described the way a waiter has always been told about them: "worktree
// <name>" and "<name> development session".
func WorktreeScopes(controlHome, repositoryID, worktreeID, name string, markers procid.Markers) (operation, development Scope) {
	l := layout.WorktreeLayout(controlHome, repositoryID, worktreeID)
	operation = Scope{Target: l.LockPath, Description: "worktree " + name, Markers: markers}
	development = Scope{Target: l.DevelopmentLockPath, Description: name + " development session", Markers: markers}
	return operation, development
}
