package router

// Router state: the two ports that belong to the router rather than to any
// worktree's registration (docs/design/routing.md, decisions 7 and 8). The
// overview is published on its own loopback port, and in host mode the
// ping entrypoint listens on one. Both are allocated once, from the same
// range as routed services' ports, under the router lock, and stored in one
// file under the treerig root. Worktree allocation reads the file and treats
// both as reserved (registry.PortPlan.Reserved).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/files"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/ports"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/stateformat"
)

// stateVersion is the version of the state file's shape.
const stateVersion = stateformat.RouterState

// State is the router's own allocated ports.
type State struct {
	Version int `json:"version"`
	// IndexPort is the loopback port the overview is published on.
	IndexPort int `json:"indexPort"`
	// PingPort is the loopback port of the host-mode ping entrypoint; 0
	// until the router first runs in host mode.
	PingPort int `json:"pingPort,omitempty"`
}

// Reserved are the ports worktree allocation must skip.
func (s State) Reserved() []int {
	var reserved []int
	for _, port := range []int{s.IndexPort, s.PingPort} {
		if port > 0 {
			reserved = append(reserved, port)
		}
	}
	return reserved
}

// ReadState reads the router state file under state. A missing file is the
// zero State (found false). It takes no lock: the file is only ever
// replaced through files.AtomicJSON.
func ReadState(state layout.RouterLayout) (State, bool, error) {
	if err := files.AssertSafeFilePathNamed(state.StateDir, state.StateFile, stateNoun); err != nil {
		return State{}, false, err
	}
	data, err := os.ReadFile(state.StateFile)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return State{}, false, nil
		}
		return State{}, false, clierr.Newf("Could not read the router state %s: %s", state.StateFile, err)
	}
	// A newer treerig's file is judged by its version alone: the rest of its
	// shape is not this build's to read, and "remove it" would throw away
	// ports a newer router still holds.
	if verdict, found := stateformat.JSON(data, stateVersion); verdict == stateformat.FromNewer {
		return State{}, false, stateformat.Newer("use", "the router state "+state.StateFile, found, stateVersion)
	}
	var parsed State
	if err := json.Unmarshal(data, &parsed); err != nil || parsed.Version != stateVersion {
		return State{}, false, clierr.Newf(
			"The router state %s is not readable. Remove it and run this again; the router picks its ports afresh.", state.StateFile)
	}
	return parsed, true, nil
}

// usablePort reports whether a stored port can still be used: a port that
// is not the router's own port and not already given to the other.
func usablePort(port, routerPort int, others ...int) bool {
	if port < 1 || port > 65535 || port == routerPort {
		return false
	}
	for _, other := range others {
		if port == other {
			return false
		}
	}
	return true
}

// ensureState returns the router's ports for mode, allocating the ones that
// are missing and writing the file. The caller holds the router lock. A
// stored port that is no longer usable (it became the router's own port) is
// replaced.
func (r *Router) ensureState(ctx context.Context, mode Mode) (State, error) {
	stored, _, err := ReadState(r.state)
	if err != nil {
		return State{}, err
	}
	next := State{Version: stateVersion, IndexPort: stored.IndexPort, PingPort: stored.PingPort}
	if !usablePort(next.IndexPort, r.Port) {
		next.IndexPort = 0
	}
	if !usablePort(next.PingPort, r.Port, next.IndexPort) {
		next.PingPort = 0
	}
	needIndex := next.IndexPort == 0
	needPing := mode == ModeHost && next.PingPort == 0
	if !needIndex && !needPing {
		if next == stored {
			return stored, nil
		}
		// A stored port that became unusable (the router's own, say) is
		// dropped from the file too, so it does not stay reserved.
		if err := files.AssertSafeFilePathNamed(r.state.StateDir, r.state.StateFile, stateNoun); err != nil {
			return State{}, err
		}
		if err := files.AtomicJSON(r.state.StateFile, next); err != nil {
			return State{}, err
		}
		return next, nil
	}

	// Every port in any registration is taken; a record that cannot be read
	// may hold one, so the allocation refuses rather than guess.
	taken, err := registry.TakenPorts(r.controlHome, next.Reserved())
	if err != nil {
		return State{}, err
	}
	taken[r.Port] = true
	probe := r.rangeProbe()
	if needIndex {
		port, err := ports.Pick(ctx, r.portRange, probe, taken, "router", "index")
		if err != nil {
			return State{}, noStatePortError("the overview", r.portRange, err)
		}
		taken[port] = true
		next.IndexPort = port
	}
	if needPing {
		port, err := ports.Pick(ctx, r.portRange, probe, taken, "router", "ping")
		if err != nil {
			return State{}, noStatePortError("the router's ping", r.portRange, err)
		}
		next.PingPort = port
	}
	if err := files.AssertSafeFilePathNamed(r.state.StateDir, r.state.StateFile, stateNoun); err != nil {
		return State{}, err
	}
	if err := files.AtomicJSON(r.state.StateFile, next); err != nil {
		return State{}, err
	}
	return next, nil
}

// noStatePortError is the refusal for a router port ports.Pick could not
// allocate; a context error passes through unchanged.
func noStatePortError(what string, r layout.PortRange, err error) error {
	switch {
	case errors.Is(err, ports.ErrExhausted):
		return clierr.Newf("No free port for %s in %s: every one is allocated to a worktree or in use. Widen [ports] range in settings.toml.", what, r)
	case errors.Is(err, ports.ErrProbeLimit):
		return clierr.Newf("No free port for %s in %s: gave up after probing %d busy ports in a row. Choose a range in settings.toml that nothing else listens in.",
			what, r, ports.MaxBusyProbes)
	}
	return fmt.Errorf("allocating a port for %s: %w", what, err)
}
