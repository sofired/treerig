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
)

// RegistryScope is the machine-wide worktree registry lock.
func RegistryScope(controlHome string) Scope {
	return Scope{
		Target:      layout.ControlLayout(controlHome).RegistryLockPath,
		Description: "global worktree registry",
	}
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
