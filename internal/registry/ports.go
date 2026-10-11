package registry

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strings"

	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/files"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/ports"
)

// PortPlan is what SyncPorts makes true of one worktree's ports.
type PortPlan struct {
	// Services are the routed services that need a port.
	Services []string
	// Release drops the port of a service that holds one and is not listed
	// in Services. Without it such a port is kept: a container of the dropped
	// service may still hold it until a full `compose up` has removed the
	// orphan, so the caller releases in a second call after that.
	Release bool
	// Reallocate names services whose current port is replaced by a new
	// one, never the one they held.
	Reallocate []string
	// Range is where new ports come from.
	Range layout.PortRange
	// Probe decides whether a candidate is free; nil means ports.LoopbackProbe.
	Probe ports.Probe
	// Reserved are ports the caller says are taken though no registration
	// holds them (the router's index and ping ports, #120).
	Reserved []int
}

// PortResult is what SyncPorts did.
type PortResult struct {
	// Ports is every service's port after the call.
	Ports map[string]int
	// Allocated are the ports newly assigned, including reallocations.
	Allocated map[string]int
	// Moved are the previous ports of reallocated services.
	Moved map[string]int
	// Released are the ports of services no longer listed.
	Released map[string]int
}

// SyncPorts makes worktreeID's recorded ports match plan under the registry
// lock: a listed service without a port gets one, a reallocated service gets
// a new one, and, with plan.Release, an unlisted service's port is released. Nothing is written
// when nothing changes, and nothing is scanned unless a port must be
// allocated.
//
// A new port is ports.Pick over the range, keyed by (worktree ID, service),
// past every taken port (takenPorts) and every port the probe reports busy.
// A registry that holds an unreadable record refuses the allocation: that
// record may hold ports.
func (s *Store) SyncPorts(ctx context.Context, worktreeID, expectedSlug string, writer Writer, plan PortPlan) (PortResult, error) {
	if err := files.SecureRegistrationDirectories(layout.WorktreeLayout(s.ControlHome, s.RepositoryID, worktreeID), writer.UserHome); err != nil {
		return PortResult{}, err
	}
	probe := plan.Probe
	if probe == nil {
		probe = ports.LoopbackProbe{}
	}
	var result PortResult
	err := s.withRegistryLock(ctx, writer, func(ctx context.Context) error {
		file, stored, err := s.readOwnedRecord(worktreeID, expectedSlug)
		if err != nil {
			return err
		}
		current := stored.Registration.Ports
		wanted := map[string]bool{}
		for _, service := range plan.Services {
			wanted[service] = true
		}
		moving := map[string]bool{}
		for _, service := range plan.Reallocate {
			moving[service] = true
		}

		next := maps.Clone(current)
		if next == nil {
			next = map[string]int{}
		}
		result = PortResult{Allocated: map[string]int{}, Moved: map[string]int{}, Released: map[string]int{}}
		for service, port := range current {
			if !wanted[service] && plan.Release {
				result.Released[service] = port
				delete(next, service)
			}
		}
		var needy []string
		for _, service := range plan.Services {
			if _, held := current[service]; !held || moving[service] {
				needy = append(needy, service)
			}
		}

		if len(needy) > 0 {
			taken, err := takenPorts(s.ControlHome, plan.Reserved)
			if err != nil {
				return err
			}
			for _, service := range needy {
				port, err := ports.Pick(ctx, plan.Range, probe, taken, worktreeID, service)
				if err != nil {
					return noPortError(service, plan.Range, err)
				}
				taken[port] = true
				if previous, held := current[service]; held {
					result.Moved[service] = previous
				}
				result.Allocated[service] = port
				next[service] = port
			}
		}

		result.Ports = next
		if len(result.Allocated) == 0 && len(result.Released) == 0 {
			return nil
		}
		registration := stored.Registration
		registration.Ports = next
		return files.AtomicJSON(file, recordFor(registration, stored.Initialization))
	})
	if err != nil {
		return PortResult{}, err
	}
	return result, nil
}

