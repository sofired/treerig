package lock

import (
	"time"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/procid"
)

// Scope identifies one lock target and how to describe it to a person, plus
// the markers that pick this scope's own processes out of a scan.
type Scope struct {
	Target      string
	Description string
	Markers     procid.Markers
	// HomeEnvVar names the environment variable that governs where Target's
	// directory lives, so CheckFS's refusal of an untrusted filesystem
	// points at the one a person can actually move (#80): brand.ControlHomeEnv
	// for a control-home-rooted scope (the registry, a worktree's own
	// locks), brand.HomeEnv for the router's, which has no control-home
	// override of its own. Left "" only by tests that never reach CheckFS's
	// refusal.
	HomeEnvVar string
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
	// within its own budget: ten bounded Docker probes (the router
	// container's `compose ps` and `inspect`, `docker info` and `context
	// inspect`, the index container's two, three holder lookups and the log
	// read after a failed start: 10 * 30s = 5m) and a `compose up --wait`
	// (5m). 5m + 5m = 10m, so 20m leaves comfortable room. (The probe and
	// start timeouts referenced here are internal/router's dockerProbeTimeout
	// and startTimeout.)
	//
	// Not every caller of this same lock scope waits this long: the checkout
	// index (internal/checkoutindex.Service) shares the router's own lock
	// target (internal/commands' routerLockScope) but times its own wait far
	// shorter (60s), since it is best-effort and must never make `down`,
	// `worktree remove`, or `up` itself stall behind a daemon that will not
	// answer.
	RouterTimeout = 20 * time.Minute
	// SharedTimeout is how long a command waits for a repository's shared
	// lock (docs/design/routing.md, decision 18). It covers the longest thing
	// done under it: a `compose up --wait` of five minutes, a pull, and a
	// recreate, as RouterTimeout is derived. 3 * 5m = 15m.
	SharedTimeout = 15 * time.Minute
	// ReconnectTimeout is how long the reconnect sweep waits for one
	// worktree's operation lock (and the shared lock beside it) before it
	// skips that worktree: a busy worktree is repaired by its own next `up`,
	// by `shared up`, or found by `doctor`, never waited for.
	ReconnectTimeout = 5 * time.Second
)

// RegistryScope is the machine-wide worktree registry lock.
func RegistryScope(controlHome string) Scope {
	return Scope{
		Target:      layout.ControlLayout(controlHome).RegistryLockPath,
		Description: "global worktree registry",
		HomeEnvVar:  brand.ControlHomeEnv,
	}
}

// SharedScope is one repository's shared lock: one lock for all of the
// repository's shared services (docs/design/routing.md, decision 18), at
// <control home>/locks/shared/<repo>. Its markers name the shared Compose
// project, so a survivor scan finds a `compose up` it started.
func SharedScope(controlHome, repositoryID string) Scope {
	return Scope{
		Target:      layout.SharedLayout(controlHome, repositoryID).LockPath,
		Description: "shared-services",
		Markers:     procid.Markers{Project: layout.SharedProjectName(repositoryID)},
		HomeEnvVar:  brand.ControlHomeEnv,
	}
}

// WorktreeScopes is one worktree's operation and development-session locks,
// described the way a waiter has always been told about them: "worktree
// <name>" and "<name> development session".
func WorktreeScopes(controlHome, repositoryID, worktreeID, name string, markers procid.Markers) (operation, development Scope) {
	l := layout.WorktreeLayout(controlHome, repositoryID, worktreeID)
	operation = Scope{Target: l.LockPath, Description: "worktree " + name, Markers: markers, HomeEnvVar: brand.ControlHomeEnv}
	development = Scope{Target: l.DevelopmentLockPath, Description: name + " development session", Markers: markers, HomeEnvVar: brand.ControlHomeEnv}
	return operation, development
}