// takenPorts are the ports the allocator must not hand out: every port in
// any registration of controlHome, plus reserved. A record that cannot be
// read refuses the call, since it may hold ports. Shared-service records
// (#123) join here.
func takenPorts(controlHome string, reserved []int) (map[int]bool, error) {
	scan, err := ScanRegistrations(controlHome)
	if err != nil {
		return nil, err
	}
	if len(scan.Problems) > 0 {
		return nil, refuseUnreadable("a port",
			"a record that cannot be read may hold a port this worktree would take.", scan.Problems)
	}
	taken := map[int]bool{}
	for _, record := range scan.Records {
		for _, port := range record.Ports {
			taken[port] = true
		}
	}
	for _, port := range reserved {
		taken[port] = true
	}
	return taken, nil
}

// noPortError is the refusal for an allocation ports.Pick could not make; a
// context error passes through unchanged.
func noPortError(service string, r layout.PortRange, err error) error {
	switch {
	case errors.Is(err, ports.ErrExhausted):
		return clierr.Newf(
			"No free port for %s in %s: every one is allocated to a worktree or in use. "+
				"Widen [ports] range in settings.toml, or remove worktrees you no longer use.", service, r)
	case errors.Is(err, ports.ErrProbeLimit):
		return clierr.Newf(
			"No free port for %s in %s: gave up after probing %d busy ports in a row. "+
				"Choose a range in settings.toml that nothing else listens in.", service, r, ports.MaxBusyProbes)
	}
	return err
}

// ReleasePorts drops every port from worktreeID's record under the registry
// lock. It is idempotent: a record without ports, or no record at all, is
// success. `worktree remove` calls it once the worktree's containers are
// gone and before the record itself is deleted, so an interrupted removal
// leaves the ports reachable.
func (s *Store) ReleasePorts(ctx context.Context, worktreeID, expectedSlug string, writer Writer) error {
	if err := files.SecureRegistrationDirectories(layout.WorktreeLayout(s.ControlHome, s.RepositoryID, worktreeID), writer.UserHome); err != nil {
		return err
	}
	return s.withRegistryLock(ctx, writer, func(context.Context) error {
		file := s.RecordPath(worktreeID)
		stored, err := s.readStoredRecord(file)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if stored.Registration.Slug != expectedSlug {
			return clierr.Newf("Worktree %s no longer owns registration %s", expectedSlug, file)
		}
		if len(stored.Registration.Ports) == 0 {
			return nil
		}
		registration := stored.Registration
		registration.Ports = nil
		return files.AtomicJSON(file, recordFor(registration, stored.Initialization))
	})
}

// PortClaim is one service's claim on a port.
type PortClaim struct {
	Worktree RegisteredWorktree
	Service  string
}

// PortConflict is a port recorded for more than one service, in one worktree
// or several. Two control homes can allocate the same port at once
// (docs/design/routing.md, decision 3a); this is how `doctor` finds it.
type PortConflict struct {
	Port   int
	Claims []PortClaim
}

// PortConflicts are the ports claimed more than once across records, by port.
func PortConflicts(records []RegisteredWorktree) []PortConflict {
	byPort := map[int][]PortClaim{}
	for _, record := range records {
		for service, port := range record.Ports {
			byPort[port] = append(byPort[port], PortClaim{Worktree: record, Service: service})
		}
	}
	var conflicts []PortConflict
	for _, port := range slices.Sorted(maps.Keys(byPort)) {
		claims := byPort[port]
		if len(claims) < 2 {
			continue
		}
		slices.SortFunc(claims, func(a, b PortClaim) int {
			if a.Worktree.WorktreeID != b.Worktree.WorktreeID {
				return strings.Compare(a.Worktree.WorktreeID, b.Worktree.WorktreeID)
			}
			return strings.Compare(a.Service, b.Service)
		})
		conflicts = append(conflicts, PortConflict{Port: port, Claims: claims})
	}
	return conflicts
}

// String names the claim for a message: "<service> of <worktree ID>".
func (c PortClaim) String() string {
	return fmt.Sprintf("%s of %s", c.Service, c.Worktree.WorktreeID)
}

// TakenPorts is takenPorts for a caller outside this package that allocates
// a port beside the registrations without holding the registry lock: the
// router, for its index and ping ports (#120). It reads every record
// without a lock, which is safe because every registry writer goes through
// files.AtomicJSON, a temporary file then a rename.
func TakenPorts(controlHome string, reserved []int) (map[int]bool, error) {
	return takenPorts(controlHome, reserved)
}
